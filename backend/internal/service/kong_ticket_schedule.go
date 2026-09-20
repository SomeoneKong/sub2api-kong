package service

import (
	"fmt"
	"time"
)

// Codex 票据的调度决策。设计见 DESIGN-codex-ticket.md §4.2 / §4.3。
//
// 这一层刻意做成纯函数：给定「此刻的状态」返回「该做什么」，不碰数据库也不发请求。理由是本
// 功能最容易出错的地方全是时序——预取失败之后隔多久才能再取、冷却与静默各自从哪一刻起算——
// 而这些只有在可以任意构造时间点的情况下才测得动。

// 取票由请求驱动，没有定时器。这里是唯一的决策入口。
const (
	// KongActionInject 有可用票，直接注入放行。
	KongActionInject = "inject"
	// KongActionInjectAndPrefetch 票还能用但已进入刷新窗口：注入当前票放行，同时异步预取下一张。
	KongActionInjectAndPrefetch = "inject_and_prefetch"
	// KongActionVerifyCandidate 无可用票，但缓存里有候选：先验它，零采集成本。
	KongActionVerifyCandidate = "verify_candidate"
	// KongActionInjectAndVerifyCandidate：当前票还能用但已进入刷新窗口，而缓存里有一张够格的候选
	// ——注入旧票，同时**异步**验证那张候选。
	//
	// 批量取票之后这条路径才有意义：非触发模型的票是在别的模型那一轮里顺带取回来的，作为候选躺在
	// 池子里。不接这一步的话它要一直等到旧票过期，那时才**同步**验证，于是每个周期都有一个请求
	// 白等一次验证（最坏三份挑战，足以超过等待预算变成拒服）。
	KongActionInjectAndVerifyCandidate = "inject_and_verify_candidate"
	// KongActionFetch 无可用票也无候选：经票据出口主动取。
	KongActionFetch = "fetch"
	// KongActionWait 已有在途的取票验证任务：加入等待，不另起一个。
	KongActionWait = "wait"
	// KongActionDeny 拒绝该请求。绝不降级放行。
	KongActionDeny = "deny"
)

// 拒服原因。它们要能直接解释给运维看——「一直拿不到合格票」这种表象背后有好几种完全不同的成因。
const (
	KongDenyAccountUnready = "account_unready"  // 账号不可调度
	KongDenyEgressUnusable = "egress_unusable"  // 票据出口失效、或与流量出口合并
	KongDenyNoTicketSource = "no_ticket_source" // 配置为 none，没有主动取票能力
	KongDenyWindowClosed   = "window_closed"    // 静默或冷却未满
	KongDenyModeNotFull    = "mode_not_full"    // 非 full 模式不参与注入
	KongDenyEgressBusy     = "egress_busy"      // 票据出口正被另一个账号占用
	KongDenyWaitTimeout    = "wait_timeout"     // 在途任务未在等待期限内产出结果
	// KongDenyOtherModelTask：等到的在途任务在给**另一个模型**取票。
	//
	// 任务按账号串行（票据出口的静默按 IP 积累，只按模型串行拦不住并发取票），所以一个模型的请求
	// 可能等到另一个模型的任务。它完成时本模型并没有新票——这一次拒服，下一个请求会重新决策。
	// 单列一个原因是为了让「为什么这个模型一直没被验证」在事件流里读得出来。
	KongDenyOtherModelTask = "other_model_task"
	// KongDenyTaskNoTicket 表示**本模型的任务跑完了但没产出可用票**：上游没下发、长度被黑名单
	// 挡住、或归因不合格。
	//
	// 它与 window_closed 必须分开：后者的含义是"还没到能取的时候"，而这里是"取过了、没成"。混在
	// 一起会把运维引向错误的原因——人工触发已经跳过了窗口，页面却提示"静默或冷却未满"。
	KongDenyTaskNoTicket = "task_no_ticket"
)

// KongTicketParams 是调度的时间参数。默认值的依据见设计 §7.1 / §7.2。
type KongTicketParams struct {
	// RefreshBefore 票剩余低于它即异步预取下一张。**越小越好**：取票间隔 = TTL − 它，
	// 而这个间隔就是票据出口能积累到的静默。下界只由一次预取的耗时决定。
	RefreshBefore time.Duration
	// TicketFetchMinIdle 两次主动取票之间该出口的最小空闲，即公式里的 M。
	TicketFetchMinIdle time.Duration
	// VerifyFailCooldown 进入冷却后的退避期，即公式里的 C。与 M 取同值**不等于**不额外加时：
	// C 从失败判定时刻起算、M 从最后活动时刻起算，两者不同。
	VerifyFailCooldown time.Duration
	// MinTicketAge 只作用于 observed 票：它压的是被动票的验证频率。fetch 票立即验证。
	MinTicketAge time.Duration
	// ObserveProbeInterval observe 模式两次探测之间的最小间隔（不是定时周期）。
	ObserveProbeInterval time.Duration
}

