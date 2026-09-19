package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// 编排层的缺陷形态几乎都是「不报错、只是一直拿不到合格票」，所以这些不变量只能靠测试锁住。

// kongStubUpstream 是可编程的上游替身。
type kongStubUpstream struct {
	mu sync.Mutex

	proxyState KongTicketProxyState
	proxyErr   error

	fetchState string
	fetchErr   error
	fetchCalls int
	// fetchHook 在取票时被调用，用于构造并发时序。
	fetchHook func()

	// answers 按调用顺序返回；用尽后重复最后一个。
	answers       []*KongUpstreamAnswer
	answerErr     error
	challengeCall int
}

func (u *kongStubUpstream) ResolveProxyURL(_ context.Context, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	return fmt.Sprintf("http://proxy-%d", *proxyID), nil
}

func (u *kongStubUpstream) ProxyState(_ context.Context, _ *int64) (KongTicketProxyState, error) {
	return u.proxyState, u.proxyErr
}

func (u *kongStubUpstream) FetchTurnState(_ context.Context, _ *Account, _, _ string) (*KongUpstreamProbe, error) {
	u.mu.Lock()
	u.fetchCalls++
	hook := u.fetchHook
	u.mu.Unlock()
	if hook != nil {
		hook()
	}
	if u.fetchErr != nil {
		return &KongUpstreamProbe{StatusCode: 500}, u.fetchErr
	}
	return &KongUpstreamProbe{State: u.fetchState, StatusCode: 200}, nil
}

func (u *kongStubUpstream) RunChallenge(_ context.Context, _ *Account, _, _ string, _ KongFingerprintChallenge, _ string) (*KongUpstreamAnswer, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	idx := u.challengeCall
	u.challengeCall++
	if u.answerErr != nil {
		return nil, u.answerErr
	}
	if len(u.answers) == 0 {
		return &KongUpstreamAnswer{Text: "1,2,3"}, nil
	}
	if idx >= len(u.answers) {
		idx = len(u.answers) - 1
	}
	return u.answers[idx], nil
}

// kongStubAccounts 是账号装载替身，支持在验证中途改变账号。
type kongStubAccounts struct {
	mu       sync.Mutex
	accounts map[int64]*Account
}

func (a *kongStubAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accounts[id], nil
}

func (a *kongStubAccounts) set(account *Account) {
	a.mu.Lock()
	a.accounts[account.ID] = account
	a.mu.Unlock()
}

func kongTestAccount(id int64, mode KongTicketMode, egress KongTicketEgress) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		ProxyID:     nil,
		Extra: map[string]any{
			KongTicketModeKey:   string(mode),
			KongTicketEgressKey: string(egress),
		},
	}
}

func kongTestService(t *testing.T, repo *kongStubRepo, up *kongStubUpstream, accounts *kongStubAccounts) *KongTicketService {
	t.Helper()
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	params := KongDefaultTicketParams()
	return NewKongTicketService(repo, up, accounts, bank, params,
		KongTicketAccept{"gpt-6-astra": []string{"gpt-6-astra"}}, 0.9)
}

// 两个账号共用同一个票据出口时不得并发取票：每次取票都是该出口上的一次活动，并发会互相把
// 静默清零，最后谁也拿不到合格票。只按账号串行拦不住这种情况。
func TestKongSharedEgressSerializesFetch(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	// 两个账号都配 direct 出口，业务流量走各自的代理（避免 both_direct 判不可用）。
	for _, id := range []int64{1, 2} {
		account := kongTestAccount(id, KongTicketModeFull, KongTicketEgressDirect)
		proxyID := id + 100
		account.ProxyID = &proxyID
		accounts.set(account)
	}

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		fetchHook: func() {
			entered <- struct{}{}
			<-release
		},
	}
	svc := kongTestService(t, repo, up, accounts)

	var wg sync.WaitGroup
	for _, id := range []int64{1, 2} {
		wg.Add(1)
		go func(accountID int64) {
			defer wg.Done()
			_, _ = svc.EnsureTicket(context.Background(), accountID, "gpt-6-astra")
		}(id)
	}

	// 第一个任务进入取票后卡住；此时第二个账号必须拿不到取票资格。
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个取票任务没能启动")
	}
	select {
	case <-entered:
		close(release)
		wg.Wait()
		t.Fatal("两个账号在同一个票据出口上并发取票了——静默会被互相清零")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	wg.Wait()

	if up.fetchCalls != 1 {
		t.Errorf("同一出口上发生了 %d 次取票，应当只有 1 次", up.fetchCalls)
	}
}

