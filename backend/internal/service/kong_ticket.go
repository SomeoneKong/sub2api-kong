package service

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"
)

// Codex 票据：领域类型与仓储接口。设计见仓库根的 DESIGN-codex-ticket.md。
//
// 账号级配置存在 accounts.extra（jsonb）里而不是新增列：上游把 ent 生成代码全部入库，
// 加列会重新生成 ent/migrate/schema.go、ent/mutation.go 等上游每周改动数次的文件。
// 三张数据表走手写 SQL（migrations/900_kong_codex_ticket.sql），同样为避开 ent 生成。

// accounts.extra 中的配置键。kong_ 前缀与上游将来可能的同名实现隔开。
const (
	KongTicketModeKey    = "kong_ticket_mode"
	KongTicketEgressKey  = "kong_ticket_egress"
	KongTicketProxyIDKey = "kong_ticket_proxy_id"
)

// KongTicketMode 决定本账号参与到哪一步。收票在三种模式下都进行，差异在探测与注入。
type KongTicketMode string

const (
	KongTicketModeOff     KongTicketMode = "off"
	KongTicketModeObserve KongTicketMode = "observe"
	KongTicketModeFull    KongTicketMode = "full"
)

// KongTicketEgress 是票据出口的三态。「没配出口」与「出口就是直连」必须能分开表达：
// 前者意味着不主动取票，后者意味着用服务器本机 IP 取票。
type KongTicketEgress string

const (
	KongTicketEgressNone   KongTicketEgress = "none"
	KongTicketEgressDirect KongTicketEgress = "direct"
	KongTicketEgressProxy  KongTicketEgress = "proxy"
)

// KongTicketConfig 是一个账号的票据配置。
type KongTicketConfig struct {
	Mode    KongTicketMode
	Egress  KongTicketEgress
	ProxyID *int64
}

// ParseKongTicketConfig 从 accounts.extra 解析配置。
//
// 任何非法取值都落到保守的一侧：mode → off（不注入、不主动取票），egress → none（不主动取票）。
// 尤其是 egress 绝不能在解析失败时落到 direct——直连取票用的是服务器自己的 IP，静默被烧掉还能
// 等回来，把账号与服务器真实出口关联起来是不可逆的。
//
// 第二个返回值列出被拒绝的键，调用方据此记事件：静默纠正配置会让「为什么这个账号不取票」
// 无从排查。
func ParseKongTicketConfig(extra map[string]any) (KongTicketConfig, []string) {
	cfg := KongTicketConfig{Mode: KongTicketModeOff, Egress: KongTicketEgressNone}
	var rejected []string

	if modeStr, typeOK := kongExtraString(extra, KongTicketModeKey); !typeOK {
		rejected = append(rejected, KongTicketModeKey)
	} else {
		switch KongTicketMode(strings.TrimSpace(modeStr)) {
		case KongTicketModeObserve:
			cfg.Mode = KongTicketModeObserve
		case KongTicketModeFull:
			cfg.Mode = KongTicketModeFull
		case KongTicketModeOff, "":
			// 缺省即 off
		default:
			rejected = append(rejected, KongTicketModeKey)
		}
	}

	egressStr, typeOK := kongExtraString(extra, KongTicketEgressKey)
	if !typeOK {
		rejected = append(rejected, KongTicketEgressKey)
		return cfg, rejected
	}
	switch KongTicketEgress(strings.TrimSpace(egressStr)) {
	case KongTicketEgressDirect:
		cfg.Egress = KongTicketEgressDirect
	case KongTicketEgressProxy:
		if id, ok := kongExtraInt64(extra[KongTicketProxyIDKey]); ok && id > 0 {
			cfg.Egress = KongTicketEgressProxy
			cfg.ProxyID = &id
		} else {
			// egress=proxy 但 id 缺失或非法：按 none 处理。配置存在 extra 里，没有外键
			// 替我们清空失效的 id，所以这个状态是预期会出现的，不是异常。
			rejected = append(rejected, KongTicketProxyIDKey)
		}
	case KongTicketEgressNone, "":
		// 缺省即 none
	default:
		rejected = append(rejected, KongTicketEgressKey)
	}

	return cfg, rejected
}

