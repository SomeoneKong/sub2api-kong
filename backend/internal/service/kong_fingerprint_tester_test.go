//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 指纹测试的流程与判定：上游与存储都是假的，回答取自 golden 样本（kong_fingerprint_golden.jsonl），
// 它们的归因结果在 kong_fingerprint_test.go 里已经锁住。
//
// 用到的样本（单份归因 / 累计）：
//   - 0：gpt-6-astra 0.9991（单份即够把握）
//   - 2：gpt-5.6-sol 0.5786；2 之后接 7：gpt-5.6-sol 0.9837（两份同向）
//   - 4：gpt-5.6-sol 0.9936（单份即够把握）
//   - 5：gpt-6-astra 0.7845；5 之后接 2：gpt-5.6-sol 0.9675，但两份单份归因不同

const kongFPTestTarget = "gpt-5.6-sol"

type kongFPFakeAccounts map[int64]*Account

func (f kongFPFakeAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	if a, ok := f[id]; ok {
		return a, nil
	}
	return nil, errors.New("not found")
}

// kongFPStep 是假上游对一份挑战的回应。block 为真时一直等到 ctx 取消。
type kongFPStep struct {
	answer *KongUpstreamAnswer
	err    error
	block  bool
}

type kongFPFakeUpstream struct {
	proxyErr   error
	steps      []kongFPStep
	mu         sync.Mutex
	challenges []string
	proxyURLs  []string
}

func (u *kongFPFakeUpstream) ResolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if u.proxyErr != nil {
		return "", u.proxyErr
	}
	if proxyID == nil {
		return "", nil
	}
	return "http://proxy.example", nil
}

func (u *kongFPFakeUpstream) RunChallenge(ctx context.Context, _ *Account, proxyURL, _ string, challenge KongFingerprintChallenge) (*KongUpstreamAnswer, error) {
	u.mu.Lock()
	i := len(u.challenges)
	u.challenges = append(u.challenges, challenge.ID)
	u.proxyURLs = append(u.proxyURLs, proxyURL)
	u.mu.Unlock()
	if i >= len(u.steps) {
		return nil, errors.New("假上游：没有安排这一份")
	}
	step := u.steps[i]
	if step.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return step.answer, step.err
}

type kongFPFakeStore struct {
	mu        sync.Mutex
	tests     []KongFingerprintTestRecord
	finished  []KongFingerprintTestRecord
	probes    []KongFingerprintProbe
	probeErr  error
	ctxErrors []error // 每次落库时 ctx 的状态：管理员断开后落库仍须用未取消的 ctx
}

func (s *kongFPFakeStore) InsertFingerprintTest(ctx context.Context, rec *KongFingerprintTestRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	s.tests = append(s.tests, *rec)
	return nil
}

func (s *kongFPFakeStore) FinishFingerprintTest(ctx context.Context, rec *KongFingerprintTestRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	s.finished = append(s.finished, *rec)
	return nil
}

func (s *kongFPFakeStore) InsertFingerprintProbe(ctx context.Context, probe *KongFingerprintProbe) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	s.probes = append(s.probes, *probe)
	return s.probeErr
}

func kongInt64Ptr(v int64) *int64 { return &v }

func kongFPOAuthAccount(id int64, proxyID *int64) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: proxyID}
}

func newKongFPTestTester(t *testing.T, up *kongFPFakeUpstream, store *kongFPFakeStore, accounts kongFPFakeAccounts) *KongFingerprintTester {
	t.Helper()
	tester, err := NewKongFingerprintTester(accounts, up, store)
	if err != nil {
		t.Fatalf("创建指纹测试服务: %v", err)
	}
	tester.newID = func() string { return "test-id" }
	return tester
}

func kongFPGoldenAnswer(t *testing.T, row int, reported string) *KongUpstreamAnswer {
	t.Helper()
	rows := kongFPLoadGolden(t)
	return &KongUpstreamAnswer{Text: rows[row].Text, ReportedModel: reported, StatusCode: 200, LatencyMs: 1000}
}