// observed 候选验证失败不得推进 F（冷却起点）：它应当立即升级为主动取票，把这种失败算进冷却
// 会让升级白等一个冷却期。
func TestKongObservedFailureDoesNotEnterCooldown(t *testing.T) {
	cases := []struct {
		source       string
		wantCooldown int
	}{
		{KongTicketSourceObserved, 0},
		{KongTicketSourceFetch, 1},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			repo := newKongStubRepo()
			accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
			account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
			proxyID := int64(100)
			account.ProxyID = &proxyID
			accounts.set(account)
			// 三份回答都只有三个数字：数字个数不足，归因给不出结论。
			up := &kongStubUpstream{
				proxyState: KongTicketProxyState{Exists: true},
				answers:    []*KongUpstreamAnswer{{Text: "1,2,3"}},
			}
			svc := kongTestService(t, repo, up, accounts)

			cfg, _ := ParseKongTicketConfig(account.Extra)
			_, err := svc.verifyTicket(context.Background(), account, cfg, "gpt-6-astra",
				7, strings.Repeat("a", 292), c.source, time.Now().Add(time.Hour), nil, time.Now())
			if err == nil {
				t.Fatal("数字不足时不该判为合格")
			}
			if got := repo.countEvents(KongEventCooldown); got != c.wantCooldown {
				t.Errorf("%s 来源产生了 %d 条 cooldown，应为 %d", c.source, got, c.wantCooldown)
			}
		})
	}
}

// 数字个数不足时，原始数字序列仍然必须落库：那是这次观测唯一的证据，事后补不回来。
func TestKongProbesKeepDigitsWhenAttributionFails(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers:    []*KongUpstreamAnswer{{Text: "[11, 22, 33]"}},
	}
	svc := kongTestService(t, repo, up, accounts)

	cfg, _ := ParseKongTicketConfig(account.Extra)
	_, _ = svc.verifyTicket(context.Background(), account, cfg, "gpt-6-astra",
		7, strings.Repeat("a", 292), KongTicketSourceObserved, time.Now().Add(time.Hour), nil, time.Now())

	if len(repo.probes) == 0 {
		t.Fatal("没有任何探测记录落库")
	}
	for _, probe := range repo.probes {
		if len(probe.Digits) != 3 || probe.DigitCount != 3 {
			t.Errorf("第 %d 份的原始数字没有保存：Digits=%v DigitCount=%d",
				probe.PartIndex, probe.Digits, probe.DigitCount)
		}
		if probe.InvalidReason == nil || *probe.InvalidReason != KongProbeInsufficientDigits {
			t.Errorf("第 %d 份应当标为数字不足，实际 %v", probe.PartIndex, probe.InvalidReason)
		}
		if probe.VerificationID == "" {
			t.Errorf("第 %d 份没有验证 ID，无法与事件对上", probe.PartIndex)
		}
	}
}

// 验证期间账号被禁用、模式被切走或出口被改，结论一律不得落库——那是「当时成立、现在不成立」
// 的判定，写进去等于凭它放行后续请求。
func TestKongVerifyRejectsStaleConclusion(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(a *Account)
	}{
		{"账号被禁用", func(a *Account) { a.Schedulable = false }},
		{"模式被切走", func(a *Account) { a.Extra[KongTicketModeKey] = string(KongTicketModeOff) }},
		{"票据出口被改", func(a *Account) { a.Extra[KongTicketEgressKey] = string(KongTicketEgressNone) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := newKongStubRepo()
			accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
			account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
			proxyID := int64(100)
			account.ProxyID = &proxyID
			accounts.set(account)

			up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}
			svc := kongTestService(t, repo, up, accounts)
			cfg, _ := ParseKongTicketConfig(account.Extra)

			// 第一次复核通过，之后立刻改变前提。
			changed := &Account{}
			*changed = *account
			changed.Extra = map[string]any{}
			for k, v := range account.Extra {
				changed.Extra[k] = v
			}
			c.mutate(changed)
			up.fetchHook = nil
			accounts.set(changed)

			_, err := svc.verifyTicket(context.Background(), account, cfg, "gpt-6-astra",
				7, strings.Repeat("a", 292), KongTicketSourceObserved, time.Now().Add(time.Hour), nil, time.Now())
			if err == nil {
				t.Fatal("前提已失效，不该给出有效结论")
			}
			if len(repo.statusSets) != 0 {
				t.Errorf("前提失效后仍然改写了票状态: %+v", repo.statusSets)
			}
		})
	}
}

