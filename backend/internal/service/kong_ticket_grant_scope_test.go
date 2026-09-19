//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 一张 verified 票只代表「按**当时**的白名单与阈值判过」，而那两个值来自环境变量、可以改。
//
// 白名单收紧之后，按旧白名单判过的票仍然满足 verified + 未过期——若据此注入，上游会接受它、
// 不重发 state，后置守卫永远不报错，于是直接交付了白名单之外的档位。阈值提高同理。
func TestKongCurrentTicketHonorsCurrentAcceptAndConfidence(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	state := strings.Repeat("a", 292)
	ticket := &KongTicket{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: state,
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
	}
	repo.setCurrent(1, "gpt-6-astra", ticket)
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	pick := func() (*KongTicket, error) {
		return svc.currentTicket(context.Background(), 1, "gpt-6-astra")
	}

	// 1) 概率全落在白名单之外：不得作为当前票
	ticket.FingerprintProbs = map[string]float64{"gpt-5.6-sol": 0.99, "gpt-6-astra": 0.01}
	if got, err := pick(); err != nil || got != nil {
		t.Errorf("白名单外的票不得成为当前票：got=%v err=%v", got, err)
	}

	// 2) 白名单内但总和低于当前阈值：不得作为当前票
	ticket.FingerprintProbs = map[string]float64{"gpt-6-astra": 0.5, "gpt-5.6-sol": 0.5}
	if got, err := pick(); err != nil || got != nil {
		t.Errorf("总和低于阈值的票不得成为当前票：got=%v err=%v", got, err)
	}

	// 3) 分布缺失（历史数据，或落库时没写分布）：同样不得放行
	ticket.FingerprintProbs = nil
	if got, err := pick(); err != nil || got != nil {
		t.Errorf("没有分布的票不得成为当前票：got=%v err=%v", got, err)
	}

	// 4) 满足当前规则：可用
	ticket.FingerprintProbs = map[string]float64{"gpt-6-astra": 0.95, "gpt-5.6-sol": 0.05}
	got, err := pick()
	if err != nil || got == nil || got.ID != 7 {
		t.Errorf("满足当前规则的票应当可用：got=%v err=%v", got, err)
	}
}

// 较新的票按当前白名单不合格时，不得遮住较旧那张合格的票。
//
// 这是仓储不能在归因判定之前截断的理由：截断按 expires_at 降序取前 N 张，而合格那张更早过期，
// 等下去也永远进不了前 N 名。后果是业务与管理面一致地误报无票，且不报错。
func TestKongCurrentTicketNotMaskedByNewerUnqualified(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)

	base := time.Now()
	// 合格的那张最早过期，所以排在最后。
	qualified := &KongTicket{
		ID: 1, AccountID: 1, Model: "gpt-6-astra", Status: KongTicketStatusVerified,
		ExpiresAt:        base.Add(10 * time.Minute),
		FingerprintProbs: map[string]float64{"gpt-6-astra": 0.95, "gpt-5.6-sol": 0.05},
	}
	tickets := []*KongTicket{qualified}
	for i := 2; i <= 40; i++ {
		tickets = append(tickets, &KongTicket{
			ID: int64(i), AccountID: 1, Model: "gpt-6-astra", Status: KongTicketStatusVerified,
			ExpiresAt: base.Add(time.Duration(i) * 10 * time.Minute),
			// 白名单收紧（或本来就只接受自己）之后，这些都不合格。
			FingerprintProbs: map[string]float64{"gpt-5.6-sol": 0.99, "gpt-6-astra": 0.01},
		})
	}
	repo.setCurrent(1, "gpt-6-astra", tickets...)

	got, err := svc.currentTicket(context.Background(), 1, "gpt-6-astra")
	if err != nil {
		t.Fatalf("取当前票: %v", err)
	}
	if got == nil {
		t.Fatal("39 张不合格的票不该把第 40 张合格的遮住——那是无谓拒服")
	}
	if got.ID != 1 {
		t.Errorf("应当选中唯一合格的那张（id=1），得到 id=%d", got.ID)
	}
}

// 提交归因 → 重新读票 → 按当前白名单授予资格，这条链路必须整条走通；白名单收紧后同一张票即失效。
func TestKongCommitThenGrantAndRejudgeOnTightenedAccept(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	// 这一轮门控 sol，并声明它接受 astra——实测取 sol 的票时归因常返回 astra。
	svc.accept = KongTicketAccept{"gpt-5.6-sol": []string{"gpt-5.6-sol", "gpt-6-astra"}}

	ticket := &KongTicket{
		ID: 9, AccountID: 1, Model: "gpt-5.6-sol", Status: KongTicketStatusUnverified,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	repo.setCurrent(1, "gpt-5.6-sol", ticket)
	repo.tickets[9] = &kongStubTicketState{
		AccountID: 1, Model: "gpt-5.6-sol", Status: KongTicketStatusUnverified,
		ExpiresAt: time.Now().Add(time.Hour), CapturedAt: time.Now(), Source: KongTicketSourceFetch,
	}

	// 归因判为 astra：sol 的白名单含 astra，应当授予。
	attr := KongAttribution{
		Model: "gpt-6-astra", P: 0.93,
		Probs: map[string]float64{"gpt-6-astra": 0.93, "gpt-5.6-sol": 0.06, "gpt-5.6-luna": 0.01},
	}
	ok, err := repo.CommitVerification(context.Background(), 9, KongTicketStatusVerified, attr,
		&KongTicketEvent{AccountID: 1, Model: "gpt-5.6-sol"})
	if err != nil || !ok {
		t.Fatalf("提交资格应当成功：ok=%v err=%v", ok, err)
	}
	got, err := svc.currentTicket(context.Background(), 1, "gpt-5.6-sol")
	if err != nil || got == nil || got.ID != 9 {
		t.Fatalf("归因为更高档位的票应当可用：got=%v err=%v", got, err)
	}

	// 白名单收紧成「只接受自己」：同一张票立刻失效，不需要重新验证。
	svc.accept = KongTicketAccept{"gpt-5.6-sol": []string{"gpt-5.6-sol"}}
	if got, err := svc.currentTicket(context.Background(), 1, "gpt-5.6-sol"); err != nil || got != nil {
		t.Errorf("白名单收紧后该票不得再被采纳：got=%v err=%v", got, err)
	}
}
