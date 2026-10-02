//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 余额记账接在节奏组件的刷新协程上：观测登记、补记、提交协议、入层与结算。

type kongPlanFakeLedgerStore struct {
	mu      sync.Mutex
	obs     [][]byte
	entered map[string]time.Time
	left    map[string]time.Time
	settled map[string]time.Time
	open    map[int64]map[int64]bool
}

func newKongPlanFakeLedgerStore() *kongPlanFakeLedgerStore {
	return &kongPlanFakeLedgerStore{entered: map[string]time.Time{}, left: map[string]time.Time{},
		settled: map[string]time.Time{}, open: map[int64]map[int64]bool{}}
}

func kongPlanFakeKey(id, seq int64) string { return fmt.Sprintf("%d:%d", id, seq) }

func (s *kongPlanFakeLedgerStore) PushObservation(_ context.Context, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.obs = append(s.obs, append([]byte(nil), raw...))
	return nil
}

func (s *kongPlanFakeLedgerStore) ListObservations(context.Context) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.obs...), nil
}

func (s *kongPlanFakeLedgerStore) MarkEntered(_ context.Context, id, seq int64, at time.Time) (bool, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := kongPlanFakeKey(id, seq)
	if t, ok := s.entered[k]; ok {
		return false, t, s.open[id][seq], nil
	}
	s.entered[k] = at
	if s.open[id] == nil {
		s.open[id] = map[int64]bool{}
	}
	s.open[id][seq] = true
	return true, at, true, nil
}

func (s *kongPlanFakeLedgerStore) MarkLeft(_ context.Context, id, seq int64, at time.Time) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := kongPlanFakeKey(id, seq)
	if t, ok := s.left[k]; ok {
		return t, nil
	}
	s.left[k] = at
	return at, nil
}

func (s *kongPlanFakeLedgerStore) LoadOpenEntries(context.Context) (map[int64][]KongPlanTierEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64][]KongPlanTierEntry{}
	for id, m := range s.open {
		for seq := range m {
			e := KongPlanTierEntry{Seq: seq, EnteredAt: s.entered[kongPlanFakeKey(id, seq)]}
			if t, ok := s.left[kongPlanFakeKey(id, seq)]; ok {
				e.LeftAt = &t
			}
			out[id] = append(out[id], e)
		}
	}
	return out, nil
}

func (s *kongPlanFakeLedgerStore) apply(extra KongPaceCommitExtra) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, raw := range extra.DoneObservations {
		for i, v := range s.obs {
			if string(v) == string(raw) {
				s.obs = append(s.obs[:i], s.obs[i+1:]...)
				break
			}
		}
	}
	for _, st := range extra.Settlements {
		s.settled[kongPlanFakeKey(st.AccountID, st.Seq)] = st.At
		delete(s.open[st.AccountID], st.Seq)
	}
}

// kongPlanFakePaceStore 把节奏状态与计划组件的部分放在一个"事务"里提交；saveErr 非空时返回错误，
// commitOnErr 为真时仍然落下这次写入（模拟已提交、确认丢失）。
type kongPlanFakePaceStore struct {
	mu          sync.Mutex
	saved       map[int64][]byte
	ledger      *kongPlanFakeLedgerStore
	saveErr     error
	commitOnErr bool
}

func (s *kongPlanFakePaceStore) LoadAccountStates(context.Context) (map[int64][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64][]byte{}
	for k, v := range s.saved {
		out[k] = v
	}
	return out, nil
}

func (s *kongPlanFakePaceStore) SaveAccountStates(_ context.Context, states map[int64][]byte, deleted []int64, extra KongPaceCommitExtra) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil && !s.commitOnErr {
		return s.saveErr
	}
	if s.saved == nil {
		s.saved = map[int64][]byte{}
	}
	for k, v := range states {
		s.saved[k] = v
	}
	for _, id := range deleted {
		delete(s.saved, id)
	}
	s.ledger.apply(extra)
	return s.saveErr
}

