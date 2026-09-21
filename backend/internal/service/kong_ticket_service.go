package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 票据子系统的编排。设计见 DESIGN-codex-ticket.md §4.2 / §4.3 / §2.4。

// KongAccountLoader 是编排层对账号的最小依赖：发上游请求需要完整的账号对象。
type KongAccountLoader interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// KongTicketService 负责「让一个门控请求拿到可注入的合格票，或明确拒服」。
type KongTicketService struct {
	repo     KongTicketRepository
	upstream KongTicketUpstream
	accounts KongAccountLoader
	bank     *KongFingerprintBank
	params   KongTicketParams

	// gatedModels 是门控模型集合，只用于批量取票（要知道"还该给谁取票"）。准入判定不看它——
	// 那是网关那一层的事。
	gatedModels []string
	// batchFetch 开启时，一次取票并发把所有门控模型的票一起取回来（见 config 里的说明）。
	batchFetch bool
	// fusedFingerprint 开启时，取票请求顺带充当第一份指纹样本（见 config 里的依据与差异说明）。
	fusedFingerprint bool

	// accept 记录每个门控模型接受哪些归因结果（永远含自己）。采纳判据只此一份。
	accept KongTicketAccept
	// stg0 记录每个门控模型额外接受哪些**上游回报值**。
	//
	// 与 accept 是两张表（见 kong_ticket_stg0.go）：那一张补偿指纹归因的测量噪声，这一张处理上游的
	// 升级投放。互抄值会让一次上游明说的降智被当成测量噪声放过。
	stg0 KongStg0Accept
	// confidence 是采纳归因结论所需的概率；不够就再加一份挑战。
	confidence float64

	mu sync.Mutex
	// inflight 按账号记在途任务；inflightEgress 按**票据出口**记。
	//
	// 两个维度都必须有：票据出口的静默是按 IP 积累的，不同账号配同一个出口时并发取票会互相
	// 把静默清零，最后谁也拿不到合格票——只按账号串行拦不住这种情况。
	inflight       map[int64]*kongTicketTask
	inflightEgress map[string]*kongTicketTask
	// lastEgressUseMem 是「该出口最后一次被本进程使用」的内存事实，用于兜住事件写入失败。
	lastEgressUseMem map[string]time.Time
	// lastCooldownMem 是 F（失败判定时刻）的内存事实，键是 (accountID, ticketEgress)。
	// 与 A 一样必须有兜底：冷却事件写失败时库里读不到 F，下一个请求会立刻重试。
	lastCooldownMem map[string]time.Time
}

// kongTicketTask 是一次在途的取票验证任务。
//
// 等待者超时只结束该等待者：已经发生的出口活动无法撤销，所以任务继续跑完、结果入缓存——它的
// 产物是一张票，对下一个请求仍有价值。
type kongTicketTask struct {
	done chan struct{}
	// model 是这个任务在给哪个模型取票。任务按账号串行，所以等待者可能等到的是另一个模型的任务
	// ——拒服原因要能区分这种情况，否则「为什么这个模型一直没被验证」查不出来。
	model string
	// ticketEgress 是本任务占用的票据出口，释放时要据它清掉出口占用。
	ticketEgress string
	ticketID     int64
	// revivedID 非零表示本任务开头把一张已拒票复位成了候选，并以它为验证目标。同步等待的人工
	// 触发要据此向调用方说明走的是重验而不是取票。
	revivedID int64
	err       error
}

// NewKongTicketService 创建编排服务。
func NewKongTicketService(repo KongTicketRepository, upstream KongTicketUpstream, accounts KongAccountLoader, bank *KongFingerprintBank, params KongTicketParams, gatedModels []string, batchFetch, fusedFingerprint bool, accept KongTicketAccept, stg0 KongStg0Accept, confidence float64) *KongTicketService {
	if confidence <= 0 || confidence >= 1 {
		confidence = 0.9
	}
	return &KongTicketService{
		repo:             repo,
		upstream:         upstream,
		accounts:         accounts,
		bank:             bank,
		params:           params,
		gatedModels:      append([]string(nil), gatedModels...),
		batchFetch:       batchFetch,
		fusedFingerprint: fusedFingerprint,
		accept:           accept,
		stg0:             stg0,
		confidence:       confidence,
		inflight:         make(map[int64]*kongTicketTask),
		inflightEgress:   make(map[string]*kongTicketTask),
		lastEgressUseMem: make(map[string]time.Time),
		lastCooldownMem:  make(map[string]time.Time),
	}
}

// KongTicketGrant 是一次准入结果。
type KongTicketGrant struct {
	// NotApplicable 表示该账号没开 full，本次请求不在保护范围内——照常服务，不注入票也不做
	// 交付判定。它与 Allowed=false 是两件事：后者是「该保护但保不了」，必须拒服。
	//
	// 哪些账号要保护是人工按观察结果配的（高概率降智的才开 full），所以「没配」是常态，
	// 不能当成拒服理由。
	NotApplicable bool
	// Allowed 为假时必须拒绝该请求。绝不降级放行。
	Allowed bool
	// State 是要注入的票原值。
	State string
	// TicketID 是本次实际使用的票。撤销必须针对它，而不是「当前票」——并发下后者可能
	// 已被新票接替，按它撤销会作废无辜的新票。
	TicketID   int64
	DenyReason string
	RetryAfter time.Time
	// RevivedTicketID 只在人工触发时非零：本次任务把一张已拒票复位成候选并以它为验证目标。
	// 它**不对外序列化**（本结构带 State，是可注入的凭据，绝不经管理接口下发）。
	RevivedTicketID int64
}

// EnsureTicket 为一个门控请求准备票。
//
// 这是请求驱动的唯一入口：没有定时器，闲置期完全无活动。闲置本身也在为下一张好票做准备——
// 票据出口因此积累静默，而静默是拿到 292 的前提。
func (s *KongTicketService) EnsureTicket(ctx context.Context, accountID int64, model string) (*KongTicketGrant, error) {
	return s.ensureTicketOpts(ctx, accountID, model, true)
}

// EnsureTicketNoHandoff 与 EnsureTicket 相同，但**不返回 preparing**：拿不到票时同步等任务出结果。
//
// 给**原生 WebSocket** 路径用。那条路上的拒服落点是"按策略关闭连接"（1008），没有 failover 可走
// ——返回 preparing 等于把一条本来只需等几十秒的连接直接断掉，而改动前它是同步等的。这是新引入的
// 无谓拒服，必须挡在这里。
//
// 接上 WS 的 failover（首帧未上送时安全切换、已建立会话保持本号）是另一件事，见 DESIGN §4.6 的遗留。
func (s *KongTicketService) EnsureTicketNoHandoff(ctx context.Context, accountID int64, model string) (*KongTicketGrant, error) {
	return s.ensureTicketOpts(ctx, accountID, model, false)
}

func (s *KongTicketService) ensureTicketOpts(ctx context.Context, accountID int64, model string, allowHandoff bool) (*KongTicketGrant, error) {
	return s.ensureTicket(ctx, accountID, model, false, false, allowHandoff)
}

// ensureTicket 是 EnsureTicket 的实现。
//
// manual 为真时跳过静默与冷却（见 KongScheduleInput.IgnoreWindow）；revive 为真时任务在持有槽位
// 之后先尝试复位一张已拒票并以它为验证目标。两者都只由人工触发置真。
func (s *KongTicketService) ensureTicket(ctx context.Context, accountID int64, model string, manual, revive, allowHandoff bool) (*KongTicketGrant, error) {
	account, err := s.accounts.GetByID(ctx, accountID)
	if errors.Is(err, ErrAccountNotFound) || (err == nil && account == nil) {
		// **账号不存在是账号级条件，不是系统故障**：调度到准入之间账号可能被删掉。报成错误会让上层
		// 包成 `ensure_failed`（系统级、不换号），于是"A 已不存在、B 持有合格票"时整个请求在 A 上终止
		// ——那是无谓拒服。account_unready 换号有用，正是它的本意。
		slog.Warn("kong ticket: 账号已不存在，按不可调度处理",
			"account_id", accountID, "model", model, "error", err)
		return &KongTicketGrant{DenyReason: KongDenyAccountUnready}, nil
	}
	if err != nil {
		// 其余读取失败是共享基础设施故障：换一个账号同样读不到，仍按系统级处理。
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	cfg, rejected := ParseKongTicketConfig(account.Extra)
	if len(rejected) > 0 {
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model,
			EventType: KongEventEgressInvalid, Outcome: KongOutcomeSkipped,
			Detail: map[string]any{"rejected_keys": rejected},
		})
	}
	if cfg.Mode != KongTicketModeFull {
		return &KongTicketGrant{NotApplicable: true, DenyReason: KongDenyModeNotFull}, nil
	}

	in, err := s.buildScheduleInput(ctx, account, cfg, model, time.Now(), manual)
	if err != nil {
		return nil, err
	}
	decision := KongDecideTicketAction(*in)

	switch decision.Action {
	case KongActionInject:
		return s.grantExisting(ctx, accountID, model)
	case KongActionInjectAndPrefetch:
		if manual {
			// 人工触发必须**真的跑一遍**。走异步预取会让它在后台按自动口径重算（manual 丢失），
			// 退回 Inject 什么都不做，而接口已经拿旧票报了成功——人工干预于是静默失效。
			return s.runTaskAndWait(ctx, account, cfg, model, decision.Action, manual, revive)
		}
		s.startPrefetch(ctx, account, cfg, model)
		return s.grantExisting(ctx, accountID, model)
	case KongActionInjectAndVerifyCandidate:
		if manual {
			// 人工触发要真的跑一遍并等结论，理由同上（异步会按自动口径重算）。
			return s.runTaskAndWait(ctx, account, cfg, model, KongActionVerifyCandidate, manual, revive)
		}
		// 当前票照常注入，候选在后台验。占**验证专用槽位**：验证走流量出口，与票据出口的静默无关，
		// 不该把出口锁占住——否则同一条出口上别的账号会被拒成 egress_busy。
		s.startVerifyCandidate(ctx, account, model)
		return s.grantExisting(ctx, accountID, model)
	case KongActionWait:
		// 已有在途任务。**这里同样要判一次交接**：不判的话后到的请求会直接进最长五分钟的同步等待，
		// 于是（一）无绑定的新请求明明能换号却原地等；（二）绑定请求首次拿到 preparing、重试一次
		// 就被这条路径吞掉，`pool_mode_retry_count` 再也控制不了等待。
		//
		// 人工触发例外：它就是为了拿结论按的，必须等。
		if allowHandoff && !manual && s.handoffWorthIt(ctx, accountID, model) {
			// 任务已经在跑，不必再起——直接让这次请求去别的账号。
			return &KongTicketGrant{Allowed: false, DenyReason: KongDenyPreparing}, nil
		}
		return s.waitForTask(ctx, accountID, model)
	case KongActionVerifyCandidate, KongActionFetch:
		// 人工触发必须真的跑一遍并等结论——它就是为了拿结论按的。
		if manual || !allowHandoff {
			return s.runTaskAndWait(ctx, account, cfg, model, decision.Action, manual, revive)
		}
		return s.prepareOrHandOff(ctx, account, cfg, model, decision.Action)
	default:
		if !in.AccountReady {
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: accountID, Model: model,
				EventType: KongEventAccountUnready, Outcome: KongOutcomeSkipped,
			})
		}
		return &KongTicketGrant{Allowed: false, DenyReason: decision.DenyReason, RetryAfter: decision.RetryAfter}, nil
	}
}

func (s *KongTicketService) buildScheduleInput(ctx context.Context, account *Account, cfg KongTicketConfig, model string, now time.Time, manual bool) (*KongScheduleInput, error) {
	ticketEgress := KongEgressKey(cfg.Egress, cfg.ProxyID)
	in := &KongScheduleInput{
		IgnoreWindow: manual,
		Now:          now,
		Mode:         cfg.Mode,
		Egress:       cfg.Egress,
		AccountReady: account.IsSchedulable(),
		Params:       s.params,
	}

	// 出口可用性在取票前判定：票据代理可能被删除、到期或停用，业务出口也可能在无人保存票据
	// 配置的情况下改变（上游的代理到期清扫会把账号改投直连），这几条都不经过票据配置的保存。
	//
	// 查的是完整状态而不是「解析得出连接串」：后者拼得出来不代表这个代理还能用，只看它会让
	// 到期与停用两个分支在生产里永远不可达。
	proxyState := KongTicketProxyState{Exists: true}
	if cfg.Egress == KongTicketEgressProxy {
		st, err := s.upstream.ProxyState(ctx, cfg.ProxyID)
		if err != nil {
			return nil, fmt.Errorf("查票据代理状态: %w", err)
		}
		proxyState = st
	}
	usable, reason := KongEvaluateEgress(cfg, account.ProxyID, proxyState, now)
	in.EgressUsable = usable
	if !usable && reason != KongEgressReasonNone {
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: account.ID, Model: model,
			EventType: KongEventEgressInvalid, Outcome: KongOutcomeSkipped,
			TrafficEgress: KongTrafficEgressKey(account.ProxyID),
			TicketEgress:  ticketEgress,
			Detail:        map[string]any{"reason": reason},
		})
	}

	current, err := s.currentTicket(ctx, account.ID, model)
	if err != nil {
		return nil, fmt.Errorf("查当前票: %w", err)
	}
	if current != nil {
		in.CurrentExpiresAt = current.ExpiresAt
		in.CurrentTicketID = current.ID
	}
	candidate, err := s.repo.OldestCandidate(ctx, account.ID, model, s.params.MinTicketAge, now)
	if err != nil {
		return nil, fmt.Errorf("查候选票: %w", err)
	}
	if candidate != nil {
		in.HasCandidate = true
		in.CandidateID = candidate.ID
	}
	if in.LastEgressUsed, err = s.repo.LastEgressActivity(ctx, ticketEgress); err != nil {
		return nil, fmt.Errorf("查出口活动: %w", err)
	}
	// 取库与内存里较晚的那个：事件写失败时库里会偏早，而那次网络活动确实已经发生。
	if mem := s.egressUseMem(ticketEgress); mem != nil && (in.LastEgressUsed == nil || mem.After(*in.LastEgressUsed)) {
		in.LastEgressUsed = mem
	}
	if in.LastCooldownAt, err = s.repo.LastFailure(ctx, account.ID, ticketEgress); err != nil {
		return nil, fmt.Errorf("查冷却: %w", err)
	}
	// 同 A 一样取较晚者：冷却事件写失败时库里偏早，而那次失败确实已经判定了。
	if mem := s.cooldownMem(account.ID, ticketEgress); mem != nil && (in.LastCooldownAt == nil || mem.After(*in.LastCooldownAt)) {
		in.LastCooldownAt = mem
	}

	s.mu.Lock()
	_, in.InflightTask = s.inflight[account.ID]
	s.mu.Unlock()
	return in, nil
}

// currentGrant 按「此刻库里那张合格票」授予资格。没有合格票时返回 nil，由调用方决定拒服原因。
//
// **刻意不要求等于决策时那一张 id。** 决策与取用之间票可以被换掉，而换上来的多半是更好的那张：
// 预取在这段窗口里完成并提交了一张新的 verified 票时，要求 id 相等会把一个本可服务的请求拒掉
// （旧票还没过期、新票也完全合格）。资格条件由 `CurrentTicket` 的查询保证——账号、模型、当前目标
// 与阈值、未过期都在 SQL 里，取回来的任何一张都是可用的。
//
// 撤销针对的是**本次实际注入的那个 id**（见 KongUpstreamAttempt.Grant.TicketID），所以换票不会
// 让撤销打到无辜的票上。
func (s *KongTicketService) currentGrant(ctx context.Context, accountID int64, model string) (*KongTicketGrant, error) {
	ticket, err := s.currentTicket(ctx, accountID, model)
	if err != nil {
		return nil, fmt.Errorf("读当前票: %w", err)
	}
	if ticket == nil {
		return nil, nil
	}
	return &KongTicketGrant{Allowed: true, State: ticket.State, TicketID: ticket.ID}, nil
}

func (s *KongTicketService) grantExisting(ctx context.Context, accountID int64, model string) (*KongTicketGrant, error) {
	grant, err := s.currentGrant(ctx, accountID, model)
	if err != nil {
		return nil, err
	}
	if grant == nil {
		// 票在决策与取用之间被撤销或过期了。不赌，直接拒这一次。
		return &KongTicketGrant{Allowed: false, DenyReason: KongDenyWindowClosed}, nil
	}
	return grant, nil
}

// kongTaskBudget 是一次取票 + 验证任务的总期限。
//
// 各个 HTTP 调用自己有超时，但那些超时管不住整条任务——数据库调用、代理解析、以及三份挑战串起来
// 的总时长都在它们之外。任务卡住会一直占着账号与出口的 single-flight 槽位，后续请求全部只能等，
// 所以必须有一个整体上限。
//
// 取值按最坏情况来：一次取票（60s）+ 最多三份挑战（各 180s）= 600s，留出数据库与解析的余量。
// 它远小于取票周期（3300s），不会让一个卡住的任务跨越到下一个周期。
const kongTaskBudget = 15 * time.Minute

