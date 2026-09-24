package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 账号指纹测试：管理员对单个 codex 协议的 OpenAI 账号主动发起，经账号自己的代理发 1～3 份挑战（不带任何
// 票），用指纹库归因，并对照上游回报的模型给出结论。设计见 DESIGN-openai-fingerprint-test.md。
//
// 结果回答的是"这几次挑战观察到了什么"，是相对可比的信号，不是绝对档位判定。每一份的原始证据与整次的
// 结论都落库长期留存，事后只凭库里的记录就能解释当时为什么得出那个结论。

const (
	// kongFingerprintConfidence 是累计归因的最高者达到多少概率才下结论。
	kongFingerprintConfidence = 0.9
	kongFingerprintMaxParts   = 3
	// kongFingerprintRuleVersion 是判定规则的版本，随每次测试落库；改动下面的判定顺序或条件时要加一。
	kongFingerprintRuleVersion = "1"
	// kongFingerprintPersistTimeout 是一次落库的期限。落库用独立的 context：管理员断开时已经取得的证据
	// 照样要留下。
	kongFingerprintPersistTimeout = 10 * time.Second
	// kongFingerprintTargetFamily 是可以作为挑战目标的模型家族。指纹库里的 Claude 候选只是归因的干扰项，
	// 发不到 codex 端点。
	kongFingerprintTargetFamily = "gpt"
	// kongFingerprintCumulativeTop 是每份结果里附带的累计分布条数。
	kongFingerprintCumulativeTop = 3
)

var (
	ErrKongFingerprintBusy    = errors.New("该账号已有指纹测试在进行")
	ErrKongFingerprintTarget  = errors.New("目标模型不在指纹库的可测范围内")
	ErrKongFingerprintAccount = errors.New("账号不存在、不是走 codex 协议的 OpenAI 账号，或是影子账号 / Agent Identity 账号")
)

// 执行结果与模型结论分开表达：前者说这次测试有没有跑完，后者说跑出来的证据指向什么。
const (
	KongFingerprintExecCompleted = "completed"
	KongFingerprintExecCancelled = "cancelled"
	KongFingerprintExecFailed    = "failed"

	KongFingerprintVerdictMatch        = "match"
	KongFingerprintVerdictMismatch     = "mismatch"
	KongFingerprintVerdictInconclusive = "inconclusive"
)

// 结束原因。
const (
	kongFingerprintEndReportedMismatch = "reported_model_mismatch"
	kongFingerprintEndConfident        = "fingerprint_confident"
	kongFingerprintEndPartsDisagree    = "parts_disagree"
	kongFingerprintEndPartsExhausted   = "parts_exhausted"
	kongFingerprintEndCancelled        = "client_cancelled"
	kongFingerprintEndProxyUnavailable = "proxy_unavailable"
	kongFingerprintEndUpstreamStatus   = "upstream_status"
	kongFingerprintEndUpstreamError    = "upstream_error"
	kongFingerprintEndTimeout          = "timeout"
)

// 一份探测留档但不计入归因的原因。
const (
	KongProbeInvalidTruncated      = "truncated"
	KongProbeInvalidRequestFailed  = "request_failed"
	KongProbeInsufficientDigits    = "insufficient_digits"
	KongProbeInvalidNonASCIIDigits = "non_ascii_digits"
	KongProbeInvalidScoreFailed    = "score_failed"
)

// KongFingerprintProbe 是一份挑战的探测记录，写入 kong_fingerprint_probes。
type KongFingerprintProbe struct {
	VerificationID string
	PartIndex      int
	AccountID      int64
	TargetModel    string
	// VerifyEgress 是这份挑战走的出口：账号的代理（`proxy:<id>`）或直连（`direct`）。
	VerifyEgress string
	ChallengeID  string
	// Digits 是归因算法的完整输入。存了它才能在换算法或换校准表之后重算历史结论。
	Digits          []int
	DigitCount      int
	Scores          map[string]float64
	PartAttribution *string
	CumProbability  *float64
	TemperatureTier *int
	// LibraryVersion 标识当时用的挑战集、模型中心、环境方向与校准表：换了校准表，同一序列会算出不同
	// 概率，没有版本标识就既不能复核也不能重算。
	LibraryVersion   map[string]any
	ParseValid       bool
	CountedInAverage bool
	InvalidReason    *string
	LatencyMs        *int
	OutputTokens     *int
	// ReportedModel 是上游在这一份响应里回报的模型；空表示没观测到。
	ReportedModel string
}

