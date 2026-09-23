//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 手工触发的价值在「改了判据之后」：同一张票的证据在新白名单下可能就合格了，而重验走流量出口、
// 不消耗票据出口的静默。所以触发要先复位一张未过期的已拒票，而不是直接去取新票。
func TestKongTriggerRefreshRevivesRejectedTicket(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)

	// 一张未过期的已拒票。
	repo.tickets[7] = &kongStubTicketState{
		AccountID: 1, Model: "gpt-6-astra", Status: KongTicketStatusRejected,
		ExpiresAt: time.Now().Add(30 * time.Minute), CapturedAt: time.Now().Add(-time.Minute),
		Source: KongTicketSourceFetch,
	}
	repo.candidates[kongStubKey(1, "gpt-6-astra")] = []*KongTicket{{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: strings.Repeat("a", 292),
		Status: KongTicketStatusRejected, Source: KongTicketSourceFetch,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}}

	out, err := svc.TriggerRefresh(context.Background(), 1, "gpt-6-astra")
	if err != nil {
		t.Fatalf("手工触发: %v", err)
	}
	if out.RevivedTicketID != 7 {
		t.Errorf("应当复位那张已拒票（id=7），得到 %d", out.RevivedTicketID)
	}
	if len(repo.revived) != 1 || repo.revived[0] != 7 {
		t.Errorf("仓储侧应记录一次复位，得到 %v", repo.revived)
	}
	// 结论字段一定有值；这里不断言 allowed——桩上游的行为不是本用例的关注点。
	if out.DenyReason == "" && !out.Allowed {
		t.Error("既未放行也没给出拒服原因，结论不完整")
	}
}

// 已过期的已拒票不得被复位：重验它只会白烧三份挑战，提交时才被期限条件挡掉。
func TestKongTriggerRefreshSkipsExpiredRejected(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect))
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)

	repo.tickets[9] = &kongStubTicketState{
		AccountID: 1, Model: "gpt-6-astra", Status: KongTicketStatusRejected,
		ExpiresAt: time.Now().Add(-time.Second), CapturedAt: time.Now().Add(-time.Hour),
		Source: KongTicketSourceFetch,
	}

	out, err := svc.TriggerRefresh(context.Background(), 1, "gpt-6-astra")
	if err != nil {
		t.Fatalf("手工触发: %v", err)
	}
	if out.RevivedTicketID != 0 {
		t.Errorf("过期票不得被复位，得到 %d", out.RevivedTicketID)
	}
}

// 非 full 模式不做复位：那两种模式本来就不该由手工触发跳过去。
func TestKongTriggerRefreshLeavesNonFullAlone(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongTestAccount(1, KongTicketModeObserve, KongTicketEgressDirect))
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)

	repo.tickets[5] = &kongStubTicketState{
		AccountID: 1, Model: "gpt-6-astra", Status: KongTicketStatusRejected,
		ExpiresAt: time.Now().Add(30 * time.Minute), Source: KongTicketSourceFetch,
	}

	out, err := svc.TriggerRefresh(context.Background(), 1, "gpt-6-astra")
	if err != nil {
		t.Fatalf("手工触发: %v", err)
	}
	if out.RevivedTicketID != 0 || len(repo.revived) != 0 {
		t.Errorf("observe 模式不该复位任何票，得到 %d / %v", out.RevivedTicketID, repo.revived)
	}
	if out.DenyReason != KongDenyModeNotFull {
		t.Errorf("应当以 mode_not_full 结束，得到 %+v", out)
	}
}