// kongPersistBudget 是收尾写库的期限。
//
// 它独立于任务期限：任务超时的那一刻，已经拿到的观测正等着落库，沿用被取消的 context 会让它们
// 全部丢掉——而那些数字是这次验证唯一的证据，事后补不回来。
const kongPersistBudget = 15 * time.Second

// startTask 起一个后台任务，并返回它。
//
// 任务不跟随调用者的 context：产物（一张票）对下一个请求仍有价值，当前请求结束不该把它取消。
// 但必须带自己的总期限，否则「不被取消」会变成「永不结束」。
func (s *KongTicketService) startTask(ctx context.Context, task *kongTicketTask, account *Account, model, action string, manual, revive bool) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), kongTaskBudget)
	go func() {
		defer cancel()
		defer s.releaseTask(account.ID, task)
		id, revived, err := s.runTask(detached, account, model, action, task.ticketEgress, manual, revive)
		task.ticketID, task.revivedID, task.err = id, revived, err
	}()
}

// runTask 在认领到任务之后重新核对调度前提，再做网络活动。
//
// 重查是必需的：决策是在认领之前算的，两者之间前一个任务可能刚结束——那时的「可以取票」到此刻
// 已经不成立，照旧决策再取一次会把刚积累的静默清零。
//
// claimedEgress 是认领时锁住的票据出口。重查后配置可能已经改到另一个出口上，而我们持有的锁还是
// 旧那个——此时必须结束本任务，不能在没持有锁的出口上发请求（那个出口可能正被别的账号占着）。
func (s *KongTicketService) runTask(ctx context.Context, account *Account, model, action, claimedEgress string, manual, revive bool) (ticketID int64, revivedID int64, err error) {
	// 任务开始时刻在**这里**固定，一路传到候选淘汰那一步。后面还有重读账号、代理解析、取票几个
	// 可能阻塞的步骤，在它们之后再取时间，会把这期间新到的票误当成「任务开始时就存在的旧候选」。
	taskStartedAt := time.Now()
	fresh, err := s.accounts.GetByID(ctx, account.ID)
	if err != nil {
		return 0, 0, fmt.Errorf("重读账号 %d: %w", account.ID, err)
	}
	if fresh == nil {
		return 0, 0, fmt.Errorf("账号 %d 已不存在", account.ID)
	}
	freshCfg, _ := ParseKongTicketConfig(fresh.Extra)

	// 复位已拒票必须在**这里**做，而不是在触发入口：入口那时还没拿到槽位，若有同模型任务在跑，
	// 它收尾时的批量跳过会按自己的边界把刚复位的票标成 skip——那张票此后既选不中、也不再是
	// rejected，连下一次人工触发都救不回来。持有槽位再复位就没有这个交错。
	var revived *KongTicket
	if revive {
		revived, err = s.repo.ReviveRejectedCandidate(ctx, fresh.ID, model)
		if err != nil {
			// 复位失败不该让整次任务失败：下面照常按正常决策走（多半是去取新票）。
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: fresh.ID, Model: model,
				EventType: KongEventManualRefresh, Outcome: KongOutcomeFailure,
				Detail: map[string]any{"stage": "revive", "error": err.Error()},
			})
			revived = nil
		} else if revived != nil {
			revivedID = revived.ID
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: fresh.ID, Model: model,
				EventType: KongEventManualRefresh, Outcome: KongOutcomeInfo,
				TicketID: &revivedID,
				Detail:   map[string]any{"stage": "revive", "note": "已拒票复位为候选，本次以它为验证目标"},
			})
		}
	}

	in, err := s.buildScheduleInput(ctx, fresh, freshCfg, model, time.Now(), manual)
	if err != nil {
		return 0, revivedID, err
	}
	// 自己已经占住槽位了，重算时不要把它当成「别人在跑」而退成 wait。
	in.InflightTask = false
	decision := KongDecideTicketAction(*in)

	// 只有真正要用票据出口的动作才受出口锁约束；候选验证走流量出口，不碰它。
	needsTicketEgress := decision.Action == KongActionFetch || decision.Action == KongActionInjectAndPrefetch
	if needsTicketEgress {
		if current := KongEgressKey(freshCfg.Egress, freshCfg.ProxyID); current != claimedEgress {
			return 0, revivedID, fmt.Errorf("票据出口已从 %s 改为 %s，本任务持有的是旧锁", claimedEgress, current)
		}
	}

	// 复位成功且账号可调度时，**直接验证那一张**，不再泛选候选。泛选可能挑到更早的另一张票，
	// 而调用方已经据 revivedID 报了"正在复用该票重验"；它也能绕开 observed 的最小年龄限制——
	// 那条限制是压被动票的**自动**验证频率，对人工指名的重验没有意义。
	if revived != nil && in.AccountReady {
		id, verr := s.verifyTicket(ctx, fresh, freshCfg, model, revived.ID, revived.State, revived.Source,
			revived.ExpiresAt, kongCaptureOf(revived), taskStartedAt, false, nil)
		return id, revivedID, verr
	}

	switch decision.Action {
	case KongActionVerifyCandidate:
		id, verr := s.verifyExistingCandidate(ctx, fresh, freshCfg, model, taskStartedAt, manual)
		return id, revivedID, verr
	case KongActionFetch, KongActionInjectAndPrefetch:
		// InjectAndPrefetch 必须**真的取票**：预取正是在这个决策下启动的，重算必然又得到它，
		// 把它并到 Inject 里直接返回当前票，等于让异步预取变成永不取票的空任务。
		id, ferr := s.fetchAndVerify(ctx, fresh, freshCfg, model, taskStartedAt)
		return id, revivedID, ferr
	case KongActionInject:
		// 认领与重查之间已经有票可用了（别的任务刚落库）。直接用它，不必再动出口。
		if in.CurrentTicketID != 0 {
			return in.CurrentTicketID, revivedID, nil
		}
		return 0, revivedID, fmt.Errorf("重查后无可用票")
	default:
		// 前提已不成立（静默不够、冷却中、出口失效、账号不可调度）。什么都不做是正确的：
		// 这一次拒服，下一次请求重新判定。
		return 0, revivedID, fmt.Errorf("重查后不再满足取票条件: %s", decision.DenyReason)
	}
}

// kongCaptureOf 取一张票行上的采集事实。没有就返回 nil——探测记录如实留空，不猜一个。
func kongCaptureOf(t *KongTicket) *kongCaptureFacts {
	if t == nil || (t.CaptureEgress == "" && t.CaptureIdleSeconds == nil) {
		return nil
	}
	return &kongCaptureFacts{Egress: t.CaptureEgress, IdleSeconds: t.CaptureIdleSeconds}
}

// startPrefetch 起一个异步预取：当前票还能用，但已进入提前换票窗口。
func (s *KongTicketService) startPrefetch(ctx context.Context, account *Account, cfg KongTicketConfig, model string) {
	task, outcome := s.claimTask(account.ID, model, KongEgressKey(cfg.Egress, cfg.ProxyID))
	if outcome != kongClaimFresh {
		// 已有在途任务，或出口被别的账号占着。预取是机会性的，不必为此拒服——当前票还能用。
		return
	}
	s.startTask(ctx, task, account, model, KongActionInjectAndPrefetch, false, false)
}

// startVerifyCandidate 起一个异步的候选验证：当前票还能用，但已进入刷新窗口而缓存里有候选。
//
// 与 startPrefetch 的差别只在槽位：这里占的是验证专用槽位（验证走流量出口），所以它既不消耗票据
// 出口的静默，也不会把出口锁从别的账号手里抢走。
func (s *KongTicketService) startVerifyCandidate(ctx context.Context, account *Account, model string) {
	task, outcome := s.claimTask(account.ID, model, kongVerifyOnlySlot(account.ID))
	if outcome != kongClaimFresh {
		// 已有在途任务。候选验证同样是机会性的——当前票还能用，不必为此拒服。
		return
	}
	s.startTask(ctx, task, account, model, KongActionVerifyCandidate, false, false)
}

// handoffWorthIt 报告"让给别的账号"是否值得：别的账号此刻有一张**按当前白名单仍然合格**的票。
//
// **必须按当前白名单重判**，不能只看 `status = verified`：那个状态只表示"按当时的白名单判过"，而
// 白名单与阈值来自环境变量、随时可改。据一张现在已经不合格的票去交接，代价不是"多换一次号"——上层
// 会把本账号加进失败列表，于是它即将补上的那张票再也用不上，请求最终被拒。
//
// 不确定时一律返回 false（退回等待）：等一定能服务，而错误的交接可能把一个可恢复的请求拒掉。查询
// 出错、没有别家、别家的票都不合格，都走这一侧。
//
// ⚠️ 它仍然看不到别的账号的 mode 与调度资格（那在调度层）。所以返回 true 只是"值得一试"，上层
// failover 找不到能接手的账号时仍会耗尽——这是已知的残余风险，见 DESIGN §4.6。
func (s *KongTicketService) handoffWorthIt(ctx context.Context, accountID int64, model string) bool {
	tickets, err := s.repo.VerifiedTicketsElsewhere(ctx, accountID, model)
	if err != nil {
		return false
	}
	for _, t := range tickets {
		if t != nil && s.accept.Accepts(model, t.FingerprintProbs, s.confidence) {
			return true
		}
	}
	return false
}

// prepareOrHandOff 起票据任务，然后决定这次请求是**等它**还是**让给别的账号**。
//
// 判据只有一个：换号能不能更快拿到票。
//
//   - 别的账号此刻有可用票 → 不等，报 preparing 让上层 failover 换号。等一轮取票加验证要几十秒，
//     而换号是立刻的。
//   - 别家也没票 → 换号救不了，退回同步等（与改动前的行为一致）。
//
// **粘性会话的差别不在这里**，在上层：`preparing` 被包成可 failover 的错误时，绑定了会话的请求会
// 先在同账号重试（等于等这个任务），没绑定的直接换号。那一层才知道有没有会话（见
// kong_ticket_gateway.go 的 KongTicketFailover）。
func (s *KongTicketService) prepareOrHandOff(ctx context.Context, account *Account, cfg KongTicketConfig, model, action string) (*KongTicketGrant, error) {
	if !s.handoffWorthIt(ctx, account.ID, model) {
		return s.runTaskAndWait(ctx, account, cfg, model, action, false, false)
	}

	// 起异步任务：这次请求虽然让给别的账号，票还是要取的——否则本账号永远补不上票，而下一个请求
	// 会再走一遍同样的判断。
	ticketEgress := KongEgressKey(cfg.Egress, cfg.ProxyID)
	if action == KongActionVerifyCandidate {
		ticketEgress = kongVerifyOnlySlot(account.ID)
	}
	task, outcome := s.claimTask(account.ID, model, ticketEgress)
	switch outcome {
	case kongClaimFresh:
		s.startTask(ctx, task, account, model, action, false, false)
	case kongClaimEgressBusy:
		// 出口被别的账号占着，本账号这一轮压根起不了任务。
		//
		// **必须报 egress_busy 而不是 preparing**：`preparing` 的含义是"本账号的任务正在跑，产物就是
		// 本账号要的票"，那是唯一值得先在同账号等一等的原因。这里等是白等——那个任务的票属于别的账号，
		// 而且它那次取票会把这条共享出口的静默清零，一释放本号立刻变成 window_closed。报成 preparing
		// 会让上层把重试预算花在一个确定不会变的状态上，还延后真正能服务的那次换号。
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: account.ID, Model: model,
			EventType: KongEventEgressInvalid, Outcome: KongOutcomeSkipped,
			TicketEgress: ticketEgress, TrafficEgress: KongTrafficEgressKey(account.ProxyID),
			Detail: map[string]any{"reason": "egress_busy_other_account", "handoff": true},
		})
		return &KongTicketGrant{Allowed: false, DenyReason: KongDenyEgressBusy}, nil
	}
	// kongClaimSameAccount 什么都不用做：已有在途任务，它的产物正是本账号要的票。
	return &KongTicketGrant{Allowed: false, DenyReason: KongDenyPreparing}, nil
}

// runTaskAndWait 在当前请求上同步跑一次取票/验证。冷启动走这条路。
func (s *KongTicketService) runTaskAndWait(ctx context.Context, account *Account, cfg KongTicketConfig, model string, action string, manual, revive bool) (*KongTicketGrant, error) {
	// 候选验证走**流量出口**，不占票据出口的静默，所以不该抢那个槽位——否则两个都配 `none`
	// 的账号会争用同一个 "none" 锁，一个在验证、另一个被拒为 egress_busy。
	ticketEgress := KongEgressKey(cfg.Egress, cfg.ProxyID)
	if action == KongActionVerifyCandidate {
		ticketEgress = kongVerifyOnlySlot(account.ID)
	}
	task, outcome := s.claimTask(account.ID, model, ticketEgress)
	switch outcome {
	case kongClaimSameAccount:
		return s.awaitTask(ctx, task, account.ID, model)
	case kongClaimEgressBusy:
		// 出口被另一个账号占着。等它没有意义——它产出的票属于那个账号。
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: account.ID, Model: model,
			EventType: KongEventEgressInvalid, Outcome: KongOutcomeSkipped,
			TicketEgress: ticketEgress, TrafficEgress: KongTrafficEgressKey(account.ProxyID),
			Detail: map[string]any{"reason": "egress_busy_other_account"},
		})
		return &KongTicketGrant{Allowed: false, DenyReason: KongDenyEgressBusy}, nil
	}
	s.startTask(ctx, task, account, model, action, manual, revive)
	return s.awaitTask(ctx, task, account.ID, model)
}

// kongVerifyOnlySlot 是「只验证候选、不碰票据出口」的任务槽位键。
//
// 它按账号取值，与真实票据出口分开：验证走流量出口，与票据出口的静默无关，不该互相排斥。
func kongVerifyOnlySlot(accountID int64) string {
	return fmt.Sprintf("verify:%d", accountID)
}

func (s *KongTicketService) waitForTask(ctx context.Context, accountID int64, model string) (*KongTicketGrant, error) {
	s.mu.Lock()
	task := s.inflight[accountID]
	s.mu.Unlock()
	if task == nil {
		// 调度看到「有任务在途」与我们来等它之间，那个任务可能已经完成并提交了一张合格票。
		// 直接拒服等于把刚拿到的票浪费掉。
		return s.grantExisting(ctx, accountID, model)
	}
	return s.awaitTask(ctx, task, accountID, model)
}

// kongWaitBudget 是一个请求愿意为在途任务等待的上限。
//
// 它必须显著小于 kongTaskBudget：任务最坏要跑十几分钟（取票 + 三份挑战），让一个业务请求挂那么久
// 毫无意义。超时只结束这个等待者，任务继续跑完——它的产物对下一个请求仍然有价值。
const kongWaitBudget = 5 * time.Minute

// awaitTask 等一个在途任务的结果。
//
// 两个出口：调用者的 context 取消，或等待期限到。两者都只结束这个等待者，任务本身继续跑完。
func (s *KongTicketService) awaitTask(ctx context.Context, task *kongTicketTask, accountID int64, model string) (*KongTicketGrant, error) {
	timer := time.NewTimer(kongWaitBudget)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// 等待被调用方的 context 中断：这不是"票不合格"，也不是系统故障，而是**这个账号这次没能及时
		// 拿到票**——换一个此刻有票的账号完全可能立刻服务。报成错误会让上层包成 `ensure_failed`
		// （系统级、带 NextAccountStop），于是别的账号有票也用不上，那是无谓拒服。
		//
		// **不按 ctx.Err() 区分"是谁取消的"**：HTTP 侧的首输出守卫用 WithCancel + AfterFunc 取消，
		// 拿到的是 Canceled，与客户端断开没有区别。而客户端真的走了那一路另有判据——上层在 failover
		// 之前先查 `failoverClientGone`，那时这次拒服不会被用来重试。
		return &KongTicketGrant{Allowed: false, DenyReason: KongDenyWaitTimeout}, nil
	case <-timer.C:
		return &KongTicketGrant{Allowed: false, DenyReason: KongDenyWaitTimeout}, nil
	case <-task.done:
	}
	// 任务完成后一律查**等待者自己这个模型**的合格票，不看任务的返回值。
	//
	// 任务是按**账号**串行的（票据出口的静默按 IP 积累，只按模型串行拦不住并发取票），所以我们等到的
	// 任务完全可能是另一个模型的——拿它的票 id 去查自己的模型，结果必然是「没有」。任务失败时同理：
	// 那次失败不代表我们这个模型没有可用的票。
	grant, err := s.currentGrant(ctx, accountID, model)
	if err != nil {
		return nil, err
	}
	if grant != nil {
		grant.RevivedTicketID = task.revivedID
		return grant, nil
	}
	// 自己确实没有合格票。这一次拒服，不在同一个请求里重新排一轮任务——账号与出口的互斥仍然握在
	// 别人手里，而下一个请求会按请求驱动的常规路径重新决策（任务槽此时已经释放）。
	// 本模型的任务跑完却没票，原因是"取过了没成"而不是"还没到能取的时候"——两者必须分开，否则
	// 人工触发（已跳过窗口）也会被解释成静默未满，把排查引向错误方向。
	reason := KongDenyTaskNoTicket
	if task.model != "" && task.model != model {
		reason = KongDenyOtherModelTask
	}
	return &KongTicketGrant{Allowed: false, DenyReason: reason, RevivedTicketID: task.revivedID}, nil
}

// kongClaimOutcome 说明一次认领的结果。
type kongClaimOutcome int

