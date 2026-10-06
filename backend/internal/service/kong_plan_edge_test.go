//go:build unit

package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 计划组件的边界情形：换算溢出、离层证据早于入层、重载与离层并发、降段位置的桶内顺序、确认时的最新定层、
// 写入结果不确定后的重载、A 路径全局阈值、WS 逐轮复核、续接绑定不删。

func TestKongPlanApplyObservation_TinyPerPointMarksUnknown(t *testing.T) {
	st := &kongPaceAccountState{}
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	tiny := 1e-320
	kongPlanApplyObservation(st, kongPlanObservation{At: t0, Balance: kongPlanF(1000)}, &tiny, 1, false)
	kongPlanApplyObservation(st, kongPlanObservation{At: t0.Add(time.Hour), Balance: kongPlanF(999)}, &tiny, 1, false)
	for _, r := range st.Rises {
		require.False(t, math.IsInf(r.Points, 0), "换算结果溢出不能记成消耗")
	}
	require.Equal(t, t0.Add(time.Hour).Add(kongPlanUnknownHold), st.UnknownUntil, "按未知处理")
	require.Equal(t, 999.0, st.Balance.Value)
}

func TestKongPlanPace_PersistAllOrNothing(t *testing.T) {
	store := &kongPlanFakePaceStore{ledger: newKongPlanFakeLedgerStore()}
	p := NewKongOpenAIAccountPace(&kongPaceFakeRepo{}, store, nil, t.TempDir()+"/absent.yaml")
	p.states = map[int64]*kongPaceAccountState{
		1: {Plan: "pro"},
		2: {Plan: "pro", Rises: []kongPaceRise{{At: time.Now(), Points: math.Inf(1)}}},
	}
	require.False(t, p.persist(context.Background(), KongPaceCommitExtra{DoneObservations: [][]byte{[]byte("x")}}))
	require.Empty(t, store.saved, "任何账号编码失败都整拍不提交")
}

func TestKongPlanLedger_LeaveBeforeEntryIgnored(t *testing.T) {
	ctx := context.Background()
	ledger := NewKongPlanLedger(newKongPlanFakeLedgerStore(), nil)
	t1 := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	require.NoError(t, ledger.MarkEntered(ctx, 1, 4, t1))
	require.NoError(t, ledger.MarkLeft(ctx, 1, 4, t1.Add(-time.Minute)))
	require.Nil(t, ledger.Entries(1)[0].LeftAt, "入层之前取到的同窗口旧读数不是离层证据")
	require.NoError(t, ledger.MarkLeft(ctx, 1, 4, t1))
	require.Nil(t, ledger.Entries(1)[0].LeftAt, "同一时刻也不算")
	require.NoError(t, ledger.MarkLeft(ctx, 1, 4, t1.Add(time.Minute)))
	require.NotNil(t, ledger.Entries(1)[0].LeftAt)
}

// reloadRaceStore 在读出未结算的入层之后、合并之前，模拟请求路径并发写下离层。
type reloadRaceStore struct {
	*kongPlanFakeLedgerStore
	during func()
}

func (s *reloadRaceStore) LoadOpenEntries(ctx context.Context) (map[int64][]KongPlanTierEntry, error) {
	out, err := s.kongPlanFakeLedgerStore.LoadOpenEntries(ctx)
	if s.during != nil {
		s.during()
		s.during = nil
	}
	return out, err
}

func TestKongPlanLedger_ReloadKeepsConcurrentLeave(t *testing.T) {
	ctx := context.Background()
	store := &reloadRaceStore{kongPlanFakeLedgerStore: newKongPlanFakeLedgerStore()}
	ledger := NewKongPlanLedger(store, nil)
	t1 := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	require.NoError(t, ledger.MarkEntered(ctx, 1, 4, t1))
	store.during = func() { require.NoError(t, ledger.MarkLeft(ctx, 1, 4, t1.Add(time.Hour))) }
	require.NoError(t, ledger.reload(ctx))
	e := ledger.Entries(1)[0]
	require.NotNil(t, e.LeftAt, "重载不能用读到的旧条目覆盖期间写下的离层时刻")
	require.True(t, e.LeftAt.Equal(t1.Add(time.Hour)))
}

