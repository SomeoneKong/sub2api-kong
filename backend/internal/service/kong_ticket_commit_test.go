//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// kongVerifyAnswers 造一份能让归因得出结论的回答，供需要走到落库那一步的用例复用。
func kongVerifyAnswers() []*KongUpstreamAnswer {
	digits := make([]string, 0, 260)
	for i := 0; i < 260; i++ {
		digits = append(digits, fmt.Sprintf("%d", (i*37)%355+1))
	}
	answer := &KongUpstreamAnswer{Text: "[" + strings.Join(digits, ",") + "]"}
	return []*KongUpstreamAnswer{answer, answer, answer}
}

// 资格与最终事件必须同时提交：提交失败时任何请求都不得看见新资格。
func TestKongVerifyDoesNotGrantWhenCommitFails(t *testing.T) {
	repo := newKongStubRepo()
	repo.commitErr = errors.New("事件写入失败")
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
	cfg, _ := ParseKongTicketConfig(account.Extra)

	id, err := svc.verifyTicket(context.Background(), account, cfg, "gpt-6-astra",
		7, strings.Repeat("a", 292), KongTicketSourceFetch, time.Now().Add(time.Hour), nil, time.Now(), false)
	if err == nil || id != 0 {
		t.Fatalf("提交失败时不该授予资格，得到 id=%d err=%v", id, err)
	}
	// 关键不变量：状态更新不能留在库里。
	if len(repo.statusSets) != 0 {
		t.Errorf("提交失败后票状态仍被改写: %+v", repo.statusSets)
	}
	// 证据先落库，不因提交失败而丢。
	if len(repo.probes) == 0 {
		t.Error("探测证据应当已经落库")
	}
}

// 归因为非目标模型时，本段无票期里的其它旧候选一并淘汰。
//
// 只 rejected 当前一张的话，缓存里的旧候选会被下一个请求逐张验证，而候选验证不经过取票冷却
// ——一张张烧额度，结论却注定相同。
func TestKongVerifyConsumesCandidateBudgetOnNonTargetModel(t *testing.T) {
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
	// 把白名单换成一个归因绝不会命中的值，逼出「不在白名单内」这个终点。
	svc.accept = KongTicketAccept{"gpt-6-astra": []string{"no-such-model"}}
	cfg, _ := ParseKongTicketConfig(account.Extra)

	id, err := svc.verifyTicket(context.Background(), account, cfg, "gpt-6-astra",
		7, strings.Repeat("a", 292), KongTicketSourceFetch, time.Now().Add(time.Hour), nil, time.Now(), false)
	if err == nil || id != 0 {
		t.Fatalf("非目标归因不该授予资格，得到 id=%d err=%v", id, err)
	}
	if len(repo.skippedBulk) == 0 {
		t.Error("非目标结论必须淘汰任务边界内的旧候选")
	}
	if repo.skipBoundary.IsZero() {
		t.Error("淘汰必须带边界，否则会误伤验证期间新到的票")
	}
	// 顺序不变量：批量淘汰必须发生在提交**之后**。反过来做的话当前票会被自己标成 skip，
	// 而提交条件要求未被 skip——结论落不下去，真实归因被一条「并发变更」的假原因顶掉。
	if len(repo.statusSets) != 1 || repo.statusSets[0].Status != KongTicketStatusRejected {
		t.Fatalf("当前票必须落成 rejected：%+v", repo.statusSets)
	}
	if repo.statusSets[0].FingerprintModel == "" {
		t.Error("真实归因结果必须保留，不能被作废原因顶掉")
	}
	var finalEvent *KongTicketEvent
	for _, ev := range repo.events {
		if ev.EventType == KongEventVerify && ev.Detail["final"] == true {
			finalEvent = ev
		}
	}
	if finalEvent == nil {
		t.Fatal("必须有一条最终事件")
	}
	if finalEvent.Detail["reason"] == "ticket_changed_during_verify" {
		t.Error("最终事件写成了并发变更，真实归因丢了")
	}
	if finalEvent.FingerprintModel == nil || *finalEvent.FingerprintModel == "" {
		t.Error("最终事件必须带归因结果")
	}
}

