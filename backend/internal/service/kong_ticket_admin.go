package service

import (
	"context"
	"fmt"
	"time"
)

// 票据功能的管理面服务。设计见 DESIGN-codex-ticket.md §6。
//
// 配置、状态与事件放在本 fork 自己的页面里，不进上游的账号编辑表单——那两个表单是 5000 行
// 以上、无 schema 挂载点的手写模板且上游每周改数次，往里插字段等于给每次 rebase 埋冲突。

// KongAccountView 是票据功能需要的账号信息子集。
//
// 刻意只取这几个字段而不是复用上游的账号类型：那个类型很大且会随上游演进，依赖它等于把本
// 功能绑在一个高频改动面上。
type KongAccountView struct {
	ID       int64
	Name     string
	Platform string
	// ProxyID 是**流量出口**。为空即直连。
	ProxyID *int64
	Extra   map[string]any
	// Ready 表示账号此刻可正常调度（未禁用、未过期、未被限流暂停、额度未耗尽）。
	Ready bool
	// NotReadyReason 供界面解释为什么诊断暂停。
	NotReadyReason string
}

// KongAccountAccess 是本功能对账号数据的最小依赖面。
type KongAccountAccess interface {
	// ListTicketCapableAccounts 返回可能参与票据功能的账号（codex 协议的那些）。
	ListTicketCapableAccounts(ctx context.Context) ([]KongAccountView, error)
	GetAccountView(ctx context.Context, accountID int64) (*KongAccountView, error)
	// UpdateAccountExtra 把**补丁**合并进账号的 extra（JSONB 合并，原子）。
	// 调用方只传本功能的键——传整份快照会覆盖并发写入的其它键。
	UpdateAccountExtra(ctx context.Context, accountID int64, extra map[string]any) error
	// GetProxyState 查票据代理此刻的状态。代理不存在时返回 Exists=false 而不是错误——
	// 配置存在 extra 里没有外键，指向已删除代理是预期状态。
	GetProxyState(ctx context.Context, proxyID int64) (KongTicketProxyState, error)
}

// KongTicketSummary 是当前票的摘要，用于界面展示。
type KongTicketSummary struct {
	ID               int64     `json:"id"`
	Model            string    `json:"model"`
	Source           string    `json:"source"`
	FingerprintModel string    `json:"fingerprint_model"`
	FingerprintP     float64   `json:"fingerprint_p"`
	ExpiresAt        time.Time `json:"expires_at"`
	ExpiresAtSource  string    `json:"expires_at_source"`
	RemainingSeconds int64     `json:"remaining_seconds"`
}

// KongTicketModelStatus 是一个账号在**某一个门控模型**上的状态。
//
// 必须按模型拆开：票与归因结论都是 (account, model) 绑定的，把多个模型的当前票混成一个、候选数
// 又跨模型累加，会让「模型 A 有票、B 没票」这种最需要看清的情况完全看不出来。
type KongTicketModelStatus struct {
	Model string `json:"model"`
	// CurrentTicket 是可支持业务的票（verified 且未过期）。为空表示这个模型此刻无票可用。
	CurrentTicket   *KongTicketSummary `json:"current_ticket"`
	UnverifiedCount int                `json:"unverified_count"`
	// Diagnosis 是最近一次指纹归因的结论，不论它来自 observe 的诊断探测还是 full 的验证。
	//
	// 它与 CurrentTicket 是两件事：一张旧的 verified astra 票可以还在服务，而最近一次探测已经
	// 归因为 sol——那正是「该给这个账号开 full 了」的信号。只显示当前票会把这个信号藏起来。
	Diagnosis *KongTicketDiagnosis `json:"diagnosis"`
	// LastSample 是最近一次**取样**的处置，与 Diagnosis 回答的问题不同。
	//
	// 归因结论只在验证真正跑起来时才有。一个账号可能一直在收票、但每张都因长度黑名单（312）
	// 或间隔未满被挡在验证之前——那时 Diagnosis 只会停在上一次的旧结论上，运维看不出「现在
	// 根本没在采样」。两者必须分开表达。
	LastSample *KongTicketSampleNote `json:"last_sample"`
}

// KongTicketSampleNote 是最近一次取样处置的摘要。
type KongTicketSampleNote struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	// Reason 取事件 detail 里的原因，如 state_len_denylisted / observe_interval_not_due。
	Reason string `json:"reason"`
	// StateLen 是那张票的长度。312 一眼就能看出这个账号拿到的是降智档。
	StateLen *int `json:"state_len"`
}