// KongDefaultTicketParams 返回默认参数。
func KongDefaultTicketParams() KongTicketParams {
	return KongTicketParams{
		RefreshBefore:        300 * time.Second,
		TicketFetchMinIdle:   1800 * time.Second,
		VerifyFailCooldown:   1800 * time.Second,
		MinTicketAge:         300 * time.Second,
		ObserveProbeInterval: 3600 * time.Second,
	}
}

// Validate 检查参数组合是否会让某条路径不可达（设计 §7.3）。
//
// 只拒绝真正不可达的组合：「预取被静默门槛推迟」本身不等于空窗——推迟之后仍可能在票过期前
// 完成，所以那不是错误配置。
func (p KongTicketParams) Validate(ticketTTL time.Duration) error {
	if p.RefreshBefore <= 0 {
		return fmt.Errorf("refresh_before 必须为正")
	}
	if p.RefreshBefore >= ticketTTL {
		return fmt.Errorf("refresh_before(%s) 不得大于等于票的可用寿命(%s)，否则票一到手就处于刷新窗口内",
			p.RefreshBefore, ticketTTL)
	}
	if p.TicketFetchMinIdle < 0 || p.VerifyFailCooldown < 0 {
		return fmt.Errorf("ticket_fetch_min_idle 与 verify_fail_cooldown 不得为负")
	}
	if p.MinTicketAge < 0 {
		return fmt.Errorf("min_ticket_age 不得为负")
	}
	if p.MinTicketAge >= ticketTTL {
		return fmt.Errorf("min_ticket_age(%s) 不得大于等于票的可用寿命(%s)，否则被动票永远拿不到验证资格",
			p.MinTicketAge, ticketTTL)
	}
	if p.ObserveProbeInterval <= 0 {
		return fmt.Errorf("observe_probe_interval 必须为正")
	}
	return nil
}

// KongScheduleInput 是做一次调度决策所需的全部事实。
type KongScheduleInput struct {
	Now    time.Time
	Mode   KongTicketMode
	Egress KongTicketEgress
	// EgressUsable 为假表示票据出口当前不可用：代理失效、到期，或与流量出口合并。
	// 判定在取票前做，不能只依赖保存时的校验——业务出口可以在无人保存票据配置的情况下改变。
	EgressUsable bool
	// AccountReady 表示账号可正常调度（未禁用、未过期、未被限流暂停、额度未耗尽）。
	// 本功能主动发起的请求都要求它为真；异常账号上的探测既烧额度又可能加重问题，
	// 而且那时测出的档位也不代表正常状态。
	AccountReady bool
	// InflightTask 表示该账号已有在途的取票验证任务。
	InflightTask bool
	// IgnoreWindow 只由**人工触发**置真：跳过静默与冷却这两条时间窗口约束。
	//
	// 它之所以正当，是因为系统只看得见**自己产生的**出口活动（§6.2）——真实静默常常比 A 记录的
	// 更长，运维据带外信息判断"现在可以取"是合理的。结构性约束（没配出口、出口失效或与流量出口
	// 合并、账号不可调度）**不受它影响**：那些不是调度规则，而是物理上做不到或会造成伤害。
	IgnoreWindow bool

	// CurrentExpiresAt 是当前可用票的过期时刻，无票时为零值。
	CurrentExpiresAt time.Time
	CurrentTicketID  int64
	// HasCandidate 表示缓存里有一张够格被验证的候选（未过期、未被本段无票期跳过、
	// observed 来源还需满 MinTicketAge）。
	HasCandidate   bool
	CandidateID    int64
	LastEgressUsed *time.Time // A：票据出口最后一次被本系统使用
	LastCooldownAt *time.Time // F：最近一次进入冷却
	Params         KongTicketParams
}

// KongScheduleDecision 是决策结果。
type KongScheduleDecision struct {
	// CandidateID 只在 InjectAndVerifyCandidate 下有值：要异步验证的那张候选。
	CandidateID int64
	Action      string
	// DenyReason 仅在 Action 为 deny 时有值。
	DenyReason string
	// TicketID 是本次要注入的票，或要验证的候选。
	TicketID int64
	// RetryAfter 是拒服时调用方可以回报的最早重试时刻；零值表示无法预估
	// （例如账号被禁用，恢复时间不由本系统决定）。
	RetryAfter time.Time
}

// KongNextFetchAllowedAt 返回下一次主动取票最早被允许的时刻：max(A+M, F+C)。
//
// 两个起算点不同是这套调度最容易算错的地方：静默从「出口最后一次活动」起算，而冷却从「失败
// 判定」起算，后者总是晚于前者（取票有耗时、验证更慢）。所以把 C 取成与 M 相同的值并不等于
// 「失败后不额外加时」，实际期限仍比单纯的静默门槛晚了 F − A。
func KongNextFetchAllowedAt(lastEgressUsed, lastCooldownAt *time.Time, params KongTicketParams) time.Time {
	var earliest time.Time
	if lastEgressUsed != nil {
		earliest = lastEgressUsed.Add(params.TicketFetchMinIdle)
	}
	if lastCooldownAt != nil {
		if t := lastCooldownAt.Add(params.VerifyFailCooldown); t.After(earliest) {
			earliest = t
		}
	}
	return earliest
}

