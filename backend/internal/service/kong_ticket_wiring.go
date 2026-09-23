package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 票据功能的装配。设计见 DESIGN-codex-ticket.md §6 / §7。
//
// 配置走环境变量而不是上游的 config 结构：那个结构是上游的高频改动面，往里加字段等于给每次
// rebase 多埋一处冲突，而本功能的开关只有运维会动、不需要进面板。

const (
	// KongTicketGatedModelsEnv 是门控模型集合（逗号分隔）。**留空即整个功能不生效**。
	KongTicketGatedModelsEnv = "KONG_TICKET_GATED_MODELS"
	// 归因的接受关系走 config.Gateway.KongCodexTicket.AcceptExtra，不在这里列。默认每个门控
	// 模型只接受自己的归因，额外关系（例如 sol 接受 astra）显式配置。
	// KongTicketConfidenceEnv 是采纳归因结论所需的概率，不够就再加一份挑战。
	KongTicketConfidenceEnv = "KONG_TICKET_CONFIDENCE"

	KongTicketRefreshBeforeEnv = "KONG_TICKET_REFRESH_BEFORE_SECONDS"
	KongTicketMinIdleEnv       = "KONG_TICKET_FETCH_MIN_IDLE_SECONDS"
	KongTicketCooldownEnv      = "KONG_TICKET_VERIFY_FAIL_COOLDOWN_SECONDS"
	KongTicketMinAgeEnv        = "KONG_TICKET_MIN_TICKET_AGE_SECONDS"
	KongTicketObserveEnv       = "KONG_TICKET_OBSERVE_PROBE_INTERVAL_SECONDS"
)

// KongTicketComponents 打包本功能对外暴露的部件。
//
// Enabled 为假时 Service 与 Gateway 是 nil，但 Admin 仍然装配——管理面只读状态、不需要校准
// 资料，这样页面能明确显示「未启用」而不是把正常的默认状态呈现成服务故障。
type KongTicketComponents struct {
	Enabled bool
	Service *KongTicketService
	Admin   *KongTicketAdminService
	// Gateway 要在装配后注入 OpenAIGatewayService；未启用时为 nil，接入点全部退化为空操作。
	Gateway *KongTicketGateway
}

// kongAccountAccess 用上游已有的仓储实现本功能的账号访问面，不碰 ent。
type kongAccountAccess struct {
	accounts AccountRepository
	proxies  ProxyRepository
}

// NewKongAccountAccess 创建账号访问适配器。
func NewKongAccountAccess(accounts AccountRepository, proxies ProxyRepository) KongAccountAccess {
	return &kongAccountAccess{accounts: accounts, proxies: proxies}
}

func (a *kongAccountAccess) ListTicketCapableAccounts(ctx context.Context) ([]KongAccountView, error) {
	accounts, err := a.accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, fmt.Errorf("按平台列账号: %w", err)
	}
	out := make([]KongAccountView, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !account.UsesOpenAICodexProtocol() {
			continue
		}
		out = append(out, kongViewOf(account))
	}
	return out, nil
}

func (a *kongAccountAccess) GetAccountView(ctx context.Context, accountID int64) (*KongAccountView, error) {
	account, err := a.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, nil
	}
	view := kongViewOf(account)
	return &view, nil
}

// UpdateAccountExtra 合并写 extra。上游的实现是 JSONB 合并（原子），所以只传本功能的键即可，
// 不会弄丢并发写入的其它键。
func (a *kongAccountAccess) UpdateAccountExtra(ctx context.Context, accountID int64, extra map[string]any) error {
	return a.accounts.UpdateExtra(ctx, accountID, extra)
}

func (a *kongAccountAccess) GetProxyState(ctx context.Context, proxyID int64) (KongTicketProxyState, error) {
	proxy, err := a.proxies.GetByID(ctx, proxyID)
	if err != nil {
		// 查不到与出错分不开时按「不存在」处理：那一侧的后果是停止主动取票，是保守方向。
		return KongTicketProxyState{Exists: false}, nil
	}
	return KongProxyStateOf(proxy, time.Now()), nil
}

func kongViewOf(account *Account) KongAccountView {
	view := KongAccountView{
		ID:       account.ID,
		Name:     account.Name,
		Platform: account.Platform,
		ProxyID:  account.ProxyID,
		Extra:    account.Extra,
		Ready:    account.IsSchedulable(),
	}
	if !view.Ready {
		view.NotReadyReason = kongNotReadyReason(account)
	}
	return view
}