// 人工触发跳过静默与冷却，但**不**跳过结构性约束。
//
// 前者是给自动运行用的保守估计（系统只看得见自己产生的出口活动），运维能据带外信息判断此刻可不可
// 以取；后者是物理上做不到或做了有害，跳过没有意义。
func TestKongManualIgnoresWindowNotStructural(t *testing.T) {
	now := time.Now()
	justUsed := now.Add(-time.Second)
	base := KongScheduleInput{
		Now: now, Mode: KongTicketModeFull, Egress: KongTicketEgressDirect,
		EgressUsable: true, AccountReady: true,
		LastEgressUsed: &justUsed, LastCooldownAt: &justUsed,
		Params: KongDefaultTicketParams(),
	}

	// 自动路径：静默与冷却都远未满 → 拒服并给出可重试时刻。
	auto := KongDecideTicketAction(base)
	if auto.Action != KongActionDeny || auto.DenyReason != KongDenyWindowClosed {
		t.Fatalf("自动路径应当以 window_closed 拒服，得到 %+v", auto)
	}
	if auto.RetryAfter.IsZero() {
		t.Error("window_closed 必须给出可重试时刻")
	}

	// 人工路径：同样的输入应当放行到取票。
	manual := base
	manual.IgnoreWindow = true
	if got := KongDecideTicketAction(manual); got.Action != KongActionFetch {
		t.Errorf("人工触发应当跳过窗口直接取票，得到 %+v", got)
	}

	// 结构性约束逐条仍然生效。
	for _, tc := range []struct {
		name   string
		mutate func(*KongScheduleInput)
		want   string
	}{
		{"没配票据出口", func(in *KongScheduleInput) { in.Egress = KongTicketEgressNone }, KongDenyNoTicketSource},
		{"出口失效或与业务出口合并", func(in *KongScheduleInput) { in.EgressUsable = false }, KongDenyEgressUnusable},
		{"账号不可调度", func(in *KongScheduleInput) { in.AccountReady = false }, KongDenyAccountUnready},
	} {
		in := base
		in.IgnoreWindow = true
		tc.mutate(&in)
		got := KongDecideTicketAction(in)
		if got.Action != KongActionDeny || got.DenyReason != tc.want {
			t.Errorf("%s：人工触发不得跳过，期望 %s，得到 %+v", tc.name, tc.want, got)
		}
	}
}

// 当前票还能用但已进刷新窗口、且窗口关闭时，人工触发必须**真的去取票**。
//
// 这是 startPrefetch 固定按自动口径重算的代价：那条路上 manual 会丢，后台退回 Inject 什么都不做，
// 而接口已经拿旧票报了成功——人工干预于是静默失效。
func TestKongManualPrefetchActuallyFetches(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("b", 292),
		answers:    kongVerifyAnswers(),
	}
	svc := kongTestService(t, repo, up, accounts)

	// 当前票只剩 2 分钟（refresh_before 默认 300s，所以已进刷新窗口）。
	now := time.Now()
	cur := &KongTicket{
		ID: 3, AccountID: 1, Model: "gpt-6-astra", State: strings.Repeat("a", 292),
		Status: KongTicketStatusVerified, ExpiresAt: now.Add(2 * time.Minute),
		FingerprintProbs: kongTestAttr(0.99).Probs,
	}
	repo.setCurrent(1, "gpt-6-astra", cur)
	// 出口刚被用过：自动路径此刻一定是 window_closed，不会取票。
	justUsed := now.Add(-time.Second)
	repo.lastEgressUsed = &justUsed

	if _, err := svc.TriggerRefresh(context.Background(), 1, "gpt-6-astra"); err != nil {
		t.Fatalf("手工触发: %v", err)
	}
	if up.fetchCalls == 0 {
		t.Error("人工触发在刷新窗口内必须真的取票，实际一次都没取")
	}
}

