//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func kongTimePtr(t time.Time) *time.Time { return &t }

const (
	kongBatchAstra = "gpt-6-astra"
	kongBatchSol   = "gpt-5.6-sol"
)

// kongBatchService 造一个门控两个模型、按需开关批量取票的服务。
func kongBatchService(t *testing.T, repo *kongStubRepo, up *kongStubUpstream, accounts *kongStubAccounts, batch bool) *KongTicketService {
	t.Helper()
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	return NewKongTicketService(repo, up, accounts, bank, KongDefaultTicketParams(),
		[]string{kongBatchAstra, kongBatchSol}, batch,
		KongTicketAccept{
			kongBatchAstra: []string{kongBatchAstra},
			kongBatchSol:   []string{kongBatchSol, kongBatchAstra},
		}, 0.9)
}

// kongBatchSetup 造一个 full + 直连票据出口、业务走代理的账号，并把出口静默设到门槛之上。
func kongBatchSetup(t *testing.T, up *kongStubUpstream, batch bool) (*KongTicketService, *kongStubRepo) {
	t.Helper()
	repo := newKongStubRepo()
	// 出口已静默 40 分钟：既过门槛，也让「全批共用同一静默值」这条断言有个明显不为 0 的值。
	idleSince := time.Now().Add(-40 * time.Minute)
	repo.lastEgressUsed = &idleSince
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	return kongBatchService(t, repo, up, accounts, batch), repo
}

// kongFetchEvents 取所有真实取票事件（fetch，不含 fetch_skipped）。
func kongFetchEvents(repo *kongStubRepo) []*KongTicketEvent {
	var out []*KongTicketEvent
	for _, e := range repo.events {
		if e.EventType == KongEventFetch {
			out = append(out, e)
		}
	}
	return out
}

// kongFinalVerifyCount 数完成了几次验证（带 final 标记的 verify 事件才是结论）。
func kongFinalVerifyCount(repo *kongStubRepo) int {
	n := 0
	for _, e := range repo.events {
		if e.EventType != KongEventVerify {
			continue
		}
		if final, _ := e.Detail["final"].(bool); final {
			n++
		}
	}
	return n
}

func kongCountEvents(repo *kongStubRepo, eventType string) int {
	n := 0
	for _, e := range repo.events {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}

// 开关关闭时只取触发模型那一张。这条是反面护栏：没有它，"批量取票"写成"无条件取全部"也能让
// 下面那条用例过。
func TestKongBatchFetchDisabledFetchesOnlyTarget(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
	}
	svc, repo := kongBatchSetup(t, up, false)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
		t.Fatalf("准入: %v", err)
	}
	if up.fetchCalls != 1 {
		t.Fatalf("关掉开关应只取一次票，实际取了 %d 次", up.fetchCalls)
	}
	for _, tk := range repo.inserted {
		if tk.Model != kongBatchSol {
			t.Errorf("不该给 %s 取票", tk.Model)
		}
	}
}

// 开关开启时一次取齐所有门控模型，但**只验证触发模型那一张**。
//
// 只取不验是刻意的：验证烧真实额度（每张三份挑战）却走流量出口、与出口静默无关，没有理由现在做；
// 在批里逐个验完还会让触发请求一直等到整批验完。
func TestKongBatchFetchCoversAllModelsWithoutExtraVerify(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
	}
	svc, repo := kongBatchSetup(t, up, true)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
		t.Fatalf("准入: %v", err)
	}
	if up.fetchCalls != 2 {
		t.Fatalf("两个门控模型应各取一次票，实际取了 %d 次", up.fetchCalls)
	}
	got := map[string]bool{}
	for _, tk := range repo.inserted {
		got[tk.Model] = true
		if tk.Source != KongTicketSourceFetch {
			t.Errorf("%s 的票来源应是 fetch，实为 %s", tk.Model, tk.Source)
		}
	}
	if !got[kongBatchSol] || !got[kongBatchAstra] {
		t.Fatalf("两个模型都该有票入库，实得 %v", got)
	}
	// 数「最终 verify 事件」而不是挑战数：置信度够时验证会在第二份就早停，挑战数不是定值。
	if n := kongFinalVerifyCount(repo); n != 1 {
		t.Errorf("只该验证触发模型那一张，实际完成了 %d 次验证", n)
	}
	for _, e := range repo.events {
		if e.EventType == KongEventVerify && e.Model == kongBatchAstra {
			t.Errorf("批内非触发模型不该被验证，却有它的 verify 事件：%v", e.Detail)
		}
	}
	// 批内成员的事件要能自证是批内成员，否则日后看到「静默 40 分钟拿到两张票」会以为记错了。
	events := kongFetchEvents(repo)
	if len(events) != 2 {
		t.Fatalf("一次真实取票只记一条 fetch 事件，两次应有两条，实得 %d", len(events))
	}
	for _, e := range events {
		batch, ok := e.Detail["batch"].(map[string]any)
		if !ok {
			t.Fatalf("%s 的事件缺 batch 标记：%v", e.Model, e.Detail)
		}
		if batch["total"] != 2 {
			t.Errorf("%s 的 batch.total 应为 2，实为 %v", e.Model, batch["total"])
		}
		if _, ok := e.Detail["state_fingerprint"]; !ok {
			t.Errorf("%s 的事件缺 state_fingerprint——那是回答「各模型是不是同一张票」的唯一记录", e.Model)
		}
	}
}