// 票在验证期间被撤销或跳过时，SetTicketStatus 不会更新——那个返回值必须被检查，否则会凭一个
// 过时的结论把票重新当成可用。
func TestKongVerifyHonorsStatusUpdateResult(t *testing.T) {
	repo := newKongStubRepo()
	notUpdated := false
	repo.setStatusOK = &notUpdated
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)

	// 用足量数字让归因得出结论，走到落库那一步。
	digits := make([]string, 0, 260)
	for i := 0; i < 260; i++ {
		digits = append(digits, fmt.Sprintf("%d", (i*37)%355+1))
	}
	answer := &KongUpstreamAnswer{Text: "[" + strings.Join(digits, ",") + "]"}
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers:    []*KongUpstreamAnswer{answer, answer, answer},
	}
	svc := kongTestService(t, repo, up, accounts)
	cfg, _ := ParseKongTicketConfig(account.Extra)

	id, err := svc.verifyTicket(context.Background(), account, cfg, "gpt-6-astra",
		7, strings.Repeat("a", 292), KongTicketSourceFetch, time.Now().Add(time.Hour), nil, time.Now())
	if err == nil || id != 0 {
		t.Fatalf("状态未更新时不该授予资格，得到 id=%d err=%v", id, err)
	}
}

// 312 是已实测的降智档位长度，必须在入库边界就挡掉——放进去会白烧一次完整验证。
func TestKongStoreTicketDenylist(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	svc := kongTestService(t, repo, &kongStubUpstream{}, accounts)

	if _, _, _, err := svc.storeTicket(context.Background(), 1, "gpt-6-astra", strings.Repeat("a", 312), KongTicketSourceObserved, nil); err == nil {
		t.Error("312 字符的票必须被拒绝入库")
	}
	if len(repo.inserted) != 0 {
		t.Errorf("被拒的票不该入库，实际插入 %d 条", len(repo.inserted))
	}
	if _, _, _, err := svc.storeTicket(context.Background(), 1, "gpt-6-astra", strings.Repeat("a", 292), KongTicketSourceObserved, nil); err != nil {
		t.Errorf("292 字符的票应当入库: %v", err)
	}
}

// 事件写失败时，出口活动必须仍然被记住：那次网络请求已经发出，静默确实被清零了。只信库里的
// 值会让下一个请求立刻再取一次票。
func TestKongEgressActivitySurvivesEventWriteFailure(t *testing.T) {
	repo := newKongStubRepo()
	repo.insertEventErr = fmt.Errorf("事件表写不进去")
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchErr:   fmt.Errorf("取票失败"),
	}
	svc := kongTestService(t, repo, up, accounts)
	cfg, _ := ParseKongTicketConfig(account.Extra)

	_, _ = svc.fetchAndVerify(context.Background(), account, cfg, "gpt-6-astra", time.Now())

	in, err := svc.buildScheduleInput(context.Background(), account, cfg, "gpt-6-astra", time.Now())
	if err != nil {
		t.Fatalf("构造调度输入: %v", err)
	}
	if in.LastEgressUsed == nil {
		t.Error("事件写失败后出口活动丢失了——下一个请求会以为出口仍然静默，立刻再取一次票")
	}
}

// 同一张票重复出现不是新信息：不得新增行、不得延长期限、不得解除候选跳过标记。
func TestKongStoreTicketDeduplicates(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	svc := kongTestService(t, repo, &kongStubUpstream{}, accounts)
	state := strings.Repeat("a", 292)

	firstID, firstExpiry, inserted, err := svc.storeTicket(context.Background(), 1, "gpt-6-astra", state, KongTicketSourceObserved, nil)
	if err != nil || !inserted {
		t.Fatalf("第一次应当入库: inserted=%v err=%v", inserted, err)
	}
	secondID, secondExpiry, inserted, err := svc.storeTicket(context.Background(), 1, "gpt-6-astra", state, KongTicketSourceObserved, nil)
	if err != nil {
		t.Fatalf("重复票不该报错: %v", err)
	}
	if inserted {
		t.Error("重复票被当成新票——它会凭空延长寿命、并被当成新信息解除跳过标记")
	}
	if secondID != firstID {
		t.Errorf("重复票应返回既有 id %d，得到 %d", firstID, secondID)
	}
	if !secondExpiry.IsZero() {
		t.Errorf("重复票不该带回新期限，得到 %v（首次 %v）", secondExpiry, firstExpiry)
	}
	if len(repo.inserted) != 1 {
		t.Errorf("库里应只有一行，实际 %d 行", len(repo.inserted))
	}
}