// KongTicketDiagnosis 是一次归因观测的摘要。
type KongTicketDiagnosis struct {
	At time.Time `json:"at"`
	// Outcome 取 success / failure / skipped：失败与未取样也要能显示，不能被旧的成功结论顶替。
	Outcome string `json:"outcome"`
	// FingerprintModel 为空表示这次没有得出归因（拒答、数字不足、候选未被接受等）。
	FingerprintModel string  `json:"fingerprint_model"`
	Probability      float64 `json:"probability"`
	TicketSource     string  `json:"ticket_source"`
	// Reason 解释「为什么没有结论」——无样本、账号不可调度、间隔未满都各自可辨。
	Reason string `json:"reason"`
	// VerificationID 用于展开探测明细。
	VerificationID string `json:"verification_id"`
	// Stale 表示这个结论已经比一张票的寿命还老，只反映历史、不代表账号此刻的档位。
	//
	// 它是**查询那一刻**的快照。页面长期驻留时靠 StaleAfterSeconds 自己推进——只看这个布尔值的话，
	// 一个加载时差 1 秒才过期的结论会永远显示成「当前」。
	Stale bool `json:"stale"`
	// StaleAfterSeconds 是从响应时刻起、这个结论还能算「当前」多少秒。已经过期时为 0。
	//
	// 给的是剩余秒数而不是绝对时刻：浏览器时钟与服务器可以差好几分钟，拿绝对时刻去跟
	// `Date.now()` 比，快五分钟的浏览器会把有效结论显示成过期，慢的则会延长它的寿命。
	StaleAfterSeconds int64 `json:"stale_after_seconds"`
}

// KongTicketAccountStatus 是一个账号在票据功能下的完整状态。
type KongTicketAccountStatus struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Ready     bool   `json:"ready"`
	NotReady  string `json:"not_ready"`

	Mode    KongTicketMode   `json:"mode"`
	Egress  KongTicketEgress `json:"egress"`
	ProxyID *int64           `json:"proxy_id"`
	// ConfigRejected 列出被拒绝的配置键。静默纠正会让「为什么这个账号不取票」无从排查。
	ConfigRejected []string `json:"config_rejected"`

	TrafficEgress string `json:"traffic_egress"`
	TicketEgress  string `json:"ticket_egress"`
	EgressUsable  bool   `json:"egress_usable"`
	EgressReason  string `json:"egress_reason"`

	// Models 按门控模型逐个给出状态。空集合表示功能未启用（门控模型集合为空）。
	Models []KongTicketModelStatus `json:"models"`

	// EgressIdleSeconds 是距**本系统**最后一次使用该票据出口的秒数，不是实际静默——系统外的
	// 活动观测不到，这个值会高估真实静默，界面上不能显示成「现在一定能取到好票」。
	// 为空表示本系统从未用过该出口。
	EgressIdleSeconds *int64 `json:"egress_idle_seconds"`
	// NextFetchAllowedAt 是下一次主动取票最早被允许的时刻，即 max(A+M, F+C)。
	NextFetchAllowedAt *time.Time `json:"next_fetch_allowed_at"`
}

// KongTicketAdminService 支撑管理页面的三个操作。
type KongTicketAdminService struct {
	// accept / confidence 与编排服务用的是同一对值。
	//
	// 管理面显示的「当前票」必须与业务实际能用的那张是同一张：口径不一致时页面会显示一张业务其实
	// 不会注入的票（目标改过之后留下的旧 verified 票），运维据此判断「有票可用」就错了。
	accept     KongTicketAccept
	confidence float64
	// svc 只用于手工触发。装配时注入而不进构造函数：未启用时 Admin 照常装配而 Service 为 nil，
	// 放进参数表会让"未启用"这条路径也得传个 nil 进来。
	svc      *KongTicketService
	repo     KongTicketRepository
	accounts KongAccountAccess
	params   KongTicketParams
	// gatedModels 是门控模型集合。按**最终上游模型**判定，不按客户端传来的别名——
	// 否则换个别名就绕过了整套保护。
	gatedModels []string
}