func (s *kongPlanFakePaceStore) PublishState(context.Context, map[int64][]byte, []byte, time.Duration) error {
	return nil
}

type kongPlanLedgerHarness struct {
	pace   *KongOpenAIAccountPace
	repo   *kongPaceFakeRepo
	store  *kongPlanFakePaceStore
	ls     *kongPlanFakeLedgerStore
	ledger *KongPlanLedger
	plan   *KongPlanStore
	now    time.Time
}

// kongPlanForecastWith 拼一份只带账号参数的慢速部分；extra 是另加的顶层字段（空或以逗号结尾），accounts 直接给 JSON 数组。
func kongPlanForecastWith(extra, accounts string) string {
	return `{"cycle_id": "c", "plan_version": 1, "computed_at": "2026-10-02T06:00:00Z", ` + extra + `
		"interval_min": 60, "per_session_pp_per_hour": 1.5, "avg_session_pp": 0.8, "demand_estimate_pp_per_hour": 1,
		"accounts": ` + accounts + `}`
}

// kongPlanPutForecast 写一份只带账号参数的慢速部分（不给单位倍数）。
func kongPlanPutForecast(t *testing.T, s *KongPlanStore, accounts string) {
	t.Helper()
	kongPlanPutForecastBody(t, s, kongPlanForecastWith("", accounts))
}

// kongPlanPutForecastUnit 写一份带单位倍数的慢速部分。
func kongPlanPutForecastUnit(t *testing.T, s *KongPlanStore, unit float64, accounts string) {
	t.Helper()
	kongPlanPutForecastBody(t, s, kongPlanForecastWith(fmt.Sprintf(`"unit_multiple": %g,`, unit), accounts))
}

func kongPlanPutForecastBody(t *testing.T, s *KongPlanStore, body string) {
	t.Helper()
	f, err := ParseKongPlanForecast(strings.NewReader(body))
	require.NoError(t, err)
	_, err = s.PutForecast(context.Background(), f)
	require.NoError(t, err)
}

// newKongPlanLedgerHarness 建一套推进者与记账；慢速部分里每个账号的套餐系数都是 1。
func newKongPlanLedgerHarness(t *testing.T, rows []KongPaceAccountRow, perPoint string) *kongPlanLedgerHarness {
	t.Helper()
	plan, _ := newKongPlanTestStore(t, newKongPlanFakeRepo())
	ctl, err := ParseKongPlanControl(strings.NewReader(`{"revision": 1, "credits_mode": "on", "allow": [1], "floor": 0,
		"per_point": ` + perPoint + `, "overflow_margin": {"default": 0}}`))
	require.NoError(t, err)
	_, err = plan.PutControl(context.Background(), ctl)
	require.NoError(t, err)
	accs := make([]string, len(rows))
	for i, r := range rows {
		accs[i] = fmt.Sprintf(`{"id": %d, "k": 1, "depth_pp_per_hour": 2.5}`, r.ID)
	}
	kongPlanPutForecast(t, plan, "["+strings.Join(accs, ",")+"]")
	h := &kongPlanLedgerHarness{repo: &kongPaceFakeRepo{rows: rows}, ls: newKongPlanFakeLedgerStore(), plan: plan, now: kongPaceT0}
	h.store = &kongPlanFakePaceStore{ledger: h.ls}
	h.ledger = NewKongPlanLedger(h.ls, plan)
	h.pace = NewKongOpenAIAccountPace(h.repo, h.store, nil, t.TempDir()+"/absent.yaml")
	h.pace.now = func() time.Time { return h.now }
	h.pace.SetKongPlanLedger(h.ledger)
	return h
}

func (h *kongPlanLedgerHarness) observe(t *testing.T, id int64, at time.Time, balance *float64) {
	t.Helper()
	usage := &OpenAIQuotaUsage{FetchedAt: at.Unix()}
	if balance != nil {
		v := fmt.Sprintf("%g", *balance)
		usage.Credits = &OpenAICredits{HasCredits: true, Balance: &v}
	}
	h.ledger.Observe(context.Background(), id, usage)
}