// KongFingerprintTestRecord 是一次测试的记录，写入 kong_fingerprint_tests，与各份探测按 VerificationID 关联。
type KongFingerprintTestRecord struct {
	VerificationID string
	AccountID      int64
	TargetModel    string
	ProxyID        *int64
	StartedAt      time.Time
	FinishedAt     time.Time
	Execution      string
	EndReason      string
	Verdict        string
	RuleVersion    string
}

// KongFingerprintTestStore 是指纹测试的持久化面（repository/kong_codex_ticket_repo.go）。
type KongFingerprintTestStore interface {
	InsertFingerprintTest(ctx context.Context, rec *KongFingerprintTestRecord) error
	FinishFingerprintTest(ctx context.Context, rec *KongFingerprintTestRecord) error
	InsertFingerprintProbe(ctx context.Context, probe *KongFingerprintProbe) error
}

// KongFingerprintAccountLoader 读账号当前的配置。
type KongFingerprintAccountLoader interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// KongFingerprintTarget 是一个可选的目标模型。
type KongFingerprintTarget struct {
	Model       string `json:"model"`
	DisplayName string `json:"display_name"`
}

// KongFingerprintCandidateView 是一个候选模型与它的概率。
type KongFingerprintCandidateView struct {
	Model       string  `json:"model"`
	DisplayName string  `json:"display_name"`
	Probability float64 `json:"probability"`
}

// KongFingerprintTestEvent 是推给管理端的一条进度事件。
type KongFingerprintTestEvent struct {
	// Type 是 started / part_started / part / done。
	Type        string                     `json:"type"`
	TestID      string                     `json:"test_id,omitempty"`
	TargetModel string                     `json:"target_model,omitempty"`
	MaxParts    int                        `json:"max_parts,omitempty"`
	Part        *KongFingerprintTestPart   `json:"part,omitempty"`
	Result      *KongFingerprintTestResult `json:"result,omitempty"`
}

// KongFingerprintTestPart 是一份挑战的结果。
type KongFingerprintTestPart struct {
	Index         int    `json:"index"`
	ChallengeID   string `json:"challenge_id"`
	StatusCode    int    `json:"status_code,omitempty"`
	Error         string `json:"error,omitempty"`
	ReportedModel string `json:"reported_model,omitempty"`
	DigitCount    int    `json:"digit_count"`
	Valid         bool   `json:"valid"`
	InvalidReason string `json:"invalid_reason,omitempty"`
	// Attribution 是这一份**自己**最像的模型；Cumulative 是累计到这一份为止的分布。
	Attribution  string                         `json:"attribution,omitempty"`
	Cumulative   []KongFingerprintCandidateView `json:"cumulative,omitempty"`
	LatencyMs    int                            `json:"latency_ms,omitempty"`
	OutputTokens *int                           `json:"output_tokens,omitempty"`
}

// KongFingerprintTestResult 是整次测试的结论。
type KongFingerprintTestResult struct {
	Execution  string                         `json:"execution"`
	EndReason  string                         `json:"end_reason"`
	Detail     string                         `json:"detail,omitempty"`
	Verdict    string                         `json:"verdict,omitempty"`
	Parts      int                            `json:"parts"`
	Candidates []KongFingerprintCandidateView `json:"candidates,omitempty"`
	// PersistError 非空表示有证据没写进库：结论照常给出，但事后读库还原不全。
	PersistError string `json:"persist_error,omitempty"`
}

