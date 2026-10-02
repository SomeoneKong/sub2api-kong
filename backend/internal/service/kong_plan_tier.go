package service

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 定层（fork 专有，设计见 DESIGN-account-selection.md 第 2.2 节、第 4.2 节第 2 步）：对每个 OpenAI OAuth 账号按实时
// 读数判断它此刻在第一层（订阅额度）、credits 层，还是按现状暂停。读数用本次请求取得的账号对象，与 A、B 两条暂停
// 路径看到的是同一份；滚动状态（窗口序号、余额基点、未知标记）读节奏组件的快照，入层记录读记账组件的镜像。

type kongPlanLayer int

const (
	kongPlanLayerSubscription kongPlanLayer = iota + 1
	kongPlanLayerCredits
	kongPlanLayerPaused
)

func (l kongPlanLayer) String() string {
	switch l {
	case kongPlanLayerSubscription:
		return "subscription"
	case kongPlanLayerCredits:
		return "credits"
	case kongPlanLayerPaused:
		return "paused"
	}
	return ""
}

// kongPlanTierResult 是一个账号此刻的定层结果。
type kongPlanTierResult struct {
	Input string // 网关此刻所用的输入：snapshot / fallback / legacy
	Layer kongPlanLayer
	// Entry 是账号在所用输入里的条目；输入里没有这个账号时为缺省（普通档、活跃）。
	Entry KongPlanEntry
	// Reached7d：已到 7d 暂停阈值（A、B 两条路径里先触发的那个，含缺省值）。
	Reached7d bool
	// Eligible：第 2.2 节三处条件都满足（不看已写下的暂停标记）；窗口未知时为假。
	Eligible      bool
	WindowSeq     int64
	WindowUnknown bool
	ReadingAt     time.Time
	// AtLine：视同到线（有未结算的入层，或未知标记在有效期内），第 4.2 节第 5 步。
	AtLine bool
	// Undecided：按现状暂停只是因为还判不出是否在 credits 层（与定层用同一份状态求出），B 路径据此只写短标记。
	Undecided bool
}

// kongPlanRuntime 汇集定层要读的输入。网关单实例，装配时设一次。
type kongPlanRuntime struct {
	store      *KongPlanStore
	ledger     *KongPlanLedger
	pace       *KongOpenAIAccountPace
	bindings   KongPlanBindingStore
	thresholds func(ctx context.Context) map[string]int
	// autoPause 返回 A 路径的全局缺省阈值。选号入口把它放进上下文，WS、标记、管理视图的上下文里没有，所以定层自己取。
	autoPause func(ctx context.Context) OpsOpenAIAccountQuotaAutoPauseSettings
	// stats 是决策记录，nil 时不记。
	stats *KongPlanStats
	now   func() time.Time
}

var kongPlanRT atomic.Pointer[kongPlanRuntime]

// SetKongPlanRuntime 接上定层要读的输入（fork 专有，在 wire_gen.go 手工装配）。bindings 是会话与续接标记的存储；
// thresholds 返回平台级停调阈值（B 路径），autoPause 返回 A 路径的全局缺省阈值；stats 是决策记录。
func SetKongPlanRuntime(store *KongPlanStore, ledger *KongPlanLedger, pace *KongOpenAIAccountPace, bindings KongPlanBindingStore,
	thresholds func(ctx context.Context) map[string]int, autoPause func(ctx context.Context) OpsOpenAIAccountQuotaAutoPauseSettings,
	stats *KongPlanStats) {
	kongPlanRT.Store(&kongPlanRuntime{store: store, ledger: ledger, pace: pace, bindings: bindings, thresholds: thresholds,
		autoPause: autoPause, stats: stats, now: time.Now})
}

// withAutoPause 保证上下文里有 A 路径的全局缺省阈值：选号入口已经放了的沿用，没有的现取（设置服务带缓存）。
func (rt *kongPlanRuntime) withAutoPause(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if rt == nil || rt.autoPause == nil {
		return ctx
	}
	if _, ok := ctx.Value(openAIQuotaAutoPauseCtxKey{}).(OpsOpenAIAccountQuotaAutoPauseSettings); ok {
		return ctx
	}
	return withOpenAIQuotaAutoPauseSettings(ctx, rt.autoPause(ctx))
}