func (h *kongPlanLedgerHarness) state(t *testing.T, id int64) kongPaceAccountState {
	t.Helper()
	snap := h.pace.snapshot.Load()
	require.NotNil(t, snap)
	f, ok := snap.accounts[id]
	require.True(t, ok)
	return f.State
}

func kongPlanCreditPoints(st kongPaceAccountState) float64 {
	var sum float64
	for _, r := range st.Rises {
		if r.Credits {
			sum += r.Points
		}
	}
	return sum
}

func kongPlanF(v float64) *float64 { return &v }

var kongPlanLedgerRows = []KongPaceAccountRow{kongPaceRow(1, "pro", 99, kongPaceT0.Add(72*time.Hour), kongPaceT0.Add(-time.Minute))}

// 没有节奏配置：刷新协程照常推进窗口序号与记账，但不做重排（active 为空）。
func TestKongPlanLedger_AdvancesWithoutPaceConfig(t *testing.T) {
	h := newKongPlanLedgerHarness(t, kongPlanLedgerRows, "2")
	h.pace.factsTick()
	require.Nil(t, h.pace.active.Load())
	st := h.state(t, 1)
	require.Equal(t, int64(1), st.WindowSeq)
	require.Contains(t, h.store.saved, int64(1))
}

func TestKongPlanLedger_ObservationsBecomeCreditRises(t *testing.T) {
	h := newKongPlanLedgerHarness(t, kongPlanLedgerRows, "2")
	h.observe(t, 1, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	// 推进者处理之前，保底判断已经看得到这条观测
	require.Equal(t, 1000.0, h.ledger.LatestBalance(1, nil).Value)
	h.pace.factsTick()
	require.Equal(t, 1000.0, h.state(t, 1).Balance.Value)
	require.Empty(t, h.ls.obs, "处理完的观测随节奏状态一起删除")

	h.observe(t, 1, kongPaceT0.Add(time.Minute), kongPlanF(980))
	h.now = kongPaceT0.Add(2 * time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 10.0, kongPlanCreditPoints(st))
	require.Equal(t, 10.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "软线的近 6h 消耗包含 credits 部分")
}

// 写入结果不确定：本拍不换快照，下一拍从 Redis 重新载入；不论那次写入有没有落下，同一次下降只记一次。
func TestKongPlanLedger_UncertainCommitReloadsWithoutDoubleCount(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			h := newKongPlanLedgerHarness(t, kongPlanLedgerRows, "2")
			h.observe(t, 1, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
			h.pace.factsTick()
			before := h.pace.snapshot.Load()

			h.observe(t, 1, kongPaceT0.Add(time.Minute), kongPlanF(980))
			h.now = kongPaceT0.Add(2 * time.Minute)
			h.store.saveErr, h.store.commitOnErr = errors.New("确认丢失"), committed
			h.pace.factsTick()
			require.Same(t, before, h.pace.snapshot.Load(), "写入不确定时不换快照")
			require.False(t, h.pace.statesReady)

			h.store.saveErr, h.store.commitOnErr = nil, false
			h.now = kongPaceT0.Add(3 * time.Minute)
			h.pace.factsTick()
			require.Equal(t, 10.0, kongPlanCreditPoints(h.state(t, 1)))
			require.Empty(t, h.ls.obs)
		})
	}
}