// 全批共用同一个静默值。
//
// 取票自己就是该出口上的活动：第一发一落地就把"上次活动"推到了现在，此后各算一次的话，后面那些
// 成员会记成 idle≈0。那个数字字面为真，却把事实记错——它们骑的是同一个窗口，产生这个窗口的静默
// 就是批前那一段。
func TestKongBatchFetchSharesPreBatchIdle(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
	}
	svc, repo := kongBatchSetup(t, up, true)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
		t.Fatalf("准入: %v", err)
	}
	events := kongFetchEvents(repo)
	if len(events) != 2 {
		t.Fatalf("应有两条 fetch 事件，实得 %d", len(events))
	}
	for _, e := range events {
		if e.IdleSeconds == nil {
			t.Fatalf("%s 的事件没有静默值", e.Model)
		}
		// 40 分钟 = 2400 秒，留点余量防机器慢。
		if *e.IdleSeconds < 2300 {
			t.Errorf("%s 记的静默是 %ds，应是批前那段约 2400s——被自己批内的兄弟请求清零了",
				e.Model, *e.IdleSeconds)
		}
	}
	if *events[0].IdleSeconds != *events[1].IdleSeconds {
		t.Errorf("全批应共用同一静默值，实得 %d 与 %d",
			*events[0].IdleSeconds, *events[1].IdleSeconds)
	}
	// 票行上的采集事实同理。
	for _, tk := range repo.inserted {
		if tk.CaptureIdleSeconds == nil || *tk.CaptureIdleSeconds < 2300 {
			t.Errorf("%s 的票行采集静默不对：%v", tk.Model, tk.CaptureIdleSeconds)
		}
	}
}

// 非触发模型取票失败时**不进冷却**：冷却是给失败的取票路径退避用的，而触发模型本周期已经证明这条
// 路是通的，罚出口只会把下一个正常周期也往后推。它也不该影响本次准入的结果。
func TestKongBatchFetchNonLeaderFailureDoesNotCooldown(t *testing.T) {
	up := &kongStubUpstream{
		proxyState:  KongTicketProxyState{Exists: true},
		fetchState:  strings.Repeat("a", 292),
		answers:     kongVerifyAnswers(),
		fetchErrFor: map[string]error{kongBatchAstra: errors.New("上游 429")},
	}
	svc, repo := kongBatchSetup(t, up, true)

	grant, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol)
	if err != nil {
		t.Fatalf("准入: %v", err)
	}
	if grant == nil || !grant.Allowed {
		t.Fatalf("触发模型自己取票成功，不该因为另一个模型失败而拒服：%+v", grant)
	}
	if n := kongCountEvents(repo, KongEventCooldown); n != 0 {
		t.Errorf("非触发模型的失败不该进冷却，实际记了 %d 条 cooldown 事件", n)
	}
	// 失败仍要留痕。
	var astraFailed bool
	for _, e := range kongFetchEvents(repo) {
		if e.Model == kongBatchAstra && e.Outcome == KongOutcomeFailure {
			astraFailed = true
		}
	}
	if !astraFailed {
		t.Error("非触发模型的失败必须记事件，否则查不出为什么它一直没票")
	}
}