const (
	// kongClaimFresh 表示本次认领成功，调用方负责执行并释放。
	kongClaimFresh kongClaimOutcome = iota
	// kongClaimSameAccount 表示该账号已有在途任务，等它的结果即可——它的产物正是我们要的票。
	kongClaimSameAccount
	// kongClaimEgressBusy 表示票据出口被**另一个账号**占着。等它没有意义（它产出的票属于那个
	// 账号），所以这一次直接拒服。
	kongClaimEgressBusy
)

// claimTask 是 single-flight 的入口：同一账号、以及同一票据出口，同一时刻只允许一次在途任务。
//
// 并发取票不只是烧额度——每次取票都是票据出口上的一次活动，并发请求会互相把静默清零，最后谁也
// 拿不到合格票。出口维度因此和账号维度一样是必需的。
func (s *KongTicketService) claimTask(accountID int64, model, ticketEgress string) (*kongTicketTask, kongClaimOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.inflight[accountID]; ok {
		return existing, kongClaimSameAccount
	}
	if _, ok := s.inflightEgress[ticketEgress]; ok {
		return nil, kongClaimEgressBusy
	}
	task := &kongTicketTask{done: make(chan struct{}), model: model, ticketEgress: ticketEgress}
	s.inflight[accountID] = task
	s.inflightEgress[ticketEgress] = task
	return task, kongClaimFresh
}

func (s *KongTicketService) releaseTask(accountID int64, task *kongTicketTask) {
	s.mu.Lock()
	if s.inflight[accountID] == task {
		delete(s.inflight, accountID)
	}
	if s.inflightEgress[task.ticketEgress] == task {
		delete(s.inflightEgress, task.ticketEgress)
	}
	s.mu.Unlock()
	close(task.done)
}

// ErrKongTicketNotAccepted 表示**上游明确重发了票**——它没接受我们注入的那张，所以那张已经不
// 作数。这是"验出问题"里最硬的一种证据，与"没能完成测量"（超时、429、写库失败）必须分开：
// 前者该作废票，后者只能保留旧结论（否则一次网络抖动就白扔一张好票，而重新取票要等约 34 分钟
// 静默——那是无谓拒服）。
var ErrKongTicketNotAccepted = errors.New("候选票未被上游接受")

// ErrKongTicketDowngraded 表示**本次验证自己测出并成功提交了"归因不合格"**这个结论。
//
// 它与"读库发现票已是 rejected"必须分开：后者可能是**别的路径**刚把票撤销掉（业务请求拿到上游
// 重发的票就会撤销当前票），而那时本次验证可能什么都没测出来。凭读库状态推断"真降档"，会在
// 那种并发下继续验第二张——白烧一整张票的额度（最多三份真实挑战）。
//
// 只在 CommitVerification 返回 committed=true 且归因不合格时包上它：没提交成功的结论不算结论。
var ErrKongTicketDowngraded = errors.New("归因不合格，已判为降档")

// kongFetchOne 是一次取票的结果。只有触发模型（leader）的这一份会回传给调用方——其余成员的
// 成败全部在 fetchStore 内部记进事件，调用方不据此改变行为（见 fetchAndVerify 的注释）。
type kongFetchOne struct {
	// answer 只在融合取票且正文读成功时非空。
	answer *KongUpstreamAnswer
	// fusedAttempted / fusedReadErr 让调用方区分"没融合"与"融合了但正文读失败"：后者额度已经花了，
	// 按约定要留一条 inconclusive，而票本身是好的、照常入库。
	fusedAttempted     bool
	fusedReadErr       string
	fusedReportedModel string
	model              string
	ticketID           int64
	state              string
	expires            time.Time
	err                error
}

// otherGatedModels 返回除 exclude 之外的门控模型。
func (s *KongTicketService) otherGatedModels(exclude string) []string {
	out := make([]string, 0, len(s.gatedModels))
	for _, m := range s.gatedModels {
		if m != exclude {
			out = append(out, m)
		}
	}
	return out
}

// fetchAndVerify 经票据出口取票，然后立即验证**触发模型**那一张。
//
// fetch 票不受 MinTicketAge 限制：它是我们自己刚要来的唯一一张，等待毫无意义，否则冷启动要
// 凭空多阻塞一个 MinTicketAge。
//
// 开了 batchFetch 时，**所有门控模型的票在同一瞬间并发取回**。三点要一起看才成立：
//
//  1. 依据是一条实测事实——292 窗口一旦打开约 4 分钟内有效，且窗口内的活动不会把它提前关闭。
//     所以一次静默换来的是一个"可连续取票的窗口"，不是一张票。
//  2. **并发而不是串行**：全部请求同一瞬间发出，于是不存在"窗口在批次中途关闭"这回事，也就不需要
//     批次预算与顺序安排。
//  3. **批内只取不验**：其余模型的票入库为候选，验证留给该模型的第一个请求。验证烧真实额度
//     （每张三份挑战）却走流量出口、与出口静默无关，没有任何理由现在做；在这里逐个验完还会让
//     触发请求一直等到整批验完。
//
// 非触发模型的失败**只记事件、不进冷却、不影响返回值**：冷却是给失败的取票路径退避用的，而触发
// 模型本周期已经证明这条路是通的，罚出口只会把下一个正常周期也往后推。
func (s *KongTicketService) fetchAndVerify(ctx context.Context, account *Account, cfg KongTicketConfig, model string, taskStartedAt time.Time) (int64, error) {
	ticketEgress := KongEgressKey(cfg.Egress, cfg.ProxyID)
	trafficEgress := KongTrafficEgressKey(account.ProxyID)

	var egressProxyURL string
	if cfg.Egress == KongTicketEgressProxy {
		url, err := s.upstream.ResolveProxyURL(ctx, cfg.ProxyID)
		if err != nil {
			// 事件类型是 fetch_skipped 而不是 fetch：一个字节都没发出去，不能被 A 的查询
			// （`event_type = fetch` 的 max(created_at)）当成一次出口活动——那会让共用同一个
			// 出口的其它账号凭空多等一个静默期。
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventFetchSkipped,
				Outcome: KongOutcomeFailure, TicketEgress: ticketEgress, TrafficEgress: trafficEgress,
				Detail: map[string]any{"error": err.Error(), "phase": "resolve_proxy"},
			})
			return 0, err
		}
		egressProxyURL = url
	}

	// **静默值全批共用，且必须在发起任何取票之前算。** 取票自己就是该出口上的活动：第一发一落地
	// noteEgressUse 就把"上次活动"推到了现在，之后再算就得到 idle≈0。那个数字字面为真，却把事实
	// 记错了——全批骑的是同一个窗口，产生这个窗口的静默就是批前这一段。记 0 会让日后读事件的人
	// 以为"静默 0 秒也能拿到 292"。
	idle := s.idleSeconds(ctx, ticketEgress, time.Now())

	models := []string{model}
	if s.batchFetch {
		models = append(models, s.otherGatedModels(model)...)
	}
	// 融合取票：整批共用**第一份**挑战。用同一份是刻意的——批内各成员的样本因此可比，"同一个窗口
	// 里取的票档位是否一致"这个问题才答得上来。
	var fusedChallenge *KongFingerprintChallenge
	if s.fusedFingerprint {
		if all := KongFingerprintChallenges(); len(all) > 0 {
			fusedChallenge = &all[0]
		}
	}
	// leader 的结果单独走一个 channel。**只等 leader 就开始验证**：等齐全部成员会把非触发模型的
	// 慢失败算进触发请求的等待时间——leader 取票 1s + 三份挑战 270s 本来 271s 就够，而一个 60s
	// 才失败的非触发模型会把总耗时推到 330s，超过 kongWaitBudget(300s)，于是本来能服务的请求被
	// 判成 wait_timeout。
	leadCh := make(chan kongFetchOne, 1)
	var wg sync.WaitGroup
	for i := range models {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := s.fetchStore(ctx, account, kongFetchArgs{
				ticketEgress: ticketEgress, trafficEgress: trafficEgress,
				egressProxyURL: egressProxyURL, model: models[i],
				idle: idle, batchIndex: i, batchTotal: len(models), leaderModel: model,
				fused: fusedChallenge,
			})
			if i == 0 {
				leadCh <- out
				return
			}
			// 非触发模型：融合样本够就地落结论，那张票立刻可用，省掉它自己第一个请求的验证等待。
			// 不够则**不重跑**（OnlyFused）——此刻没有请求在等这张票，不值得再花一份长答案。
			//
			// 取票失败时 out.ticketID 为 0，没有可结算的对象。
			// 条件用 fusedAttempted 而不是 answer != nil：正文读失败时样本不可用，但那次调用已经
			// 花了额度，按约定要留一条 inconclusive——跳过就什么记录都没有。
			if out.err == nil && out.fusedAttempted && fusedChallenge != nil && out.ticketID != 0 {
				capture := &kongCaptureFacts{Egress: ticketEgress, IdleSeconds: idle}
				_, _ = s.verifyTicket(ctx, account, cfg, models[i], out.ticketID, out.state,
					KongTicketSourceFetch, out.expires, capture, taskStartedAt, false,
					&kongFusedSample{
						Challenge: *fusedChallenge, Answer: out.answer, ReadErr: out.fusedReadErr,
						ReportedModel: out.fusedReportedModel,
						Egress:        ticketEgress, OnlyFused: true,
					})
			}
		}(i)
	}
	lead := <-leadCh
	// **槽位释放之前仍要等齐所有取票**：任务返回即释放出口锁，而此刻出口上可能还有在途请求，
	// 别的账号这时开始取票会把静默算错。放 defer 里，于是它等在验证之后而不是验证之前。
	defer wg.Wait()

	if lead.err != nil {
		return 0, lead.err
	}
	capture := &kongCaptureFacts{Egress: ticketEgress, IdleSeconds: idle}
	// 触发模型的融合样本进 verifyTicket 当第一份；不达标它会丢弃并按常规路径重跑（有请求在等，
	// 值得那几份长答案）。
	var fused *kongFusedSample
	if lead.fusedAttempted && fusedChallenge != nil {
		fused = &kongFusedSample{
			Challenge: *fusedChallenge, Answer: lead.answer, ReadErr: lead.fusedReadErr,
			ReportedModel: lead.fusedReportedModel,
			Egress:        ticketEgress,
		}
	}
	return s.verifyTicket(ctx, account, cfg, model, lead.ticketID, lead.state, KongTicketSourceFetch,
		lead.expires, capture, taskStartedAt, false, fused)
}

// kongFetchArgs 是一次取票的入参。批内成员共用同一个出口、同一个静默值。
type kongFetchArgs struct {
	ticketEgress   string
	trafficEgress  string
	egressProxyURL string
	model          string
	idle           *int64
	batchIndex     int
	batchTotal     int
	leaderModel    string
	// fused 非 nil 时取票请求用它的 prompt，并把正文当第一份指纹样本带回。
	fused *KongFingerprintChallenge
}

// fetchStore 取一张票并入库。不验证——验证由调用方对触发模型那一张单独发起。
//
// 只有触发模型（batchIndex == 0）的失败会推进冷却，理由见 fetchAndVerify 的注释。
func (s *KongTicketService) fetchStore(ctx context.Context, account *Account, in kongFetchArgs) kongFetchOne {
	out := kongFetchOne{model: in.model}
	leader := in.batchIndex == 0
	batched := in.batchTotal > 1
	// **取票耗时必须进事件，否则并发看起来像串行。** 事件的 CreatedAt 是请求**完成**时刻（理由见
	// 下面的 sentAt），批内各条 fetch 于是在事件流里显示成一前一后——而它们是同一瞬间发出的，
	// 时间差只是各自响应耗时之差。融合取票要等整份挑战答案生成完（实测 27–44s），这个差可以有
	// 几十秒，足以让人得出"批量取票是串行的"这个错误结论。记下耗时，读事件的人各自减回发起时刻
	// 就能自证。
	//
	// 起点取自 `probe.SentAt`（请求交给传输层的那一刻），**不是**进入本函数的时刻：凭据准备在那
	// 之前，而批内共用一个账号——第一个成员触发 OAuth 刷新、其余等锁时实际发送本就错开，把那段
	// 算进耗时会让反推起点凭空对齐，恰好掩盖要看的信号。一个字节都没发出去时它是零值，那时没有
	// 上游耗时可言（与 resolve_proxy 那条 fetch_skipped 一致，两处都不记这个字段）。
	var sentAt, sendStart time.Time
	detail := func(extra map[string]any) map[string]any {
		d := map[string]any{}
		if !sendStart.IsZero() && !sentAt.IsZero() {
			d["duration_ms"] = sentAt.Sub(sendStart).Milliseconds()
		}
		if batched {
			d["batch"] = map[string]any{
				"leader_model": in.leaderModel, "index": in.batchIndex, "total": in.batchTotal,
			}
		}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	cooldown := func(reason string) {
		if leader {
			s.enterCooldown(ctx, account.ID, in.model, in.ticketEgress, reason)
		}
	}

	probe, err := s.upstream.FetchTurnState(ctx, account, in.egressProxyURL, in.model, in.fused)
	// 活动结束时刻在这里定死，后面存票、清标记、写事件的耗时都不再影响它。事件的 CreatedAt 与
	// 内存事实用同一个值，两者才对得上——不固定的话持久化的 A 会比真实活动晚上百毫秒，而调度
	// 判的是「距上次活动多久」。
	sentAt = time.Now()
	if probe != nil {
		sendStart = probe.SentAt
	}
	if KongIsUpstreamNotAttempted(err) {
		// 请求还没送出去就失败了（缺凭据这类本地错误）：没有清零任何静默，不能推进 A。
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: account.ID, Model: in.model, EventType: KongEventFetchSkipped,
			Outcome: KongOutcomeFailure, TicketEgress: in.ticketEgress, TrafficEgress: in.trafficEgress,
			CreatedAt: sentAt,
			Detail:    detail(map[string]any{"error": err.Error(), "phase": "build_request"}),
		})
		out.err = err
		return out
	}
	// 请求已经发出去了，成败都一样清零了这个出口的静默——必须在判成败之前记。
	s.noteEgressUse(in.ticketEgress, sentAt)
	event := &KongTicketEvent{
		AccountID: account.ID, Model: in.model, EventType: KongEventFetch,
		TicketEgress: in.ticketEgress, TrafficEgress: in.trafficEgress, IdleSeconds: in.idle,
		CreatedAt: sentAt,
	}
	if probe != nil {
		// StatusCode 为 0 表示没等到响应头（传输层就失败了）。记 0 会让事件看起来像"上游回了 0"，
		// 而那一列的语义是"上游返回的状态码"。
		if probe.StatusCode > 0 {
			event.StatusCode = &probe.StatusCode
		}
		if probe.State != "" {
			n := len(probe.State)
			event.StateLen = &n
		}
	}
	if err != nil {
		event.Outcome = KongOutcomeFailure
		event.Detail = detail(map[string]any{"error": err.Error()})
		s.logEvent(ctx, event)
		cooldown("fetch_failed")
		out.err = err
		return out
	}
	if probe.State == "" {
		// 上游认为本次请求已带有效票，与我们「无票」的认知冲突——值得记，它意味着别处
		// 还有一条在用同一账号的链路。
		event.Outcome = KongOutcomeFailure
		event.Detail = detail(map[string]any{"reason": "no_state_returned"})
		s.logEvent(ctx, event)
		cooldown("no_state_returned")
		out.err = fmt.Errorf("上游未下发票")
		return out
	}
	// 成批时额外记票原值的指纹（长度 + 前 8 字符，与探测记录同一种表示，不是凭据本身）。
	// 用途是回答一个开放问题：批内各模型拿到的是不是同一张票。若恒为同一张，就等于实测出"票与
	// 模型无关"，跨模型复用可以直接落地，这套批量取票还能再简化一层。
	extra := map[string]any{}
	if batched {
		extra["state_fingerprint"] = kongTicketFingerprint(probe.State)
	}
	// 标明这次取票是否融合了指纹挑战。**按"是否发起过"记，不按"是否读到答案"记**：读失败时额度
	// 一样花了，记成 false 会让"额度花在哪了"这个问题答错。读失败另记原因。
	if probe.FusedAttempted {
		extra["fused"] = true
		if probe.FusedReadErr != "" {
			extra["fused_read_error"] = probe.FusedReadErr
		}
	}
	// 事件在存票之后才写：这样它能带上票 id，验证记录与「这张票是怎么采到的」（出口、静默值）
	// 才对得上——票缓存会随过期被删，只靠时间相邻去猜是猜不准的。
	// 一次真实的网络取票**只记一条** fetch 事件，不论入库这一步结果如何：记两条的话成功率统计
	// 会把同一次取票算两遍，而「取了几次」正是额度口径。入库的后处理结果一律进 detail。
	capture := &kongCaptureFacts{Egress: in.ticketEgress, IdleSeconds: in.idle}
	ticketID, expiresAt, inserted, err := s.storeTicket(ctx, account.ID, in.model, probe.State, KongTicketSourceFetch, capture)
	if err != nil {
		extra["error"] = err.Error()
		extra["phase"] = "store_ticket"
		// **按既定规则拒收不是故障**：取到 312（长度黑名单）说明上游给的是降智档票，我们照规则不收
		// ——那是预期结果。记成 store_ticket_failed 会让排查往"数据库/存储坏了"的方向走，而真实原因
		// 是上游给了什么。判据用既有的 KongIsExpectedTicketRejection（observe 路径已经在用它）。
		//
		// fetch 事件本身仍记 failure：这次取票确实没拿到可用票，而成功率统计要算这一次。
		reason := "store_ticket_failed"
		if KongIsExpectedTicketRejection(err) {
			extra["phase"] = "ticket_rejected"
			reason = "ticket_rejected"
		}
		event.Outcome = KongOutcomeFailure
		event.Detail = detail(extra)
		s.logEvent(ctx, event)
		// 退避照样推进，两种情况都要：网络活动已经发生，否则下一个请求立刻再取一次——预期拒收更需要
		// 退避，因为上游正在发降智档票，立刻重试只会再拿一张 312。
		cooldown(reason)
		out.err = err
		return out
	}
	event.TicketID = &ticketID
	if !inserted {
		// 主动取票拿回了一张我们已经有的票：那张票的结论（含 rejected）仍然有效，不该重新验证。
		extra["reason"] = "duplicate_state"
		event.Outcome = KongOutcomeFailure
		event.Detail = detail(extra)
		s.logEvent(ctx, event)
		// 同样要推进退避。min_idle=0 是合法配置，不进冷却时两个相邻请求会各取一次、各拿回同一张
		// 重复票，净效果是零收益的双倍网络活动。
		cooldown("duplicate_state")
		out.err = fmt.Errorf("取到的票与库中已有的重复")
		return out
	}
	// 一次成功的主动取票是「新信息」：解除本段无票期里的候选跳过标记。按各自模型解——跳过标记是
	// (账号, 模型) 维度的。
	//
	// 放在写事件之前，失败原因随那一条事件走：解不掉标记只会让本可重试的候选继续被跳过，
	// 不影响这张新票，所以不中止——但必须留痕，否则「为什么那几张候选再也没被选过」查不出来。
	if clearErr := s.repo.ClearSkipMarks(ctx, account.ID, in.model); clearErr != nil {
		extra["clear_skip_marks_error"] = clearErr.Error()
	}
	event.Outcome = KongOutcomeSuccess
	event.Detail = detail(extra)
	s.logEvent(ctx, event)

	out.ticketID = ticketID
	out.state = probe.State
	out.expires = expiresAt
	// 融合样本随结果带回：调用方拿它当第一份指纹证据。只在这条成功路径上带——失败时那张票压根
	// 没入库，样本没有可挂的对象。读正文失败也要带，那时样本不可用但要留记录。
	out.answer = probe.Answer
	out.fusedAttempted = probe.FusedAttempted
	out.fusedReadErr = probe.FusedReadErr
	out.fusedReportedModel = probe.FusedReportedModel
	return out
}