// kongFPRun 跑一次测试，返回全部事件与结论。
func kongFPRun(t *testing.T, ctx context.Context, tester *KongFingerprintTester, accountID int64, model string) ([]KongFingerprintTestEvent, *KongFingerprintTestResult) {
	t.Helper()
	run, err := tester.Begin(context.Background(), accountID, model)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	var events []KongFingerprintTestEvent
	run.Execute(ctx, func(e KongFingerprintTestEvent) { events = append(events, e) })
	if len(events) == 0 || events[len(events)-1].Type != "done" || events[len(events)-1].Result == nil {
		t.Fatalf("最后一条事件必须是带结论的 done：%+v", events)
	}
	return events, events[len(events)-1].Result
}

func kongFPCheckResult(t *testing.T, got *KongFingerprintTestResult, execution, reason, verdict string, parts int) {
	t.Helper()
	if got.Execution != execution || got.EndReason != reason || got.Verdict != verdict || got.Parts != parts {
		t.Fatalf("结论 = %s/%s/%q/%d 份，期望 %s/%s/%q/%d 份（detail=%q）",
			got.Execution, got.EndReason, got.Verdict, got.Parts, execution, reason, verdict, parts, got.Detail)
	}
}

func TestKongFingerprintTargetsAreGPTOnly(t *testing.T) {
	tester := newKongFPTestTester(t, &kongFPFakeUpstream{}, &kongFPFakeStore{}, kongFPFakeAccounts{})
	targets := tester.Targets()
	if len(targets) == 0 {
		t.Fatal("可测目标不能为空")
	}
	seen := map[string]bool{}
	for _, target := range targets {
		seen[target.Model] = true
		if target.DisplayName == "" {
			t.Errorf("%s 缺显示名", target.Model)
		}
	}
	for _, m := range tester.bank.Models {
		if want := m.Family == kongFingerprintTargetFamily; seen[m.ID] != want {
			t.Errorf("%s（家族 %s）是否可测 = %v，期望 %v", m.ID, m.Family, seen[m.ID], want)
		}
	}
	if !seen[kongFPTestTarget] {
		t.Fatalf("%s 必须可测", kongFPTestTarget)
	}
}

