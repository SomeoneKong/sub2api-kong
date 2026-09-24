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

// 指纹测试的流程与判定：上游与存储都是假的，回答取自 golden 资料（kong_fingerprint_text_golden.json），
// 它们的分段、得分与判定在 kong_fingerprint_text_test.go 里已经锁住。
//
// 用到的序列（内置库下逐份判定的结果）：
//   - gpt-6-sol#0：一份即判为 gpt-6-sol；gpt-6-luna#0、gpt-6-astra#0 同理各自一份落定（后者落在难分的一对里）
//   - gpt-6.1-sol#0 → #1：目标在这一对里时第一份不下结论，第二份判为 gpt-6.1-sol
//   - gpt-6-astra#0 → gpt-6-sol#0 → gpt-6-sol#1：前两份累计不够把握，第三份累计够了但各份第一层不一致

const kongFPTestTarget = "gpt-6-sol"

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
	challenges []KongFingerprintChallenge
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
	u.challenges = append(u.challenges, challenge)
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

// kongFPAnswer 是一份取自 golden 的成功回答。
func kongFPAnswer(t *testing.T, name, reported string) kongFPStep {
	t.Helper()
	return kongFPStep{answer: &KongUpstreamAnswer{Text: kongFPGoldenText(t, name), ReportedModel: reported, StatusCode: 200, LatencyMs: 1000}}
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

// kongFPRunSteps 用安排好的各份回答对目标跑一次测试。
func kongFPRunSteps(t *testing.T, target string, steps ...kongFPStep) (*kongFPFakeUpstream, *kongFPFakeStore, []KongFingerprintTestEvent, *KongFingerprintTestResult) {
	t.Helper()
	up := &kongFPFakeUpstream{steps: steps}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	events, got := kongFPRun(t, context.Background(), tester, 1, target)
	return up, store, events, got
}

func kongFPCheckResult(t *testing.T, got *KongFingerprintTestResult, execution, reason, verdict string, parts int) {
	t.Helper()
	if got.Execution != execution || got.EndReason != reason || got.Verdict != verdict || got.Parts != parts {
		t.Fatalf("结论 = %s/%s/%q/%d 份，期望 %s/%s/%q/%d 份（detail=%q）",
			got.Execution, got.EndReason, got.Verdict, got.Parts, execution, reason, verdict, parts, got.Detail)
	}
}

// kongFPPartEvents 取出各份的结果事件。
func kongFPPartEvents(events []KongFingerprintTestEvent) []*KongFingerprintTestPart {
	var out []*KongFingerprintTestPart
	for _, e := range events {
		if e.Type == "part" {
			out = append(out, e.Part)
		}
	}
	return out
}

func TestKongFingerprintTargetsAreBankModels(t *testing.T) {
	tester := newKongFPTestTester(t, &kongFPFakeUpstream{}, &kongFPFakeStore{}, kongFPFakeAccounts{})
	targets := tester.Targets()
	if len(targets) != len(tester.bank.Models) {
		t.Fatalf("可测目标 = %+v，期望库里的 %v", targets, tester.bank.Models)
	}
	for i, target := range targets {
		if target.Model != tester.bank.Models[i] || target.DisplayName == "" {
			t.Errorf("第 %d 个目标 = %+v", i, target)
		}
	}
	for _, m := range []string{kongFPTestTarget, "gpt-6.1-sol", "gpt-6-astra"} {
		if !tester.bank.HasModel(m) {
			t.Fatalf("%s 必须可测", m)
		}
	}
}

func TestKongFingerprintBeginValidates(t *testing.T) {
	accounts := kongFPFakeAccounts{
		1: kongFPOAuthAccount(1, nil),
		2: {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		3: {ID: 3, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		4: {ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: kongInt64Ptr(1)},
		5: {ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}},
	}
	tester := newKongFPTestTester(t, &kongFPFakeUpstream{}, &kongFPFakeStore{}, accounts)

	cases := []struct {
		name    string
		account int64
		model   string
		want    error
	}{
		{"不在库里的模型", 1, "gpt-unknown", ErrKongFingerprintTarget},
		{"难分的一对合并成的类不是模型", 1, tester.bank.GroupClass(), ErrKongFingerprintTarget},
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
		answer   string
		reported string
		verdict  string
		decided  string
	}{
		{"指纹指向目标", "gpt-6-sol#0", "gpt-6-sol", KongFingerprintVerdictMatch, "gpt-6-sol"},
		{"回报带快照后缀仍算同一模型", "gpt-6-sol#0", "gpt-6-sol-2026-09-30", KongFingerprintVerdictMatch, "gpt-6-sol"},
		{"没回报模型不算对不上", "gpt-6-sol#0", "", KongFingerprintVerdictMatch, "gpt-6-sol"},
		{"指纹指向别的模型", "gpt-6-luna#0", "gpt-6-sol", KongFingerprintVerdictMismatch, "gpt-6-luna"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, store, events, got := kongFPRunSteps(t, kongFPTestTarget, kongFPAnswer(t, tc.answer, tc.reported))
			kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, tc.verdict, 1)
			if got.Decided != tc.decided || !strings.Contains(got.Detail, tc.decided) || len(got.Pair) != 0 {
				t.Fatalf("结论指向 = %q（detail=%q，pair=%+v），期望 %s", got.Decided, got.Detail, got.Pair, tc.decided)
			}
			if len(got.Candidates) != 7 || got.Candidates[0].Model != tc.decided || got.Candidates[0].Probability < 0.95 {
				t.Fatalf("结论应附按概率排序的第一层分布：%+v", got.Candidates)
			}
			if len(up.challenges) != 1 || up.challenges[0].ID != "text-packed-v1" || up.challenges[0].Effort != "low" ||
				!strings.Contains(up.challenges[0].Prompt, "### 1") {
				t.Fatalf("挑战 = %+v", up.challenges)
			}

			wantTypes := []string{"started", "part_started", "part", "done"}
			if len(events) != len(wantTypes) || events[0].MaxParts != 4 {
				t.Fatalf("事件 = %+v", events)
			}
			for i, typ := range wantTypes {
				if events[i].Type != typ || events[i].TestID != "test-id" {
					t.Fatalf("第 %d 条事件 = %+v，期望 %s", i, events[i], typ)
				}
			}
			part := events[2].Part
			if part == nil || !part.Valid || part.Attribution != tc.decided || part.SectionCount != 10 ||
				part.ReportedModel != tc.reported || len(part.Cumulative) != kongFingerprintCumulativeTop || len(part.Pair) != 0 {
				t.Fatalf("份结果 = %+v", part)
			}

			if len(store.tests) != 1 || len(store.finished) != 1 || len(store.probes) != 1 {
				t.Fatalf("落库：%d 条测试 / %d 次收尾 / %d 份探测", len(store.tests), len(store.finished), len(store.probes))
			}
			fin := store.finished[0]
			if fin.Verdict != tc.verdict || fin.EndReason != kongFingerprintEndConfident || fin.RuleVersion != "2" || fin.FinishedAt.IsZero() {
				t.Fatalf("收尾记录 = %+v", fin)
			}
			probe := store.probes[0]
			if !probe.CountedInAverage || !probe.ParseValid || probe.PartAttribution == nil || *probe.PartAttribution != tc.decided ||
				probe.CumProbability == nil || *probe.CumProbability < 0.95 || probe.TemperatureTier == nil || *probe.TemperatureTier != 1 ||
				len(probe.Scores) != 8 || len(probe.SectionScores) != 10 || probe.DigitCount != 10 || probe.Digits == nil || len(probe.Digits) != 0 ||
				probe.AnswerText != kongFPGoldenText(t, tc.answer) || probe.ReportedModel != tc.reported || probe.VerifyEgress != "direct" ||
				probe.ChallengeID != "text-packed-v1" || probe.LibraryVersion["rule_version"] != "2" || probe.LibraryVersion["digest"] == "" {
				t.Fatalf("探测记录 = %+v", probe)
			}
		})
	}
}