// verifyExistingCandidate 验证缓存里已有的候选。
//
// manual 只改**挑哪一张**：人工触发挑最新的（剩余 TTL 最长，且批量取票下它通常就是刚在 292
// 窗口里取回的那张），自动触发挑最老的（在清一个队列，先验快过期的才有机会用上它）。挑法之外
// 两条路径完全相同——验证本身没有"人工版"。
func (s *KongTicketService) verifyExistingCandidate(ctx context.Context, account *Account, cfg KongTicketConfig, model string, taskStartedAt time.Time, manual bool) (int64, error) {
	var candidate *KongTicket
	var err error
	if manual {
		candidate, err = s.repo.NewestCandidate(ctx, account.ID, model, time.Now())
	} else {
		candidate, err = s.repo.OldestCandidate(ctx, account.ID, model, s.params.MinTicketAge, time.Now())
	}
	if err != nil {
		return 0, fmt.Errorf("查候选票: %w", err)
	}
	if candidate == nil {
		return 0, fmt.Errorf("没有可验证的候选")
	}
	// 采集事实取自票行本身，不按当前配置重建：这张 fetch 候选可能是上一个进程取的，那之后出口
	// 配置完全可以变过——用现在的出口去填，等于给探测记录写一个错的事实。
	//
	// observed 候选本来就没有采集事实（业务响应顺带带回来的），fetch 候选来自旧版本时也可能没有
	// ——两种情况都传 nil，探测记录如实留空，而不是猜一个。
	var capture *kongCaptureFacts
	if candidate.CaptureEgress != "" || candidate.CaptureIdleSeconds != nil {
		capture = &kongCaptureFacts{Egress: candidate.CaptureEgress, IdleSeconds: candidate.CaptureIdleSeconds}
	}
	return s.verifyTicket(ctx, account, cfg, model, candidate.ID, candidate.State, candidate.Source, candidate.ExpiresAt, capture, taskStartedAt, false, nil)
}

// kongVerifySnapshot 是任务发起时的前提快照。
//
// 验证要跑好几分钟，其间账号可能被禁用、模式可能被切走、出口可能被改、票可能过期。拿旧快照
// 一路跑到底再把结论落库，等于把一个「当时成立、现在不成立」的判定写成事实。
type kongVerifySnapshot struct {
	AccountID int64
	Model     string
	// Mode 是发起时的模式。复核比对的是「有没有变」而不是「是不是 full」：observe 的诊断探测
	// 同样要复核，它本来就不在 full 下跑。
	Mode         KongTicketMode
	TicketEgress string
	// TrafficEgress 是挑战实际使用的出口。它必须在快照里：验证期间 accounts.proxy_id 被改掉后，
	// 后续挑战仍会走解析好的旧代理，那些回答反映的是另一个出口的行为，不能用来判定这张票。
	TrafficEgress string
	// TrafficProxyDigest 是流量出口**实际连接配置**的指纹。
	//
	// 只比 `proxy:<id>` 不够：管理端改同一条代理记录的 host / port / 协议 / 认证时 id 不变，而挑战
	// 全程复用验证开始时解析出的那个 URL——于是结论是在旧连接上得出的，却被提交为 verified，之后
	// 业务走的是新连接。指纹存哈希而不是 URL 本身：URL 里可能带认证信息，不该进日志或事件。
	TrafficProxyDigest string
	// IdentityDigest 是挑战**实际使用的上游账号身份**的指纹。
	//
	// 理由与 TrafficProxyDigest 同一条：挑战全程复用验证开始时读到的那个 `account`，而管理端可以
	// 在验证期间把它换绑到另一个上游账号（改 setup token、改 chatgpt-account-id）。那时后续挑战
	// 仍以旧身份出站，结论却会被提交成当前账号的合格票——把一张别人的票的证据授予了这个账号。
	//
	// **刻意不含 OAuth access token**：它会正常刷新，比它会让每次刷新都误杀一次验证。换绑时变的
	// 是账号类型与 chatgpt-account-id，比这两项足够。存哈希而不是原值：setup token 是凭据。
	IdentityDigest string
	TicketID       int64
	ExpiresAt      time.Time
}

// kongAccountIdentityDigest 把「这次挑战以谁的身份出站」压成短指纹。
//
// 判据是**这个值参与了出站请求的构造吗**，不是"它看起来像不像配置"。逐项对应：
//
//   - 账号主体走 `codexAccountIdentityNamespace`——项目已有的投影，按
//     `(chatgpt_account_id, chatgpt_user_id)` → 指纹 seed → setup token 指纹三级退化。同一个
//     Team 换用户、账号 id 缺失这两种情况它都能区分，而**正常的 token 轮换不影响它**（比
//     access token 会让每次刷新都误杀一次验证）。
//   - FedRAMP 决定 `x-openai-fedramp`（见 setOpenAIChatGPTAccountHeaders）。
//   - 账号级 User-Agent 参与重建最终的 `User-Agent / originator / version`（见
//     enforceCodexIdentityHeadersWithUA）。
//   - setup token 本身就是 bearer，换它就是换身份；主体投影只在没有账号 id 与 seed 时才退到它，
//     所以这里要单列。
//
// 各项**带长度前缀**再拼：直接用分隔符拼接时，值里含分隔符能让不同组合撞同一个摘要。
//
// ⚠️ 已知边界：一个既没有 chatgpt_account_id、也没有指纹 seed 的 OAuth 账号，主体投影为空，此时
// 换绑检测不到。不拿 refresh token 去补——它在刷新时会轮换，补上等于把正常刷新也判成换绑。
//
// 账号级头覆写不在其中：`headerOverrideBlockedNames` 已经挡住了 authorization / session_id /
// chatgpt-account-id 这些身份类头，能覆写的不构成"以谁的身份出站"。
func kongAccountIdentityDigest(account *Account) string {
	if account == nil {
		return ""
	}
	parts := []string{
		string(account.Type),
		codexAccountIdentityNamespace(account),
		strconv.FormatBool(account.IsChatGPTAccountFedRAMP()),
		account.GetOpenAIUserAgent(),
	}
	if account.Type == AccountTypeSetupToken {
		parts = append(parts, account.GetOpenAIAccessToken())
	}
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		encoded = append(encoded, strconv.Itoa(len(part))+":"+part)
	}
	return kongProxyDigest(strings.Join(encoded, ""))
}

// kongProxyDigest 把一个代理连接串压成短指纹。
//
// 空串（直连）返回空串，这样「直连 → 直连」不会因为指纹比较而误判成变化。
func kongProxyDigest(proxyURL string) string {
	if proxyURL == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(proxyURL))
	return hex.EncodeToString(sum[:8])
}

// recheckVerify 在每次主动请求前、以及落结论前复核快照仍然成立。
func (s *KongTicketService) recheckVerify(ctx context.Context, snap kongVerifySnapshot, now time.Time) error {
	if !snap.ExpiresAt.IsZero() && !snap.ExpiresAt.After(now) {
		return fmt.Errorf("票已过期")
	}
	account, err := s.accounts.GetByID(ctx, snap.AccountID)
	if err != nil {
		return fmt.Errorf("复核账号: %w", err)
	}
	if account == nil {
		return fmt.Errorf("账号已不存在")
	}
	if !account.IsSchedulable() {
		return fmt.Errorf("账号已不可调度")
	}
	cfg, _ := ParseKongTicketConfig(account.Extra)
	// 判的是**模式变没变**，不是"当前是哪个模式"。off 同样可以验票：验证走流量出口、只是发几份
	// 挑战问上游"这张票对应哪个模型"，不注入、不占票据出口静默。一个 off 账号手上的 observed 票
	// 照样值得问——那正是"该不该给它开 full"的判据。
	if cfg.Mode != snap.Mode {
		return fmt.Errorf("模式已从 %s 改为 %s", snap.Mode, cfg.Mode)
	}
	if KongEgressKey(cfg.Egress, cfg.ProxyID) != snap.TicketEgress {
		return fmt.Errorf("票据出口已改变")
	}
	if KongTrafficEgressKey(account.ProxyID) != snap.TrafficEgress {
		return fmt.Errorf("流量出口已从 %s 改为 %s", snap.TrafficEgress, KongTrafficEgressKey(account.ProxyID))
	}
	if kongAccountIdentityDigest(account) != snap.IdentityDigest {
		return fmt.Errorf("账号的上游身份已改变")
	}
	// 同一个代理 id 下的连接配置也可能被改掉。重新解析并比指纹——不比较 URL 本身，避免把带认证
	// 信息的串写进错误消息。
	if snap.TrafficProxyDigest != "" || account.ProxyID != nil {
		current, resolveErr := s.upstream.ResolveProxyURL(ctx, account.ProxyID)
		if resolveErr != nil {
			return fmt.Errorf("复核流量出口配置: %w", resolveErr)
		}
		if kongProxyDigest(current) != snap.TrafficProxyDigest {
			return fmt.Errorf("流量出口 %s 的连接配置已改变", snap.TrafficEgress)
		}
	}
	return nil
}

// kongCaptureFacts 是这张票**被采到时**的事实，随探测记录一起长期留存。
//
// 必须显式传进来，不能在验证时现查：验证发生在采集之后（observed 票可能隔了很久），那时的出口与
// 静默值已经不是采集时的了。票缓存会随过期被删，届时只有探测记录能还原「这张票是怎么采到的」。
type kongCaptureFacts struct {
	Egress      string
	IdleSeconds *int64
}

// verifyTicket 对一张票做指纹验证，份数按置信度递增、达阈值即停。
//
// 验证走**流量出口**，不占用票据出口的静默。
// taskStartedAt 是本次任务真正开始的时刻，用作候选淘汰的水位。**必须由调用方传入**：在本函数里
// 现取会晚于任务开始（代理解析、取票都在前面），那段时间里新到的票会被当成「任务开始时就存在的
// 旧候选」一起标掉，而它恰恰是唯一的恢复机会（`full + none` 下尤甚）。
// kongFusedPartIndex 是融合样本在探测记录里的序号。
//
// 取 0 而不是 1：常规挑战用 1..N，融合被丢弃后 leader 会重跑一份同样是"第 1 份"的常规样本。两者
// 共用 1 的话，同一个 verification 下出现两条 part_index=1——页面显示两个第 1 份，离线重算也分不清
// 先后。0 同时表达了"它发生在所有常规挑战之前"。
const kongFusedPartIndex = 0

// attrErrText 把归因错误转成可入库的文本，nil 时为空串。
func attrErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// kongFusedSample 是取票请求顺带拿回的第一份指纹样本（融合取票）。
type kongFusedSample struct {
	Challenge KongFingerprintChallenge
	// Answer 为 nil 表示取票成功但正文没读回来（提前 EOF、SSE 错误）。**不能因此当作"没融合"**：
	// 那次调用已经花了额度，而且按约定要留一条 inconclusive。
	Answer *KongUpstreamAnswer
	// ReadErr 是读正文的失败原因，仅在 Answer 为 nil 时有值。
	ReadErr string
	// ReportedModel 是读正文失败时**仍然拿到的**上游回报 model。
	//
	// 单独一个字段而不是挂进 Answer：下游以 `Answer != nil` 作为"可归因"的条件，挂上残缺正文会被
	// 送进 stg1 甚至凑够数字早停通过。它只进事件留档——融合样本的 stg0 不判死票（那次请求本来就
	// 没带票），但"上游当时说它给了什么"必须留得下来，否则事后分不清真降智与该往白名单加一条。
	ReportedModel string
	// Egress 是这份样本产生时用的出口——**票据出口**，与常规样本的流量出口不同。
	Egress string
	// OnlyFused 为真表示不达标时不再发后续挑战。批内非触发模型用它：此刻没有请求在等那张票，
	// 不值得再花一份长答案，留作候选等它自己的第一个请求。
	OnlyFused bool
}