// 触发模型自己失败时，冷却照旧推进——上一条用例放宽的只是非触发模型。
func TestKongBatchFetchLeaderFailureStillCooldowns(t *testing.T) {
	up := &kongStubUpstream{
		proxyState:  KongTicketProxyState{Exists: true},
		fetchState:  strings.Repeat("a", 292),
		answers:     kongVerifyAnswers(),
		fetchErrFor: map[string]error{kongBatchSol: errors.New("上游 500")},
	}
	svc, repo := kongBatchSetup(t, up, true)

	// 准入不以 error 表达拒服：拿不到票是正常结论，走 grant 的 Allowed/DenyReason。
	grant, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol)
	if err != nil {
		t.Fatalf("准入: %v", err)
	}
	if grant == nil || grant.Allowed {
		t.Fatalf("触发模型取票失败时不该放行：%+v", grant)
	}
	if n := kongCountEvents(repo, KongEventCooldown); n == 0 {
		t.Error("触发模型失败必须进冷却，否则下一个请求会立刻再取一次")
	}
}

// 并发必须是真并发：全部成员都进到 FetchTurnState 之后才放行。
//
// 这条是为了锁住已拍板的形态——改成串行循环时，前面那些"取了几次、事件几条"的断言照样会过，
// 而并发正是"不存在窗口在批次中途关闭"这个前提的全部依据。
func TestKongBatchFetchIsActuallyConcurrent(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
	}
	var (
		mu       sync.Mutex
		entered  int
		timedOut bool
	)
	release := make(chan struct{})
	up.fetchHook = func() {
		mu.Lock()
		entered++
		if entered == 2 {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(3 * time.Second):
			// 串行实现下第二发永远不会在第一发返回前进来，这里必然超时。
			mu.Lock()
			timedOut = true
			mu.Unlock()
		}
	}
	svc, _ := kongBatchSetup(t, up, true)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
		t.Fatalf("准入: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entered != 2 {
		t.Fatalf("两个成员都该进到取票里，实得 %d", entered)
	}
	if timedOut {
		t.Error("有成员在别人返回之后才进来——取票没有真并发")
	}
}

// 无条件补全：非触发模型已经有一张健康的当前票时，照样为它取新票。
//
// 只补缺票的会让各模型的周期长期错开，错开 30 分钟就意味着 30 分钟后要在出口上再取一次，而那
// 低于静默门槛、必拿 312。无条件补全把各模型的到期时刻对齐。
func TestKongBatchFetchRefetchesModelThatAlreadyHasTicket(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
	}
	svc, repo := kongBatchSetup(t, up, true)
	// 非触发模型（astra）已有一张还剩 50 分钟的合格票。
	now := time.Now()
	healthy := &KongTicket{
		ID: 90, AccountID: 1, Model: kongBatchAstra, State: strings.Repeat("h", 292),
		Status: KongTicketStatusVerified, Source: KongTicketSourceFetch,
		ExpiresAt: now.Add(50 * time.Minute), CapturedAt: now.Add(-10 * time.Minute),
		FingerprintModel: kongStrPtr(kongBatchAstra), FingerprintP: kongFloatPtr(0.99),
		FingerprintProbs: map[string]float64{kongBatchAstra: 0.99},
	}
	repo.setCurrent(1, kongBatchAstra, healthy)
	repo.tickets[healthy.ID] = &kongStubTicketState{
		AccountID: 1, Model: kongBatchAstra, Status: KongTicketStatusVerified,
		ExpiresAt: healthy.ExpiresAt, CapturedAt: healthy.CapturedAt, Source: KongTicketSourceFetch,
	}

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
		t.Fatalf("准入: %v", err)
	}
	if up.fetchCalls != 2 {
		t.Fatalf("已有好票的模型也要一起取，实际只取了 %d 次", up.fetchCalls)
	}
	var astraNew bool
	for _, tk := range repo.inserted {
		if tk.Model == kongBatchAstra {
			astraNew = true
		}
	}
	if !astraNew {
		t.Error("非触发模型应当入库一张新票，否则它的周期会与触发模型错开")
	}
}