func TestKongFingerprintBeginValidates(t *testing.T) {
	var claude string
	accounts := kongFPFakeAccounts{
		1: kongFPOAuthAccount(1, nil),
		2: {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		3: {ID: 3, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		4: {ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: kongInt64Ptr(1)},
		5: {ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}},
	}
	tester := newKongFPTestTester(t, &kongFPFakeUpstream{}, &kongFPFakeStore{}, accounts)
	for _, m := range tester.bank.Models {
		if m.Family != kongFingerprintTargetFamily {
			claude = m.ID
			break
		}
	}
	if claude == "" {
		t.Fatal("指纹库里应有非 GPT 的干扰候选")
	}

	cases := []struct {
		name    string
		account int64
		model   string
		want    error
	}{
		{"不在库里的模型", 1, "gpt-unknown", ErrKongFingerprintTarget},
		{"干扰候选不可测", 1, claude, ErrKongFingerprintTarget},
		{"账号不存在", 99, kongFPTestTarget, ErrKongFingerprintAccount},
		{"API key 账号", 2, kongFPTestTarget, ErrKongFingerprintAccount},
		{"非 OpenAI 账号", 3, kongFPTestTarget, ErrKongFingerprintAccount},
		{"影子账号", 4, kongFPTestTarget, ErrKongFingerprintAccount},
		{"Agent Identity 账号", 5, kongFPTestTarget, ErrKongFingerprintAccount},
	}
	for _, tc := range cases {
		if _, err := tester.Begin(context.Background(), tc.account, tc.model); !errors.Is(err, tc.want) {
			t.Errorf("%s：err = %v，期望 %v", tc.name, err, tc.want)
		}
	}

	run, err := tester.Begin(context.Background(), 1, " "+kongFPTestTarget+" ")
	if err != nil {
		t.Fatalf("目标名两端的空白应被忽略：%v", err)
	}
	if _, err := tester.Begin(context.Background(), 1, kongFPTestTarget); !errors.Is(err, ErrKongFingerprintBusy) {
		t.Fatalf("同一账号进行中时再发起：err = %v，期望 busy", err)
	}
	// Execute 结束后释放账号（这里假上游没安排任何一份，第一份即失败）。
	run.Execute(context.Background(), func(KongFingerprintTestEvent) {})
	if _, err := tester.Begin(context.Background(), 1, kongFPTestTarget); err != nil {
		t.Fatalf("结束后应能再次发起：%v", err)
	}
}

func TestKongFingerprintConfidentSinglePart(t *testing.T) {
	cases := []struct {
		name     string
		row      int
		reported string
		verdict  string
	}{
		{"指纹指向目标", 4, "gpt-5.6-sol", KongFingerprintVerdictMatch},
		{"回报带快照后缀仍算同一模型", 4, "gpt-5.6-sol-2026-03-17", KongFingerprintVerdictMatch},
		{"没回报模型不算对不上", 4, "", KongFingerprintVerdictMatch},
		{"指纹指向别的模型", 0, "gpt-5.6-sol", KongFingerprintVerdictMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, tc.row, tc.reported)}}}
			store := &kongFPFakeStore{}
			tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
			events, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
			kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, tc.verdict, 1)
			if len(got.Candidates) == 0 || got.Candidates[0].Probability < kongFingerprintConfidence {
				t.Fatalf("结论应附按概率排序的候选：%+v", got.Candidates)
			}

			wantTypes := []string{"started", "part_started", "part", "done"}
			if len(events) != len(wantTypes) {
				t.Fatalf("事件 = %+v", events)
			}
			for i, typ := range wantTypes {
				if events[i].Type != typ || events[i].TestID != "test-id" {
					t.Fatalf("第 %d 条事件 = %+v，期望 %s", i, events[i], typ)
				}
			}
			part := events[2].Part
			if part == nil || !part.Valid || part.Attribution == "" || part.ReportedModel != tc.reported || len(part.Cumulative) == 0 {
				t.Fatalf("份结果 = %+v", part)
			}

			if len(store.tests) != 1 || len(store.finished) != 1 || len(store.probes) != 1 {
				t.Fatalf("落库：%d 条测试 / %d 次收尾 / %d 份探测", len(store.tests), len(store.finished), len(store.probes))
			}
			fin := store.finished[0]
			if fin.Verdict != tc.verdict || fin.EndReason != kongFingerprintEndConfident || fin.RuleVersion != kongFingerprintRuleVersion || fin.FinishedAt.IsZero() {
				t.Fatalf("收尾记录 = %+v", fin)
			}
			probe := store.probes[0]
			if !probe.CountedInAverage || probe.PartAttribution == nil || probe.CumProbability == nil || probe.Scores == nil ||
				probe.DigitCount == 0 || probe.ReportedModel != tc.reported || probe.VerifyEgress != "direct" || probe.LibraryVersion == nil {
				t.Fatalf("探测记录 = %+v", probe)
			}
		})
	}
}

// 规则 2 先于规则 3：指纹再像目标，上游回报的是另一个模型就是对不上。
func TestKongFingerprintReportedModelMismatch(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, 4, "gpt-6-astra")}}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndReportedMismatch, KongFingerprintVerdictMismatch, 1)
	if got.Detail == "" {
		t.Fatal("应说明上游回报了什么")
	}
}

func TestKongFingerprintSecondPartReachesConfidence(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{
		{answer: kongFPGoldenAnswer(t, 2, "")},
		{answer: kongFPGoldenAnswer(t, 7, "")},
	}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMatch, 2)
	if len(up.challenges) != 2 || up.challenges[0] != "query-01" || up.challenges[1] != "query-02" {
		t.Fatalf("挑战应按顺序取用：%v", up.challenges)
	}
	if len(store.probes) != 2 || store.probes[1].PartIndex != 2 {
		t.Fatalf("探测记录 = %+v", store.probes)
	}
}