func TestKongPlanOrder_GhostAfterTargetBucketMembers(t *testing.T) {
	a := kongPlanFacts{ID: 1, Tier: kongPlanTierResult{Layer: kongPlanLayerSubscription,
		Entry: KongPlanEntry{Tier: KongPlanTierAsap, Active: true, AdmitBelow: &KongPlanAdmitBelow{H6: 50, H24: 50}}},
		SessionsKnown: true, SessionLimit: 2, Grace: 1, LoadKnown: true}
	b := kongPlanFacts{ID: 2, Tier: kongPlanTierResult{Layer: kongPlanLayerSubscription, Entry: KongPlanEntry{Tier: KongPlanTierNormal, Active: true}},
		Sessions: 2, SessionsKnown: true, SessionLimit: 2, Grace: 1, LoadKnown: true}
	slots := kongPlanOrder([]kongPlanFacts{a, b}, map[int64]kongPlanSeg{})
	var grace []string
	for _, s := range slots {
		if s.Key.Seg == kongPlanSegGrace {
			grace = append(grace, kongPlanSlotName(s))
		}
	}
	require.Equal(t, []string{"2", "1*"}, grace, "降段来的账号排在宽限段本来的成员之后")
}

func TestKongPlanSelect_ConfirmUsesRecheckedTier(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[2]", 0)
	h.publish(t, 1, "["+h.creditsEntry(2, 1)+"]")
	h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	listed := h.selAccount(2, 1, 0, 98) // 进入第 2 层时的读数还在第一层
	fresh := h.selAccount(2, 1, 0, 99)  // 复核时已到阈值
	p := (&OpenAIGatewayService{}).kongPlanBegin(context.Background(), []*Account{&listed}, nil, nil)
	require.Equal(t, kongPlanLayerSubscription, p.facts[2].Tier.Layer)
	require.False(t, p.confirm(context.Background(), nil, &fresh), "复核时才进 credits 层：不在第一层的位置上用它")
	require.Equal(t, kongPlanLayerCredits, p.facts[2].Tier.Layer, "记下新的定层")
	require.Empty(t, h.ledger.Entries(2))
	order := p.sequenceAccounts([]*Account{&listed})
	require.Len(t, order, 1)
	require.True(t, p.next(2))
	require.Equal(t, kongPlanSegCreditsRoom, p.cur.Key.Seg, "之后的轮次按 credits 层排")
	require.True(t, p.confirm(context.Background(), nil, &fresh))
	entries := h.ledger.Entries(2)
	require.Len(t, entries, 1, "按 credits 层确认时登记入层")
	require.Equal(t, int64(4), entries[0].Seq)

	h.lstore.enterErr = errors.New("redis down")
	other := h.selAccount(2, 1, 0, 99)
	h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 5,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	require.False(t, p.confirm(context.Background(), nil, &other), "登记不进去就不用这个账号的 credits")
}

func TestKongPlanStore_RuntimeReadReloadsAfterUncertainWrite(t *testing.T) {
	repo := newKongPlanFakeRepo()
	s, _ := newKongPlanTestStore(t, repo)
	req, err := ParseKongPlanHold(strings.NewReader(`{"trigger": "manual:t", "reason": "manual"}`))
	require.NoError(t, err)
	repo.saveErr, repo.commitOnErr = errors.New("连接在提交后断开"), true
	_, err = s.Hold(context.Background(), req)
	require.Error(t, err)
	repo.saveErr, repo.commitOnErr = nil, false
	require.Empty(t, s.state.Load().control.LatchedHolds, "内存里还是写入之前的状态")
	s.current()
	require.Eventually(t, func() bool {
		st := s.state.Load()
		return st != nil && len(st.control.LatchedHolds) == 1 && !s.stale.Load()
	}, 2*time.Second, 10*time.Millisecond, "选号路径读状态时在后台重载，看到已提交的暂停")
}

func TestKongPlanTier_AutoPauseGlobalFromRuntime(t *testing.T) {
	h := newKongPlanTierHarness(t)
	acc := h.account(95, 10)
	acc.Credentials = map[string]any{"account_scheduling_threshold": 99}
	require.Equal(t, kongPlanLayerSubscription, h.rt.tier(context.Background(), acc, h.now).Layer)
	h.rt.autoPause = func(context.Context) OpsOpenAIAccountQuotaAutoPauseSettings {
		return OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.99, DefaultThreshold7d: 0.9}
	}
	require.Equal(t, kongPlanLayerCredits, h.rt.tier(context.Background(), acc, h.now).Layer, "上下文里没有 A 的全局阈值时由运行时取")
	require.Equal(t, 90.0, h.rt.windowThresholdPercent(context.Background(), acc, "7d", h.now))
	ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.97})
	require.Equal(t, kongPlanLayerSubscription, h.rt.tier(ctx, acc, h.now).Layer, "上下文里已有的沿用")
}