// 规则 2 先于规则 3：指纹再像目标，上游回报的是另一个模型就是对不上。
func TestKongFingerprintReportedModelMismatch(t *testing.T) {
	_, _, _, got := kongFPRunSteps(t, kongFPTestTarget, kongFPAnswer(t, "gpt-6-sol#0", "gpt-6-astra"))
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndReportedMismatch, KongFingerprintVerdictMismatch, 1)
	if !strings.Contains(got.Detail, "gpt-6-astra") || got.Decided != "" {
		t.Fatalf("应说明上游回报了什么、不给指纹结论：detail=%q decided=%q", got.Detail, got.Decided)
	}
}

// 目标在难分的一对里：第一层落在这一对上之后，至少要两份第二层证据才下结论。
func TestKongFingerprintPairNeedsTwoParts(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		verdict string
	}{
		{"目标是 6.1-sol", "gpt-6.1-sol", KongFingerprintVerdictMatch},
		{"目标是 astra 而实际是 6.1-sol", "gpt-6-astra", KongFingerprintVerdictMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, store, events, got := kongFPRunSteps(t, tc.target,
				kongFPAnswer(t, "gpt-6.1-sol#0", ""), kongFPAnswer(t, "gpt-6.1-sol#1", ""))
			kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, tc.verdict, 2)
			if got.Decided != "gpt-6.1-sol" || len(got.Pair) != 2 || got.Pair[0].Model != "gpt-6.1-sol" || got.Pair[0].Probability < 0.95 {
				t.Fatalf("结论 = %q，第二层 = %+v", got.Decided, got.Pair)
			}
			if len(up.challenges) != 2 || up.challenges[0] != up.challenges[1] {
				t.Fatalf("每份发的都是同一个挑战：%+v", up.challenges)
			}
			parts := kongFPPartEvents(events)
			group := "gpt-6-astra|gpt-6.1-sol"
			if len(parts) != 2 || parts[0].Attribution != group || parts[0].Cumulative[0].Model != group ||
				parts[0].Cumulative[0].DisplayName != "gpt-6-astra / gpt-6.1-sol" || len(parts[0].Pair) != 2 {
				t.Fatalf("第一份 = %+v", parts[0])
			}
			if p := store.probes[1]; p.TemperatureTier == nil || *p.TemperatureTier != 2 || p.CumProbability == nil || *p.CumProbability < 0.95 {
				t.Fatalf("第二份探测记录 = %+v", p)
			}
			// 第一份时第二层的概率已经算出来了，但还不到两份，不下结论。
			if p := store.probes[0]; p.CumProbability == nil || *p.CumProbability >= 0.99 {
				t.Fatalf("第一份记的应是第二层的概率：%+v", p.CumProbability)
			}
		})
	}
}