// 累计够把握但各份的单份归因不一致：不带票的各份请求不保证落到同一个模型，只报告观察到了差异。
func TestKongFingerprintPartsDisagree(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{
		{answer: kongFPGoldenAnswer(t, 5, "")},
		{answer: kongFPGoldenAnswer(t, 2, "")},
	}}
	tester := newKongFPTestTester(t, up, &kongFPFakeStore{}, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndPartsDisagree, KongFingerprintVerdictInconclusive, 2)
}

func TestKongFingerprintPartsExhausted(t *testing.T) {
	short := &KongUpstreamAnswer{Text: "[1, 2, 3]", StatusCode: 200}
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: short}, {answer: short}, {answer: short}}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndPartsExhausted, KongFingerprintVerdictInconclusive, 3)
	if len(got.Candidates) != 0 {
		t.Fatalf("没有可用回答时不该有候选：%+v", got.Candidates)
	}
	if len(store.probes) != 3 {
		t.Fatalf("三份都应留档：%d", len(store.probes))
	}
	for _, p := range store.probes {
		if p.CountedInAverage || p.InvalidReason == nil || *p.InvalidReason != KongProbeInsufficientDigits || p.DigitCount != 3 {
			t.Fatalf("探测记录 = %+v", p)
		}
	}
}

// 2xx 之后读流出错：这一份用掉了、原始数字留档，但不计入归因；回报的模型照样作数。
func TestKongFingerprintTruncatedPart(t *testing.T) {
	t.Run("不计入归因", func(t *testing.T) {
		up := &kongFPFakeUpstream{steps: []kongFPStep{
			{answer: kongFPGoldenAnswer(t, 0, ""), err: errors.New("stream reset")},
			{answer: kongFPGoldenAnswer(t, 4, "")},
		}}
		store := &kongFPFakeStore{}
		tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
		events, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
		// 第一份若计入，累计会被拉向 astra；只计第二份时是 gpt-5.6-sol 0.9936。
		kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMatch, 2)
		p := store.probes[0]
		if p.CountedInAverage || p.InvalidReason == nil || *p.InvalidReason != KongProbeInvalidTruncated || p.DigitCount == 0 {
			t.Fatalf("截断的一份 = %+v", p)
		}
		first := events[2].Part
		if first.Valid || first.InvalidReason != KongProbeInvalidTruncated || first.Error == "" {
			t.Fatalf("截断的一份的事件 = %+v", first)
		}
	})
	t.Run("回报的模型照样作数", func(t *testing.T) {
		up := &kongFPFakeUpstream{steps: []kongFPStep{
			{answer: kongFPGoldenAnswer(t, 4, "gpt-6-astra"), err: errors.New("stream reset")},
		}}
		tester := newKongFPTestTester(t, up, &kongFPFakeStore{}, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
		_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
		kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndReportedMismatch, KongFingerprintVerdictMismatch, 1)
	})
}

func TestKongFingerprintRequestFailure(t *testing.T) {
	cases := []struct {
		name   string
		step   kongFPStep
		reason string
	}{
		{"上游非 2xx", kongFPStep{answer: &KongUpstreamAnswer{StatusCode: 429}, err: errors.New("status 429")}, kongFingerprintEndUpstreamStatus},
		{"发送失败", kongFPStep{err: errors.New("dial failed")}, kongFingerprintEndUpstreamError},
		{"超时", kongFPStep{answer: &KongUpstreamAnswer{Text: "[1, 2", StatusCode: 200}, err: context.DeadlineExceeded}, kongFingerprintEndTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &kongFPFakeUpstream{steps: []kongFPStep{tc.step, {answer: kongFPGoldenAnswer(t, 4, "")}}}
			store := &kongFPFakeStore{}
			tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
			_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
			kongFPCheckResult(t, got, KongFingerprintExecFailed, tc.reason, "", 1)
			if len(up.challenges) != 1 {
				t.Fatalf("失败后不应再发下一份：%v", up.challenges)
			}
			if len(store.probes) != 1 || store.probes[0].InvalidReason == nil || *store.probes[0].InvalidReason != KongProbeInvalidRequestFailed {
				t.Fatalf("失败的一份也要留档：%+v", store.probes)
			}
			if len(store.finished) != 1 || store.finished[0].Execution != KongFingerprintExecFailed || store.finished[0].Verdict != "" {
				t.Fatalf("收尾记录 = %+v", store.finished)
			}
		})
	}
}