func TestKongPlanWS_TurnRecheckServeability(t *testing.T) {
	h, _ := newKongPlanBindingHarness(t, nil)
	svc, ctx := h.sess.svc, context.Background()
	sub := h.selAccount(2, 5, 0, 50)
	st := svc.KongPlanWSBegin(ctx, &sub)

	paused5h := h.selAccount(2, 5, 0, 50)
	paused5h.Extra["codex_5h_used_percent"] = 99.5
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{paused5h}}
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &sub, st), "5h 到线")

	limited := h.selAccount(2, 5, 0, 50)
	until := time.Now().Add(time.Hour)
	limited.RateLimitResetAt = &until
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{limited}}
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &sub, st), "429 限流中")

	disabled := h.selAccount(2, 5, 0, 50)
	disabled.Schedulable = false
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{disabled}}
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &sub, st), "人工停用")

	credits := h.selAccount(1, 1, 0, 99)
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{credits}}
	cst := svc.KongPlanWSBegin(ctx, &credits)
	require.True(t, svc.KongPlanWSTurnAllowed(ctx, &credits, cst), "只到 7d 阈值的 credits 账号照常")
}

type kongPlanRecordingWSStore struct {
	OpenAIWSStateStore
	deleted []string
}

func (s *kongPlanRecordingWSStore) DeleteResponseAccount(_ context.Context, _ int64, responseID string) error {
	s.deleted = append(s.deleted, responseID)
	return nil
}

func TestKongPlanResponse_BindingKeptWhilePlanInEffect(t *testing.T) {
	h := newKongPlanTierHarness(t)
	rec := &kongPlanRecordingWSStore{}
	store := kongPlanKeepResponseBindings(rec)
	require.NoError(t, store.DeleteResponseAccount(context.Background(), 1, "resp_1"))
	require.Empty(t, rec.deleted, "计划输入生效时只跳过、不删除")

	req, err := ParseKongPlanPublish(strings.NewReader(`{"action": "disable", "clear": "all", "note": "回到现状调度"}`))
	require.NoError(t, err)
	_, err = h.store.PutPublish(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, store.DeleteResponseAccount(context.Background(), 1, "resp_1"))
	require.Equal(t, []string{"resp_1"}, rec.deleted, "现状调度时与上游相同")

	kongPlanRT.Store(nil)
	require.Same(t, rec, kongPlanKeepResponseBindings(rec).(*kongPlanRecordingWSStore), "未装配时原样返回")
}

func TestKongPlanOrder_GhostsInSameBucketByPriority(t *testing.T) {
	asap := KongPlanEntry{Tier: KongPlanTierAsap, Active: true, AdmitBelow: &KongPlanAdmitBelow{H6: 50, H24: 50}}
	normal := KongPlanEntry{Tier: KongPlanTierNormal, Active: true}
	fact := func(id int64, entry KongPlanEntry, sessions, priority int) kongPlanFacts {
		return kongPlanFacts{ID: id, Tier: kongPlanTierResult{Layer: kongPlanLayerSubscription, Entry: entry}, Priority: priority,
			Sessions: sessions, SessionsKnown: true, SessionLimit: 2, Grace: 1, LoadKnown: true}
	}
	grace := func(facts ...kongPlanFacts) []string {
		var out []string
		for _, s := range kongPlanOrder(facts, map[int64]kongPlanSeg{}) {
			if s.Key.Seg == kongPlanSegGrace {
				out = append(out, kongPlanSlotName(s))
			}
		}
		return out
	}
	// 输入按来源桶排（尽快档的 1 在前），两个重试位置之间按原优先级
	require.Equal(t, []string{"2*", "1*"}, grace(fact(1, asap, 0, 9), fact(2, normal, 0, 1)))
	require.Equal(t, []string{"3", "2*", "1*"}, grace(fact(1, asap, 0, 9), fact(2, normal, 0, 1), fact(3, normal, 2, 5)),
		"本来的成员仍在重试位置之前")
}

func TestKongPlanSticky_MarkAndReuseRegisterEntry(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	svc := h.sess.svc
	ctx := context.Background()
	group := int64(1)
	acc := h.selAccount(1, 1, 0, 99)

	// 等待计划之后才到阈值：选号时没有登记入层，写会话标记时补上
	require.NoError(t, svc.kongPlanMarkSession(ctx, &group, "openai:s1", 1, time.Minute))
	require.Contains(t, fake.sessions, kongPlanMarkerKey(1, "openai:s1", 1, 4))
	require.Len(t, h.ledger.Entries(1), 1, "写 credits 层的会话标记之前登记入层")

	// 新窗口：标记已在、入层还没有，复用时补登记；登记不进去就不复用
	h.setPace(1, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 5,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	fake.sessions[kongPlanMarkerKey(1, "openai:s1", 1, 5)] = time.Minute
	h.lstore.enterErr = errors.New("redis down")
	require.False(t, svc.kongPlanReuseSticky(ctx, &group, "s1", &acc, false))
	require.False(t, svc.kongPlanReuseResponse(ctx, 1, "resp_x", &acc))
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &acc, KongPlanWSState{credits: true, seq: 5}))
	require.False(t, svc.KongPlanWSBegin(ctx, &acc).credits, "建连时登记不进去：按不在 credits 层记下，逐轮复核会关闭")
	require.Error(t, svc.kongPlanMarkSession(ctx, &group, "openai:s2", 1, time.Minute))
	require.NotContains(t, fake.sessions, kongPlanMarkerKey(1, "openai:s2", 1, 5), "登记不进去就不写标记")
	h.lstore.enterErr = nil
	require.True(t, svc.kongPlanReuseSticky(ctx, &group, "s1", &acc, false))
	require.Len(t, h.ledger.Entries(1), 2)
}