// kongNotReadyReason 给界面一句能解释「为什么诊断暂停」的话。
func kongNotReadyReason(account *Account) string {
	switch {
	case !account.IsActive():
		return "账号非 active"
	case !account.Schedulable:
		return "已手工置为不可调度"
	case account.IsRateLimited():
		return "限流窗口未过"
	case account.IsOverloaded():
		return "过载窗口未过"
	case account.IsQuotaExceeded():
		return "额度已耗尽"
	default:
		return "当前不可调度"
	}
}

// NewKongTicketComponents 按环境变量与 config 装配本功能。
//
// 门控集合**未设置时取内置默认**（`KongDefaultGatedModels`）；**显式设成空串即整个功能不装配**。
// 区分这两者的理由与时间参数一致（见 kongEnvDuration）：把"配了个空值"静默当成默认，等于运维
// 以为关了、系统其实在跑。
//
// 默认开着门控不等于默认改变行为：账号级 `mode` 缺省是 off，此时每个请求都判 `NotApplicable`
// ——照常服务、不注入也不拒服。真正的开关是账号级的 mode。
//
// 一旦启用，校准资料加载失败必须让启动失败（fail-closed）：启用了却验证不了，等于放行降智
// 请求，那比启动失败糟得多。
func NewKongTicketComponents(repo KongTicketRepository, upstream KongTicketUpstream, accountRepo AccountRepository, proxyRepo ProxyRepository, ticketCfg config.KongCodexTicketConfig) (*KongTicketComponents, error) {
	raw, present := os.LookupEnv(KongTicketGatedModelsEnv)
	if !present {
		raw = strings.Join(KongDefaultGatedModels, ",")
	}
	gated := kongSplitModels(raw)
	if len(gated) == 0 {
		// 功能不生效，但管理面照常装配：它只读状态，不需要校准资料。页面因此能明确显示
		// 「未启用、原因是门控集合为空」，而不是把这个正常的默认状态呈现成服务故障
		// （端点 503 → 页面只能显示加载失败）。
		return &KongTicketComponents{
			Enabled: false,
			// 未启用时接受关系与阈值取不到也无所谓：没有门控模型，就不会查任何「当前票」。
			Admin: NewKongTicketAdminService(repo, NewKongAccountAccess(accountRepo, proxyRepo), KongDefaultTicketParams(), nil, nil, nil, 0),
		}, nil
	}

	bank, err := KongFingerprintBankLoad()
	if err != nil {
		return nil, fmt.Errorf("codex 票据功能已启用（%s 非空）但校准资料加载失败: %w", KongTicketGatedModelsEnv, err)
	}

	// 门控模型自己必须在校准资料里：闭集归因下库外模型会被归到最相似的现有候选，那时
	// 「判为它」根本不成立，表现却只是一直拿不到合格票。
	for _, model := range gated {
		if !bank.HasModel(model) {
			return nil, fmt.Errorf("门控模型 %q 不在校准资料里，准入判定不成立", model)
		}
	}
	accept, acceptWarnings, err := KongParseTicketAccept(gated, ticketCfg.AcceptExtra, bank)
	if err != nil {
		return nil, err
	}
	for _, w := range acceptWarnings {
		// 死配置只告警不拒启动（理由见 KongParseTicketAccept）。用 Warn 而不是 Info：它一定是
		// 配错了，只是错得不值得把整个网关停掉。
		slog.Warn("codex 票据：接受白名单有条目被忽略", "detail", w)
	}

	params := KongDefaultTicketParams()
	for _, item := range []struct {
		env    string
		target *time.Duration
	}{
		{KongTicketRefreshBeforeEnv, &params.RefreshBefore},
		{KongTicketMinIdleEnv, &params.TicketFetchMinIdle},
		{KongTicketCooldownEnv, &params.VerifyFailCooldown},
		{KongTicketMinAgeEnv, &params.MinTicketAge},
		{KongTicketObserveEnv, &params.ObserveProbeInterval},
	} {
		value, convErr := kongEnvDuration(item.env, *item.target)
		if convErr != nil {
			return nil, convErr
		}
		*item.target = value
	}
	if err := params.Validate(kongTicketTTL); err != nil {
		return nil, fmt.Errorf("codex 票据的时间参数不合法: %w", err)
	}
	eventRetention, err := kongEventRetention(ticketCfg.EventRetentionDays, params)
	if err != nil {
		return nil, err
	}

	confidence := 0.9
	if raw := strings.TrimSpace(os.Getenv(KongTicketConfidenceEnv)); raw != "" {
		v, convErr := strconv.ParseFloat(raw, 64)
		// NaN 参与任何比较都为假，所以必须显式挡掉——否则 NaN 会通过区间检查，之后
		// 「概率 >= 置信度」恒为假，表现成永远拿不到合格票。
		if convErr != nil || math.IsNaN(v) || v <= 0 || v >= 1 {
			return nil, fmt.Errorf("%s 必须是 (0,1) 之间的小数，得到 %q", KongTicketConfidenceEnv, raw)
		}
		confidence = v
	}

	// stg0 的白名单与 accept 分开解析：它的取值不受校准资料约束（上游可能回报任何 model 名，
	// 升级投放的目标压根不在指纹库里），照 accept 那样校验会把正确配置当笔误忽略。
	stg0, stg0Warnings, err := KongParseStg0Accept(gated, ticketCfg.Stg0Accept)
	if err != nil {
		return nil, err
	}
	for _, w := range stg0Warnings {
		slog.Warn("codex 票据：stg0 白名单有条目被忽略", "detail", w)
	}

	access := NewKongAccountAccess(accountRepo, proxyRepo)
	ticketService := NewKongTicketService(repo, upstream, accountRepo, bank, params,
		gated, ticketCfg.BatchFetchAllModels, ticketCfg.FetchFusedFingerprint, accept, stg0, confidence)
	ticketService.SetEventRetention(eventRetention)
	adminService := NewKongTicketAdminService(repo, access, params, gated, accept, stg0, confidence)
	// 手工触发要走编排服务的正常决策路径，所以 Admin 需要它。
	adminService.SetTicketService(ticketService)
	return &KongTicketComponents{
		Enabled: true,
		Service: ticketService,
		// 接受关系与阈值必须与编排服务同源：管理面显示的「当前票」要和业务实际会注入的那张一致。
		Admin:   adminService,
		Gateway: NewKongTicketGateway(ticketService, gated),
	}, nil
}