// kongExtraString 取一个字符串配置项。第二个返回值区分「键不存在」（合法，用缺省）与
// 「存在但类型不对」（配置写错了，必须进 rejected）——把后者也当成缺省，会让写错的配置
// 静默生效为缺省值，而调用方无从记事件。
func kongExtraString(extra map[string]any, key string) (string, bool) {
	raw, present := extra[key]
	if !present || raw == nil {
		return "", true
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	return s, true
}

func kongExtraInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		// JSON 数字都走这一支，三类都必须挡住：带小数部分（12.5 静默截成 12，而 12 可能是
		// 另一个真实存在的代理，取票就走到了错误出口）、超出 IEEE754 安全整数范围（舍入在
		// json 解码阶段就已发生，这里再判 int64 范围也来不及）、以及 NaN / Inf。
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		if n != math.Trunc(n) {
			return 0, false
		}
		const maxSafeInteger = 1<<53 - 1
		if n > maxSafeInteger || n < -maxSafeInteger {
			return 0, false
		}
		return int64(n), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// 票的来源与状态。
const (
	KongTicketSourceObserved = "observed"
	KongTicketSourceFetch    = "fetch"

	KongTicketStatusUnverified = "unverified"
	KongTicketStatusVerified   = "verified"
	KongTicketStatusRejected   = "rejected"

	// KongTicketExpiryParsed 表示过期时刻取自票本身；Estimated 表示按收到时刻 + TTL 推算。
	// Estimated 是下界估计（被动收到的票可能已用掉一部分寿命），续期提前量必须覆盖该误差。
	KongTicketExpiryParsed    = "parsed"
	KongTicketExpiryEstimated = "estimated"
)

// KongTicket 是一张票。model 用最终上游模型名——票与验证结论按 (account, model) 绑定。
type KongTicket struct {
	ID        int64
	AccountID int64
	Model     string
	State     string
	StateLen  int
	Source    string
	Status    string
	// FingerprintModel / FingerprintP 是最像的那个模型与它的概率——证据，不是采纳结论。
	FingerprintModel *string
	FingerprintP     *float64
	// FingerprintProbs 是归因的完整分布。采纳判据按它与**当前**白名单求和，所以白名单改了
	// 存量票仍能重判；只留 argmax 的话就只剩「照旧放行」与「全部作废」两个都不对的选项。
	FingerprintProbs map[string]float64
	// SkipUntilNew 为真时本段无票期内不再选它。用于「候选未被上游接受」这个终点：
	// 档位没有被证伪，但重选同一张只会重复同一个无效尝试。
	SkipUntilNew    bool
	CapturedAt      time.Time
	ExpiresAt       time.Time
	ExpiresAtSource string
	// CaptureEgress / CaptureIdleSeconds 是采集时的事实，只有 fetch 票有。
	//
	// 随票行持久化而不是验证时重建：两者之间可以隔着重启或配置改动，按当时配置重建会得出一个
	// 与事实不符的出口。
	CaptureEgress      string
	CaptureIdleSeconds *int64
}

// 事件类型。
const (
	KongEventFetch          = "fetch"           // 主动取票
	KongEventObserve        = "observe"         // 被动收票
	KongEventVerify         = "verify"          // 指纹验证出结论
	KongEventInjectFail     = "inject_fail"     // 注入未被上游接受
	KongEventCooldown       = "cooldown"        // 进入冷却
	KongEventEgressInvalid  = "egress_invalid"  // 票据出口配置失效或与流量出口合并
	KongEventAccountUnready = "account_unready" // 账号不可调度，跳过主动请求
	KongEventFetchSkipped   = "fetch_skipped"   // 取票未发出（本地失败，不算出口活动）
	KongEventProbeSkipped   = "probe_skipped"   // observe 未取样
	KongEventObserveProbe   = "observe_probe"   // observe 模式的诊断探测
	KongEventManualRefresh  = "manual_refresh"  // 管理面手工触发取票/验票
)

// 事件成败。单独一列而不是塞进 detail：冷却要查「最近一次失败」（F），参数校准要算
// 「成功率 vs 空闲时长」，两者都不该依赖 jsonb 取值。
const (
	KongOutcomeSuccess = "success"
	KongOutcomeFailure = "failure"
	KongOutcomeSkipped = "skipped"
	KongOutcomeInfo    = "info"
)

// KongTicketEvent 是一条事件。两个出口都要记：出口合并只表现为「一直拿不到合格票」，
// 不记下当时判定的两个出口就只能靠猜。
type KongTicketEvent struct {
	ID               int64     `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	AccountID        int64     `json:"account_id"`
	Model            string    `json:"model"`
	EventType        string    `json:"event_type"`
	Outcome          string    `json:"outcome"`
	StatusCode       *int      `json:"status_code"`
	TrafficEgress    string    `json:"traffic_egress"`
	TicketEgress     string    `json:"ticket_egress"`
	StateLen         *int      `json:"state_len"`
	TicketID         *int64    `json:"ticket_id"`
	FingerprintModel *string   `json:"fingerprint_model"`
	// IdleSeconds 是距本系统最后一次使用该出口的秒数，不是「实际静默」——系统外的活动
	// 观测不到，这个值会高估真实静默。
	IdleSeconds *int64         `json:"idle_seconds"`
	Detail      map[string]any `json:"detail"`
}

// KongFingerprintProbe 是一份指纹探测记录。一次验证含 1~3 份（份数按置信度递增），
// 所以它与事件表分开存。每行自带解释所需的上下文快照，不依赖对票的引用——票会随过期被删，
// 而这些记录要留很久。
type KongFingerprintProbe struct {
	ID                int64
	CreatedAt         time.Time
	VerificationID    string
	PartIndex         int
	AccountID         int64
	TargetModel       string
	TicketID          *int64
	TicketFingerprint string
	TicketSource      string
	VerifyEgress      string
	// CaptureEgress 是这张票被采到时用的票据出口；observed 票为空（它是业务响应带回的）。
	// 与 VerifyEgress 分开：验证走流量出口，两者通常不同，混用会让「哪个出口取到好票」失真。
	CaptureEgress string
	IdleSeconds   *int64
	ChallengeID   string
	// Digits 是归因算法的完整输入。存了它才能在换算法或换校准表之后重算历史结论。
	Digits          []int
	DigitCount      int
	Scores          map[string]float64
	PartAttribution *string
	CumProbability  *float64
	TemperatureTier *int
	// LibraryVersion 标识当时用的挑战集、模型中心、环境方向与校准表。档 "1" 不是一份永不变
	// 的资料——换了校准表，同一序列会算出不同概率，没有版本标识就既不能复核也不能重算。
	LibraryVersion   map[string]any
	ParseValid       bool
	CountedInAverage bool
	// InvalidReason 的取值不止解析失败：候选未被接受、晚到结果作废、验证期间票过期，都会
	// 留下格式完全正常的数字序列。它们可作观测样本，但不是该候选的有效指纹。
	InvalidReason *string
	LatencyMs     *int
	OutputTokens  *int
}

// 探测作废原因。
const (
	KongProbeInvalidRefusal            = "refusal"
	KongProbeInvalidTruncated          = "truncated"
	KongProbeInsufficientDigits        = "insufficient_digits"
	KongProbeInvalidCandidateNotAccept = "candidate_not_accepted"
	KongProbeInvalidStaleResult        = "stale_result"
	KongProbeInvalidTicketExpired      = "ticket_expired"
	KongProbeInvalidNonASCIIDigits     = "non_ascii_digits"
	KongProbeInvalidScoreFailed        = "score_failed"
)

// KongNormalizeEventLimit 把分页上限收敛到有效区间。
//
// 规范化必须只有一处：仓储自己悄悄把越界值改成 100、handler 却把原始值回给调用方时，按响应里的
// limit 推进 offset 的客户端会漏页或重复读。
func KongNormalizeEventLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

// KongTicketEventFilter 是事件查询条件。
// 三个多值条件都是**组内 OR、组间 AND**：空切片表示该维度不过滤。页面上的筛选器是多选的，
// 而「一次看两个账号的同一类事件」正是排查时最常要的对照。
type KongTicketEventFilter struct {
	AccountIDs []int64
	// Models 按最终上游模型过滤。诊断是 (account, model) 绑定的，不按它过滤会取到别的模型的事件。
	Models     []string
	EventTypes []string
	// FinalOnly 只取「最终事件」（detail.final = true）。
	//
	// 诊断必须带它：verify 事件流里既有最终结论，也有单份挑战失败这种非最终事件，不筛就会把
	// 某一份的失败当成本次验证的结论，把真结论盖掉。
	FinalOnly bool
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Offset    int
}

// KongAttribution 是一次归因的可持久化结论。
//
// 三个字段的分工是刻意的：Model / P 是**证据**（最像哪个、有多像），Probs 是重判所需的完整
// 分布。采纳与否不在这里——它由 KongTicketAccept 按当前白名单求和得出，所以不该被固化进票行。
type KongAttribution struct {
	Model string
	P     float64
	Probs map[string]float64
}

// KongTicketRepository 是票据子系统的持久化边界。
type KongTicketRepository interface {
	// VerifiedTickets 返回该账号该模型下所有 verified 且未过期的票，按 expires_at 降序。
	//
	// **归因判据不在这里**：采纳与否由 KongTicketAccept 在 Go 侧统一判定。judge 放在 SQL 里
	// 就成了第二套规则——白名单求和这件事 SQL 也能写，但两处写法只要有一点出入，同一张票在
	// 「读当前票」与「落结论」两条路径上就会得出不同答案，而不一致的那一侧不会报错。
	//
	// 只筛 verified + 未过期仍是必要的：verified 只代表「按当时的白名单与阈值判过」，那两个
	// 值来自环境变量、可以改，所以调用方必须按当前配置重判一次。返回多张而不是一张，是为了
	// 让「较新但按当前白名单不合格的票」不挡住较旧而合格的那张。
	VerifiedTickets(ctx context.Context, accountID int64, model string) ([]*KongTicket, error)
	// OldestCandidate 返回最早的一张可验证候选：unverified、未过期、未被本段无票期跳过，
	// 且 observed 来源需满足 minAge。fetch 票不受 minAge 限制（立即验证）。
	OldestCandidate(ctx context.Context, accountID int64, model string, minAge time.Duration, now time.Time) (*KongTicket, error)
	// NewestCandidate 返回**最新**的一张可验证候选，供人工「立即验票」用。
	//
	// 与 OldestCandidate 相反的顺序是刻意的，两条路径要的不是同一件事：自动路径在清一个队列，
	// 先验快过期的那张才有机会用上它；人工按下按钮要的是「这个模型尽快有一张能用的当前票」，
	// 那就该挑剩余 TTL 最长的。批量取票之后最新那张还通常正是刚在 292 窗口里取回的 fetch 票。
	// 不受 minAge 约束——那条压的是自动验证的频率。
	NewestCandidate(ctx context.Context, accountID int64, model string, now time.Time) (*KongTicket, error)
	// InsertTicket 插入一张票。第二个返回值为假表示这张票原值已经存在——重复出现不是新信息，
	// 调用方不得据此延长期限、解除候选跳过标记或触发诊断探测。
	InsertTicket(ctx context.Context, t *KongTicket) (int64, bool, error)
	// TicketByID 按 (账号, 票 id) 读一张票，含票原值。**账号必须参与匹配**——否则换个 id 就能让
	// 一个账号去验别人的票。不存在或不属于该账号时返回 nil。
	TicketByID(ctx context.Context, accountID, id int64) (*KongTicket, error)
	// TicketStatus 读一张票当前的状态。人工重验要据它区分"已被判不合格"与"没得出结论"——
	// 前者 verifyTicket 自己已经提交成 rejected，后者旧结论仍然有效。
	TicketStatus(ctx context.Context, id int64) (string, error)
	// SetTicketStatus 落指纹结论。票已是 rejected 时不生效，返回是否更新——
	// 晚到的结果不能覆盖已被改写的状态。
	SetTicketStatus(ctx context.Context, id int64, status string, attr KongAttribution) (bool, error)
	// SkipCandidate 标记该候选在本段无票期内不再被选中。
	SkipCandidate(ctx context.Context, id int64) error
	// ReviveRejectedCandidate 把最晚过期的那张**未过期的已拒票**复位成候选，返回其 id（没有则 0）。
	//
	// 只给手工触发用。被拒有两种来路：验证判为不合格（白名单或阈值改了之后同一张票可能就合格了），
	// 以及注入未被上游接受后被撤销（与配置无关，重验会再次以 candidate_not_accepted 失败）。两者
	// 都安全，所以不必分辨——验证路径本身能识别后者。
	//
	// 复位而不是新取票的理由是资源：验证走**流量出口**，不消耗票据出口的静默，而静默是这套系统
	// 最稀缺的资源（门槛约 34 分钟）。
	// 返回复位后的那张票（含票原值），便于调用方把它**直接**钉成本次验证目标——否则泛选候选可能
	// 选到别张票，而页面已经报了"正在复用该票重验"。没有可复位的票时返回 nil。
	ReviveRejectedCandidate(ctx context.Context, accountID int64, model string) (*KongTicket, error)
	// PrepareTicketForManualVerify 把一张**未过期的**票准备成"可以立刻重验"的状态，返回准备后的
	// 它（不满足条件时返回 nil）。两件事一起做，因为它们的理由是同一个——人工指名要验这一张：
	//
	//   - `rejected` → `unverified`：改过接受白名单或阈值之后，同一份证据可能就合格了；
	//   - 清掉 `skip_until_new`：不清的话 CommitVerification 的 `NOT skip_until_new` 必然挡住结论
	//     ——挑战照样发出去、额度照样烧，而任何结论都提交不了。
	//
	// **只动这一张**（按 id）。不要用 ClearSkipMarks：那会解除整个 (账号, 模型) 候选池的标记，把
	// 别的票也一起放回池子，而人工只点了一张。
	PrepareTicketForManualVerify(ctx context.Context, id int64) (*KongTicket, error)
	// ListTickets 列一个账号名下的票，最近采集的在前，供管理页面展示。
	//
	// **返回值含票原值**（`State`），与库里一致；调用方绝不可把它下发给客户端——那是可注入的凭据。
	// 管理响应只放派生信息（长度、归因、时间）。limit 为 0 时用一个保守上限。
	ListTickets(ctx context.Context, accountID int64, limit int) ([]*KongTicket, error)
	// CommitVerification 在同一个事务里落「资格 + 最终事件」。返回假表示条件不满足
	// （票已过期、已被改写或已被跳过），此时什么都没写。
	//
	// 不能拆成「先改状态、再写事件」：事件写失败时资格已经对所有并发请求可见，而库里没有任何
	// 记录解释这张票为什么可用。
	CommitVerification(ctx context.Context, id int64, status string, attr KongAttribution, event *KongTicketEvent) (bool, error)
	// SkipCandidatesFor 标记该账号该模型下当前所有未验证候选。用于「本段无票期的候选机会已用掉」
	// 这个终点——只标一张会让缓存里的其它旧候选被逐张验证，而候选验证不经过取票冷却。
	// capturedBefore 之后收到的票不受影响——验证期间新到的票属于新信息。
	SkipCandidatesFor(ctx context.Context, accountID int64, model string, capturedBefore time.Time) error
	// ClearSkipMarks 在出现「新信息」（新到的 observed 票或一次成功 fetch）时解除跳过标记。
	ClearSkipMarks(ctx context.Context, accountID int64, model string) error
	// RevokeTicket 撤销一张票的服务资格。撤销对象必须是本次实际使用的票，不是「当前票」：
	// 并发下前者可能已被新票接替，按后者撤销会作废无辜的新票。
	RevokeTicket(ctx context.Context, id int64) error
	CountTickets(ctx context.Context, accountID int64, model string, status string) (int, error)
	DeleteExpiredTickets(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)

	InsertEvent(ctx context.Context, e *KongTicketEvent) error
	ListEvents(ctx context.Context, filter *KongTicketEventFilter) ([]*KongTicketEvent, int, error)
	// LastEgressActivity 返回该票据出口最后一次被本系统使用的时刻，即 A。取票本身有耗时，
	// 记的是最后一次网络活动而不是请求开始时刻，否则静默会被算多。
	LastEgressActivity(ctx context.Context, ticketEgress string) (*time.Time, error)
	// LastFailure 返回该出口最近一次**进入冷却**的时刻，即 F。冷却从它起算，而静默从 A 起算
	// ——两者不同，下一次尝试的期限是 max(A+M, F+C)。
	//
	// 它只看 KongEventCooldown 事件，不看所有 failure：observed 候选验证失败应当立即升级为
	// 主动取票，把那种失败也算进 F 会让升级白等一个冷却期。所以「是否进冷却」由写入方判断并
	// 落一条 cooldown 事件，该事件的 CreatedAt 即失败判定时刻。
	LastFailure(ctx context.Context, accountID int64, ticketEgress string) (*time.Time, error)

	// LastEventAt 返回该账号该模型最近一次指定类型事件的时刻。
	// observe 的探测间隔靠它推进——这套设计没有定时器，"上次探测是什么时候"只能从事件里读。
	LastEventAt(ctx context.Context, accountID int64, model, eventType string) (*time.Time, error)

	// InsertProbes 写入一次验证的全部探测记录。
	InsertProbes(ctx context.Context, probes []*KongFingerprintProbe) error
	ListProbesByVerification(ctx context.Context, verificationID string) ([]*KongFingerprintProbe, error)
}