func kongPlanRuntimeNow() *kongPlanRuntime { return kongPlanRT.Load() }

// observeLayer 在复用路径与标记写入处看到账号回到第一层时登记离层（与完整选号同一个 markLeft）：一直经已有绑定或 WS
// 服务的账号也要能开始结算。
func (rt *kongPlanRuntime) observeLayer(ctx context.Context, account *Account, res kongPlanTierResult, now time.Time) {
	if res.Layer != kongPlanLayerSubscription {
		return
	}
	ps, _ := rt.paceState(account.ID)
	rt.markLeft(ctx, account, ps, now)
}

// enterCredits 登记入层：账号作为 credits 层承接请求之前（选中、写标记、复用绑定、WS 逐轮），这个窗口都要有入层记录。
// 镜像里已有时不访问 Redis。返回错误时调用方不把它当 credits 层用。
func (rt *kongPlanRuntime) enterCredits(ctx context.Context, account *Account, res kongPlanTierResult, now time.Time) error {
	if rt.ledger == nil {
		return nil
	}
	if err := rt.ledger.MarkEntered(ctx, account.ID, res.WindowSeq, kongPlanReadingTime(account, now)); err != nil {
		kongPlanWarn("kong plan: 入层登记失败，本次不用这个账号的 credits", "account_id", account.ID, "window_seq", res.WindowSeq, "error", err)
		return err
	}
	return nil
}

func (rt *kongPlanRuntime) paceState(id int64) (*kongPaceAccountState, bool) {
	if rt == nil || rt.pace == nil {
		return nil, false
	}
	snap := rt.pace.snapshot.Load()
	if snap == nil {
		return nil, false
	}
	f, ok := snap.accounts[id]
	if !ok {
		return nil, false
	}
	return &f.State, true
}

// kongPlanAutoResetCardsReady 复刻 A 路径自动用卡分支的"有卡放行"：开着自动用卡、新鲜状态显示有可用卡时，普通暂停阈值
// 不拦，只按消费阈值暂停。
func kongPlanAutoResetCardsReady(cfg OpenAIAutoResetCreditConfig, account *Account, now time.Time) bool {
	if !cfg.Enabled {
		return false
	}
	state := openAIAutoResetStateFromExtra(account.Extra)
	return state != nil && state.Status == OpenAIAutoResetStatusAvailable && state.AvailableCount > 0 && !openAIAutoResetStateStale(state, now)
}

// windowThresholdPercent 返回账号此刻一个窗口生效的暂停阈值（百分数，A、B 两条路径里较低的那个）；都没有时为 100。
// 与 kongPlanReached7d 一样是 A、B 两处判定的复刻，上游改那两处时要同步。
func (rt *kongPlanRuntime) windowThresholdPercent(ctx context.Context, account *Account, window string, now time.Time) float64 {
	ctx = rt.withAutoPause(ctx)
	limit := 100.0
	key := "auto_pause_7d_disabled"
	if window == "5h" {
		key = "auto_pause_5h_disabled"
	}
	t5, t7 := resolveOpenAIQuotaAutoPauseThresholds(ctx, account)
	a := t7
	if window == "5h" {
		a = t5
	}
	cfg := ResolveOpenAIAutoResetCreditConfig(account)
	if !resolveAccountExtraBool(account.Extra, key) && a > 0 && !kongPlanAutoResetCardsReady(cfg, account, now) {
		limit = min(limit, a*100)
	}
	if cfg.Enabled {
		c := cfg.Threshold7d
		if window == "5h" {
			c = cfg.Threshold5h
		}
		if c > 0 {
			limit = min(limit, c*100)
		}
	}
	var thresholds map[string]int
	if rt != nil && rt.thresholds != nil {
		thresholds = rt.thresholds(ctx)
	}
	if b, ok := resolveEffectiveAccountSchedulingThreshold(account, thresholds, PlatformOpenAI); ok && b < 100 {
		limit = min(limit, float64(b))
	}
	return limit
}