// 取票拿回一张重复票时必须推进退避：min_idle=0 是合法配置，不进冷却就会连续空转取票。
func TestKongDuplicateFetchEntersCooldown(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	accounts.set(account)
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)

	state := strings.Repeat("b", 292)
	// 先存一张，第二次入库即命中去重。
	if _, _, inserted, err := svc.storeTicket(context.Background(), 1, "gpt-6-astra", state, KongTicketSourceFetch, nil); err != nil || !inserted {
		t.Fatalf("首次入库应当成功: inserted=%v err=%v", inserted, err)
	}
	if _, _, inserted, err := svc.storeTicket(context.Background(), 1, "gpt-6-astra", state, KongTicketSourceFetch, nil); err != nil || inserted {
		t.Fatalf("重复票应当被去重: inserted=%v err=%v", inserted, err)
	}
	// 冷却的内存事实由 enterCooldown 写；这里直接验证它对重复票生效。
	svc.enterCooldown(context.Background(), 1, "gpt-6-astra", KongEgressKey(KongTicketEgressDirect, nil), "duplicate_state")
	if svc.cooldownMem(1, KongEgressKey(KongTicketEgressDirect, nil)) == nil {
		t.Error("冷却必须留下内存事实，事件写失败时才不会立刻重取")
	}
}

// 有限但极大的分数不得被标准化静默归零。
//
// `variance += d*d` 在 d 约 1e200 时溢出成 Inf，scale 随之成 Inf，整个向量被除成零——融合结果
// 仍然有限，后置的有限性检查捕获不到，于是这一支的判据静默消失而归因照样给出高置信结论。
func TestKongFPStandardizeRejectsOverflow(t *testing.T) {
	out, err := kongFPStandardize([]float64{1e200, -1e200, 3e200}, "测试")
	if err != nil {
		t.Fatalf("有限输入不该报错: %v", err)
	}
	allZero := true
	for _, v := range out {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("标准化结果必须有限: %v", out)
		}
		if v != 0 {
			allZero = false
		}
	}
	if allZero {
		t.Errorf("整支判据被静默除成零: %v", out)
	}

	// 上一步已经溢出的向量必须报错，而不是被标准化抹平成看起来正常的数字。
	if _, err := kongFPStandardize([]float64{math.Inf(1), 0, 1}, "测试"); err == nil {
		t.Error("非有限输入必须报错")
	}
	// 常量向量走近零方差下限，保留官方口径：结果是零向量但不报错。
	if out, err := kongFPStandardize([]float64{5, 5, 5}, "测试"); err != nil || out[0] != 0 {
		t.Errorf("常量向量应当得到零向量: out=%v err=%v", out, err)
	}
}

