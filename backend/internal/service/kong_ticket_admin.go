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
	// targetModel / confidence 与编排服务用的是同一对值。
	//
	// 管理面显示的「当前票」必须与业务实际能用的那张是同一张：口径不一致时页面会显示一张业务其实
	// 不会注入的票（目标改过之后留下的旧 verified 票），运维据此判断「有票可用」就错了。
	targetModel string
	confidence  float64
	repo        KongTicketRepository
	accounts    KongAccountAccess
	params      KongTicketParams
	// gatedModels 是门控模型集合。按**最终上游模型**判定，不按客户端传来的别名——
	// 否则换个别名就绕过了整套保护。
	gatedModels []string
}

// NewKongTicketAdminService 创建管理面服务。
func NewKongTicketAdminService(repo KongTicketRepository, accounts KongAccountAccess, params KongTicketParams, gatedModels []string, targetModel string, confidence float64) *KongTicketAdminService {
	return &KongTicketAdminService{
		repo: repo, accounts: accounts, params: params, gatedModels: gatedModels,
		targetModel: targetModel, confidence: confidence,
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

	if cfg.Mode == KongTicketModeOff {
		return status, nil
	}

	// 出口的空闲与下一次可取票时刻只在配了票据出口时有意义。
	if cfg.Egress != KongTicketEgressNone {
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
		ticket, err := s.repo.CurrentTicket(ctx, account.ID, model, s.targetModel, s.confidence)
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
		AccountID: &accountID,
		Model:     model,
		EventType: KongEventVerify,
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
			AccountID: &accountID,
			Model:     model,
			EventType: eventType,
			Limit:     1,
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