// 复位 → 重验 → 成为可用票，整条链要走通，且**不消耗票据出口**（重验走流量出口）。
//
// 这是人工触发最主要的用途：改过白名单或阈值之后，同一张票的证据在新判据下就合格了。
func TestKongManualRevivedTicketBecomesUsable(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers:    kongVerifyAnswers(),
	}
	svc := kongTestService(t, repo, up, accounts)
	// 用 sol + 生产那套默认白名单：合成回答的归因落在 sol 上，而 sol 接受 sol / astra / 5.5。
	// 这正是本功能的主用场景——改过白名单之后，同一张票的证据变得合格。
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model, "gpt-6-astra", "gpt-5.5"}}

	now := time.Now()
	// 一张未过期的已拒票，放在候选池里。
	rejected := &KongTicket{
		ID: 7, AccountID: 1, Model: model, State: strings.Repeat("a", 292),
		Status: KongTicketStatusRejected, Source: KongTicketSourceFetch,
		ExpiresAt: now.Add(40 * time.Minute), CapturedAt: now.Add(-2 * time.Minute),
	}
	repo.candidates[kongStubKey(1, model)] = []*KongTicket{rejected}
	repo.tickets[7] = &kongStubTicketState{
		AccountID: 1, Model: model, Status: KongTicketStatusRejected,
		ExpiresAt: rejected.ExpiresAt, CapturedAt: rejected.CapturedAt, Source: KongTicketSourceFetch,
	}

	out, err := svc.TriggerRefresh(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("手工触发: %v", err)
	}
	if out.RevivedTicketID != 7 {
		t.Fatalf("应当以复位的那张票为目标，得到 %d", out.RevivedTicketID)
	}
	// **不该取新票**：重验走流量出口，票据出口的静默是最稀缺的资源。
	if up.fetchCalls != 0 {
		t.Errorf("复位重验不该取新票，实际取了 %d 次", up.fetchCalls)
	}
	if up.challengeCall == 0 {
		t.Error("应当真的发出挑战来重验那张票")
	}
	// 验证的目标必须是复位那一张，而不是泛选到别张。
	if len(repo.statusSets) == 0 || repo.statusSets[len(repo.statusSets)-1].ID != 7 {
		t.Errorf("落结论的应当是票 7，得到 %+v", repo.statusSets)
	}
	if !out.Allowed || out.TicketID != 7 {
		t.Errorf("重验通过后应当授予该票，得到 allowed=%v id=%d deny=%s", out.Allowed, out.TicketID, out.DenyReason)
	}
	// 随后的普通请求要能读到它——这一步锁住「验证成功后真的成为可用票」。
	grant, err := svc.EnsureTicket(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("普通请求: %v", err)
	}
	if !grant.Allowed || grant.TicketID != 7 {
		t.Errorf("普通请求应当能用上那张票，得到 %+v", grant)
	}
}

// 人工刷新认领任务之后、后台重读账号之前，模式被切出 full：不复位、不发挑战，已拒票原样保留。
func TestKongRefreshTaskSkipsReviveAfterLeavingFull(t *testing.T) {
	const model = "gpt-6-astra"
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	claimed := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	claimed.ProxyID = &proxyID
	now := kongTestAccount(1, KongTicketModeObserve, KongTicketEgressDirect)
	now.ProxyID = &proxyID
	accounts.set(now)
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)

	repo.tickets[7] = &kongStubTicketState{
		AccountID: 1, Model: model, Status: KongTicketStatusRejected,
		ExpiresAt: time.Now().Add(30 * time.Minute), CapturedAt: time.Now().Add(-time.Minute),
		Source: KongTicketSourceFetch,
	}
	repo.candidates[kongStubKey(1, model)] = []*KongTicket{{
		ID: 7, AccountID: 1, Model: model, State: strings.Repeat("a", 780),
		Status: KongTicketStatusRejected, Source: KongTicketSourceFetch,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}}

	_, revivedID, _ := svc.runTask(context.Background(), claimed, model, KongActionFetch,
		KongEgressKey(KongTicketEgressDirect, nil), true, true)
	if revivedID != 0 || len(repo.revived) != 0 {
		t.Errorf("已退出 full 时不该复位，得到 %d / %v", revivedID, repo.revived)
	}
	up.mu.Lock()
	calls := up.challengeCall
	up.mu.Unlock()
	if calls != 0 {
		t.Errorf("已退出 full 时不该发挑战，实发 %d 份", calls)
	}
	if st, _ := repo.TicketStatus(context.Background(), 7); st != KongTicketStatusRejected {
		t.Errorf("已拒票应原样保留，实为 %s", st)
	}
}