// kongPlanReached7d 判断账号是否已到 7d 暂停阈值：A 路径（按缓存现算）或 B 路径（停调阈值）里先触发的那个。
// 这是两处判定的复刻（A：shouldAutoPauseOpenAIAccountByQuota，含自动用卡分支；B：EvaluateAccountSchedulingThreshold
// 的 7d 候选），上游改那两处时要同步，windowThresholdPercent 同理。
func (rt *kongPlanRuntime) kongPlanReached7d(ctx context.Context, account *Account, now time.Time) bool {
	if u, ok := resolveOpenAIQuotaUtilization(account.Extra, "7d", now); ok {
		cfg := ResolveOpenAIAutoResetCreditConfig(account)
		if cfg.Enabled && cfg.Threshold7d > 0 && u >= cfg.Threshold7d {
			return true
		}
		disabled := resolveAccountExtraBool(account.Extra, "auto_pause_7d_disabled")
		if _, t7 := resolveOpenAIQuotaAutoPauseThresholds(ctx, account); !disabled && t7 > 0 && u >= t7 && !kongPlanAutoResetCardsReady(cfg, account, now) {
			return true
		}
	}
	var thresholds map[string]int
	if rt != nil && rt.thresholds != nil {
		thresholds = rt.thresholds(ctx)
	}
	threshold, ok := resolveEffectiveAccountSchedulingThreshold(account, thresholds, PlatformOpenAI)
	if !ok || threshold >= 100 || !openAICodexSnapshotIdentityTrusted(account) {
		return false
	}
	return candidateMatchesThreshold(openAIThresholdCandidate(account.Extra, "7d", now), threshold, now)
}

func kongPlanControlAllows(c KongPlanControl, id int64) bool {
	if c.CreditsMode != KongPlanCreditsModeOn || c.FileHold != nil || len(c.LatchedHolds) > 0 {
		return false
	}
	i := sort.Search(len(c.Allow), func(i int) bool { return c.Allow[i] >= id })
	return i < len(c.Allow) && c.Allow[i] == id
}

// tier 给一个账号定层。非 OpenAI OAuth 账号返回零值（Layer 为 0），由调用方按中转处理。
func (rt *kongPlanRuntime) tier(ctx context.Context, account *Account, now time.Time) kongPlanTierResult {
	res := kongPlanTierResult{Input: KongPlanInUseLegacy, Entry: KongPlanEntry{Tier: KongPlanTierNormal, Active: true}}
	if rt == nil || account == nil || !account.IsOpenAIOAuth() {
		return res
	}
	var st *kongPlanState
	if rt.store != nil {
		st = rt.store.current()
	}
	res.Input = st.inUse(now)
	if e := st.entryFor(account.ID, res.Input); e != nil {
		res.Entry = *e
		res.Entry.ID = account.ID
	} else {
		res.Entry.ID = account.ID
	}
	res.ReadingAt = parseExtraTime(account.Extra["codex_usage_updated_at"])

	// 先读入层镜像、后读节奏快照：推进者先发布快照（含未知标记）、后从镜像移出结算的入层，按这个顺序读不会两头都错过。
	entries := rt.ledger.Entries(account.ID)
	ps, ok := rt.paceState(account.ID)
	if ok {
		res.WindowSeq = ps.WindowSeq
	}
	// 窗口未知：节奏状态里还没有当前窗口，或读数的重置时刻比当前窗口晚出容差（窗口序号还没推进）。
	res.WindowUnknown = !ok || !ps.HasWindow
	if ok && ps.HasWindow {
		if reset := parseExtraTime(account.Extra["codex_7d_reset_at"]); !reset.IsZero() && reset.Sub(ps.ResetAt) > kongPaceWindowTolerance {
			res.WindowUnknown = true
		}
	}
	res.AtLine = kongPlanAtLine(entries, ps, now)

	res.Reached7d = rt.kongPlan7dReachedOrFalse(ctx, account, now)
	if !res.Reached7d {
		res.Layer = kongPlanLayerSubscription
		return res
	}
	res.Eligible = rt.creditsEligible(st, res.Entry, account, ps, now) && !res.WindowUnknown
	if res.Eligible {
		res.Layer = kongPlanLayerCredits
	} else {
		res.Layer = kongPlanLayerPaused
		res.Undecided = rt.creditsUndecided(st, res, account, now)
	}
	return res
}