// verifyTicket 的 reverify 为真表示这是**重验一张正在服务的票**（人工「立即验票」），不是候选的
// 首次验证。两处行为因此不同，理由都是"这张票已经有过一次完整通过的结论"：
//
//   - 不淘汰别的候选、不推进取票冷却：那些是候选路径的机制，重验一张旧票失败说明不了出口有问题；
//   - 证据不完整时不提交 rejected：一份有效回答加两份 429 不足以证伪一个既有结论。
func (s *KongTicketService) verifyTicket(ctx context.Context, account *Account, cfg KongTicketConfig, model string, ticketID int64, state, source string, expiresAt time.Time, capture *kongCaptureFacts, taskStartedAt time.Time, reverify bool, fused *kongFusedSample) (int64, error) {
	if s.bank == nil {
		return 0, fmt.Errorf("校准资料未加载，无法验证")
	}
	trafficProxyURL, err := s.upstream.ResolveProxyURL(ctx, account.ProxyID)
	if err != nil {
		return 0, fmt.Errorf("解析流量出口: %w", err)
	}
	ticketEgress := KongEgressKey(cfg.Egress, cfg.ProxyID)
	trafficEgress := KongTrafficEgressKey(account.ProxyID)
	verificationID := uuid.NewString()
	fingerprint := kongTicketFingerprint(state)
	snap := kongVerifySnapshot{
		AccountID: account.ID, Model: model, Mode: cfg.Mode, TicketEgress: ticketEgress,
		TrafficEgress: trafficEgress, TrafficProxyDigest: kongProxyDigest(trafficProxyURL),
		IdentityDigest: kongAccountIdentityDigest(account),
		TicketID:       ticketID, ExpiresAt: expiresAt,
	}

	// 无论走到哪个终点，已经拿到的观测都要落库——它们是这次验证唯一的证据，事后补不回来。
	var probes []*KongFingerprintProbe
	newProbe := func(partIndex int, challengeID string) *KongFingerprintProbe {
		probe := &KongFingerprintProbe{
			VerificationID: verificationID, PartIndex: partIndex,
			AccountID: account.ID, TargetModel: model,
			TicketID: &ticketID, TicketFingerprint: fingerprint, TicketSource: source,
			VerifyEgress: trafficEgress, ChallengeID: challengeID,
			LibraryVersion: s.bank.Version(),
		}
		// 采集事实（出口 + 当时的静默值）随每份记录留存。observed 票没有采集静默可言，
		// 那时 capture 为空——留空比填一个验证时刻的静默值好，后者是另一回事。
		if capture != nil {
			probe.IdleSeconds = capture.IdleSeconds
			if capture.Egress != "" {
				probe.CaptureEgress = capture.Egress
			}
		}
		return probe
	}
	// 收尾用独立的短期限 context，不复用任务 context。
	//
	// 任务 context 带总期限（kongTaskBudget）；期限一到它就被取消，而落证据恰恰发生在任务结束时
	// ——沿用它的话，超时任务的 InsertProbes 必然因 context 取消而失败，已经拿到的数字全部丢掉。
	// 收尾只用于持久化，不会再发起挑战，所以脱离取消是安全的。
	persistCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), kongPersistBudget)
	}

	// saveProbes 落这次验证的全部观测。它是授予资格的前置条件，不是事后动作。
	saveProbes := func() error {
		pctx, cancel := persistCtx()
		defer cancel()
		return s.repo.InsertProbes(pctx, probes)
	}

	// finish 写这次验证的**最终**事件。
	//
	// `FingerprintModel` 与来源必须写在列上而不是只塞进 detail：管理面的诊断读的是列，写进 detail
	// 就等于「验证成功了，但页面显示未得出结论」。事件里还要能区分最终事件与单份失败事件，
	// 否则最新一条 verify 事件可能是某一份的失败、盖掉真正的结论。
	buildFinalEvent := func(outcome string, detail map[string]any) *KongTicketEvent {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["verification_id"] = verificationID
		detail["final"] = true
		detail["ticket_source"] = source
		event := &KongTicketEvent{
			AccountID: account.ID, Model: model, EventType: KongEventVerify,
			Outcome: outcome, TicketEgress: ticketEgress, TrafficEgress: trafficEgress,
			TicketID: &ticketID, Detail: detail,
		}
		if fingerprint, ok := detail["fingerprint"].(string); ok && fingerprint != "" {
			event.FingerprintModel = kongStrPtr(fingerprint)
		}
		return event
	}
	finish := func(id int64, outcome string, detail map[string]any, retErr error) (int64, error) {
		event := buildFinalEvent(outcome, detail)
		pctx, cancel := persistCtx()
		defer cancel()
		if err := s.repo.InsertEvent(pctx, event); err != nil {
			// 最终处置事件写不进去，就没有任何记录解释这张票为什么可用/不可用。授予资格前必须
			// 有可追溯的结论，所以这一次按失败收尾——票的状态已落库，下一个请求会重新判定。
			slog.Error("kong ticket: 验证的最终事件写入失败",
				"account_id", account.ID, "model", model, "ticket_id", ticketID,
				"verification_id", verificationID, "error", err)
			return 0, fmt.Errorf("记录验证结论: %w", err)
		}
		return id, retErr
	}

	// failBeforeConclusion 用在还没落过证据的终点上：先存观测，再记事件。
	// withProbeInsertError 把证据写库的失败并进 detail。**不覆盖原始失败原因**：证据写失败不该把
	// 「为什么这次没成」盖掉。
	withProbeInsertError := func(detail map[string]any, insErr error) map[string]any {
		if insErr == nil {
			return detail
		}
		if detail == nil {
			detail = map[string]any{}
		}
		detail["probe_insert_error"] = insErr.Error()
		return detail
	}
	failBeforeConclusion := func(outcome string, detail map[string]any, retErr error) (int64, error) {
		if insErr := saveProbes(); insErr != nil {
			if detail == nil {
				detail = map[string]any{}
			}
			// 保留原始失败原因：证据写失败不该把「为什么这次没成」覆盖掉。
			detail["probe_insert_error"] = insErr.Error()
		}
		return finish(0, outcome, detail, retErr)
	}
	// skipCandidatesForDrySpell 淘汰本段无票期里**所有**现存候选。
	//
	// 用在「没有可用回答」这个终点上：那说明这个账号此刻答不出可用序列，与具体哪张票无关。只标
	// 一张的话，缓存里的其它旧候选会被下一个请求逐张验证，而候选验证不经过取票冷却判断。
	// 之后真正新到的票不带这个标记，仍然算新信息。
	// failCooldown 推进 F（失败判定时刻）。
	//
	// 罚的必须是**采到这张票的那个出口**，不是当前配置算出来的出口：一张 fetch 票可能是上个进程
	// 用 proxy:8 采的，此后配置改成 direct，那时按当前配置罚会冷却一个从未取过这张票的出口
	// ——真正该退避的出口反而没被罚。
	//
	// observed 票不推进 F：它是业务响应顺带带回来的，没有消耗任何票据出口的静默。
	cooldownEgress := ticketEgress
	cooldownEgressKnown := true
	if capture != nil && capture.Egress != "" {
		cooldownEgress = capture.Egress
	} else if source == KongTicketSourceFetch {
		// 旧库里的 fetch 票没有采集事实。不知道是哪个出口采的，就不能罚任何一个出口——猜错
		// 比不罚更糟（罚错的那个会白白退避，该退避的继续被打）。
		cooldownEgressKnown = false
	}
	failCooldown := func(reason string) {
		if source != KongTicketSourceFetch {
			return
		}
		// 重验一张正在服务的旧票：它的失败说明不了这条出口现在有问题（票可能是一小时前取的），
		// 罚出口只会把下一个正常取票周期也往后推。
		if reverify {
			return
		}
		if !cooldownEgressKnown {
			pctx, cancel := persistCtx()
			defer cancel()
			s.logEventWith(pctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventCooldown,
				Outcome: KongOutcomeSkipped, TicketID: &ticketID,
				Detail: map[string]any{"reason": reason, "skipped": "capture_egress_unknown"},
			})
			return
		}
		s.enterCooldown(ctx, account.ID, model, cooldownEgress, reason)
	}
	// boundary 取任务开始时刻（调用方传入，见函数注释）。淘汰候选只能淘汰这个时刻之前就存在的
	// ——验证期间新到的票是新信息，把它也标掉会封掉唯一的恢复机会。
	boundary := taskStartedAt
	if boundary.IsZero() {
		boundary = time.Now()
	}
	skipDrySpell := func(reason string) {
		// 同上：候选淘汰是候选路径的机制，重验一张旧票失败与别的候选无关。
		if reverify {
			return
		}
		pctx, cancel := persistCtx()
		defer cancel()
		if skipErr := s.repo.SkipCandidatesFor(pctx, account.ID, model, boundary); skipErr != nil {
			s.logEventWith(pctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventVerify,
				Outcome: KongOutcomeFailure, TicketID: &ticketID,
				Detail: map[string]any{"error": skipErr.Error(), "phase": "skip_candidates", "reason": reason},
			})
		}
	}
	skipCandidate := func(reason string) {
		if skipErr := s.repo.SkipCandidate(ctx, ticketID); skipErr != nil {
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventVerify,
				Outcome: KongOutcomeFailure, TicketID: &ticketID,
				Detail: map[string]any{"error": skipErr.Error(), "phase": "skip_candidate", "reason": reason},
			})
		}
	}

	// stg0Failed 判上游在这次回答里回报的 model。返回 true 表示 stg0 已判 fail，调用方应当立刻
	// 停止并把票作废——**不要继续发后面的挑战**，那是分层的全部收益：一道零成本的门已经给出结论，
	// 再烧两份长答案不会改变它。
	//
	// 处置与 candidate_not_accepted 刻意一致（作废候选 + fetch 来源退避）：两者都意味着"这张票此刻
	// 没法支持这个模型"，区别只在证据来自上游重发票还是上游回报别的模型。
	//
	// ⚠️ **必须在 EchoedState 检查之后调用**：上游重发了票说明这次注入压根没被接受，那时回报的 model
	// 反映的不是被验证的那张票，据它判死票是把别人的锅记在它头上。
	// stg0Reject 落 stg0 判死的结论：**把上游回报的 model 当作归因结果写进票行**，状态 rejected。
	//
	// 为什么借用归因这条通路而不另立一套：下游全都按 fingerprint_probs 工作——页面显示、
	// kongPickCurrent 按当前白名单重判、离线重算。用同一种表达，这些地方自动得出正确结论（回报值
	// 不在白名单里，那张票永远不会被选为当前票），不必在每处加一个 stg0 分支。
	//
	// P 记 1.0 并在事件里标 `stg: 0`：这不是概率估计，是上游自己的声明。两者的可信度不同，混起来看
	// 会把一条确凿证据读成"归因恰好很确定"。
	stg0Reject := func(probe *KongFingerprintProbe, reported string, part int) (int64, error) {
		probe.InvalidReason = kongStrPtr(KongProbeInvalidStg0Mismatch)
		probe.CountedInAverage = false
		probes = append(probes, probe)

		// **落结论之前复核前提**，与 stg1 那条路径同一道关口。
		//
		// CommitVerification 的条件（状态、期限、未被跳过）补不上这个保护——它看不到账号的 mode、
		// 调度资格与出口配置。少了这一段，验证期间把流量代理换掉时，**旧出口上测到的 mismatch 会被
		// 用来判死一张票**，而那个结论属于已经不存在的环境。那是无谓拒服。
		//
		// 前提失效时只留观测，不改票状态、不淘汰候选、不推进冷却：什么都没测出来。
		if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
			probe.InvalidReason = kongStrPtr(KongProbeInvalidStaleResult)
			return failBeforeConclusion(KongOutcomeInconclusive,
				map[string]any{
					"reason": "stale_result", "stg": 0,
					"reported_model": reported, "detail": lost.Error(),
				},
				fmt.Errorf("stg0 结论作废，前提已失效: %w", lost))
		}

		if insErr := saveProbes(); insErr != nil {
			slog.Error("kong ticket: stg0 判死时证据落库失败",
				"account_id", account.ID, "model", model, "ticket_id", ticketID, "error", insErr)
		}

		// **落库之后再复核一次**，与 stg1 那条路径同一道关口。证据落库可能耗上一段时间，期间前提照样
		// 会变（模式被切走、代理被改、调度资格被取消），而 CommitVerification 的条件只看票状态、期限
		// 与跳过标记，补不上这个保护。只在保存之前复核，等于拿一个「保存开始时成立」的前提去作废票。
		//
		// 这里用 `finish` 而不是 `failBeforeConclusion`：探测刚才已经存过，后者会再存一遍。
		// 那几条观测的 invalid_reason 仍是 stg0_mismatch——观测本身是真的，作废的只是据它下的结论。
		if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
			return finish(0, KongOutcomeInconclusive,
				map[string]any{
					"reason": "stale_after_probe_persist", "stg": 0,
					"reported_model": reported, "requested_model": model,
					"part": part, "verification_id": verificationID, "detail": lost.Error(),
				},
				fmt.Errorf("stg0 结论作废，前提已失效: %w", lost))
		}
		detail := map[string]any{
			"reason": "stg0_model_mismatch", "stg": 0,
			// 回报值必须入库：判死一张票时要能回答"上游到底说它给了什么"，否则无从区分真降智与
			// "该往 stg0_accept 加一条"。
			"reported_model": reported, "requested_model": model,
			"part": part, "verification_id": verificationID,
			// fingerprint 这个键是 buildFinalEvent 填事件 FingerprintModel 的来源，而管理面的
			// latestDiagnosis 读的是事件列、不读票行。不写它，页面会把一张**已经判死**的票显示成
			// "未得出结论"，摘要里也看不到上游回报了什么。
			"fingerprint": reported,
		}
		// **冷却写在提交之后**：`failCooldown` 会同步写一条冷却事件，那是一次数据库往返，期间前提照样
		// 会变（改代理、切模式、取消调度资格），而提交只检查票状态、期限与跳过标记。放在提交之前等于在
		// 最后一道复核与提交之间又开一个写库窗口，那个窗口里失效的前提没人再核。
		pctx, cancel := persistCtx()
		committed, commitErr := s.repo.CommitVerification(pctx, ticketID, KongTicketStatusRejected,
			KongAttribution{Model: reported, P: 1, Probs: map[string]float64{reported: 1}},
			buildFinalEvent(KongOutcomeFailure, detail))
		cancel()
		if commitErr != nil {
			return 0, fmt.Errorf("落 stg0 结论: %w", commitErr)
		}
		if !committed {
			// 票在这期间已过期、被撤销或被跳过。结论过时，不能凭它改写任何状态——但**回报值要留着**，
			// 否则这条由上游自己给出的降档声明在并发撤销下会完全失去可追溯性。
			return finish(0, KongOutcomeInconclusive,
				map[string]any{
					"reason": "ticket_changed_during_verify", "stg": 0,
					"reported_model": reported, "requested_model": model,
					"part": part, "verification_id": verificationID,
				},
				fmt.Errorf("票在验证期间已被改写，stg0 结论作废"))
		}
		// 同期其它旧候选一并淘汰：降智是账号/时段级的现象，逐张验只会把同一个结论重复烧一遍额度。
		// 必须在提交**之后**——SkipCandidatesFor 会把 unverified 候选全标掉，含正在验的这张，而提交
		// 的条件里有 skip_until_new = FALSE。冷却同理：结论没落库就不该罚出口。
		failCooldown("stg0_model_mismatch")
		skipDrySpell("stg0_model_mismatch")
		return 0, fmt.Errorf("%w：上游回报 %s，不是请求的 %s", ErrKongTicketDowngraded, reported, model)
	}

	var (
		answers []KongFingerprintAnswer
		result  *KongFingerprintResult
		// fusedAccepted 只由融合样本达标置真。**不能用 `result != nil` 代替**：循环体每轮都会
		// 给 result 赋值（它是"到目前为止的累计归因"），拿它当跳过条件会让三份挑战退化成一份。
		fusedAccepted bool
	)
	// takeAnswer 把一份回答计入证据、回填该份的观测字段，返回累计归因。
	//
	// 抽出来是因为融合样本与常规挑战必须走**同一套**归因与回填：两处各写一遍的话，其中一处漏填
	// 就会让那类样本在探测记录里少一个维度，而事后分析看不出少了什么。
	takeAnswer := func(probe *KongFingerprintProbe, text string, expected int) (*KongFingerprintResult, error) {
		answers = append(answers, KongFingerprintAnswer{Text: text, ExpectedCount: expected})
		attributed, attrErr := KongFingerprintAttribute(answers, s.bank)
		if attributed != nil && len(attributed.Parts) > 0 {
			part := attributed.Parts[len(attributed.Parts)-1]
			probe.Digits = part.Numbers
			probe.DigitCount = part.ParsedNumbers
			probe.ParseValid = part.Accepted
			probe.CountedInAverage = part.Accepted
			if part.Accepted {
				probe.Scores = kongScoresByModel(part.Scores, s.bank)
				if pred, ok := KongFingerprintPartPrediction(part, s.bank); ok {
					probe.PartAttribution = kongStrPtr(pred)
				}
			} else if part.InvalidReason != "" {
				probe.InvalidReason = kongStrPtr(part.InvalidReason)
			}
		}
		return attributed, attrErr
	}

	// ---- 融合样本：取票请求顺带拿回的第一份 ----
	//
	// 取票那次响应本就是这张票所钉住的目标生成的（turn-state 是 sticky-routing token），所以它能
	// 当第一份指纹样本用，省掉一次往返。但它与常规样本的产生条件不同——**在票据出口上、且当时没有
	// 注入票**，所以未达标时刻意不据它判不合格：那可能只是"打票出口上的档位"，而票在流量出口上仍
	// 然可用，据此作废就是无谓拒服。
	if fused != nil {
		// 融合样本占序号 0：常规挑战从 1 起。**不能与常规首份共用 1**——融合被丢弃后 leader 会重跑
		// 一份同样是第 1 份的常规样本，同一个 verification 下出现两条 part_index=1，页面显示两个
		// "第 1 份"，离线重算也分不清先后。
		probe := newProbe(kongFusedPartIndex, fused.Challenge.ID)
		// 这一份的验证出口是**票据出口**，与常规样本不同，必须如实记——混算会直接毁掉"哪个出口
		// 取到好票"这个判断。
		probe.VerifyEgress = fused.Egress
		probe.Fused = true
		// 耗时与 token 用量照常回填：这一份同样是一次真实调用，"额度花在哪了"要靠它们回答。
		if fused.Answer != nil {
			probe.LatencyMs = kongIntPtr(fused.Answer.LatencyMs)
			probe.OutputTokens = fused.Answer.OutputTokens
		}

		var attributed *KongFingerprintResult
		var attrErr error
		reason := "fused_unreadable"
		if fused.Answer == nil {
			// 取票成功但正文没读回来（提前 EOF、SSE 错误）。样本不可用，但**票是好的**——留档、
			// 记一条 inconclusive，绝不因此丢票。
			attrErr = fmt.Errorf("融合取票没有拿回可用正文")
			if fused.ReadErr != "" {
				attrErr = fmt.Errorf("融合取票读取正文失败: %s", fused.ReadErr)
			}
			probe.InvalidReason = kongStrPtr(KongProbeInvalidTruncated)
		} else {
			attributed, attrErr = takeAnswer(probe, fused.Answer.Text, fused.Challenge.ExpectedCount)
			if attributed != nil {
				probe.CumProbability = &attributed.Probability
				probe.TemperatureTier = kongIntPtr(kongTierToInt(attributed.CalibrationTier))
			}
			reason = "fused_insufficient"
			if attrErr != nil {
				reason = "fused_unparsable"
			}
		}

		// stg0 在融合样本上的语义**与常规挑战相反**：这一份产生于取票请求，那次请求**本来就没带票**
		// （票是它的产物）。所以上游回报别的模型，说的是"无票时会被降智"——那正是我们需要票的理由，
		// 不是刚取到的这张票不合格。据它判死票会把一张好票扼掉，是无谓拒服。
		//
		// 处置因此是**丢弃这一份样本**，而不是作废票：与"归因不达标"走同一条路（leader 重跑常规挑战，
		// 那时带着票、走流量出口，stg0 才有判定意义；非触发模型不重跑、票留候选池）。
		stg0Bad := fused.Answer != nil && s.stg0.Verdict(model, fused.Answer.ReportedModel) == KongStg0Fail
		// 正文读失败时回报值仍要入事件：样本不可用，但"上游当时说它给了什么"是这次调用唯一留下的
		// 判据。空着就等于这条声明在断流时彻底消失。**不参与 stg0Bad**——票是这次请求的产物，
		// 据无票时的回报判死它是无谓拒服。
		stg0Reported := fused.ReportedModel
		if stg0Bad {
			reason = "fused_stg0_mismatch"
			probe.InvalidReason = kongStrPtr(KongProbeInvalidStg0Mismatch)
			stg0Reported = fused.Answer.ReportedModel
			// 归因本身可能完全正常（回答能解析、甚至达标），只是它不是这个模型产出的。那时 attrErr
			// 为 nil，而下面两处都要一个非 nil 的原因——缺了它 `%w` 会打出 `%!w(<nil>)`，事件里躺着
			// 一条没人看得懂的记录。
			if attrErr == nil {
				attrErr = fmt.Errorf("上游回报 %s，不是请求的 %s", stg0Reported, model)
			}
		}

		if !stg0Bad && attrErr == nil && s.accept.Accepts(model, kongProbsOf(attributed), s.confidence) {
			// 达标：一次上游请求就完成了取票 + 验票，后面整个挑战循环都不必跑。
			result = attributed
			fusedAccepted = true
			probes = append(probes, probe)
		} else {
			// 不达标：丢弃这份样本，按常规路径重跑（带票、走流量出口）。混进后续样本会让归因概率
			// 失去意义——两类样本的产生条件不同，而"证据不完整"那条保护也是按同条件样本设计的。
			//
			// **观测留档但必须标成不采用**：不置假的话，离线按 counted_in_average 重算会把这份
			// 混回去，得出与线上不同的结论。
			probe.DiscardedReason = reason
			probe.CountedInAverage = false
			probes = append(probes, probe)
			answers = nil
			if fused.OnlyFused {
				// 批内非触发模型：此刻没有请求在等这张票，不值得再花一份长答案去重跑——票留在
				// 候选池，等该模型自己的第一个请求按常规路径验。
				//
				// 只落**一条**最终记录：failBeforeConclusion 自己会存探测并写最终事件，这里再
				// 显式存一次会让同一份观测入库两遍（真实仓储是普通 INSERT，不去重）。
				return failBeforeConclusion(KongOutcomeInconclusive,
					map[string]any{
						"reason": reason, "fused": true, "retry": false,
						"detail": attrErrText(attrErr), "reported_model": stg0Reported,
					},
					fmt.Errorf("融合样本不足以判定，留作候选: %w", attrErr))
			}
			// leader 有请求在等，值得重跑。阶段事件说明这一份为什么被丢弃。
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventVerify,
				Outcome: KongOutcomeInconclusive, TicketID: &ticketID,
				TicketEgress: fused.Egress, TrafficEgress: trafficEgress,
				Detail: map[string]any{
					"reason": reason, "fused": true, "verification_id": verificationID,
					"final": false, "retry": true, "detail": attrErrText(attrErr),
					"reported_model": stg0Reported,
				},
			})
		}
	}

	// 融合样本已判定接受时整个循环都不跑——那次取票请求就是全部证据。
	for i, challenge := range KongFingerprintChallenges() {
		if fusedAccepted {
			break
		}
		// 每次主动请求之前复核：前提已经不成立时继续打上游只是白烧额度。
		if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
			probe := newProbe(i+1, challenge.ID)
			probe.InvalidReason = kongStrPtr(KongProbeInvalidStaleResult)
			probes = append(probes, probe)
			return failBeforeConclusion(KongOutcomeInconclusive,
				map[string]any{"reason": "precondition_lost", "detail": lost.Error()},
				fmt.Errorf("验证前提已失效: %w", lost))
		}

		answer, runErr := s.upstream.RunChallenge(ctx, account, trafficProxyURL, model, challenge, state)
		probe := newProbe(i+1, challenge.ID)
		if answer != nil {
			probe.LatencyMs = &answer.LatencyMs
			probe.OutputTokens = answer.OutputTokens
			// 即使这一份最终不可用，也先把原始数字序列留下来。
			if numbers := kongFPParseNumbers(answer.Text); len(numbers) > 0 {
				probe.Digits = numbers
				probe.DigitCount = len(numbers)
			}
		}

		// **stg0 的判定必须排在 runErr 之前**：回报的 model 在流首（response.created）就到了，
		// 而"上游说它给了别的模型"这个结论不因正文残缺而失效。
		//
		// 顺序上仍然让 EchoedState 优先——上游重发了票说明这次注入压根没被接受，那时回报的 model
		// 反映的不是被验证的那张票。读流失败时拿不到响应头以外的东西，EchoedState 照样有效。
		//
		// ⚠️ 少了这一段，上游只要「先在 created 里声明别的模型、然后断流」就能绕过整个 stg0：
		// 这一份被 continue 跳过，后两份正常作答，stg1 放行——净效果是放行降智。
		if answer != nil && answer.EchoedState == "" &&
			s.stg0.Verdict(model, answer.ReportedModel) == KongStg0Fail {
			return stg0Reject(probe, answer.ReportedModel, i+1)
		}
		if runErr != nil {
			probe.InvalidReason = kongStrPtr(KongProbeInvalidTruncated)
			probes = append(probes, probe)
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventVerify,
				Outcome: KongOutcomeFailure, TicketEgress: ticketEgress, TrafficEgress: trafficEgress,
				TicketID: &ticketID,
				Detail: map[string]any{
					"error": runErr.Error(), "part": i + 1,
					"verification_id": verificationID, "final": false,
				},
			})
			continue
		}
		if answer.EchoedState != "" {
			// 上游又下发了票 = 本次注入没被接受。这次回答的档位反映的不是被验证的那张票，
			// 不能用来判定它合格（否则会把一张坏票标成 verified）。
			probe.InvalidReason = kongStrPtr(KongProbeInvalidCandidateNotAccept)
			probes = append(probes, probe)
			// **顺序是刻意的：先落证据，再复核，最后才施加处置。**
			//
			// 三件事都不可逆——排除候选让它不再被自动选中、冷却把下一个正常周期往后推、返回的
			// sentinel 会让人工入口撤销这张票。而落库本身要花时间，期间模式可能被切走、出口被改、
			// 账号被换绑；那之后这一份失败说明不了那张票的任何问题，它是在另一套前提下拿到的。
			// 复核放在落库**之后**而不是之前，才覆盖得住这段窗口（stg0 / stg1 两条路径同一口径）。
			probeErr := saveProbes()
			if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
				return finish(0, KongOutcomeInconclusive,
					withProbeInsertError(map[string]any{
						"reason": "precondition_lost", "detail": lost.Error(),
					}, probeErr),
					fmt.Errorf("验证前提已失效: %w", lost))
			}
			skipCandidate("candidate_not_accepted")
			// 这是一次真实的挑战开销，且说明刚采到的票已经作废。fetch 来源必须退避，否则
			// `min_idle=0` 下相邻两个请求会各取一张新票、各烧一次挑战，全部无效。
			failCooldown("candidate_not_accepted")
			return finish(0, KongOutcomeFailure,
				withProbeInsertError(map[string]any{"reason": "candidate_not_accepted"}, probeErr),
				ErrKongTicketNotAccepted)
		}
		attributed, attrErr := takeAnswer(probe, answer.Text, challenge.ExpectedCount)
		if attrErr != nil {
			// 还没有任何可用回答。留下这一份的观测，继续下一条挑战。
			probes = append(probes, probe)
			result = nil
			continue
		}
		result = attributed
		probe.CumProbability = &result.Probability
		probe.TemperatureTier = kongIntPtr(kongTierToInt(result.CalibrationTier))
		probes = append(probes, probe)

		// 停止条件用**白名单内的概率之和**，与最终采纳判据同一个量。用 argmax 会在证据分散于
		// 两个都接受的归因之间时白加挑战，加满还是拒——每一份挑战都是一次上游请求。
		if s.accept.Accepts(model, kongProbsOf(result), s.confidence) {
			break
		}
	}

	if result == nil {
		// 同 EchoedState 分支：先落证据，复核放在落库之后，再施加不可逆的处置。
		probeErr := saveProbes()
		if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
			return finish(0, KongOutcomeInconclusive,
				withProbeInsertError(map[string]any{
					"reason": "precondition_lost", "detail": lost.Error(),
				}, probeErr),
				fmt.Errorf("验证前提已失效: %w", lost))
		}
		// 淘汰本段无票期里的 observed 候选：它们会被逐张验证，每次都绕过取票冷却。
		// fetch 候选**不在**批量淘汰范围内（见 SkipCandidatesFor 的注释），所以正在验的这张要
		// 单独标——不标的话 OldestCandidate 下一次照样选中它，同一张票被无限重验。
		// SkipCandidate 只对 unverified 生效，因此重验一张正在服务的票不会被它误伤。
		skipCandidate("no_valid_answer")
		skipDrySpell("no_valid_answer")
		// 只有主动取来的票才进冷却：observed 候选失败应当立即升级为主动取票，把那种失败也算进
		// F 会让升级白等一个冷却期。
		failCooldown("no_valid_answer")
		// 没有有效回答是"没测出来"，不是"票不合格"——票留在候选池等下一次机会。
		return finish(0, KongOutcomeInconclusive,
			withProbeInsertError(map[string]any{"reason": "no_valid_answer"}, probeErr),
			fmt.Errorf("没有可用回答，无法判定"))
	}

	// 落结论之前再复核一次：晚到的结果不能授予服务资格。
	if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
		return failBeforeConclusion(KongOutcomeInconclusive,
			map[string]any{"reason": "stale_result", "detail": lost.Error()},
			fmt.Errorf("结论作废，前提已失效: %w", lost))
	}

	// 证据必须先落库再授予资格：反过来的话，探测写失败会留下一张 verified 票（下一个请求照样
	// 用它），而支撑那个结论的原始观测已经永久丢失。
	if insErr := saveProbes(); insErr != nil {
		// 写库失败是基础设施故障，对票本身什么都没说。
		return finish(0, KongOutcomeInconclusive,
			map[string]any{"reason": "probe_persist_failed", "detail": insErr.Error()},
			fmt.Errorf("记录探测: %w", insErr))
	}

	// 证据落库可能耗上一段时间，期间前提照样会变（模式被切走、出口被改、票过期）。资格提交
	// 之前必须再复核一次——只在保存之前复核，等于拿一个「保存开始时成立」的前提去提交。
	if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
		return finish(0, KongOutcomeInconclusive,
			map[string]any{"reason": "stale_after_probe_persist", "detail": lost.Error()},
			fmt.Errorf("结论作废，前提已失效: %w", lost))
	}

	probs := kongProbsOf(result)
	accepted := s.accept.Accepts(model, probs, s.confidence)
	// 重验一张正在服务的票时，**证据不完整不足以推翻一个既有结论**：一份有效回答加两份 429 会让
	// 归因概率天然偏低，据此提交 rejected 等于让基础设施故障作废一张好票（无谓拒服）。挑战总数是
	// 定值，早停只发生在"已经判定接受"时，所以 `!accepted && 有效份数 < 总份数` 就是证据不完整。
	//
	// 候选的首次验证不适用这条：那时不提交只是让候选留在 unverified、下次再验，而提交 rejected
	// 才是把它排除掉——两种处置都不会让降智输出流出去。
	if reverify && !accepted && result.UsedAnswers < len(KongFingerprintChallenges()) {
		return failBeforeConclusion(KongOutcomeInconclusive,
			map[string]any{"reason": "incomplete_evidence", "used_answers": result.UsedAnswers,
				"challenges": len(KongFingerprintChallenges())},
			fmt.Errorf("只有 %d/%d 份有效回答，证据不足以推翻既有结论",
				result.UsedAnswers, len(KongFingerprintChallenges())))
	}
	status := KongTicketStatusRejected
	if accepted {
		status = KongTicketStatusVerified
	}
	detail := map[string]any{
		"probability":      result.Probability,
		"used_answers":     result.UsedAnswers,
		"calibration_tier": result.CalibrationTier,
		"fingerprint":      result.Prediction,
		// 采纳看的是白名单内的概率之和，不是 probability。两个都记：只记前者看不出最像哪个，
		// 只记后者解释不了「argmax 概率不够却仍然放行」。
		"accept_models": s.accept.Of(model),
		"accept_mass":   s.accept.Mass(model, probs),
		// 前三名连概率一起记。只留 argmax 解释不了拒票：一张真的 sol 票可能是
		// sol 0.82 / 5.5 0.18，看不见第二名就不知道该往白名单里加什么，还是该换校准资料。
		//
		// 记在事件里而不是只靠票行的 fingerprint_probs：票会随过期被清理，事件长期保留，而
		// 「当时归因成什么」正是事后唯一能复核的依据。
		"top_models": kongTopModels(probs, 3),
	}
	outcome := KongOutcomeSuccess
	var retErr error
	grantedID := ticketID
	if !accepted {
		outcome = KongOutcomeFailure
		retErr = fmt.Errorf("归因为 %s（%.4f），%s 可接受的归因 %v 合计只有 %.4f，不足 %.4f",
			result.Prediction, result.Probability, model, s.accept.Of(model),
			s.accept.Mass(model, probs), s.confidence)
		grantedID = 0
	}

	// 资格与最终事件同一个事务。事件写失败时资格不得对任何请求可见。
	pctx, cancelCommit := persistCtx()
	committed, commitErr := s.repo.CommitVerification(pctx, ticketID, status,
		KongAttribution{Model: result.Prediction, P: result.Probability, Probs: probs},
		buildFinalEvent(outcome, detail))
	cancelCommit()
	if commitErr != nil {
		slog.Error("kong ticket: 结论提交失败",
			"account_id", account.ID, "model", model, "ticket_id", ticketID,
			"verification_id", verificationID, "error", commitErr)
		return 0, fmt.Errorf("落指纹结论: %w", commitErr)
	}
	if !committed {
		// 票在验证期间已过期、被撤销或被跳过。不能凭一个过时的结论把它重新当成可用。
		return finish(0, KongOutcomeInconclusive, map[string]any{"reason": "ticket_changed_during_verify"},
			fmt.Errorf("票在验证期间已被改写，结论作废"))
	}
	if !accepted {
		// 候选预算的消费必须在提交**之后**：`SkipCandidatesFor` 会把边界内所有 unverified 候选
		// 标成 skip，其中就包括正在验证的这一张，而提交的条件里有 `skip_until_new = FALSE`
		// ——反过来做必然更新 0 行，真实归因被一条「并发变更」的假原因顶掉，票还留在 unverified。
		//
		// 提交之后这张票已是 rejected，不在 `SkipCandidatesFor` 的 unverified 范围内，只有别的
		// 旧候选会被标掉，正是想要的效果。
		// 冷却与候选预算同理，都要在提交之后：`failCooldown` 会同步写一条冷却事件（一次数据库往返），
		// 放在提交之前等于在最后一道复核与提交之间开一个没人再核的写库窗口；结论没落库时罚出口本身也
		// 没有依据。
		failCooldown("not_target_model")
		skipDrySpell("not_target_model")
		// 结论已落库，这才是"本次测出的真降档"。包上 sentinel 供人工路径辨识（见
		// ErrKongTicketDowngraded），文本原样保留——调用方与页面都按文本显示原因。
		retErr = fmt.Errorf("%w：%w", ErrKongTicketDowngraded, retErr)
	}
	return grantedID, retErr
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