// NewKongTicketAdminService 创建管理面服务。
func NewKongTicketAdminService(repo KongTicketRepository, accounts KongAccountAccess, params KongTicketParams, gatedModels []string, accept KongTicketAccept, confidence float64) *KongTicketAdminService {
	return &KongTicketAdminService{
		repo: repo, accounts: accounts, params: params, gatedModels: gatedModels,
		accept: accept, confidence: confidence,
	}
}

// GatedModels 返回当前生效的门控模型集合。界面要能看到它——判断「这个请求到底受不受保护」
// 是运维的日常动作，靠读代码是不行的。
func (s *KongTicketAdminService) GatedModels() []string {
	out := make([]string, len(s.gatedModels))
	copy(out, s.gatedModels)
	return out
}

// Params 返回当前生效的时间参数。
func (s *KongTicketAdminService) Params() KongTicketParams { return s.params }

// Overview 汇总所有账号的票据状态。
func (s *KongTicketAdminService) Overview(ctx context.Context, now time.Time) ([]KongTicketAccountStatus, error) {
	accounts, err := s.accounts.ListTicketCapableAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出账号: %w", err)
	}
	out := make([]KongTicketAccountStatus, 0, len(accounts))
	for i := range accounts {
		status, err := s.statusOf(ctx, &accounts[i], now)
		if err != nil {
			return nil, err
		}
		out = append(out, *status)
	}
	return out, nil
}

func (s *KongTicketAdminService) statusOf(ctx context.Context, account *KongAccountView, now time.Time) (*KongTicketAccountStatus, error) {
	cfg, rejected := ParseKongTicketConfig(account.Extra)
	status := &KongTicketAccountStatus{
		AccountID:      account.ID,
		Name:           account.Name,
		Platform:       account.Platform,
		Ready:          account.Ready,
		NotReady:       account.NotReadyReason,
		Mode:           cfg.Mode,
		Egress:         cfg.Egress,
		ProxyID:        cfg.ProxyID,
		ConfigRejected: rejected,
		TrafficEgress:  KongTrafficEgressKey(account.ProxyID),
		TicketEgress:   KongEgressKey(cfg.Egress, cfg.ProxyID),
	}

	proxyState := KongTicketProxyState{}
	if cfg.Egress == KongTicketEgressProxy && cfg.ProxyID != nil {
		st, err := s.accounts.GetProxyState(ctx, *cfg.ProxyID)
		if err != nil {
			return nil, fmt.Errorf("查票据代理 %d: %w", *cfg.ProxyID, err)
		}
		proxyState = st
	}
	status.EgressUsable, status.EgressReason = KongEvaluateEgress(cfg, account.ProxyID, proxyState, now)

	// 出口的空闲与下一次可取票时刻只在**会取票**且配了票据出口时才有意义：off 从不取票，报一个
	// 「下次可取票时刻」等于陈述一件不会发生的事。
	//
	// 但下面的逐模型信息 off 照样输出——off 与 observe 的差别只是不主动探测/取票，票仍然被动
	// 收下（§2.3），候选数、取样长度、历史诊断都已经在库里，不显示只是把已有的信息藏起来。
	if cfg.Mode != KongTicketModeOff && cfg.Egress != KongTicketEgressNone {
		lastUsed, err := s.repo.LastEgressActivity(ctx, status.TicketEgress)
		if err != nil {
			return nil, fmt.Errorf("查出口活动: %w", err)
		}
		if lastUsed != nil {
			idle := int64(now.Sub(*lastUsed).Seconds())
			if idle < 0 {
				idle = 0
			}
			status.EgressIdleSeconds = &idle
		}
		lastCooldown, err := s.repo.LastFailure(ctx, account.ID, status.TicketEgress)
		if err != nil {
			return nil, fmt.Errorf("查冷却: %w", err)
		}
		if allowed := KongNextFetchAllowedAt(lastUsed, lastCooldown, s.params); !allowed.IsZero() && allowed.After(now) {
			status.NextFetchAllowedAt = &allowed
		}
	}

	// 逐个门控模型输出：票与结论是 (account, model) 绑定的，混成一个就看不出哪个模型没票。
	for _, model := range s.gatedModels {
		modelStatus := KongTicketModelStatus{Model: model}
		ticket, err := kongPickCurrent(ctx, s.repo, s.accept, s.confidence, account.ID, model)
		if err != nil {
			return nil, fmt.Errorf("查当前票: %w", err)
		}
		if ticket != nil {
			modelStatus.CurrentTicket = kongSummarizeTicket(ticket, now)
		}
		n, err := s.repo.CountTickets(ctx, account.ID, model, KongTicketStatusUnverified)
		if err != nil {
			return nil, fmt.Errorf("统计候选: %w", err)
		}
		modelStatus.UnverifiedCount = n

		diagnosis, err := s.latestDiagnosis(ctx, account.ID, model)
		if err != nil {
			return nil, err
		}
		modelStatus.Diagnosis = diagnosis
		sample, err := s.lastSample(ctx, account.ID, model)
		if err != nil {
			return nil, err
		}
		modelStatus.LastSample = sample
		status.Models = append(status.Models, modelStatus)
	}
	return status, nil
}