// KongFingerprintTester 执行账号指纹测试。
type KongFingerprintTester struct {
	accounts KongFingerprintAccountLoader
	upstream KongFingerprintUpstream
	store    KongFingerprintTestStore
	bank     *KongFingerprintBank
	// running 按账号互斥：每份挑战都是一次真实的上游请求，重复点击会白白消耗额度。
	running sync.Map
	now     func() time.Time
	newID   func() string
}

// NewKongFingerprintTester 创建指纹测试服务。指纹库加载失败时返回错误。
func NewKongFingerprintTester(accounts KongFingerprintAccountLoader, upstream KongFingerprintUpstream, store KongFingerprintTestStore) (*KongFingerprintTester, error) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		return nil, fmt.Errorf("加载指纹库: %w", err)
	}
	return &KongFingerprintTester{
		accounts: accounts, upstream: upstream, store: store, bank: bank,
		now: time.Now, newID: func() string { return uuid.NewString() },
	}, nil
}

// Targets 返回可选的目标模型：指纹库里的 GPT 候选。
func (t *KongFingerprintTester) Targets() []KongFingerprintTarget {
	out := make([]KongFingerprintTarget, 0, len(t.bank.Models))
	for _, m := range t.bank.Models {
		if m.Family == kongFingerprintTargetFamily {
			out = append(out, KongFingerprintTarget{Model: m.ID, DisplayName: t.bank.DisplayName(m.ID)})
		}
	}
	return out
}

func (t *KongFingerprintTester) isTarget(model string) bool {
	for _, target := range t.Targets() {
		if target.Model == model {
			return true
		}
	}
	return false
}

// KongFingerprintRun 是一次已通过校验、占住了账号的测试。必须调用 Execute，它会在结束时释放账号。
type KongFingerprintRun struct {
	t       *KongFingerprintTester
	account *Account
	model   string
}

// Begin 校验目标与账号并占住账号。账号在执行环境里只读这一次：中途改了账号或代理，不影响进行中的测试。
//
// 影子账号没有自己的凭据（读母账号的），测它的母账号即可。Agent Identity 账号不持有 access token，每个
// 请求都要现场签名，签名流程会登记或恢复 task、改动账号状态，所以不在测试范围内。
func (t *KongFingerprintTester) Begin(ctx context.Context, accountID int64, model string) (*KongFingerprintRun, error) {
	model = strings.TrimSpace(model)
	if !t.isTarget(model) {
		return nil, ErrKongFingerprintTarget
	}
	account, err := t.accounts.GetByID(ctx, accountID)
	if err != nil || account == nil || !account.IsOpenAIOAuthLike() || account.IsShadow() || account.IsOpenAIAgentIdentity() {
		return nil, ErrKongFingerprintAccount
	}
	if _, busy := t.running.LoadOrStore(account.ID, struct{}{}); busy {
		return nil, ErrKongFingerprintBusy
	}
	return &KongFingerprintRun{t: t, account: account, model: model}, nil
}

// kongFingerprintState 是一次执行过程中的累计状态。
type kongFingerprintState struct {
	answers      []KongFingerprintAnswer
	result       *KongFingerprintResult
	predictions  []string // 各份有效回答自己的归因
	parts        int
	persistError string
}

