//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type kongPlanFakeStatsRepo struct {
	upserts [][]KongPlanStatRow
	pruned  []time.Time
	err     error
}

func (r *kongPlanFakeStatsRepo) UpsertKongPlanStats(_ context.Context, rows []KongPlanStatRow) error {
	if r.err != nil {
		return r.err
	}
	r.upserts = append(r.upserts, append([]KongPlanStatRow(nil), rows...))
	return nil
}

func (r *kongPlanFakeStatsRepo) ListKongPlanStats(context.Context, time.Time, time.Time) ([]KongPlanStatRow, error) {
	var out []KongPlanStatRow
	for _, u := range r.upserts {
		out = append(out, u...)
	}
	return out, nil
}

func (r *kongPlanFakeStatsRepo) PruneKongPlanStats(_ context.Context, before time.Time) error {
	r.pruned = append(r.pruned, before)
	return nil
}

func TestKongPlanStats_MergeFlushAndRetry(t *testing.T) {
	repo := &kongPlanFakeStatsRepo{}
	s := NewKongPlanStats(repo)
	key := KongPlanStatKey{Hour: time.Unix(1790000000, 0).UTC().Truncate(time.Hour), AccountID: 1, Layer: "credits", Seg: kongPlanStatsDwellSeg}
	s.add(key, KongPlanStatRow{DwellN: 1, DwellSumS: 10, DwellMaxS: 10})
	s.add(key, KongPlanStatRow{DwellN: 1, DwellSumS: 30, DwellMaxS: 30})

	repo.err = errors.New("db down")
	s.Flush(context.Background())
	require.Empty(t, repo.upserts)
	require.Len(t, s.rows, 1, "写库失败时放回，下次再写")

	repo.err = nil
	s.add(key, KongPlanStatRow{DwellN: 1, DwellSumS: 5, DwellMaxS: 5})
	s.Flush(context.Background())
	require.Len(t, repo.upserts, 1)
	require.Equal(t, KongPlanStatRow{KongPlanStatKey: key, DwellN: 3, DwellSumS: 45, DwellMaxS: 30}, repo.upserts[0][0])
	require.Empty(t, s.rows)
	require.Len(t, repo.pruned, 1, "每天清理一次 30 天前的行")
	s.Flush(context.Background())
	require.Len(t, repo.upserts, 1, "没有增量不写")
	require.Len(t, repo.pruned, 1)
}

func kongPlanStatRows(s *KongPlanStats) map[[3]string]KongPlanStatRow {
	out := map[[3]string]KongPlanStatRow{}
	for k, r := range s.rows {
		out[[3]string{string(rune('0' + k.AccountID)), k.Layer, k.Seg}] = *r
	}
	return out
}

func TestKongPlanSelect_RecordsSelectionAheadAndConfirmFailure(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 1, 1, 99)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.creditsEntry(2, 1)+"]", 2)
	h.control(t, 11, `"on"`, "[2]", 0)
	stats := NewKongPlanStats(nil)
	h.rt.stats = stats

	h.sess.cache.put(1, "other")
	require.Equal(t, int64(1), h.sess.selectAccount(t, context.Background(), "s-new").Account.ID)
	rows := kongPlanStatRows(stats)
	grace := rows[[3]string{"1", "subscription", "grace"}]
	require.Equal(t, int64(1), grace.Selected, "宽限段被选中")
	require.Equal(t, int64(1), grace.SessionsSum, "选中时已有的会话数")

	h.sess.cache.put(1, "third")
	require.Equal(t, int64(2), h.sess.selectAccount(t, context.Background(), "s-next").Account.ID)
	rows = kongPlanStatRows(stats)
	require.Equal(t, int64(1), rows[[3]string{"2", "credits", "credits_room"}].Selected)
	require.Equal(t, int64(1), rows[[3]string{"1", "subscription", "overflow"}].AheadLimit, "前面的第一层账号已到上限加宽限")
	for k := range stats.rows {
		require.Equal(t, KongPlanInUseSnapshot, k.InUse)
		require.Equal(t, int64(1), k.PlanVersion)
		require.Positive(t, k.PublishSeq)
	}

	// 名额被别的请求先占：记一次确认失败，降到宽限段再选中
	th2 := newKongPlanTierHarness(t)
	h2 := newKongPlanSelHarness(t, []Account{th2.selAccount(1, 1, 1, 50), th2.selAccount(2, 2, 1, 99)}, stubConcurrencyCache{},
		"["+th2.creditsEntry(2, 1)+"]", 2)
	stats2 := NewKongPlanStats(nil)
	h2.rt.stats = stats2
	h2.sess.cache.stealOnRegister[1] = "thief"
	require.Equal(t, int64(1), h2.sess.selectAccount(t, context.Background(), "s-new").Account.ID)
	rows = kongPlanStatRows(stats2)
	require.Equal(t, int64(1), rows[[3]string{"1", "subscription", "room"}].ConfirmFailed)
	require.Equal(t, int64(1), rows[[3]string{"1", "subscription", "grace"}].Selected)
}