// 目标不在难分的一对里时，回答落在这一对上一份就够判不一致。
func TestKongFingerprintGroupAnswerForOtherTarget(t *testing.T) {
	_, _, _, got := kongFPRunSteps(t, kongFPTestTarget, kongFPAnswer(t, "gpt-6-astra#0", ""))
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMismatch, 1)
	if got.Decided != "gpt-6-astra" {
		t.Fatalf("结论指向 = %q", got.Decided)
	}
}

// 四份用完、第一层确定是这一对，但第二层不够把握：如实报告"二者之一"。
func TestKongFingerprintPairUnresolved(t *testing.T) {
	up := &kongFPFakeUpstream{steps: []kongFPStep{
		kongFPAnswer(t, "gpt-6-astra#0", ""), kongFPAnswer(t, "gpt-6-astra#1", ""),
		kongFPAnswer(t, "gpt-6-astra#2", ""), kongFPAnswer(t, "gpt-6-astra#outlier", ""),
	}}
	tester := newKongFPTestTester(t, up, &kongFPFakeStore{}, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	// 每份证据截得很小，四份相加也到不了阈值。
	bank := *tester.bank
	bank.pairCap = 0.5
	tester.bank = &bank
	_, got := kongFPRun(t, context.Background(), tester, 1, "gpt-6-astra")
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndPairUnresolved, KongFingerprintVerdictInconclusive, 4)
	if got.Decided != bank.GroupClass() || !strings.Contains(got.Detail, "分不清") || len(got.Pair) != 2 {
		t.Fatalf("结论 = %q（detail=%q，pair=%+v）", got.Decided, got.Detail, got.Pair)
	}
}

// 累计够把握但各份的第一层归因不一致：不带票的各份请求不保证落到同一个模型，只报告观察到了差异。
func TestKongFingerprintPartsDisagree(t *testing.T) {
	_, _, _, got := kongFPRunSteps(t, "gpt-6-astra",
		kongFPAnswer(t, "gpt-6-astra#0", ""), kongFPAnswer(t, "gpt-6-sol#0", ""), kongFPAnswer(t, "gpt-6-sol#1", ""))
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndPartsDisagree, KongFingerprintVerdictInconclusive, 3)
	if got.Decided != "" {
		t.Fatalf("各份不一致时不给指纹结论：%q", got.Decided)
	}
}