func (rt *kongPlanRuntime) kongPlan7dReachedOrFalse(ctx context.Context, account *Account, now time.Time) bool {
	return rt.kongPlanReached7d(rt.withAutoPause(ctx), account, now)
}

// creditsEligible 是第 2.2 节"三处一起决定"里除了"已到阈值"之外的部分：计划给了级别、人工约束允许、
// 除额度外可服务、最近一次有数值的余额高于保底、还没到作废时刻。不看已写下的暂停标记。Spark 影子账号与母账号共用
// 余额、记账不为它补记，不进 credits 层。
func (rt *kongPlanRuntime) creditsEligible(st *kongPlanState, entry KongPlanEntry, account *Account, ps *kongPaceAccountState, now time.Time) bool {
	if st == nil || entry.Credits == nil || !now.Before(entry.Credits.ExpiresAt) || account.IsShadow() {
		return false
	}
	if !kongPlanControlAllows(st.control, account.ID) {
		return false
	}
	if !account.IsActive() || !account.Schedulable {
		return false
	}
	var committed *kongPlanBalancePoint
	if ps != nil {
		committed = ps.Balance
	}
	latest := rt.ledger.LatestBalance(account.ID, committed)
	return latest != nil && latest.Value > st.control.Floor
}

// kongPlanSkip7d 是 A、B 两条暂停路径在判定处的挂点：账号此刻在 credits 层时不按 7d 暂停（5h 照常）。
func kongPlanSkip7d(ctx context.Context, account *Account) bool {
	rt := kongPlanRuntimeNow()
	if rt == nil || account == nil || !account.IsOpenAIOAuth() {
		return false
	}
	return rt.tier(ctx, account, rt.now()).Layer == kongPlanLayerCredits
}

// kongPlanUndecidedHold 是判不出账号是否在 credits 层时 B 路径 7d 标记的时长：照常拦下，但不留到 7d 重置，
// 判得出之后由下一次判定定夺。
const kongPlanUndecidedHold = 2 * time.Minute

// creditsUndecided：账号此刻不在 credits 层只是因为还判不出——状态还没加载（不知道所用输入），或所用输入里计划给了
// credits、约束允许，只差节奏状态里还没有当前窗口（启动后节奏首拍之前、推进者停更时）。st 是定层读到的那一份。
func (rt *kongPlanRuntime) creditsUndecided(st *kongPlanState, res kongPlanTierResult, account *Account, now time.Time) bool {
	if rt.store == nil {
		return false
	}
	if st == nil {
		return true
	}
	c := res.Entry.Credits
	return res.Input != KongPlanInUseLegacy && res.WindowUnknown && c != nil && now.Before(c.ExpiresAt) &&
		kongPlanControlAllows(st.control, account.ID)
}

// kongPlanDrop7dCandidates 是 B 路径窗口候选阶段的挂点：账号在 credits 层时去掉 7d 候选，同时触发的 5h 照常。
// 判不出账号是否在 credits 层时，7d 候选的截止时刻收短到 kongPlanUndecidedHold 之后（now 是 B 路径的判定时刻）：
// 账号照常被拦下，但不写下到 7d 重置才解除的标记，免得判得出之后 credits 层账号仍被停调。
func kongPlanDrop7dCandidates(account *Account, candidates []*accountSchedulingThresholdCandidate, now time.Time) []*accountSchedulingThresholdCandidate {
	rt := kongPlanRuntimeNow()
	if rt == nil || account == nil || !account.IsOpenAIOAuth() {
		return candidates
	}
	res := rt.tier(context.Background(), account, rt.now())
	inCredits := res.Layer == kongPlanLayerCredits
	if !inCredits && !res.Undecided {
		return candidates
	}
	hold := now.Add(kongPlanUndecidedHold)
	out := make([]*accountSchedulingThresholdCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if c.window == "7d" {
			if inCredits {
				continue
			}
			if c.until != nil && c.until.After(hold) {
				short := *c
				short.until = &hold
				c = &short
			}
		}
		out = append(out, c)
	}
	return out
}