func TestKongFingerprintProxyUnavailable(t *testing.T) {
	proxyID := int64(7)
	up := &kongFPFakeUpstream{proxyErr: errors.New("代理 7 不存在")}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, &proxyID)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecFailed, kongFingerprintEndProxyUnavailable, "", 0)
	if len(up.challenges) != 0 {
		t.Fatal("代理解析失败时不得发出任何挑战（更不能退回直连）")
	}
	if len(store.tests) != 1 || store.tests[0].ProxyID == nil || *store.tests[0].ProxyID != 7 {
		t.Fatalf("测试记录应带当时的代理：%+v", store.tests)
	}
}

func TestKongFingerprintEgressFollowsAccountProxy(t *testing.T) {
	proxyID := int64(7)
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, 4, "")}}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, &proxyID)})
	kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	if up.proxyURLs[0] != "http://proxy.example" {
		t.Fatalf("挑战应经账号的代理发出：%q", up.proxyURLs[0])
	}
	if store.probes[0].VerifyEgress != "proxy:7" {
		t.Fatalf("出口 = %q", store.probes[0].VerifyEgress)
	}
}

// 管理员断开：进行中的那一份立即中止；已经取得的证据用独立的 ctx 照样落库，账号随之释放。
func TestKongFingerprintCancelled(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, 2, "")}, {block: true}}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	run, err := tester.Begin(context.Background(), 1, kongFPTestTarget)
	if err != nil {
		t.Fatal(err)
	}
	var events []KongFingerprintTestEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.Execute(ctx, func(e KongFingerprintTestEvent) {
			events = append(events, e)
			if e.Type == "part_started" && e.Part.Index == 2 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后测试应很快收尾")
	}
	got := events[len(events)-1].Result
	kongFPCheckResult(t, got, KongFingerprintExecCancelled, kongFingerprintEndCancelled, "", 2)
	if len(store.probes) != 2 || len(store.finished) != 1 || store.finished[0].Execution != KongFingerprintExecCancelled {
		t.Fatalf("取消前后的证据都应落库：%d 份探测 / %+v", len(store.probes), store.finished)
	}
	for i, err := range store.ctxErrors {
		if err != nil {
			t.Fatalf("第 %d 次落库用的 ctx 已失效：%v", i, err)
		}
	}
	if _, err := tester.Begin(context.Background(), 1, kongFPTestTarget); err != nil {
		t.Fatalf("取消后应释放账号：%v", err)
	}
}

func TestKongFingerprintCancelledBeforeFirstPart(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, 4, "")}}}
	tester := newKongFPTestTester(t, up, &kongFPFakeStore{}, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, got := kongFPRun(t, ctx, tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCancelled, kongFingerprintEndCancelled, "", 0)
	if len(up.challenges) != 0 {
		t.Fatal("已取消时不应再发挑战")
	}
}

// 读代理的请求随管理员断开一起被取消：记为取消，不是代理不可用。
func TestKongFingerprintCancelledWhileResolvingProxy(t *testing.T) {
	proxyID := int64(7)
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, 4, "")}}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, &proxyID)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, got := kongFPRun(t, ctx, tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCancelled, kongFingerprintEndCancelled, "", 0)
	if len(up.challenges) != 0 {
		t.Fatal("已取消时不应再发挑战")
	}
	if len(store.finished) != 1 || store.finished[0].Execution != KongFingerprintExecCancelled {
		t.Fatalf("收尾记录 = %+v", store.finished)
	}
}

// 落库失败不改变结论，但要让管理员知道库里的记录不全。
func TestKongFingerprintPersistErrorSurfaces(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{{answer: kongFPGoldenAnswer(t, 4, "")}}}
	store := &kongFPFakeStore{probeErr: errors.New("db down")}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMatch, 1)
	if got.PersistError == "" {
		t.Fatal("落库失败应体现在结论里")
	}
}