func TestKongPlanLedger_EnterLeaveSettle(t *testing.T) {
	ctx := context.Background()
	h := newKongPlanLedgerHarness(t, kongPlanLedgerRows, "2")
	h.observe(t, 1, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	h.pace.factsTick()
	seq := h.state(t, 1).WindowSeq

	require.NoError(t, h.ledger.MarkEntered(ctx, 1, seq, kongPaceT0))
	require.NoError(t, h.ledger.MarkEntered(ctx, 1, seq, kongPaceT0.Add(time.Minute)), "同一窗口第二次不访问 Redis")
	entries := h.ledger.Entries(1)
	require.Len(t, entries, 1)
	require.Equal(t, kongPaceT0, entries[0].EnteredAt)
	st := h.state(t, 1)
	require.True(t, kongPlanAtLine(entries, &st, h.now))

	leftAt := kongPaceT0.Add(10 * time.Minute)
	require.NoError(t, h.ledger.MarkLeft(ctx, 1, seq, leftAt))
	h.now = kongPaceT0.Add(20 * time.Minute)
	h.pace.factsTick()
	require.False(t, h.ledger.Entries(1)[0].Settled, "离层之后还没有观测：不结算")

	h.observe(t, 1, kongPaceT0.Add(15*time.Minute), kongPlanF(960))
	h.now = kongPaceT0.Add(16 * time.Minute)
	h.store.saveErr = errors.New("写入失败")
	h.pace.factsTick()
	require.False(t, h.ledger.Entries(1)[0].Settled, "提交失败时镜像不标结算")
	h.store.saveErr = nil
	h.pace.factsTick()
	entries = h.ledger.Entries(1)
	require.True(t, entries[0].Settled)
	st = h.state(t, 1)
	require.Equal(t, 20.0, kongPlanCreditPoints(st), "结算时离层前的消耗已经可见")
	require.False(t, kongPlanAtLine(entries, &st, h.now))
	require.Contains(t, h.ls.settled, kongPlanFakeKey(1, seq))

	// 重启：镜像从 Redis 读回，已结算的不再回来
	reloaded := NewKongPlanLedger(h.ls, nil)
	require.NoError(t, reloaded.reload(ctx))
	require.Empty(t, reloaded.Entries(1))
	require.NoError(t, reloaded.MarkEntered(ctx, 1, seq, kongPaceT0.Add(time.Hour)))
	require.True(t, reloaded.Entries(1)[0].Settled, "同一窗口再次入层不重新打开")
}

// Spark 影子账号的 credits 是父账号的：不为它记账。
func TestKongPlanLedger_ShadowObservationsIgnored(t *testing.T) {
	row := kongPaceRow(2, "pro", 99, kongPaceT0.Add(72*time.Hour), kongPaceT0.Add(-time.Minute))
	parent := int64(1)
	row.ParentAccountID = &parent
	h := newKongPlanLedgerHarness(t, []KongPaceAccountRow{row}, "2")
	h.observe(t, 2, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	h.pace.factsTick()
	require.Nil(t, h.state(t, 2).Balance)
	require.Empty(t, h.ls.obs)
}

func TestKongPlanLedger_ObservationWireFormat(t *testing.T) {
	ls := newKongPlanFakeLedgerStore()
	l := NewKongPlanLedger(ls, nil)
	unlimited := &OpenAIQuotaUsage{FetchedAt: 100, Credits: &OpenAICredits{Unlimited: true}}
	l.Observe(context.Background(), 7, unlimited)
	var obs kongPlanObservation
	require.NoError(t, json.Unmarshal(ls.obs[0], &obs))
	require.Nil(t, obs.Balance, "无限额度没有余额数值")
	require.Equal(t, int64(7), obs.AccountID)
	require.Equal(t, time.Unix(100, 0).UTC(), obs.At)
	require.NotEmpty(t, obs.ID)
}

func TestKongPlanBalanceOf_RejectsNonFinite(t *testing.T) {
	of := func(s string) *float64 {
		return kongPlanBalanceOf(&OpenAIQuotaUsage{Credits: &OpenAICredits{HasCredits: true, Balance: &s}})
	}
	for _, s := range []string{"NaN", "Inf", "+Infinity", "-inf"} {
		require.Nil(t, of(s), s)
	}
	require.Equal(t, 12.5, *of(" 12.5 "))
}