// kongStateLenDenylist 是一眼就能判定为降智的票长度。
//
// 312 是已实测的降智档位长度。它照样会被上游接受，所以不拦住的话会进候选、烧掉一次完整验证
// （三份挑战、每份几百 token），而结论是注定的。292 不在表里——同为 292 的票也可能不同，长度
// 只能否定、不能肯定，所以只用它做排除，判定合格仍然必须靠指纹。
var kongStateLenDenylist = map[int]bool{312: true}

// kongExpiredSweepBatch 是一次机会性清理的上限。
const kongExpiredSweepBatch = 200

// storeTicket 把一张票收进缓存，返回它的 id 与期限。
//
// 三件事挡在入库之前：长度黑名单、按票原值去重、以及顺手清掉已过期的行。放在这里而不是各调用
// 点，是因为 fetch 与 observed 两条路径都要遵守同一套规则。
func (s *KongTicketService) storeTicket(ctx context.Context, accountID int64, model, state, source string, capture *kongCaptureFacts) (int64, time.Time, bool, error) {
	if len(state) == 0 {
		return 0, time.Time{}, false, fmt.Errorf("空票")
	}
	if kongStateLenDenylist[len(state)] {
		n := len(state)
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventObserve,
			Outcome: KongOutcomeSkipped, StateLen: &n,
			Detail: map[string]any{"reason": "state_len_denylisted"},
		})
		return 0, time.Time{}, false, &KongErrTicketRejected{Reason: fmt.Sprintf("票长度 %d 在黑名单中", n)}
	}

	now := time.Now()
	// 过期行没有任何用途，票原值本身也是敏感串，留着只会无限堆积。请求驱动地顺手清理——
	// 这套设计刻意没有定时器，闲置期必须完全无活动。
	if _, err := s.repo.DeleteExpiredTickets(ctx, now, kongExpiredSweepBatch); err != nil {
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventObserve,
			Outcome: KongOutcomeFailure,
			Detail:  map[string]any{"error": err.Error(), "phase": "sweep_expired"},
		})
	}

	// TTL 只能按「收到时刻 + TTL」推算，所以它是下界估计。
	ticket := &KongTicket{
		AccountID: accountID, Model: model, State: state, StateLen: len(state),
		Source: source, Status: KongTicketStatusUnverified,
		CapturedAt:      now,
		ExpiresAt:       now.Add(kongTicketTTL),
		ExpiresAtSource: KongTicketExpiryEstimated,
	}
	// 采集事实随票行一起落库，验证时（可能隔着一次重启）照原样取回。
	if capture != nil {
		ticket.CaptureEgress = capture.Egress
		ticket.CaptureIdleSeconds = capture.IdleSeconds
	}
	id, inserted, err := s.repo.InsertTicket(ctx, ticket)
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("存票: %w", err)
	}
	if !inserted {
		// 这张票原值已经在库里。重复出现不是新信息：期限、状态、跳过标记都保持原样，
		// 调用方也不该据此解除标记或触发诊断探测。
		return id, time.Time{}, false, nil
	}
	return id, ticket.ExpiresAt, true, nil
}