// Execute 逐份发挑战、落证据、推进度，直到命中判定规则、三份用完、失败或被取消。ctx 取消（管理员断开）
// 时正在进行的那一份立即中止，已完成的份已经落库。
func (r *KongFingerprintRun) Execute(ctx context.Context, emit func(KongFingerprintTestEvent)) {
	t := r.t
	defer t.running.Delete(r.account.ID)

	rec := &KongFingerprintTestRecord{
		VerificationID: t.newID(), AccountID: r.account.ID, TargetModel: r.model,
		ProxyID: kongCopyInt64Ptr(r.account.ProxyID), StartedAt: t.now(), RuleVersion: kongFingerprintRuleVersion,
	}
	st := &kongFingerprintState{}
	st.notePersist(t.persist(func(pctx context.Context) error { return t.store.InsertFingerprintTest(pctx, rec) }))
	emit(KongFingerprintTestEvent{Type: "started", TestID: rec.VerificationID, TargetModel: r.model, MaxParts: kongFingerprintMaxParts})

	finish := func(execution, reason, detail, verdict string) {
		rec.Execution, rec.EndReason, rec.Verdict, rec.FinishedAt = execution, reason, verdict, t.now()
		st.notePersist(t.persist(func(pctx context.Context) error { return t.store.FinishFingerprintTest(pctx, rec) }))
		result := &KongFingerprintTestResult{
			Execution: execution, EndReason: reason, Detail: detail, Verdict: verdict,
			Parts: st.parts, PersistError: st.persistError,
		}
		if st.result != nil {
			result.Candidates = kongFingerprintCandidates(st.result, 0)
		}
		slog.Info("kong fingerprint: 指纹测试结束", "account_id", r.account.ID, "target_model", r.model,
			"test_id", rec.VerificationID, "execution", execution, "end_reason", reason, "verdict", verdict, "parts", st.parts)
		emit(KongFingerprintTestEvent{Type: "done", TestID: rec.VerificationID, Result: result})
	}

	proxyURL, err := t.upstream.ResolveProxyURL(ctx, r.account.ProxyID)
	if err != nil {
		// 读代理的请求随管理员断开一起被取消时，那是取消，不是代理不可用。
		if ctx.Err() != nil {
			finish(KongFingerprintExecCancelled, kongFingerprintEndCancelled, "", "")
			return
		}
		finish(KongFingerprintExecFailed, kongFingerprintEndProxyUnavailable, err.Error(), "")
		return
	}
	egress := kongFingerprintEgress(r.account.ProxyID)
	challenges := KongFingerprintChallenges()
	for i := 0; i < kongFingerprintMaxParts && i < len(challenges); i++ {
		if ctx.Err() != nil {
			finish(KongFingerprintExecCancelled, kongFingerprintEndCancelled, "", "")
			return
		}
		challenge := challenges[i]
		emit(KongFingerprintTestEvent{Type: "part_started", TestID: rec.VerificationID,
			Part: &KongFingerprintTestPart{Index: i + 1, ChallengeID: challenge.ID}})
		answer, runErr := t.upstream.RunChallenge(ctx, r.account, proxyURL, r.model, challenge)
		st.parts++
		probe := &KongFingerprintProbe{
			VerificationID: rec.VerificationID, PartIndex: i + 1, AccountID: r.account.ID, TargetModel: r.model,
			VerifyEgress: egress, ChallengeID: challenge.ID, LibraryVersion: t.bank.Version(),
		}
		part := &KongFingerprintTestPart{Index: i + 1, ChallengeID: challenge.ID}
		if answer != nil {
			probe.LatencyMs, probe.OutputTokens, probe.ReportedModel = kongIntPtr(answer.LatencyMs), answer.OutputTokens, answer.ReportedModel
			part.StatusCode, part.LatencyMs, part.OutputTokens, part.ReportedModel = answer.StatusCode, answer.LatencyMs, answer.OutputTokens, answer.ReportedModel
			// 即使这一份最终不可用，也先把原始数字序列留下来。
			if numbers := kongFPParseNumbers(answer.Text); len(numbers) > 0 {
				probe.Digits, probe.DigitCount = numbers, len(numbers)
			}
		}

		// 规则 1：这一份请求失败（发送失败、上游非 2xx、超时）或被取消，测试到此为止。
		cancelled := ctx.Err() != nil
		failed := answer == nil || answer.StatusCode >= 400 || errors.Is(runErr, context.DeadlineExceeded)
		if cancelled || failed {
			reason := KongProbeInvalidRequestFailed
			probe.InvalidReason = &reason
			if runErr != nil {
				part.Error = runErr.Error()
			}
			st.notePersist(t.persist(func(pctx context.Context) error { return t.store.InsertFingerprintProbe(pctx, probe) }))
			part.DigitCount = probe.DigitCount
			emit(KongFingerprintTestEvent{Type: "part", TestID: rec.VerificationID, Part: part})
			switch {
			case cancelled:
				finish(KongFingerprintExecCancelled, kongFingerprintEndCancelled, "", "")
			case errors.Is(runErr, context.DeadlineExceeded):
				finish(KongFingerprintExecFailed, kongFingerprintEndTimeout, part.Error, "")
			case answer != nil:
				finish(KongFingerprintExecFailed, kongFingerprintEndUpstreamStatus, part.Error, "")
			default:
				finish(KongFingerprintExecFailed, kongFingerprintEndUpstreamError, part.Error, "")
			}
			return
		}

		if runErr != nil {
			// 2xx 之后读流出错：正文不完整，这一份用掉了、但不计入归因；回报的模型照样作数。
			reason := KongProbeInvalidTruncated
			probe.InvalidReason = &reason
			part.Error = runErr.Error()
		} else {
			t.takeAnswer(st, probe, answer.Text, challenge.ExpectedCount)
		}
		st.notePersist(t.persist(func(pctx context.Context) error { return t.store.InsertFingerprintProbe(pctx, probe) }))
		part.DigitCount, part.Valid = probe.DigitCount, probe.CountedInAverage
		if probe.InvalidReason != nil {
			part.InvalidReason = *probe.InvalidReason
		}
		if probe.PartAttribution != nil {
			part.Attribution = *probe.PartAttribution
		}
		if st.result != nil {
			part.Cumulative = kongFingerprintCandidates(st.result, kongFingerprintCumulativeTop)
		}
		emit(KongFingerprintTestEvent{Type: "part", TestID: rec.VerificationID, Part: part})

		// 规则 2：上游回报了模型名且与目标对不上。没回报不算对不上，只是少一项证据。
		if answer.ReportedModel != "" && !kongModelSnapshotMatch(r.model, answer.ReportedModel) {
			finish(KongFingerprintExecCompleted, kongFingerprintEndReportedMismatch,
				fmt.Sprintf("上游回报 %s", answer.ReportedModel), KongFingerprintVerdictMismatch)
			return
		}
		// 规则 3、4：累计归因够把握时，各份有效回答的单份归因都指向同一个候选才下结论——不带票的各份请求
		// 不保证被路由到同一个模型，各份指向不同时只报告观察到了差异。
		if st.result != nil && st.result.Probability >= kongFingerprintConfidence {
			if !st.allPredict(st.result.Prediction) {
				finish(KongFingerprintExecCompleted, kongFingerprintEndPartsDisagree, "", KongFingerprintVerdictInconclusive)
				return
			}
			verdict := KongFingerprintVerdictMismatch
			if st.result.Prediction == r.model {
				verdict = KongFingerprintVerdictMatch
			}
			finish(KongFingerprintExecCompleted, kongFingerprintEndConfident, "", verdict)
			return
		}
	}
	// 规则 5：份数用完仍未命中以上任何一条。
	finish(KongFingerprintExecCompleted, kongFingerprintEndPartsExhausted, "", KongFingerprintVerdictInconclusive)
}