// 旧 observed 候选验证失败，**不得**把批量取回的 fetch 候选一起跳过。
//
// 那张 fetch 票是一整段出口静默换来的，连带标掉等于白扔一次取票，而出口约 34 分钟内补不回来
// ——表现就是这个模型白等一个静默期。批量取票让 fetch 候选常态留在池里，这条损害才真正打开。
func TestKongBulkSkipSparesFetchCandidate(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		// 挑战全失败 → 走 no_valid_answer，那里会做批量跳过。
		answerErr: errors.New("上游 429"),
	}
	svc, repo := kongBatchSetup(t, up, false)
	now := time.Now()
	// 旧的 observed 候选（会被先选中验证）。
	observed := &KongTicket{
		ID: 70, AccountID: 1, Model: kongBatchAstra, State: strings.Repeat("o", 292),
		Status: KongTicketStatusUnverified, Source: KongTicketSourceObserved,
		ExpiresAt: now.Add(40 * time.Minute), CapturedAt: now.Add(-30 * time.Minute),
	}
	repo.candidate[kongStubKey(1, kongBatchAstra)] = observed
	repo.tickets[observed.ID] = &kongStubTicketState{
		AccountID: 1, Model: kongBatchAstra, Status: KongTicketStatusUnverified,
		ExpiresAt: observed.ExpiresAt, CapturedAt: observed.CapturedAt, Source: KongTicketSourceObserved,
	}
	// 批量取票给它存下的 fetch 候选，采集时刻更晚但仍在本次任务开始之前。
	fetched := &KongTicket{
		ID: 71, AccountID: 1, Model: kongBatchAstra, State: strings.Repeat("f", 292),
		Status: KongTicketStatusUnverified, Source: KongTicketSourceFetch,
		ExpiresAt: now.Add(55 * time.Minute), CapturedAt: now.Add(-5 * time.Minute),
	}
	repo.tickets[fetched.ID] = &kongStubTicketState{
		AccountID: 1, Model: kongBatchAstra, Status: KongTicketStatusUnverified,
		ExpiresAt: fetched.ExpiresAt, CapturedAt: fetched.CapturedAt, Source: KongTicketSourceFetch,
	}
	repo.inserted = append(repo.inserted, fetched)

	// 这一发会挑最早的 observed 候选去验，然后失败。
	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchAstra); err != nil {
		t.Fatalf("准入: %v", err)
	}
	if !repo.tickets[observed.ID].SkipUntilNew {
		t.Error("验失败的那张 observed 候选应当被跳过，否则下一个请求还会再验它")
	}
	if repo.tickets[fetched.ID].SkipUntilNew {
		t.Fatal("批量取回的 fetch 候选被连带跳过了——那是一整段出口静默换来的票")
	}
	next, err := repo.OldestCandidate(context.Background(), 1, kongBatchAstra,
		svc.params.MinTicketAge, time.Now())
	if err != nil {
		t.Fatalf("查候选: %v", err)
	}
	if next == nil || next.ID != fetched.ID {
		t.Fatalf("下一个请求应当还能验那张 fetch 候选，实得 %v", next)
	}
}

// 刷新窗口内有候选时：注入旧票 + **异步**验证候选，不去取新票。
//
// 这一步是批量取票的收益能不能落袋的关键：非触发模型的票是别的模型那一轮顺带取回来的候选，不接
// 这一步它要等到旧票过期才**同步**验证，于是每个周期都有一个请求白等一次验证（最坏三份挑战，
// 足以超过等待预算变成拒服）。
func TestKongRefreshWindowPrefersCandidateOverFetch(t *testing.T) {
	now := time.Now()
	params := KongDefaultTicketParams()
	in := KongScheduleInput{
		Now: now, Mode: KongTicketModeFull, AccountReady: true,
		Egress: KongTicketEgressDirect, EgressUsable: true,
		Params: params,
		// 当前票已进入刷新窗口（剩余 < refresh_before）。
		CurrentTicketID: 1, CurrentExpiresAt: now.Add(params.RefreshBefore / 2),
		// 出口静默也够，所以"能取票"不是拦住预取的原因——候选优先才是。
		LastEgressUsed: kongTimePtr(now.Add(-2 * params.TicketFetchMinIdle)),
		HasCandidate:   true, CandidateID: 42,
	}
	got := KongDecideTicketAction(in)
	if got.Action != KongActionInjectAndVerifyCandidate {
		t.Fatalf("有候选时应当注入旧票并异步验候选，实得 %s", got.Action)
	}
	if got.TicketID != 1 {
		t.Errorf("本次请求注入的仍是当前票 #1，实得 #%d", got.TicketID)
	}
	if got.CandidateID != 42 {
		t.Errorf("要验的候选应是 #42，实得 #%d", got.CandidateID)
	}

	// 没有候选时才回到预取。
	in.HasCandidate, in.CandidateID = false, 0
	if got := KongDecideTicketAction(in); got.Action != KongActionInjectAndPrefetch {
		t.Fatalf("没有候选时才该预取，实得 %s", got.Action)
	}
}