// KongPlanWindowView 是 GET /accounts 里一个窗口的状态：active 有用量与重置时刻；inactive 额度已恢复、等第一个请求
// 起算（含重置时刻已过）；not_applicable 账号没有这个窗口（只有 7d 的账号的 5h）；unknown 读数缺失或判不出。
type KongPlanWindowView struct {
	State     string     `json:"state"`
	Used      *float64   `json:"used,omitempty"`
	Threshold float64    `json:"threshold,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// KongPlanAccountView 是 GET /api/v1/admin/kong-plan/accounts 的一项。
type KongPlanAccountView struct {
	ID              int64                         `json:"id"`
	Tier            string                        `json:"tier"`
	CreditsEligible bool                          `json:"credits_eligible"`
	WindowSeq       int64                         `json:"window_seq"`
	WindowUnknown   bool                          `json:"window_unknown"`
	ReadingAt       *time.Time                    `json:"reading_at"`
	Windows         map[string]KongPlanWindowView `json:"windows"`
	OpenEntries     []KongPlanTierEntry           `json:"open_entries"`
}

func kongPlanWindowState(extra map[string]any, window string, now time.Time) KongPlanWindowView {
	if window == "5h" && kongCodexRecordedShape(extra) == kongCodexShape7d {
		return KongPlanWindowView{State: "not_applicable"}
	}
	raw, ok := extra["codex_"+window+"_used_percent"]
	if !ok || raw == nil {
		return KongPlanWindowView{State: "unknown"}
	}
	used := parseExtraFloat64(raw)
	v := KongPlanWindowView{Used: &used}
	if minutes, ok := extra["codex_"+window+"_window_minutes"]; ok && minutes != nil && parseExtraInt(minutes) <= 0 {
		v.State = "inactive"
		return v
	}
	reset := parseExtraTime(extra["codex_"+window+"_reset_at"])
	switch {
	case reset.IsZero():
		v.State = "unknown"
	case !reset.After(now):
		v.State = "inactive"
	default:
		v.State = "active"
		r := reset.UTC()
		v.ResetAt = &r
	}
	return v
}

// KongPlanAccounts 返回网关此刻对每个 OpenAI OAuth 账号的判断：定层、credits 资格与窗口状态用的是选号时的
// 同一份读数与同一个函数，边车据此清除暂停标记（DESIGN-account-selection.md 第 3.3 节），不必自己重算。
func KongPlanAccounts(ctx context.Context, repo AccountRepository) ([]KongPlanAccountView, error) {
	rt := kongPlanRuntimeNow()
	if rt == nil || repo == nil {
		return nil, infraerrors.ServiceUnavailable(KongPlanReasonUnavailable, "计划组件未启用")
	}
	accounts, err := repo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	now := rt.now()
	out := make([]KongPlanAccountView, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if !a.IsOpenAIOAuth() {
			continue
		}
		res := rt.tier(ctx, a, now)
		v := KongPlanAccountView{
			ID:              a.ID,
			Tier:            res.Layer.String(),
			CreditsEligible: res.Eligible,
			WindowSeq:       res.WindowSeq,
			WindowUnknown:   res.WindowUnknown,
			Windows:         map[string]KongPlanWindowView{},
			OpenEntries:     []KongPlanTierEntry{},
		}
		if !res.ReadingAt.IsZero() {
			at := res.ReadingAt.UTC()
			v.ReadingAt = &at
		}
		for _, w := range []string{"5h", "7d"} {
			wv := kongPlanWindowState(a.Extra, w, now)
			if wv.State != "not_applicable" {
				wv.Threshold = rt.windowThresholdPercent(ctx, a, w, now)
			}
			v.Windows[w] = wv
		}
		for _, e := range rt.ledger.Entries(a.ID) {
			if !e.Settled {
				v.OpenEntries = append(v.OpenEntries, e)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
