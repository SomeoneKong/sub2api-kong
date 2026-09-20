package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
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

	// accept 记录每个门控模型接受哪些归因结果（永远含自己）。采纳判据只此一份。
	accept KongTicketAccept
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
func NewKongTicketService(repo KongTicketRepository, upstream KongTicketUpstream, accounts KongAccountLoader, bank *KongFingerprintBank, params KongTicketParams, accept KongTicketAccept, confidence float64) *KongTicketService {
	if confidence <= 0 || confidence >= 1 {
		confidence = 0.9
	}
	return &KongTicketService{
		repo:             repo,
		upstream:         upstream,
		accounts:         accounts,
		bank:             bank,
		params:           params,
		accept:           accept,
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
	return s.ensureTicket(ctx, accountID, model, false, false)
}

// ensureTicket 是 EnsureTicket 的实现。
//
// manual 为真时跳过静默与冷却（见 KongScheduleInput.IgnoreWindow）；revive 为真时任务在持有槽位
// 之后先尝试复位一张已拒票并以它为验证目标。两者都只由人工触发置真。
func (s *KongTicketService) ensureTicket(ctx context.Context, accountID int64, model string, manual, revive bool) (*KongTicketGrant, error) {
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %d 不存在", accountID)
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
	case KongActionWait:
		return s.waitForTask(ctx, accountID, model)
	case KongActionVerifyCandidate, KongActionFetch:
		return s.runTaskAndWait(ctx, account, cfg, model, decision.Action, manual, revive)
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
			revived.ExpiresAt, kongCaptureOf(revived), taskStartedAt)
		return id, revivedID, verr
	}

	switch decision.Action {
	case KongActionVerifyCandidate:
		id, verr := s.verifyExistingCandidate(ctx, fresh, freshCfg, model, taskStartedAt)
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
		return nil, ctx.Err()
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

// fetchAndVerify 经票据出口取一张票，立即验证。
//
// fetch 票不受 MinTicketAge 限制：它是我们自己刚要来的唯一一张，等待毫无意义，否则冷启动要
// 凭空多阻塞一个 MinTicketAge。
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

	idle := s.idleSeconds(ctx, ticketEgress, time.Now())
	probe, err := s.upstream.FetchTurnState(ctx, account, egressProxyURL, model)
	// 活动结束时刻在这里定死，后面存票、清标记、写事件的耗时都不再影响它。事件的 CreatedAt 与
	// 内存事实用同一个值，两者才对得上——不固定的话持久化的 A 会比真实活动晚上百毫秒，而调度
	// 判的是「距上次活动多久」。
	sentAt := time.Now()
	if KongIsUpstreamNotAttempted(err) {
		// 请求还没送出去就失败了（缺凭据这类本地错误）：没有清零任何静默，不能推进 A。
		s.logEvent(ctx, &KongTicketEvent{
			AccountID: account.ID, Model: model, EventType: KongEventFetchSkipped,
			Outcome: KongOutcomeFailure, TicketEgress: ticketEgress, TrafficEgress: trafficEgress,
			CreatedAt: sentAt,
			Detail:    map[string]any{"error": err.Error(), "phase": "build_request"},
		})
		return 0, err
	}
	// 请求已经发出去了，成败都一样清零了这个出口的静默——必须在判成败之前记。
	s.noteEgressUse(ticketEgress, sentAt)
	event := &KongTicketEvent{
		AccountID: account.ID, Model: model, EventType: KongEventFetch,
		TicketEgress: ticketEgress, TrafficEgress: trafficEgress, IdleSeconds: idle,
		CreatedAt: sentAt,
	}
	if probe != nil {
		event.StatusCode = &probe.StatusCode
		if probe.State != "" {
			n := len(probe.State)
			event.StateLen = &n
		}
	}
	if err != nil {
		event.Outcome = KongOutcomeFailure
		event.Detail = map[string]any{"error": err.Error()}
		s.logEvent(ctx, event)
		s.enterCooldown(ctx, account.ID, model, ticketEgress, "fetch_failed")
		return 0, err
	}
	if probe.State == "" {
		// 上游认为本次请求已带有效票，与我们「无票」的认知冲突——值得记，它意味着别处
		// 还有一条在用同一账号的链路。
		event.Outcome = KongOutcomeFailure
		event.Detail = map[string]any{"reason": "no_state_returned"}
		s.logEvent(ctx, event)
		s.enterCooldown(ctx, account.ID, model, ticketEgress, "no_state_returned")
		return 0, fmt.Errorf("上游未下发票")
	}
	// 事件在存票之后才写：这样它能带上票 id，验证记录与「这张票是怎么采到的」（出口、静默值）
	// 才对得上——票缓存会随过期被删，只靠时间相邻去猜是猜不准的。
	// 一次真实的网络取票**只记一条** fetch 事件，不论入库这一步结果如何：记两条的话成功率统计
	// 会把同一次取票算两遍，而「取了几次」正是额度口径。入库的后处理结果一律进 detail。
	capture := &kongCaptureFacts{Egress: ticketEgress, IdleSeconds: idle}
	ticketID, expiresAt, inserted, err := s.storeTicket(ctx, account.ID, model, probe.State, KongTicketSourceFetch, capture)
	if err != nil {
		event.Outcome = KongOutcomeFailure
		event.Detail = map[string]any{"error": err.Error(), "phase": "store_ticket"}
		s.logEvent(ctx, event)
		// 网络活动已经发生，退避必须推进：否则下一个请求立刻再取一次，同一个故障被无限重试。
		s.enterCooldown(ctx, account.ID, model, ticketEgress, "store_ticket_failed")
		return 0, err
	}
	event.TicketID = &ticketID
	if !inserted {
		// 主动取票拿回了一张我们已经有的票：那张票的结论（含 rejected）仍然有效，不该重新验证。
		event.Outcome = KongOutcomeFailure
		event.Detail = map[string]any{"reason": "duplicate_state"}
		s.logEvent(ctx, event)
		// 同样要推进退避。min_idle=0 是合法配置，不进冷却时两个相邻请求会各取一次、各拿回同一张
		// 重复票，净效果是零收益的双倍网络活动。
		s.enterCooldown(ctx, account.ID, model, ticketEgress, "duplicate_state")
		return 0, fmt.Errorf("取到的票与库中已有的重复")
	}
	// 一次成功的主动取票是「新信息」：解除本段无票期里的候选跳过标记。
	//
	// 放在写事件之前，失败原因随那一条事件走：解不掉标记只会让本可重试的候选继续被跳过，
	// 不影响这张新票，所以不中止——但必须留痕，否则「为什么那几张候选再也没被选过」查不出来。
	if clearErr := s.repo.ClearSkipMarks(ctx, account.ID, model); clearErr != nil {
		event.Detail = map[string]any{"clear_skip_marks_error": clearErr.Error()}
	}
	event.Outcome = KongOutcomeSuccess
	s.logEvent(ctx, event)

	return s.verifyTicket(ctx, account, cfg, model, ticketID, probe.State, KongTicketSourceFetch, expiresAt, capture, taskStartedAt)
}