func TestKongPlanResponse_MarkerUsesLatestAccountAndDetachedContext(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	svc := h.sess.svc
	stale := h.selAccount(1, 1, 0, 99) // 连接持有的旧对象
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{h.selAccount(1, 1, 0, 20), h.selAccount(2, 5, 0, 50)}}
	svc.kongPlanMarkResponse(context.Background(), 7, "resp_1", &stale, time.Hour)
	require.Empty(t, fake.responses, "最新读数已回到第一层：不写标记")

	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{h.selAccount(1, 1, 0, 99), h.selAccount(2, 5, 0, 50)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端收到终端事件就断开
	svc.kongPlanMarkResponse(ctx, 7, "resp_2", &stale, time.Hour)
	require.Contains(t, fake.responses, kongPlanMarkerKey(7, "resp_2", 1, 4), "请求上下文已取消也写得进")
	require.Len(t, h.ledger.Entries(1), 1)
}

type kongPlanGoneRepo struct{ stubOpenAIAccountRepo }

func (kongPlanGoneRepo) GetByID(context.Context, int64) (*Account, error) {
	return nil, ErrAccountNotFound
}

func TestKongPlanWS_TurnRejectsDeletedAccount(t *testing.T) {
	h, _ := newKongPlanBindingHarness(t, nil)
	svc := h.sess.svc
	sub := h.selAccount(2, 5, 0, 50)
	st := svc.KongPlanWSBegin(context.Background(), &sub)
	require.True(t, svc.KongPlanWSTurnAllowed(context.Background(), &sub, st))
	svc.accountRepo = kongPlanGoneRepo{}
	require.False(t, svc.KongPlanWSTurnAllowed(context.Background(), &sub, st), "账号已删除")
	svc.accountRepo = stubOpenAIAccountRepo{}
	require.True(t, svc.KongPlanWSTurnAllowed(context.Background(), &sub, st), "临时读取失败按建连时的对象判断")
}

func TestKongPlanStore_RuntimeReadRecoversFirstLoadFailure(t *testing.T) {
	repo := newKongPlanFakeRepo()
	repo.loadErr = errors.New("db down")
	s, _ := newKongPlanTestStore(t, repo)
	require.Nil(t, s.state.Load())
	repo.mu.Lock()
	repo.loadErr = nil
	repo.mu.Unlock()
	require.Nil(t, s.current(), "加载完成之前按现状调度")
	require.Eventually(t, func() bool { return s.state.Load() != nil }, 2*time.Second, 10*time.Millisecond,
		"选号路径读状态时在后台补加载")
}

func TestKongPlanSelect_RecheckedIntoCreditsTriedBeforeRelay(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[1]", 0)
	h.publish(t, 1, "["+h.creditsEntry(1, 1)+"]")
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5}
	listed, fresh := h.selAccount(1, 5, 0, 98), h.selAccount(1, 5, 0, 99) // 候选列表里还在第一层，复核时已到阈值
	ctx := context.Background()
	p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&listed, &relay}, nil, nil)
	// 与旧版选号的循环相同：按尝试序列逐个位置，复核后的账号交给 confirm
	var tried, picked []int64
	for _, acc := range p.sequenceAccounts([]*Account{&listed, &relay}) {
		if !p.next(acc.ID) {
			continue
		}
		tried = append(tried, acc.ID)
		cur := acc
		if acc.ID == 1 {
			cur = &fresh
		}
		if p.confirm(ctx, nil, cur) {
			picked = append(picked, acc.ID)
			break
		}
	}
	require.Equal(t, []int64{1, 1}, tried, "第一层的位置放弃之后，在预留的 credits 位置上再试，先于中转")
	require.Equal(t, []int64{1}, picked)
	require.Len(t, h.ledger.Entries(1), 1, "按 credits 层选中前登记入层")
}