// KongDecideTicketAction 按当前状态决定下一步。
//
// 顺序是刻意的：先排除「根本不该发起主动请求」的情形（模式、账号状态），再看有没有可用票，
// 最后才考虑取票。有票时永远不会因为出口不可用而拒服——配置出错不该作废一张好票。
func KongDecideTicketAction(in KongScheduleInput) KongScheduleDecision {
	if in.Mode != KongTicketModeFull {
		return KongScheduleDecision{Action: KongActionDeny, DenyReason: KongDenyModeNotFull}
	}

	hasCurrent := !in.CurrentExpiresAt.IsZero() && in.CurrentExpiresAt.After(in.Now)
	if hasCurrent {
		remaining := in.CurrentExpiresAt.Sub(in.Now)
		if remaining > in.Params.RefreshBefore {
			return KongScheduleDecision{Action: KongActionInject, TicketID: in.CurrentTicketID}
		}
		// 进入刷新窗口。能不能预取取决于出口与窗口，但无论如何当前票仍然可用——
		// 阻塞只发生在完全无票时。
		if in.InflightTask {
			// 预取已在路上，本次请求照常用当前票，不重复发起。single-flight 是兜底，
			// 不该是唯一防线：这里就不产出第二个「去预取」的指令。
			return KongScheduleDecision{Action: KongActionInject, TicketID: in.CurrentTicketID}
		}
		// 候选优先于预取：验证候选走**流量出口**，一点票据出口的静默都不消耗，而预取要花掉整段
		// 静默。手上已经有一张够格的候选时去取新票，等于把最稀缺的资源用在已经有的东西上。
		if in.HasCandidate {
			return KongScheduleDecision{
				Action: KongActionInjectAndVerifyCandidate,
				// TicketID 仍是当前票——本次请求注入的是它；候选 id 单独给。
				TicketID: in.CurrentTicketID, CandidateID: in.CandidateID,
			}
		}
		if in.canStartTask() == "" {
			return KongScheduleDecision{Action: KongActionInjectAndPrefetch, TicketID: in.CurrentTicketID}
		}
		return KongScheduleDecision{Action: KongActionInject, TicketID: in.CurrentTicketID}
	}

	// 无可用票，当前请求必须等——或者被拒。
	if in.InflightTask {
		return KongScheduleDecision{Action: KongActionWait}
	}
	if !in.AccountReady {
		return KongScheduleDecision{Action: KongActionDeny, DenyReason: KongDenyAccountUnready}
	}
	// 缓存里的候选优先：验证它不消耗票据出口的静默（验证走流量出口）。
	if in.HasCandidate {
		return KongScheduleDecision{Action: KongActionVerifyCandidate, TicketID: in.CandidateID}
	}
	if reason := in.canStartTask(); reason != "" {
		decision := KongScheduleDecision{Action: KongActionDeny, DenyReason: reason}
		if reason == KongDenyWindowClosed {
			decision.RetryAfter = KongNextFetchAllowedAt(in.LastEgressUsed, in.LastCooldownAt, in.Params)
		}
		return decision
	}
	return KongScheduleDecision{Action: KongActionFetch}
}

// canStartTask 返回空串表示此刻可以发起一次主动取票，否则给出拒绝原因。
func (in KongScheduleInput) canStartTask() string {
	if !in.AccountReady {
		return KongDenyAccountUnready
	}
	if in.Egress == KongTicketEgressNone {
		return KongDenyNoTicketSource
	}
	if !in.EgressUsable {
		return KongDenyEgressUnusable
	}
	if !in.IgnoreWindow {
		if allowed := KongNextFetchAllowedAt(in.LastEgressUsed, in.LastCooldownAt, in.Params); in.Now.Before(allowed) {
			return KongDenyWindowClosed
		}
	}
	return ""
}

// KongObserveShouldProbe 判断 observe 模式此刻是否该对新收到的票做一次探测。
//
// observe 同样是请求驱动的：只有业务响应带回新票才可能触发，没有业务就没有探测。它还受
// 「账号可正常调度」约束——探测是主动发起的请求，账号异常时测出的档位也不代表正常状态。
func KongObserveShouldProbe(now time.Time, accountReady bool, lastProbeAt *time.Time, params KongTicketParams) bool {
	if !accountReady {
		return false
	}
	if lastProbeAt == nil {
		return true
	}
	return !now.Before(lastProbeAt.Add(params.ObserveProbeInterval))
}