// 测试替身必须忠实反映生产的状态条件。
//
// 这条用例守的是**桩自己**：它两次掩盖过真实回归（`SkipCandidatesFor` 只记调用不改状态，于是
// 「先 skip 后提交必然 0 行」全绿通过）。桩少一个条件，那个条件的错误就测不出来。
func TestKongStubRepoHonorsProductionConditions(t *testing.T) {
	repo := newKongStubRepo()
	ctx := context.Background()
	mk := func(accountID int64, model, state, source, status string, expires time.Time, captured time.Time) int64 {
		id, _, err := repo.InsertTicket(ctx, &KongTicket{
			AccountID: accountID, Model: model, State: state, StateLen: len(state),
			Source: source, Status: status, ExpiresAt: expires, CapturedAt: captured,
			FingerprintModel: kongStrPtr("gpt-6-astra"), FingerprintP: kongFloatPtr(0.99),
		})
		if err != nil {
			t.Fatalf("入库: %v", err)
		}
		return id
	}
	now := time.Now()

	// 撤销必须影响读取。
	verified := mk(1, "gpt-6-astra", "S-V", KongTicketSourceFetch, KongTicketStatusVerified, now.Add(time.Hour), now)
	repo.setCurrent(1, "gpt-6-astra", repo.inserted[0])
	if got, _ := repo.VerifiedTickets(ctx, 1, "gpt-6-astra"); len(got) == 0 {
		t.Fatal("verified 且未过期的票应当读得到")
	}
	if err := repo.RevokeTicket(ctx, verified); err != nil {
		t.Fatalf("撤销: %v", err)
	}
	if got, _ := repo.VerifiedTickets(ctx, 1, "gpt-6-astra"); len(got) != 0 {
		t.Error("撤销后不该再读到")
	}

	// 批量 skip 只影响同一 (账号, 模型)、且只影响 observed 候选，clear 必须真的解除。
	a := mk(2, "gpt-6-astra", "S-A", KongTicketSourceObserved, KongTicketStatusUnverified, now.Add(time.Hour), now.Add(-time.Minute))
	b := mk(3, "gpt-6-astra", "S-B", KongTicketSourceObserved, KongTicketStatusUnverified, now.Add(time.Hour), now.Add(-time.Minute))
	// 同账号同模型下的 fetch 候选：批量跳过**不得**碰它——那是一整段出口静默换来的票，被旧
	// observed 候选的失败连带标掉就等于白扔那次取票，而出口约 34 分钟内补不回来。
	fetched := mk(5, "gpt-6-astra", "S-F", KongTicketSourceFetch, KongTicketStatusUnverified, now.Add(time.Hour), now.Add(-time.Minute))
	if err := repo.SkipCandidatesFor(ctx, 2, "gpt-6-astra", now); err != nil {
		t.Fatalf("批量跳过: %v", err)
	}
	if ok, _ := repo.CommitVerification(ctx, fetched, KongTicketStatusVerified, kongTestAttr(0.99), &KongTicketEvent{AccountID: 2}); !ok {
		t.Error("批量跳过不该碰 fetch 候选")
	}
	if ok, _ := repo.CommitVerification(ctx, a, KongTicketStatusRejected, kongTestAttr(0.5), &KongTicketEvent{AccountID: 2}); ok {
		t.Error("被跳过的票不得提交成功")
	}
	if ok, _ := repo.CommitVerification(ctx, b, KongTicketStatusRejected, kongTestAttr(0.5), &KongTicketEvent{AccountID: 3}); !ok {
		t.Error("别的账号不该受影响——跨账号一起标是桩自己的 bug")
	}
	if err := repo.ClearSkipMarks(ctx, 2, "gpt-6-astra"); err != nil {
		t.Fatalf("解除标记: %v", err)
	}
	if ok, _ := repo.CommitVerification(ctx, a, KongTicketStatusRejected, kongTestAttr(0.5), &KongTicketEvent{AccountID: 2}); !ok {
		t.Error("解除标记后应当可以提交")
	}

	// 过期票不得提交。
	expired := mk(4, "gpt-6-astra", "S-E", KongTicketSourceFetch, KongTicketStatusUnverified, now.Add(-time.Second), now.Add(-time.Hour))
	if ok, _ := repo.CommitVerification(ctx, expired, KongTicketStatusVerified, kongTestAttr(0.99), &KongTicketEvent{AccountID: 4}); ok {
		t.Error("过期票不得提交成功")
	}

	// observed 候选要满足最小年龄；fetch 不受限。
	fresh := &KongTicket{ID: 900, AccountID: 5, Model: "gpt-6-astra", Source: KongTicketSourceObserved}
	repo.candidate[kongStubKey(5, "gpt-6-astra")] = fresh
	repo.tickets[900] = &kongStubTicketState{
		AccountID: 5, Model: "gpt-6-astra", Status: KongTicketStatusUnverified,
		ExpiresAt: now.Add(time.Hour), CapturedAt: now, Source: KongTicketSourceObserved,
	}
	if got, _ := repo.OldestCandidate(ctx, 5, "gpt-6-astra", 30*time.Minute, now); got != nil {
		t.Error("刚到的 observed 候选不该满足最小年龄")
	}
	repo.tickets[900].Source = KongTicketSourceFetch
	if got, _ := repo.OldestCandidate(ctx, 5, "gpt-6-astra", 30*time.Minute, now); got == nil {
		t.Error("fetch 候选不受最小年龄限制")
	}
}