// takeAnswer 把一份回答计入证据、回填该份的观测字段，并更新累计归因。
func (t *KongFingerprintTester) takeAnswer(st *kongFingerprintState, probe *KongFingerprintProbe, text string, expected int) {
	st.answers = append(st.answers, KongFingerprintAnswer{Text: text, ExpectedCount: expected})
	attributed, err := KongFingerprintAttribute(st.answers, t.bank)
	if attributed != nil && len(attributed.Parts) > 0 {
		last := attributed.Parts[len(attributed.Parts)-1]
		probe.Digits, probe.DigitCount = last.Numbers, last.ParsedNumbers
		probe.ParseValid, probe.CountedInAverage = last.Accepted, last.Accepted
		if last.Accepted {
			probe.Scores = kongScoresByModel(last.Scores, t.bank)
			if pred, ok := KongFingerprintPartPrediction(last, t.bank); ok {
				probe.PartAttribution = &pred
				st.predictions = append(st.predictions, pred)
			}
		} else if last.InvalidReason != "" {
			reason := last.InvalidReason
			probe.InvalidReason = &reason
		}
	}
	if err != nil {
		// 还没有任何可用回答：这一份照常留档，继续下一份。
		return
	}
	st.result = attributed
	probe.CumProbability = &attributed.Probability
	probe.TemperatureTier = kongIntPtr(kongTierToInt(attributed.CalibrationTier))
}