// kongTicketTTL 是票的可用寿命。
const kongTicketTTL = 3600 * time.Second

// currentTicket 取一张能支持该模型请求的票，与管理面共用同一份挑选逻辑。
func (s *KongTicketService) currentTicket(ctx context.Context, accountID int64, model string) (*KongTicket, error) {
	return kongPickCurrent(ctx, s.repo, s.accept, s.confidence, accountID, model)
}

// idleSeconds 是采样时刻该出口的静默时长，随探测记录长期留存。
//
// 口径必须与调度判 A（`kongScheduleInput.LastEgressUsed`）一致：库与内存取较晚的那个。只读库的话，
// 活动事件写失败时长期记录会把那次活动整个漏掉——同一次取票，调度认为「刚用过」，而探测记录写着
// 「静默了很久」，事后对不上。
func (s *KongTicketService) idleSeconds(ctx context.Context, ticketEgress string, now time.Time) *int64 {
	last, err := s.repo.LastEgressActivity(ctx, ticketEgress)
	if err != nil {
		last = nil
	}
	if mem := s.egressUseMem(ticketEgress); mem != nil && (last == nil || mem.After(*last)) {
		last = mem
	}
	if last == nil {
		return nil
	}
	v := int64(now.Sub(*last).Seconds())
	if v < 0 {
		v = 0
	}
	return &v
}

// enterCooldown 写一条 cooldown 事件。F（调度公式里的失败判定时刻）只由它推进。
//
// 同时记一份内存事实：事件写失败时库里读不到 F，下一个请求会立刻再取一次票，冷却形同不存在。
// 内存与事件用同一个时刻，两者才对得上。
func (s *KongTicketService) enterCooldown(ctx context.Context, accountID int64, model, ticketEgress, reason string) {
	at := time.Now()
	s.noteCooldown(accountID, ticketEgress, at)
	s.logEvent(ctx, &KongTicketEvent{
		AccountID: accountID, Model: model, EventType: KongEventCooldown,
		Outcome: KongOutcomeFailure, TicketEgress: ticketEgress,
		CreatedAt: at,
		Detail:    map[string]any{"reason": reason},
	})
}

func kongCooldownKey(accountID int64, ticketEgress string) string {
	return fmt.Sprintf("%d\x00%s", accountID, ticketEgress)
}

// noteCooldown 记下本进程判定的失败时刻。
func (s *KongTicketService) noteCooldown(accountID int64, ticketEgress string, at time.Time) {
	key := kongCooldownKey(accountID, ticketEgress)
	s.mu.Lock()
	if s.lastCooldownMem == nil {
		s.lastCooldownMem = map[string]time.Time{}
	}
	if prev, ok := s.lastCooldownMem[key]; !ok || at.After(prev) {
		s.lastCooldownMem[key] = at
	}
	s.mu.Unlock()
}

// cooldownMem 取内存里记录的失败时刻。
func (s *KongTicketService) cooldownMem(accountID int64, ticketEgress string) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.lastCooldownMem[kongCooldownKey(accountID, ticketEgress)]; ok {
		return &t
	}
	return nil
}

// logEvent 记一条事件。
//
// 写不进去不该让业务请求失败，但**绝不能静默**：fetch 与 cooldown 事件同时是调度事实的唯一
// 来源（A 与 F 都从事件表读），丢一条就会让下一个请求以为出口已经静默够了，转头再取一次票，
// 把刚积累的静默清零。所以失败要落日志，且出口活动另有一份内存兜底（noteEgressUse）。
func (s *KongTicketService) logEvent(ctx context.Context, event *KongTicketEvent) {
	s.logEventWith(ctx, event)
}

// logEventWith 与 logEvent 相同，区别只在于调用方显式给出用于持久化的 context。
// 任务收尾时必须用独立的短期限 context——任务自己的那个此刻已经被总期限取消了。
func (s *KongTicketService) logEventWith(ctx context.Context, event *KongTicketEvent) {
	if err := s.repo.InsertEvent(ctx, event); err != nil {
		slog.Error("kong ticket: 事件写入失败",
			"account_id", event.AccountID, "model", event.Model,
			"event_type", event.EventType, "outcome", event.Outcome,
			"ticket_egress", event.TicketEgress, "error", err)
	}
}

// noteEgressUse 记下「本进程刚在这个票据出口上发过请求」。
//
// 它是事件表的兜底而不是替代：事件写失败时，A 在库里不会推进，但这一次网络活动**已经发生**，
// 静默确实被清零了。只信库里的值会让下一个请求立刻再取一次票。进程重启后这份内存没了，那时
// 只能退回库里的值——这是可接受的，重启本身就意味着一段静默。
func (s *KongTicketService) noteEgressUse(ticketEgress string, at time.Time) {
	if ticketEgress == "" {
		return
	}
	s.mu.Lock()
	if s.lastEgressUseMem == nil {
		s.lastEgressUseMem = map[string]time.Time{}
	}
	if prev, ok := s.lastEgressUseMem[ticketEgress]; !ok || at.After(prev) {
		s.lastEgressUseMem[ticketEgress] = at
	}
	s.mu.Unlock()
}

// egressUseMem 取内存里记录的该出口最后活动时刻。
func (s *KongTicketService) egressUseMem(ticketEgress string) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.lastEgressUseMem[ticketEgress]; ok {
		return &t
	}
	return nil
}

// RevokeUsedTicket 撤销本次实际使用的票。调用方必须传本次上送用的那张，不是「当前票」。
func (s *KongTicketService) RevokeUsedTicket(ctx context.Context, accountID int64, model string, ticketID int64) {
	if ticketID == 0 {
		return
	}
	detail := map[string]any{"reason": "upstream_reissued_state"}
	if err := s.repo.RevokeTicket(ctx, ticketID); err != nil {
		// 撤销失败意味着这张已被上游拒绝的票还挂在「当前票」上，下一个请求会拿它再撞一次墙。
		// 业务侧此刻已经拒服，这里补不了更多，但必须留痕。
		slog.Error("kong ticket: 撤销票失败",
			"account_id", accountID, "model", model, "ticket_id", ticketID, "error", err)
		detail["revoke_error"] = err.Error()
	}
	s.logEvent(ctx, &KongTicketEvent{
		AccountID: accountID, Model: model, EventType: KongEventInjectFail,
		Outcome: KongOutcomeFailure, TicketID: &ticketID,
		Detail: detail,
	})
}

// ObserveState 收下业务响应带回的票（三种模式都收），供模式切换后立即有票可用。
//
// observe 模式下它还负责触发诊断探测——那是这个模式存在的理由：告诉运维「这个账号当前在什么
// 档位」，人工据此决定要不要给它开 full。探测走业务出口，不占用票据出口的静默。
func (s *KongTicketService) ObserveState(ctx context.Context, accountID int64, model, state string) {
	if strings.TrimSpace(state) == "" {
		return
	}
	n := len(state)
	s.logEvent(ctx, &KongTicketEvent{
		AccountID: accountID, Model: model, EventType: KongEventObserve,
		Outcome: KongOutcomeInfo, StateLen: &n,
	})
	// observed 票没有采集事实：它是业务响应顺带带回来的，不存在「取票出口」与「取票前静默」。
	ticketID, expiresAt, inserted, err := s.storeTicket(ctx, accountID, model, state, KongTicketSourceObserved, nil)
	if err != nil {
		// 长度黑名单这类**预期拒绝**已经由 storeTicket 自己记过事件，不重复记；其余是持久化故障
		// ——那种情况下票没进缓存、诊断不会启动，而管理面只看到一条正常的 info，看不出库坏了。
		if !KongIsExpectedTicketRejection(err) {
			slog.Error("kong ticket: 被动收票入库失败",
				"account_id", accountID, "model", model, "state_len", n, "error", err)
			s.logEvent(ctx, &KongTicketEvent{
				AccountID: accountID, Model: model, EventType: KongEventObserve,
				Outcome: KongOutcomeFailure, StateLen: &n,
				Detail: map[string]any{"error": err.Error(), "phase": "store_ticket"},
			})
		}
		return
	}
	if !inserted {
		// 同一张票重复出现不是新信息：不解除跳过标记，也不触发诊断探测。
		//
		// 但要留下处置记录：一个账号可能长期只回送同一张票，那时管理面看到的是一串正常的
		// info，看不出「这一段时间根本没有新样本」。
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventObserve,
			Outcome: KongOutcomeSkipped, StateLen: &n, TicketID: &ticketID,
			Detail: map[string]any{"reason": "duplicate_state"},
		})
		return
	}
	// 新到的 observed 票是「新信息」：解除本段无票期里的跳过标记。
	if clearErr := s.repo.ClearSkipMarks(ctx, accountID, model); clearErr != nil {
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventObserve,
			Outcome: KongOutcomeFailure,
			Detail:  map[string]any{"error": clearErr.Error(), "phase": "clear_skip_marks"},
		})
	}
	s.maybeObserveProbe(ctx, accountID, model, ticketID, state, expiresAt)
}

// maybeObserveProbe 在 observe 模式下用刚收到的票跑一次诊断探测。
//
// 三道闸：模式必须是 observe（off 不探测，full 自有取票流程）、账号必须可调度、距上次探测必须
// 满 ObserveProbeInterval。探测烧真实额度（三份挑战、每份几百 token），没有间隔下限会把每个
// 业务响应都变成一次探测。
func (s *KongTicketService) maybeObserveProbe(ctx context.Context, accountID int64, model string, ticketID int64, state string, expiresAt time.Time) {
	// 诊断探测是可选能力：缺了账号装载或校准资料就不做，收票本身照常。
	if s.accounts == nil || s.upstream == nil || s.bank == nil {
		return
	}
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return
	}
	cfg, _ := ParseKongTicketConfig(account.Extra)
	if cfg.Mode != KongTicketModeObserve {
		return
	}
	// **先认领槽位，再读间隔**。反过来会留一个窗口：两个请求都读到旧的「上次探测时刻」，
	// 第一个探完并释放槽位后，第二个才认领，于是沿用那个旧读数又探一轮——一小时的间隔里跑了
	// 两轮、六份挑战。
	//
	// 占的是账号槽位而不是票据出口槽位：探测走业务出口，与取票争的是「同一账号别同时发两拨
	// 挑战」，不是出口的静默。
	task, outcome := s.claimTask(accountID, model, kongObserveSlot(accountID))
	if outcome != kongClaimFresh {
		return
	}
	handedOff := false
	defer func() {
		// 没能移交给后台任务的每条返回路径都要释放槽位，否则这个账号再也探测不了。
		if !handedOff {
			s.releaseTask(accountID, task)
		}
	}()

	lastProbe, err := s.repo.LastEventAt(ctx, accountID, model, KongEventObserveProbe)
	if err != nil {
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventProbeSkipped,
			Outcome: KongOutcomeFailure,
			Detail:  map[string]any{"error": err.Error(), "phase": "last_probe_at"},
		})
		return
	}
	if !KongObserveShouldProbe(time.Now(), account.IsSchedulable(), lastProbe, s.params) {
		reason := "interval_not_elapsed"
		if !account.IsSchedulable() {
			reason = "account_unready"
		}
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventProbeSkipped,
			Outcome: KongOutcomeSkipped, Detail: map[string]any{"reason": reason},
		})
		return
	}

	// started 事件**必须确认写入成功**才开始探测：间隔完全靠它推进，写失败却照样探测的话，
	// 下一张新票又读不到上次时刻，于是接着探——同一个小时里可以跑好几轮。
	if err := s.repo.InsertEvent(ctx, &KongTicketEvent{
		AccountID: accountID, Model: model, EventType: KongEventObserveProbe,
		Outcome: KongOutcomeInfo, TicketID: &ticketID,
		Detail: map[string]any{"phase": "started"},
	}); err != nil {
		slog.Error("kong ticket: observe 探测的起始事件写入失败，本次不探测",
			"account_id", accountID, "model", model, "error", err)
		return
	}

	handedOff = true
	// 诊断探测的任务开始时刻，同样用于候选淘汰的水位。
	probeStartedAt := time.Now()
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), kongTaskBudget)
	go func() {
		defer cancel()
		defer s.releaseTask(accountID, task)
		// 诊断只求一个结论，不授予服务资格——observe 模式本来就不注入。
		_, _ = s.verifyTicket(detached, account, cfg, model, ticketID, state, KongTicketSourceObserved, expiresAt, nil, probeStartedAt, false, nil)
	}()
}

// kongObserveSlot 是 observe 诊断探测占用的槽位键，与真实票据出口分开。
func kongObserveSlot(accountID int64) string {
	return fmt.Sprintf("observe:%d", accountID)
}

func kongTicketFingerprint(state string) string {
	if len(state) <= 12 {
		return fmt.Sprintf("len:%d", len(state))
	}
	return fmt.Sprintf("len:%d:%s", len(state), state[:8])
}

func kongStrPtr(s string) *string     { return &s }
func kongIntPtr(v int) *int           { return &v }
func kongFloatPtr(v float64) *float64 { return &v }

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

// KongManualRefresh 是一次手工触发的结果。
//
// **刻意不内嵌 KongTicketGrant**：那个结构带 `State`，也就是票原值——可注入的凭据。把它交给
// 管理接口等于送进浏览器与前端日志。这里只暴露判断结论所需的字段。
type KongManualRefresh struct {
	// RevivedTicketID 非零表示本次先把一张未过期的已拒票复位成候选，于是走的是"重验"而不是"取票"。
	RevivedTicketID int64 `json:"revived_ticket_id"`
	// Allowed 为真表示拿到了可用票。
	Allowed bool `json:"allowed"`
	// TicketID 是本次实际拿到的票；未拿到时为 0。
	TicketID int64 `json:"ticket_id"`
	// DenyReason 说明为什么没拿到。多数不是故障：静默未满、模式不是 full 都是正常结论。
	DenyReason string `json:"deny_reason"`
	// NotApplicable 表示该账号不在保护范围内（mode 不是 full），与"该保护但保不了"是两件事。
	NotApplicable bool `json:"not_applicable"`
	// RetryAfter 是可以再试的最早时刻，拿不到估计时为空。
	RetryAfter *time.Time `json:"retry_after"`
}

// KongManualVerify 是「立即验票」的结果。与 KongManualRefresh 分开：那个可能取新票，这个只认
// 当前这一张，两者的失败含义完全不同。
type KongManualVerify struct {
	// TicketID 是被验的那张票；没有当前票时为 0。
	TicketID int64 `json:"ticket_id"`
	// Accepted 为真表示这张票重新自证合格，仍可用。
	Accepted bool `json:"accepted"`
	// Revoked 为真表示本次已把它作废：证据完整但归因不合格，或上游明确重发了票。
	Revoked bool `json:"revoked"`
	// Inconclusive 为真表示**没能完成测量**（超时、429、前提失效、写库失败），旧票与旧结论保留。
	// 它与 Revoked 互斥，两者都为假且 Accepted 为假时说明压根没开始验（见 DenyReason）。
	Inconclusive bool `json:"inconclusive"`
	// Candidate 为真表示验的是一张**候选**，不是正在服务的票。调用方据它措辞：候选通过是
	// "进入可用集合"、被拒是"从候选池排除"，与作废一张在服务的票不是一回事。
	//
	// **不承诺"成为当前票"**：当前票按剩余寿命最长的合格票选，一张更早过期的验过了也不会接替；
	// 而 off / observe 压根不注入，那两个模式下永远不会有当前票。
	Candidate bool `json:"candidate"`
	// BudgetExhausted 为真表示还有一张候选没验——本端点是同步的，余量不够再跑一张（最坏约 90s）
	// 时宁可不开始，调用方据它提示"可以再点一次"。
	BudgetExhausted bool `json:"budget_exhausted"`
	// Steps 是本次实际验过的每一张票，按执行顺序。**顶层那几个字段等于最后一步**——最后一步决定
	// 最终局面，但只看它会漏掉过程中发生的事（比如当前票已被作废、随后候选补上了），而"当前票
	// 被作废"恰恰是调用方最需要看到的。
	Steps []KongManualVerifyStep `json:"steps"`
	// Reason 是没通过的原因，取 verifyTicket 的错误文本。
	Reason string `json:"reason"`
	// NotApplicable 表示压根没票可验（当前票与可验候选都没有）。模式不是它的成因——三种模式都能验。
	NotApplicable bool `json:"not_applicable"`
	// DenyReason 说明为什么没能开始验（同账号有任务在途之类）。
	DenyReason string `json:"deny_reason"`
}