// 拆不出足够的题：这一份用掉了、原文留档，但不计入归因。
func TestKongFingerprintSectionsMissing(t *testing.T) {
	t.Run("四份都拆不出", func(t *testing.T) {
		step := kongFPAnswer(t, "edge:no_headers", "")
		_, store, _, got := kongFPRunSteps(t, kongFPTestTarget, step, step, step, step)
		kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndPartsExhausted, KongFingerprintVerdictInconclusive, 4)
		if len(got.Candidates) != 0 {
			t.Fatalf("没有可用回答时不该有候选：%+v", got.Candidates)
		}
		if len(store.probes) != 4 {
			t.Fatalf("四份都应留档：%d", len(store.probes))
		}
		for _, p := range store.probes {
			if p.CountedInAverage || p.InvalidReason == nil || *p.InvalidReason != KongProbeInvalidSectionsMissing ||
				p.DigitCount != 0 || p.AnswerText == "" || p.CumProbability != nil || p.TemperatureTier != nil {
				t.Fatalf("探测记录 = %+v", p)
			}
		}
	})
	t.Run("少于七题的一份不计入", func(t *testing.T) {
		_, store, events, got := kongFPRunSteps(t, kongFPTestTarget,
			kongFPAnswer(t, "edge:four_headers_missing", ""), kongFPAnswer(t, "gpt-6-sol#0", ""))
		kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMatch, 2)
		p := store.probes[0]
		if p.CountedInAverage || p.InvalidReason == nil || *p.InvalidReason != KongProbeInvalidSectionsMissing ||
			p.DigitCount != 6 || len(p.SectionScores) != 6 || p.PartAttribution != nil {
			t.Fatalf("拆出六题的一份 = %+v", p)
		}
		if first := kongFPPartEvents(events)[0]; first.Valid || first.SectionCount != 6 || first.InvalidReason != KongProbeInvalidSectionsMissing {
			t.Fatalf("拆出六题的一份的事件 = %+v", first)
		}
		if p := store.probes[1]; p.TemperatureTier == nil || *p.TemperatureTier != 1 {
			t.Fatalf("有效份数只算计入的那一份：%+v", p.TemperatureTier)
		}
	})
}

// 2xx 之后读流出错：这一份用掉了、原文留档，但不计入归因；回报的模型照样作数。
func TestKongFingerprintTruncatedPart(t *testing.T) {
	t.Run("不计入归因", func(t *testing.T) {
		first := kongFPAnswer(t, "gpt-6-astra#0", "")
		first.err = errors.New("stream reset")
		_, store, events, got := kongFPRunSteps(t, kongFPTestTarget, first, kongFPAnswer(t, "gpt-6-sol#0", ""))
		// 第一份若计入，它落在难分的一对上，与第二份不一致，不会判为一致。
		kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMatch, 2)
		p := store.probes[0]
		if p.CountedInAverage || p.InvalidReason == nil || *p.InvalidReason != KongProbeInvalidTruncated || p.AnswerText == "" {
			t.Fatalf("截断的一份 = %+v", p)
		}
		if part := kongFPPartEvents(events)[0]; part.Valid || part.InvalidReason != KongProbeInvalidTruncated || part.Error == "" {
			t.Fatalf("截断的一份的事件 = %+v", part)
		}
	})
	t.Run("回报的模型照样作数", func(t *testing.T) {
		step := kongFPAnswer(t, "gpt-6-sol#0", "gpt-6-astra")
		step.err = errors.New("stream reset")
		_, _, _, got := kongFPRunSteps(t, kongFPTestTarget, step)
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
		{"超时", kongFPStep{answer: &KongUpstreamAnswer{Text: "### 1\nThank", StatusCode: 200}, err: context.DeadlineExceeded}, kongFingerprintEndTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, store, _, got := kongFPRunSteps(t, kongFPTestTarget, tc.step, kongFPAnswer(t, "gpt-6-sol#0", ""))
			kongFPCheckResult(t, got, KongFingerprintExecFailed, tc.reason, "", 1)
			if len(up.challenges) != 1 {
				t.Fatalf("失败后不应再发下一份：%d 份", len(up.challenges))
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
	up := &kongFPFakeUpstream{steps: []kongFPStep{kongFPAnswer(t, "gpt-6-sol#0", "")}}
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
	up := &kongFPFakeUpstream{steps: []kongFPStep{kongFPAnswer(t, "gpt-6.1-sol#0", ""), {block: true}}}
	store := &kongFPFakeStore{}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	run, err := tester.Begin(context.Background(), 1, "gpt-6.1-sol")
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
	up := &kongFPFakeUpstream{steps: []kongFPStep{kongFPAnswer(t, "gpt-6-sol#0", "")}}
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
	up := &kongFPFakeUpstream{steps: []kongFPStep{kongFPAnswer(t, "gpt-6-sol#0", "")}}
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
	up := &kongFPFakeUpstream{steps: []kongFPStep{kongFPAnswer(t, "gpt-6-sol#0", "")}}
	store := &kongFPFakeStore{probeErr: errors.New("db down")}
	tester := newKongFPTestTester(t, up, store, kongFPFakeAccounts{1: kongFPOAuthAccount(1, nil)})
	_, got := kongFPRun(t, context.Background(), tester, 1, kongFPTestTarget)
	kongFPCheckResult(t, got, KongFingerprintExecCompleted, kongFingerprintEndConfident, KongFingerprintVerdictMatch, 1)
	if got.PersistError == "" {
		t.Fatal("落库失败应体现在结论里")
	}
}