func (st *kongFingerprintState) allPredict(model string) bool {
	for _, p := range st.predictions {
		if p != model {
			return false
		}
	}
	return true
}

func (st *kongFingerprintState) notePersist(err error) {
	if err != nil && st.persistError == "" {
		st.persistError = err.Error()
	}
}

// persist 用独立于请求的短期限 context 落库：管理员断开时已经取得的证据照样要留下。
func (t *KongFingerprintTester) persist(fn func(context.Context) error) error {
	if t.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), kongFingerprintPersistTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		slog.Warn("kong fingerprint: 指纹测试证据落库失败", "error", err)
		return err
	}
	return nil
}

// kongFingerprintCandidates 按概率从高到低列出候选；limit 为 0 表示全部。
func kongFingerprintCandidates(result *KongFingerprintResult, limit int) []KongFingerprintCandidateView {
	out := make([]KongFingerprintCandidateView, 0, len(result.Candidates))
	for _, c := range result.Candidates {
		out = append(out, KongFingerprintCandidateView{Model: c.Model, DisplayName: c.DisplayName, Probability: c.Probability})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Probability > out[j].Probability })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// kongScoresByModel 把按 model_order 排列的分数向量变成带模型名的映射。
// 存名字而不是下标：换一份校准资料时 model_order 会变，靠下标读历史记录会读到别的模型上去。
func kongScoresByModel(scores []float64, bank *KongFingerprintBank) map[string]float64 {
	if bank == nil || len(scores) != len(bank.Robust.ModelOrder) {
		return nil
	}
	out := make(map[string]float64, len(scores))
	for i, id := range bank.Robust.ModelOrder {
		out[id] = scores[i]
	}
	return out
}

func kongTierToInt(tier string) int {
	switch tier {
	case "1":
		return 1
	case "2":
		return 2
	case "3":
		return 3
	default:
		return 0
	}
}

// kongModelSnapshotMatch 判定 reported 是不是 base 这个模型（含它的快照版本）。
//
// 规则：大小写无关地相等，或者 reported 去掉 `base-` 前缀后**以数字开头**。后缀容错是必需的：实测存在
// `gpt-5.4-mini` → `gpt-5.4-mini-2026-03-17` 这种回报。而"后缀必须以数字开头"把容错限制在快照与日期上：
// 纯前缀匹配会让 `gpt-6` 吞掉 `gpt-6-astra`、让 `gpt-5.6-sol` 吞掉 `gpt-5.6-sol-mini`——那都是另一个模型。
func kongModelSnapshotMatch(base, reported string) bool {
	if strings.EqualFold(base, reported) {
		return true
	}
	prefix := base + "-"
	if len(reported) <= len(prefix) || !strings.EqualFold(reported[:len(prefix)], prefix) {
		return false
	}
	return reported[len(prefix)] >= '0' && reported[len(prefix)] <= '9'
}

// kongFingerprintEgress 是探测记录里的出口标识。
func kongFingerprintEgress(proxyID *int64) string {
	if proxyID == nil {
		return "direct"
	}
	return fmt.Sprintf("proxy:%d", *proxyID)
}

func kongCopyInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