func kongSplitModels(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// kongMaxEventRetentionDays 是保留天数能换算成 time.Duration 的上限，再大乘法就会回绕成负数或一个
// 很短的时长——前者让清理永远跳过，后者会删掉几分钟前的事件。
const kongMaxEventRetentionDays = int(math.MaxInt64 / int64(24*time.Hour))

// kongEventRetention 把事件保留天数换算成时长，并确认它盖得住调度回看事件表的每个窗口。
//
// 调度从事件表读三个时刻：出口最后一次活动、最近一次失败、上次诊断探测，分别按最小静默、验证失败
// 冷却、探测间隔判定；在用票的验证结论还要在票的寿命内查得到。保留期短于其中任何一个，清理就会删掉
// 仍在窗口里的事件，调度读不到便按"从未发生"处理——提前取票、提前重试、提前付费探测。
func kongEventRetention(days int, params KongTicketParams) (time.Duration, error) {
	if days <= 0 || days > kongMaxEventRetentionDays {
		return 0, fmt.Errorf("%s 必须在 1 到 %d 之间，得到 %d",
			config.KongTicketEventRetentionDaysEnv, kongMaxEventRetentionDays, days)
	}
	retention := time.Duration(days) * 24 * time.Hour
	need := kongTicketTTL
	for _, window := range []time.Duration{params.TicketFetchMinIdle, params.VerifyFailCooldown, params.ObserveProbeInterval} {
		if window > need {
			need = window
		}
	}
	if retention < need {
		return 0, fmt.Errorf("%s=%d 天短于调度要回看的最长窗口 %s（票寿命、%s、%s、%s 中的最大者）",
			config.KongTicketEventRetentionDaysEnv, days, need,
			KongTicketMinIdleEnv, KongTicketCooldownEnv, KongTicketObserveEnv)
	}
	return retention, nil
}

// kongEnvDuration 读一个以秒为单位的时间参数。
//
// **只有"未设置"才用默认值**。显式给出的值一律严格解析，非法就让启动失败——把拼错的变量名或
// 写错的数值静默换成默认值，等于运维以为改了、系统其实按另一套参数在跑，而这些参数决定的是
// 「多久取一次票」「失败后等多久」这类不会报错、只会表现成一直拿不到好票的行为。
//
// 零是合法取值（例如不额外冷却），所以不能用"非正即回落"那种写法；负数与溢出才是非法。
func kongEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	// 用 LookupEnv 而不是 Getenv：「没设置」与「设成空串」是两回事。后者是写错了配置，按未设置
	// 处理等于把一个明显的错误静默替换成默认值，与「只有未设置才用默认值」的约定相反。
	value, present := os.LookupEnv(key)
	if !present {
		return fallback, nil
	}
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, fmt.Errorf("%s 被设成空值；要用默认值请不要设置该变量", key)
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数秒，得到 %q", key, raw)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("%s 不能为负，得到 %d", key, seconds)
	}
	// time.Duration 是 int64 纳秒，秒数过大会在乘法里回绕成负值。
	if seconds > int64(math.MaxInt64/int64(time.Second)) {
		return 0, fmt.Errorf("%s 过大，%d 秒无法表示", key, seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}