// latestDiagnosis 取最近一次归因观测的摘要。
//
// 读的是事件而不是票：一次验证可能以「没有可用回答」「候选未被接受」收尾，那些终点不会在任何票
// 上留下归因，但它们恰恰是运维要看的——「这个账号最近一直探不出结论」和「探出来是 sol」都要能看见。
func (s *KongTicketAdminService) latestDiagnosis(ctx context.Context, accountID int64, model string) (*KongTicketDiagnosis, error) {
	events, _, err := s.repo.ListEvents(ctx, &KongTicketEventFilter{
		AccountIDs: []int64{accountID},
		Models:     []string{model},
		EventTypes: []string{KongEventVerify},
		// 只要最终事件。不筛的话，一条「第 2 份挑战失败」这种非最终事件会排在最前面，
		// 把本次验证真正的结论盖掉——页面于是显示「未得出结论」，而库里明明有。
		FinalOnly: true,
		Limit:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("查诊断事件: %w", err)
	}
	if len(events) > 0 {
		event := events[0]
		age := time.Since(event.CreatedAt)
		diagnosis := &KongTicketDiagnosis{
			At:      event.CreatedAt,
			Outcome: event.Outcome,
			Stale:   age > kongTicketTTL,
		}
		if remaining := kongTicketTTL - age; remaining > 0 {
			diagnosis.StaleAfterSeconds = int64(remaining.Seconds())
		}
		if event.FingerprintModel != nil {
			diagnosis.FingerprintModel = *event.FingerprintModel
		}
		if p, ok := event.Detail["probability"].(float64); ok {
			diagnosis.Probability = p
		}
		if reason, ok := event.Detail["reason"].(string); ok {
			diagnosis.Reason = reason
		}
		if id, ok := event.Detail["verification_id"].(string); ok {
			diagnosis.VerificationID = id
		}
		if src, ok := event.Detail["ticket_source"].(string); ok {
			diagnosis.TicketSource = src
		}
		return diagnosis, nil
	}
	return nil, nil
}

// lastSample 取最近一次取样处置（observe 事件）。
//
// 它回答的是「现在还在采样吗、采到的是什么」：312 被长度黑名单挡掉、间隔未满跳过、去重命中，
// 这些都只写 observe 事件，一条也不会进 verify 流。只读 verify 的话，一个持续只能拿到降智票的
// 账号会显示成「最近一次结论是 astra」——那个结论可能是好几天前的。
func (s *KongTicketAdminService) lastSample(ctx context.Context, accountID int64, model string) (*KongTicketSampleNote, error) {
	// 两类事件都要看，取较新的那条。取样的处置分散在两处：收票本身与长度黑名单、去重写 observe，
	// 而「间隔未满」「账号不可调度」写的是 probe_skipped。只查一种就会漏掉一半原因，页面显示成
	// 空原因——那恰恰是「为什么这个账号一直没有新结论」最常见的答案。
	var latest *KongTicketEvent
	for _, eventType := range []string{KongEventObserve, KongEventProbeSkipped} {
		events, _, err := s.repo.ListEvents(ctx, &KongTicketEventFilter{
			AccountIDs: []int64{accountID},
			Models:     []string{model},
			EventTypes: []string{eventType},
			Limit:      1,
		})
		if err != nil {
			return nil, fmt.Errorf("查取样事件: %w", err)
		}
		for _, event := range events {
			if latest == nil || event.CreatedAt.After(latest.CreatedAt) {
				latest = event
			}
		}
	}
	if latest == nil {
		return nil, nil
	}
	note := &KongTicketSampleNote{
		At:       latest.CreatedAt,
		Outcome:  latest.Outcome,
		StateLen: latest.StateLen,
	}
	if reason, ok := latest.Detail["reason"].(string); ok {
		note.Reason = reason
	}
	return note, nil
}

func kongSummarizeTicket(t *KongTicket, now time.Time) *KongTicketSummary {
	summary := &KongTicketSummary{
		ID:              t.ID,
		Model:           t.Model,
		Source:          t.Source,
		ExpiresAt:       t.ExpiresAt,
		ExpiresAtSource: t.ExpiresAtSource,
	}
	if t.FingerprintModel != nil {
		summary.FingerprintModel = *t.FingerprintModel
	}
	if t.FingerprintP != nil {
		summary.FingerprintP = *t.FingerprintP
	}
	if remaining := int64(t.ExpiresAt.Sub(now).Seconds()); remaining > 0 {
		summary.RemainingSeconds = remaining
	}
	return summary
}

// Enabled 报告本功能是否生效（门控模型集合非空）。
//
// 界面靠它区分「未启用」与「服务故障」，也靠它决定配置是否可写。
func (s *KongTicketAdminService) Enabled() bool { return len(s.gatedModels) > 0 }

// SetTicketService 注入编排服务，供手工触发使用。未启用时不注入。
func (s *KongTicketAdminService) SetTicketService(svc *KongTicketService) { s.svc = svc }

// TriggerRefresh 手工触发一次取票/验票，并把该行的最新状态一起返回。
//
// 同步执行：整条路径最长受任务预算约束，而调用方是管理页上的一次点击——拿不到结果的异步触发
// 等于让人对着页面猜。
func (s *KongTicketAdminService) TriggerRefresh(ctx context.Context, accountID int64, model string) (*KongManualRefresh, *KongTicketAccountStatus, error) {
	if s.svc == nil {
		return nil, nil, fmt.Errorf("票据功能未启用，无法手工触发")
	}
	if !s.isGated(model) {
		return nil, nil, fmt.Errorf("模型 %q 不在门控集合里", model)
	}
	result, err := s.svc.TriggerRefresh(ctx, accountID, model)
	if err != nil {
		return nil, nil, err
	}
	// 触发之后重新读这一行：当前票、出口可用性、下次可取时刻都可能变了。让页面一次拿到新状态，
	// 而不是靠调用方再发一次 overview——那会把其它行未保存的草稿一起冲掉。
	account, err := s.accounts.GetAccountView(ctx, accountID)
	if err != nil || account == nil {
		return result, nil, nil
	}
	status, err := s.statusOf(ctx, account, time.Now())
	if err != nil {
		return result, nil, nil
	}
	return result, status, nil
}

// TriggerVerify 验现有的票（当前票 + 真降档后的一张最新候选），永不取票。触发后回一份新状态。
func (s *KongTicketAdminService) TriggerVerify(ctx context.Context, accountID int64, model string) (*KongManualVerify, *KongTicketAccountStatus, error) {
	if s.svc == nil {
		return nil, nil, fmt.Errorf("票据功能未启用，无法手工验票")
	}
	if !s.isGated(model) {
		return nil, nil, fmt.Errorf("模型 %q 不在门控集合里", model)
	}
	result, err := s.svc.TriggerVerify(ctx, accountID, model)
	if err != nil {
		return nil, nil, err
	}
	account, err := s.accounts.GetAccountView(ctx, accountID)
	if err != nil || account == nil {
		return result, nil, nil
	}
	status, err := s.statusOf(ctx, account, time.Now())
	if err != nil {
		return result, nil, nil
	}
	return result, status, nil
}

// TriggerVerifyTicket 验指名的那一张票（详情页逐行触发）。触发后回一份新的详情页，页面不必自己
// 猜哪些行变了——尤其"当前票换成了哪张"必须由服务端重算。
func (s *KongTicketAdminService) TriggerVerifyTicket(ctx context.Context, accountID, ticketID int64) (*KongManualVerify, *KongTicketDetailPage, error) {
	if s.svc == nil {
		return nil, nil, fmt.Errorf("票据功能未启用，无法手工验票")
	}
	result, err := s.svc.TriggerVerifyTicket(ctx, accountID, ticketID)
	if err != nil {
		return nil, nil, err
	}
	// 模型不在门控集合时不拦：这一张票是库里真实存在的行，而门控集合是可以改的——改窄之后仍然
	// 应该能把遗留的票验掉或看清它，那正是详情页的用处。
	page, err := s.TicketDetail(ctx, accountID)
	if err != nil {
		return result, nil, nil
	}
	return result, page, nil
}

// isGated 判断模型是否在门控集合里。
func (s *KongTicketAdminService) isGated(model string) bool {
	for _, m := range s.gatedModels {
		if m == model {
			return true
		}
	}
	return false
}

// UpdateConfig 改一个账号的票据配置。
func (s *KongTicketAdminService) UpdateConfig(ctx context.Context, accountID int64, cfg KongTicketConfig) (*KongTicketAccountStatus, error) {
	if !s.Enabled() {
		// 未启用时管理面是只读的：此时写配置不会有任何效果，但会留下一份「看起来配好了」的
		// 状态，等真启用时行为突变。
		return nil, fmt.Errorf("codex 票据功能未启用（门控模型集合为空），配置不可写")
	}
	account, err := s.accounts.GetAccountView(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %d 不存在", accountID)
	}
	if err := KongValidateTicketConfig(cfg, account.ProxyID); err != nil {
		return nil, err
	}
	// 只把本功能的三个键写下去。带上整份 extra 快照会覆盖读取之后别处并发写入的键。
	if err := s.accounts.UpdateAccountExtra(ctx, accountID, KongTicketConfigPatch(cfg)); err != nil {
		return nil, fmt.Errorf("保存配置: %w", err)
	}
	// 本地快照只用于把保存后的状态回给界面，不再写库。
	account.Extra = KongApplyTicketConfig(account.Extra, cfg)

	_ = s.repo.InsertEvent(ctx, &KongTicketEvent{
		AccountID:     accountID,
		EventType:     KongEventEgressInvalid,
		Outcome:       KongOutcomeInfo,
		TrafficEgress: KongTrafficEgressKey(account.ProxyID),
		TicketEgress:  KongEgressKey(cfg.Egress, cfg.ProxyID),
		Detail: map[string]any{
			"action": "config_updated",
			"mode":   string(cfg.Mode),
			"egress": string(cfg.Egress),
		},
	})
	return s.statusOf(ctx, account, time.Now())
}

// ListEvents 分页查事件。
func (s *KongTicketAdminService) ListEvents(ctx context.Context, filter *KongTicketEventFilter) ([]*KongTicketEvent, int, error) {
	return s.repo.ListEvents(ctx, filter)
}

// ListProbes 取一次验证的探测明细。原始数字序列不进界面——几百个数字在页面上看不出任何东西，
// 它的用途是离线重算。
func (s *KongTicketAdminService) ListProbes(ctx context.Context, verificationID string) ([]*KongFingerprintProbe, error) {
	return s.repo.ListProbesByVerification(ctx, verificationID)
}

// kongTicketDetailLimit 是详情页一次列多少张票。仓储层另有一个同值的硬上限，两者分工不同：
// 这个是"页面要多少"，那个是"库层最多允许多少"。Truncated 按这个值判。
const kongTicketDetailLimit = 200

// KongTicketDetail 是票据详情页上的一行。**不含票原值**——那是可注入的凭据，管理响应只放派生
// 信息（长度、归因、时间）。
type KongTicketDetail struct {
	ID       int64  `json:"id"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	Source   string `json:"source"`
	StateLen int    `json:"state_len"`
	// SkipUntilNew 为真表示这张票已被排除出候选池（注入未被上游接受，或本段无票期机会用完）。
	SkipUntilNew     bool      `json:"skip_until_new"`
	CapturedAt       time.Time `json:"captured_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	ExpiresAtSource  string    `json:"expires_at_source"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	FingerprintModel string    `json:"fingerprint_model"`
	FingerprintP     float64   `json:"fingerprint_p"`
	// FingerprintProbs 是各模型的归因概率。页面据它解释"为什么这张判不合格"。
	FingerprintProbs map[string]float64 `json:"fingerprint_probs"`
	// IsCurrent 为真表示**此刻业务注入的就是它**。两个条件：按当前接受白名单与阈值它是该模型的
	// 首选票，**且这个账号真的会注入**（`mode=full`）。
	//
	// 后一半不能省：`off` / `observe` 都不注入，而 statusOf 刻意保留了它们的存量票信息（那是
	// "该不该开 full"的依据）。只按首选票标"在用"，页面就会在不注入的模式下宣称有票在服务。
	IsCurrent bool `json:"is_current"`
	// Preferred 为真表示按当前判据它是该模型的首选票——与 IsCurrent 的差别只在模式。`off` /
	// `observe` 下看的就是这个：存量票里哪张最合格，而它并没有在被注入。
	Preferred bool `json:"preferred"`
	// Verifiable 为真表示这张票现在可以手工验。四个条件缺一不可：功能已启用、模式非 off
	// （那下面 recheckVerify 必然以「已退出保护」中止，问不出答案）、账号可调度、票未过期。
	//
	// **不含"票据出口可用"**：验证走流量出口。已拒的与被跳过的都可以验——服务端会先按 id 准备
	// （复位 + 清跳过标记），否则结论必然提交不上。
	Verifiable bool `json:"verifiable"`
	// NotVerifiableReason 说明为什么不能验。空字符串表示可以验。不给原因的话页面只能猜，而
	// "已过期"与"该模式不验票"是完全不同的两件事。
	NotVerifiableReason string `json:"not_verifiable_reason"`
}

// KongTicketDetailPage 是某个账号的票据详情。
type KongTicketDetailPage struct {
	Account *KongTicketAccountStatus `json:"account"`
	// Tickets 含已过期与已拒的：详情页要能回答"为什么现在没票可用"，只列可用的等于把答案藏起来。
	Tickets []*KongTicketDetail `json:"tickets"`
	// Truncated 为真表示还有更早的票没列出来（撞到行数上限）。
	Truncated bool `json:"truncated"`
}

// TicketDetail 列一个账号名下的票。
func (s *KongTicketAdminService) TicketDetail(ctx context.Context, accountID int64) (*KongTicketDetailPage, error) {
	account, err := s.accounts.GetAccountView(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d: %w", accountID, err)
	}
	if account == nil {
		// 账号不存在（或不是 codex 协议）。返回 nil 让处理层报 404，不编一个空页面出来。
		return nil, nil
	}
	status, err := s.statusOf(ctx, account, time.Now())
	if err != nil {
		return nil, err
	}
	cfg, _ := ParseKongTicketConfig(account.Extra)
	rows, err := s.repo.ListTickets(ctx, accountID, kongTicketDetailLimit)
	if err != nil {
		return nil, err
	}
	// 逐模型算一次"此刻在用的是哪张"，与业务注入用的是同一个判据（同一对 accept/confidence）。
	current := map[string]int64{}
	for _, m := range status.Models {
		if m.CurrentTicket != nil {
			current[m.Model] = m.CurrentTicket.ID
		}
	}
	// 账号级的"能不能验"只算一次：它与具体哪张票无关。顺序是刻意的——先报最根本的原因，
	// 否则页面会把"功能没开"显示成"账号不可调度"。
	accountReason := ""
	switch {
	case len(s.gatedModels) == 0 || s.svc == nil:
		accountReason = "票据功能未启用"
	case cfg.Mode == KongTicketModeOff:
		accountReason = "该账号已退出保护（off），验证流程必然以「已退出保护」中止"
	case !status.Ready:
		accountReason = "账号当前不可调度：" + status.NotReady
	}

	out := &KongTicketDetailPage{Account: status, Truncated: len(rows) >= kongTicketDetailLimit}
	for _, t := range rows {
		remaining := int64(time.Until(t.ExpiresAt).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		reason := accountReason
		if reason == "" && remaining <= 0 {
			reason = "票已过期"
		}
		preferred := current[t.Model] == t.ID
		d := &KongTicketDetail{
			ID: t.ID, Model: t.Model, Status: t.Status, Source: t.Source,
			StateLen: len(t.State), SkipUntilNew: t.SkipUntilNew,
			CapturedAt: t.CapturedAt, ExpiresAt: t.ExpiresAt,
			ExpiresAtSource: t.ExpiresAtSource, RemainingSeconds: remaining,
			FingerprintProbs: t.FingerprintProbs,
			Preferred:        preferred,
			// 只有 full 才在注入。见 IsCurrent 的说明。
			IsCurrent:           preferred && cfg.Mode == KongTicketModeFull,
			Verifiable:          reason == "",
			NotVerifiableReason: reason,
		}
		if t.FingerprintModel != nil {
			d.FingerprintModel = *t.FingerprintModel
		}
		if t.FingerprintP != nil {
			d.FingerprintP = *t.FingerprintP
		}
		out.Tickets = append(out.Tickets, d)
	}
	return out, nil
}