func TestKongPlanSelect_RecordsWaited(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 99)}
	h := newKongPlanSelHarness(t, accounts, kongSessionAllBusy(1, 2), "["+th.creditsEntry(2, 1)+"]", 2)
	stats := NewKongPlanStats(nil)
	h.rt.stats = stats
	h.sess.cache.put(1, "a")
	h.sess.cache.put(1, "b")
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.NotNil(t, sel.WaitPlan)
	row := kongPlanStatRows(stats)[[3]string{"2", "credits", "credits_room"}]
	require.Equal(t, int64(1), row.Selected)
	require.Equal(t, int64(1), row.Waited, "第 3 层选中即排队等槽")
	require.Equal(t, int64(1), kongPlanStatRows(stats)[[3]string{"1", "subscription", "overflow"}].AheadLimit)
}

func TestKongPlanSticky_RecordsCreditsDwell(t *testing.T) {
	h := newKongPlanTierHarness(t)
	b := newKongPlanFakeBindings()
	h.rt.bindings = b
	stats := NewKongPlanStats(nil)
	h.rt.stats = stats
	svc := &OpenAIGatewayService{}
	group := int64(3)
	acc := h.account(99, 10)
	key := svc.openAISessionCacheKey("s1")
	require.NoError(t, b.MarkSession(context.Background(), group, key, acc.ID, 4, h.now.Add(-10*time.Minute), time.Hour))

	require.True(t, svc.kongPlanReuseSticky(context.Background(), &group, "s1", acc, true))
	row := kongPlanStatRows(stats)[[3]string{"1", "credits", kongPlanStatsDwellSeg}]
	require.Equal(t, KongPlanStatRow{KongPlanStatKey: row.KongPlanStatKey, DwellN: 1, DwellSumS: 600, DwellMaxS: 600}, row)

	b.markedAt[kongPlanMarkerKey(group, key, acc.ID, 4)] = time.Time{} // 早期版本的标记：读不出时刻
	require.True(t, svc.kongPlanReuseSticky(context.Background(), &group, "s1", acc, true))
	require.Equal(t, int64(1), kongPlanStatRows(stats)[[3]string{"1", "credits", kongPlanStatsDwellSeg}].DwellN, "读不出时刻不记")
}

func TestKongPlanStats_ListReportsWriteFailure(t *testing.T) {
	repo := &kongPlanFakeStatsRepo{err: errors.New("db down")}
	s := NewKongPlanStats(repo)
	key := KongPlanStatKey{Hour: time.Unix(1790000000, 0).UTC().Truncate(time.Hour), AccountID: 1, Layer: "credits", Seg: "credits_room"}
	s.add(key, KongPlanStatRow{Selected: 1})
	_, err := s.List(context.Background(), key.Hour, key.Hour.Add(time.Hour))
	require.Error(t, err, "增量写不进去时不返回缺了它的结果")
	require.Len(t, s.rows, 1)
	repo.err = nil
	rows, err := s.List(context.Background(), key.Hour, key.Hour.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, []KongPlanStatRow{{KongPlanStatKey: key, Selected: 1}}, rows, "恢复后只计一次")
}

func TestKongPlanSelect_RecordsBusyAccountFilteredBeforeSequence(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 0, 50), th.selAccount(2, 2, 0, 99)}
	h := newKongPlanSelHarness(t, accounts, kongSessionAllBusy(1), "["+th.creditsEntry(2, 1)+"]", 2)
	stats := NewKongPlanStats(nil)
	h.rt.stats = stats
	require.Equal(t, int64(2), h.sess.selectAccount(t, context.Background(), "s-new").Account.ID)
	rows := kongPlanStatRows(stats)
	require.Equal(t, int64(1), rows[[3]string{"1", "subscription", "room"}].AheadBusy, "满并发、被过滤出尝试序列的第一层账号也记为忙")
	require.Equal(t, int64(1), rows[[3]string{"2", "credits", "credits_room"}].Selected)
}

func TestKongPlanSelect_RecordsRecheckedTier(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[1]", 0)
	h.publish(t, 1, "["+h.creditsEntry(1, 1)+"]")
	stats := NewKongPlanStats(nil)
	h.rt.stats = stats
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5}
	listed, fresh := h.selAccount(1, 5, 0, 98), h.selAccount(1, 5, 0, 99)
	ctx := context.Background()
	p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&listed, &relay}, nil, nil)
	for _, acc := range p.sequenceAccounts([]*Account{&listed, &relay}) {
		if !p.next(acc.ID) {
			continue
		}
		cur := acc
		if acc.ID == 1 {
			cur = &fresh
		}
		if p.confirm(ctx, nil, cur) {
			break
		}
	}
	rows := kongPlanStatRows(stats)
	require.Equal(t, int64(1), rows[[3]string{"1", "credits", "credits_room"}].Selected, "改判之后按 credits 层记")
	require.Zero(t, rows[[3]string{"1", "subscription", "credits_room"}].Selected)
}