// verifyExistingCandidate 验证缓存里已有的候选。
func (s *KongTicketService) verifyExistingCandidate(ctx context.Context, account *Account, cfg KongTicketConfig, model string, taskStartedAt time.Time) (int64, error) {
	candidate, err := s.repo.OldestCandidate(ctx, account.ID, model, s.params.MinTicketAge, time.Now())
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
	return s.verifyTicket(ctx, account, cfg, model, candidate.ID, candidate.State, candidate.Source, candidate.ExpiresAt, capture, taskStartedAt)
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
	TicketID           int64
	ExpiresAt          time.Time
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
	if cfg.Mode != snap.Mode {
		return fmt.Errorf("模式已从 %s 改为 %s", snap.Mode, cfg.Mode)
	}
	if cfg.Mode == KongTicketModeOff {
		return fmt.Errorf("已退出保护")
	}
	if KongEgressKey(cfg.Egress, cfg.ProxyID) != snap.TicketEgress {
		return fmt.Errorf("票据出口已改变")
	}
	if KongTrafficEgressKey(account.ProxyID) != snap.TrafficEgress {
		return fmt.Errorf("流量出口已从 %s 改为 %s", snap.TrafficEgress, KongTrafficEgressKey(account.ProxyID))
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
func (s *KongTicketService) verifyTicket(ctx context.Context, account *Account, cfg KongTicketConfig, model string, ticketID int64, state, source string, expiresAt time.Time, capture *kongCaptureFacts, taskStartedAt time.Time) (int64, error) {
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
		TicketID: ticketID, ExpiresAt: expiresAt,
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

	var (
		answers []KongFingerprintAnswer
		result  *KongFingerprintResult
	)
	for i, challenge := range KongFingerprintChallenges() {
		// 每次主动请求之前复核：前提已经不成立时继续打上游只是白烧额度。
		if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
			probe := newProbe(i+1, challenge.ID)
			probe.InvalidReason = kongStrPtr(KongProbeInvalidStaleResult)
			probes = append(probes, probe)
			return failBeforeConclusion(KongOutcomeSkipped,
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
			skipCandidate("candidate_not_accepted")
			// 这是一次真实的挑战开销，且说明刚采到的票已经作废。fetch 来源必须退避，否则
			// `min_idle=0` 下相邻两个请求会各取一张新票、各烧一次挑战，全部无效。
			failCooldown("candidate_not_accepted")
			return failBeforeConclusion(KongOutcomeFailure, map[string]any{"reason": "candidate_not_accepted"},
				fmt.Errorf("候选票未被上游接受"))
		}

		answers = append(answers, KongFingerprintAnswer{Text: answer.Text, ExpectedCount: challenge.ExpectedCount})
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
		// 淘汰本段无票期的全部现存候选——两种来源都要：这张票仍是 unverified，不标的话
		// OldestCandidate 下一次照样选中它；而缓存里的其它旧候选同样会被逐张验证，
		// 每次都绕过取票冷却。
		skipDrySpell("no_valid_answer")
		// 只有主动取来的票才进冷却：observed 候选失败应当立即升级为主动取票，把那种失败也算进
		// F 会让升级白等一个冷却期。
		failCooldown("no_valid_answer")
		return failBeforeConclusion(KongOutcomeFailure, map[string]any{"reason": "no_valid_answer"},
			fmt.Errorf("没有可用回答，无法判定"))
	}

	// 落结论之前再复核一次：晚到的结果不能授予服务资格。
	if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
		return failBeforeConclusion(KongOutcomeSkipped,
			map[string]any{"reason": "stale_result", "detail": lost.Error()},
			fmt.Errorf("结论作废，前提已失效: %w", lost))
	}

	// 证据必须先落库再授予资格：反过来的话，探测写失败会留下一张 verified 票（下一个请求照样
	// 用它），而支撑那个结论的原始观测已经永久丢失。
	if insErr := saveProbes(); insErr != nil {
		return finish(0, KongOutcomeFailure,
			map[string]any{"reason": "probe_persist_failed", "detail": insErr.Error()},
			fmt.Errorf("记录探测: %w", insErr))
	}

	// 证据落库可能耗上一段时间，期间前提照样会变（模式被切走、出口被改、票过期）。资格提交
	// 之前必须再复核一次——只在保存之前复核，等于拿一个「保存开始时成立」的前提去提交。
	if lost := s.recheckVerify(ctx, snap, time.Now()); lost != nil {
		return finish(0, KongOutcomeSkipped,
			map[string]any{"reason": "stale_after_probe_persist", "detail": lost.Error()},
			fmt.Errorf("结论作废，前提已失效: %w", lost))
	}

	probs := kongProbsOf(result)
	accepted := s.accept.Accepts(model, probs, s.confidence)
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
		failCooldown("not_target_model")
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
		return finish(0, KongOutcomeSkipped, map[string]any{"reason": "ticket_changed_during_verify"},
			fmt.Errorf("票在验证期间已被改写，结论作废"))
	}
	if !accepted {
		// 候选预算的消费必须在提交**之后**：`SkipCandidatesFor` 会把边界内所有 unverified 候选
		// 标成 skip，其中就包括正在验证的这一张，而提交的条件里有 `skip_until_new = FALSE`
		// ——反过来做必然更新 0 行，真实归因被一条「并发变更」的假原因顶掉，票还留在 unverified。
		//
		// 提交之后这张票已是 rejected，不在 `SkipCandidatesFor` 的 unverified 范围内，只有别的
		// 旧候选会被标掉，正是想要的效果。
		skipDrySpell("not_target_model")
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
		_, _ = s.verifyTicket(detached, account, cfg, model, ticketID, state, KongTicketSourceObserved, expiresAt, nil, probeStartedAt)
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
	grant, err := s.ensureTicket(ctx, accountID, model, true, true)
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