// KongManualVerifyStep 是「立即验票」序列里一张票的结果，三态语义与 KongManualVerify 顶层相同。
type KongManualVerifyStep struct {
	TicketID int64 `json:"ticket_id"`
	// Candidate 区分"正在服务的票"与"候选"：失败后果不同（作废 vs 从候选池排除）。
	Candidate    bool   `json:"candidate"`
	Accepted     bool   `json:"accepted"`
	Revoked      bool   `json:"revoked"`
	Inconclusive bool   `json:"inconclusive"`
	Reason       string `json:"reason"`
}

// kongManualVerifyBudgetFloor 是开始验下一张之前要求的剩余期限。
//
// 生产实测单份挑战 27–31s、一张票用满三份约 87s，所以要留 100s 才不会把第二张掐断在中途——
// 掐断的代价是额度已经烧掉、结论却没落，两头都亏。
const kongManualVerifyBudgetFloor = 100 * time.Second

// TriggerVerify 人工验票：验**现有的票**，同步返回结果。本端点**永不取票**——那是 TriggerRefresh
// 的职责，这条分界是两个按钮的全部区别。
//
// 一次最多验两张，顺序固定：**先正在服务的那一张，再一张未验候选**。
//
//   - 先验当前票是安全优先而不是效率优先：它正在被注入，降智输出正在流出，哪怕它马上就要过期。
//   - 候选只验**一张**，取最新的那张（剩余 TTL 最长，批量取票下它通常正是刚在 292 窗口里取回的）。
//     不往更老的翻，是因为候选都来自同一条出口、入库时已过长度黑名单：最新那张归因不合格，基本
//     就说明账号当前档位低，更老的多半同样不合格，而每张的代价是最多三份真实挑战。
//
// 结论分三态，**只在"真验出问题"时作废**：证据完整而归因不合格（真降档）、或上游明确重发了票
// （ErrKongTicketNotAccepted，我们那张注入已不作数）→ 作废；重新自证合格 → 通过；超时、429、
// 前提失效、写库失败 → 未得出结论，旧票与旧结论都保留。第三类刻意不作废——那些结果没有证伪原
// 结论，而重新取票要等约 34 分钟静默，据此扔掉一张仍在 TTL 内的好票就是无谓拒服。
//
// 验证走**流量出口**，不消耗票据出口的静默（那是全系统最稀缺的资源），所以不受静默与冷却约束。
func (s *KongTicketService) TriggerVerify(ctx context.Context, accountID int64, model string) (*KongManualVerify, error) {
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %d 不存在", accountID)
	}
	out := &KongManualVerify{}
	cfg, _ := ParseKongTicketConfig(account.Extra)
	// **三种模式都能验**：验证走流量出口、不注入、不消耗票据出口的静默，问的是"手上这张票对应
	// 哪个模型"。off 账号照样收票入库（见 ObserveState），它那些票的档位正是"该不该开 full"的
	// 判据。只有**取票**是 full 专属（那是保护行为，见 ensureTicket 的模式判定）。
	if !account.IsSchedulable() {
		out.DenyReason = KongDenyAccountUnready
		return out, nil
	}

	current, err := kongPickCurrent(ctx, s.repo, s.accept, s.confidence, accountID, model)
	if err != nil {
		return nil, err
	}
	// 先确认有东西可验再占槽位：没有的话占了也只是白挡住别的调用。
	if current == nil {
		candidate, cerr := s.repo.NewestCandidate(ctx, accountID, model, time.Now())
		if cerr != nil {
			return nil, cerr
		}
		if candidate == nil {
			out.NotApplicable = true
			return out, nil
		}
	}

	// 占账号槽位而不是票据出口槽位：验证走流量出口，与出口静默无关（同 kongVerifyOnlySlot 的理由）。
	// 争的是"同一账号别同时发两拨挑战"。
	task, outcome := s.claimTask(accountID, model, kongVerifyOnlySlot(accountID))
	if outcome != kongClaimFresh {
		// 已有在途任务时不等它：那个任务可能在取新票，与"验这几张"不是同一件事，等来的结论
		// 挂到这些票上是错的。
		out.DenyReason = KongDenyOtherModelTask
		if outcome == kongClaimEgressBusy {
			out.DenyReason = KongDenyEgressBusy
		}
		return out, nil
	}
	defer s.releaseTask(accountID, task)

	// 脱离请求的取消：调用方超时后这次验证仍要跑完并落结论，否则会留下一张"验过但没记"的票。
	//
	// 期限取 kongWaitBudget 而不是 kongTaskBudget：这个端点是**同步**的，调用方的超时是 330s，
	// 给它 15 分钟等于让页面必然先报失败、而后台还在改票的状态，最终结论无人看见。
	// **占到槽位之后重读当前票**：上面那次读只用来判断"有没有东西可验"。槽位只保证不同时执行，
	// 挡不住先后交错——刚释放槽位的自动任务可能已经换掉或撤销了当前票，按旧快照验等于验一张
	// 已经不在服务的票，而结论会被挂到它身上。
	if current != nil {
		fresh, ferr := kongPickCurrent(ctx, s.repo, s.accept, s.confidence, accountID, model)
		if ferr != nil {
			return nil, ferr
		}
		// 当前票没了（被撤销或过期）：不回退去验候选。那是另一件事，由调用方再点一次决定——
		// 悄悄改掉验证对象，页面报的结论就挂错了票。
		if fresh == nil {
			out.NotApplicable = true
			return out, nil
		}
		current = fresh
	}

	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kongWaitBudget)
	defer cancel()
	deadline, _ := tctx.Deadline()

	verifyCandidate := true
	if current != nil {
		step, verifyErr, fatal := s.verifyOneManually(ctx, tctx, account, cfg, model, current, true)
		out.Steps = append(out.Steps, step)
		if fatal != nil {
			return kongFinishManualVerify(out), fatal
		}
		// **只有"本次测出的真降档"才值得再花一张候选的额度**，其余一律到此为止：
		//   - 通过：票好着，没有替代需求；
		//   - 上游重发了票：我们注入的票**一律**不被接受，候选必然同样被拒，验它纯属白烧；
		//   - 未得出结论：旧票与旧结论都还在，**并不缺票**；那类失败（429、超时）多半会在候选上
		//     原样重演。
		//
		// 判据用 ErrKongTicketDowngraded 而**不是** step.Revoked：后者读的是票此刻的库状态，而
		// 那个状态可能是**别的路径**刚撤销的（业务请求拿到上游重发的票就会撤销当前票）。那种并发
		// 下本次验证其实什么都没测出来，凭库状态往下走就会白烧一整张票的额度。
		verifyCandidate = errors.Is(verifyErr, ErrKongTicketDowngraded)
	}

	if verifyCandidate {
		if time.Until(deadline) < kongManualVerifyBudgetFloor {
			// 余量不够再验一张就别开始：开了也是烧掉额度、在中途被掐断。如实说出来，让调用方
			// 知道还有一张没验、可以再点一次。
			out.BudgetExhausted = true
			return kongFinishManualVerify(out), nil
		}
		candidate, cerr := s.repo.NewestCandidate(ctx, accountID, model, time.Now())
		if cerr != nil {
			return kongFinishManualVerify(out), cerr
		}
		if candidate != nil {
			step, _, fatal := s.verifyOneManually(ctx, tctx, account, cfg, model, candidate, false)
			out.Steps = append(out.Steps, step)
			if fatal != nil {
				return kongFinishManualVerify(out), fatal
			}
		}
	}
	return kongFinishManualVerify(out), nil
}

// kongFinishManualVerify 把最后一步的结果抬到顶层字段。最后一步决定最终局面，而 Steps 保留了
// 过程——两者都需要：只看顶层会漏掉"当前票已被作废、随后候选补上了"这种事。
func kongFinishManualVerify(out *KongManualVerify) *KongManualVerify {
	if len(out.Steps) == 0 {
		out.NotApplicable = true
		return out
	}
	last := out.Steps[len(out.Steps)-1]
	out.TicketID = last.TicketID
	out.Candidate = last.Candidate
	out.Accepted = last.Accepted
	out.Revoked = last.Revoked
	out.Inconclusive = last.Inconclusive
	out.Reason = last.Reason
	return out
}

// verifyOneManually 人工验一张票并按三态归档。
//
// reverify 为真表示这是正在服务的那一张（见 verifyTicket 的说明）。第三个返回值是**致命错误**
// ——只有"该作废却没作废成"算致命：那时调用方以为票没了、实际下一个请求还会注入它，必须让它
// 看见。验证本身失败不算致命，它是三态里的一种结果。
func (s *KongTicketService) verifyOneManually(ctx, tctx context.Context, account *Account,
	cfg KongTicketConfig, model string, ticket *KongTicket, reverify bool,
) (KongManualVerifyStep, error, error) {
	step := KongManualVerifyStep{TicketID: ticket.ID, Candidate: !reverify}
	granted, verifyErr := s.verifyTicket(tctx, account, cfg, model, ticket.ID, ticket.State,
		ticket.Source, ticket.ExpiresAt, kongCaptureOf(ticket), time.Now(), reverify, nil)
	if granted != 0 && verifyErr == nil {
		step.Accepted = true
		return step, nil, nil
	}
	if verifyErr != nil {
		step.Reason = verifyErr.Error()
	}
	if errors.Is(verifyErr, ErrKongTicketNotAccepted) {
		// 收尾用独立的短期限 context：上面那个 tctx 可能正是因为超时才走到这里，复用它的话
		// 撤销的 SQL 必然失败，于是调用方以为票没了、下一个请求还在注入它。
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rcancel()
		if err := s.repo.RevokeTicket(rctx, ticket.ID); err != nil {
			s.logEventWith(rctx, &KongTicketEvent{
				AccountID: account.ID, Model: model, EventType: KongEventVerify,
				Outcome: KongOutcomeFailure, TicketID: &ticket.ID,
				Detail: map[string]any{"error": err.Error(), "phase": "manual_revoke"},
			})
			return step, verifyErr, fmt.Errorf("上游未接受该票，但作废它失败: %w", err)
		}
		step.Revoked = true
		return step, verifyErr, nil
	}
	// 重读票的状态，区分"已被判不合格"与"没得出结论"。读不到就按"没得出结论"报——那是保守的
	// 一侧（不谎称票已作废）。
	if st, err := s.repo.TicketStatus(ctx, ticket.ID); err == nil && st == KongTicketStatusRejected {
		step.Revoked = true
		return step, verifyErr, nil
	}
	step.Inconclusive = true
	return step, verifyErr, nil
}

// TriggerRefresh 手工触发一次取票/验票，同步返回结果。这是**人工干预手段**，不是自动路径。
//
// **跳过静默与冷却这两条时间窗口约束**。它们是给自动运行用的保守估计：系统只看得见自己产生的
// 出口活动（设计 §6.2），真实静默常常更长，而运维能据带外信息判断此刻可不可以取。代价由触发者
// 承担——静默确实不足时会拿到坏票、把静默清零并进冷却。
//
// **不跳过结构性约束**：没配票据出口、出口失效或与流量出口合并、账号不可调度。那些不是调度规则，
// 而是物理上做不到（没有出口可用）或做了有害（票据出口与业务出口合并会让业务流量自己毁掉好票）。
//
// 另有一个前置动作：先尝试复位一张未过期的已拒票（见 ReviveRejectedCandidate）。它在改过白名单
// 或阈值之后最有价值——同一张票的证据在新判据下可能就合格了，而重验走流量出口、不消耗票据出口的
// 静默，比取一张新票便宜得多。
func (s *KongTicketService) TriggerRefresh(ctx context.Context, accountID int64, model string) (*KongManualRefresh, error) {
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %d 不存在", accountID)
	}
	out := &KongManualRefresh{}
	// manual=true 跳过静默与冷却；revive=true 让任务在持有槽位之后先复位一张已拒票。**复位不在
	// 这里做**——此刻还没拿到槽位，若有同模型任务在跑，它收尾时的批量跳过会把刚复位的票标成
	// skip，那张票此后连人工触发都救不回来。
	grant, err := s.ensureTicket(ctx, accountID, model, true, true, false)
	if err != nil {
		return nil, err
	}
	out.RevivedTicketID = grant.RevivedTicketID
	out.Allowed = grant.Allowed
	out.TicketID = grant.TicketID
	out.DenyReason = grant.DenyReason
	out.NotApplicable = grant.NotApplicable
	if !grant.RetryAfter.IsZero() {
		retry := grant.RetryAfter
		out.RetryAfter = &retry
	}
	return out, nil
}

// TriggerVerifyTicket 人工验**指名的那一张**票（票据详情页逐行触发用）。
//
// 与 TriggerVerify 的分工：那个按规则挑对象（当前票 + 最多一张候选），这个只验调用方点的那一张，
// 不多验、不改挑法——页面上点了第 3 行却去验第 1 行，是最容易让人误判的一种行为。
//
// 三种对象都能验，语义各不相同：
//   - 正在服务的那张 → reverify，受"证据不完整不推翻既有结论"保护；
//   - 候选 → 首次验证，走正常候选路径；
//   - **已拒票 → 先按 id 复位成候选再验**。改过接受白名单或阈值之后，同一份证据可能就合格了；
//     而注入未被接受那种拒绝重验会再次以 candidate_not_accepted 失败，两者都安全，不必分辨。
//
// 已过期的票不验：票本身已失效，验它只是白烧额度（最多三份真实挑战）。
func (s *KongTicketService) TriggerVerifyTicket(ctx context.Context, accountID, ticketID int64) (*KongManualVerify, error) {
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %d 不存在", accountID)
	}
	out := &KongManualVerify{}
	cfg, _ := ParseKongTicketConfig(account.Extra)
	// 模式不参与判定，理由同 TriggerVerify。
	if !account.IsSchedulable() {
		out.DenyReason = KongDenyAccountUnready
		return out, nil
	}

	// 占槽之前先粗查一次，只为"有没有这张票"与"属不属于这个账号"——**必须带 accountID**，否则
	// 换个 id 就能让一个账号去验别人的票。真正的判据要等占到槽位之后重读（见下）。
	probe, err := s.repo.TicketByID(ctx, accountID, ticketID)
	if err != nil {
		return nil, err
	}
	if probe == nil {
		out.NotApplicable = true
		return out, nil
	}
	model := probe.Model

	task, outcome := s.claimTask(accountID, model, kongVerifyOnlySlot(accountID))
	if outcome != kongClaimFresh {
		out.DenyReason = KongDenyOtherModelTask
		if outcome == kongClaimEgressBusy {
			out.DenyReason = KongDenyEgressBusy
		}
		return out, nil
	}
	defer s.releaseTask(accountID, task)

	// **占到槽位之后必须重读**。槽位只保证"不同时执行"，挡不住先后交错：粗查与占槽之间，一个刚
	// 释放槽位的自动任务可能已经把这张票判成 rejected。按旧快照走下去会跳过该做的准备，然后发出
	// 三份挑战、最后必被 CommitVerification 的状态条件挡掉——额度烧了、结论没落。
	ticket, err := s.repo.PrepareTicketForManualVerify(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	// 返回 nil 只有一种原因：票已过期（期限条件在 SQL 的 WHERE 里）。过期票不验——票本身已失效，
	// 验它只是白烧额度。
	if ticket == nil {
		out.NotApplicable = true
		return out, nil
	}
	if probe.Status == KongTicketStatusRejected || probe.SkipUntilNew {
		// 记下这次人工准备动作：一张 rejected 票突然又变成 unverified、或跳过标记被解除，事后只
		// 能靠这条事件解释。
		s.logEventWith(ctx, &KongTicketEvent{
			AccountID: accountID, Model: model, EventType: KongEventVerify,
			Outcome: KongOutcomeInfo, TicketID: &ticketID,
			Detail: map[string]any{
				"phase": "manual_prepare", "from_status": probe.Status,
				"cleared_skip": probe.SkipUntilNew,
			},
		})
	}

	// **重验语义按票此刻的状态定，不按"它是否排第一"**：一张仍然合格、只是过期较早的备用 verified
	// 票，同样已经完整通过过一次，同样该受"证据不完整不推翻既有结论"的保护。按 id 比对当前票会把
	// 它当成首次验证的候选，于是一份不合格归因加两份 429 就能把它判成 rejected——无谓拒服。
	reverify := ticket.Status == KongTicketStatusVerified

	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kongWaitBudget)
	defer cancel()
	step, _, fatal := s.verifyOneManually(ctx, tctx, account, cfg, model, ticket, reverify)
	out.Steps = append(out.Steps, step)
	return kongFinishManualVerify(out), fatal
}
