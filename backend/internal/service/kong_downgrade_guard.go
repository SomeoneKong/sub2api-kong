package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 降档哨兵：持续盯住服务分组里的 OpenAI OAuth 账号，上游不认它自己刚签发的票（静默降到 luna 档的特征）时关掉它的调度，
// 再用指纹测试确认，并记下受影响的时段与会话。只做止损：确认之后保持关闭，怎么恢复由人处理。设计见
// DESIGN-account-downgrade-guard.md。
//
// 并发结构：所有状态判断与写库都在主循环这一个协程里串行完成；探测与指纹测试是慢的上游 I/O，放在工作协程里，
// 只产出结论、交回主循环。

const (
	kongGuardTick           = 30 * time.Second
	kongGuardRescan         = 5 * time.Minute
	kongGuardIssuedKeep     = 60 * time.Minute
	kongGuardScanLimit      = 5000
	kongGuardStartBack      = 10 * time.Minute
	kongGuardTrafficWindow  = 30 * time.Minute
	kongGuardCapacityWindow = 10 * time.Minute
	kongGuardConcurrency    = 2
	kongGuardProbeGap       = 3 * time.Second
	kongGuardProbeBudget    = 70 * time.Second
	kongGuardFPBudget       = 15 * time.Minute
	kongGuardRetest         = 10 * time.Minute
	kongGuardCooldown       = 24 * time.Hour
	kongGuardDetectEvery    = 2 * time.Hour
	kongGuardDetectMin      = 30 * time.Minute
	kongGuardPassiveGap     = 5 * time.Minute
	kongGuardObserveEvery   = 6 * time.Hour
	kongGuardImpactFirst    = 30 * time.Minute
	kongGuardImpactEvery    = time.Hour
	kongGuardImpactCutoff   = 24 * time.Hour
	kongGuardImpactSettle   = 30 * time.Minute
	// kongGuardImpactLag 放宽影响汇总里"开始早于关闭"的上界：用量行的 created_at 是异步记账的时刻，比请求结束略晚，
	// 推算出的开始时刻随之偏晚；关闭之后调度快照同步之前，请求也还可能落到这个账号上。
	kongGuardImpactLag       = 5 * time.Minute
	kongGuardUnknownLookback = 24 * time.Hour
	kongGuardRetention       = 90 * 24 * time.Hour
	kongGuardMaintainEvery   = time.Hour
	kongGuardDBTimeout       = 15 * time.Second
)

var ErrKongGuardBusy = errors.New("该账号正在探测或测指纹")

// 主动探测的结论。
const (
	kongGuardProbeAccept       = "accept"
	kongGuardProbeRefuse       = "refuse"
	kongGuardProbeInconclusive = "inconclusive"
)

// 指纹结论。
const (
	kongGuardFPLuna         = "luna"
	kongGuardFPNormal       = "normal"
	kongGuardFPInconclusive = "inconclusive"
	kongGuardFPNotStarted   = "not_started"
)

// kongGuardFingerprinter 取得一个账号的指纹测试执行权（有人正在测时返回 ErrKongFingerprintBusy）。
type kongGuardFingerprinter interface {
	Begin(ctx context.Context, accountID int64, model string) (kongGuardFPRun, error)
}

// kongGuardFPRun 是取得了执行权的一次测试：Run 执行并返回整次测试的结论，Abort 不执行、放弃执行权。
type kongGuardFPRun interface {
	Run(ctx context.Context) (*KongFingerprintTestResult, string, error)
	Abort()
}

// kongGuardIssued 是本网关见到的一张上游签发的票。
type kongGuardIssued struct {
	accountID int64
	model     string
	at        time.Time
}

// kongGuardAcct 是主循环里一个账号的内存状态。
type kongGuardAcct struct {
	lastGood    time.Time // 最后一次正常证据（被动接受、主动探测接受）
	lastVerdict time.Time // 最后一次被动或主动结论
	lastTraffic time.Time // 最后一次有用量
	lastOutput  time.Time // 最后一次在服务分组里对探测模型有输出
	lastProbe   time.Time
	cooldown    time.Time // 误报冷却截止
	fpStart     map[string]time.Time
	knownOp     string // 上一轮看到的持有记录操作号，用来发现人工解除
}

// kongGuardJob 是交给工作协程的一次探测或指纹测试。
type kongGuardJob struct {
	accountID int64
	rev       int64  // 任务开始时的调度开关版本
	op        string // 确认任务的持有记录操作号
	fp        bool   // 指纹测试；否则是主动探测
	purpose   string
	trigger   string        // 触发原因：passive / patrol / manual / retest
	every     time.Duration // 检测任务的限频间隔，没能启动、重新派发时复核
}

// KongGuardProbeOutcome 是一次主动探测的结论：accept / refuse / inconclusive。
type KongGuardProbeOutcome struct {
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
}

type kongGuardResult struct {
	job       kongGuardJob
	startedAt time.Time
	probe     KongGuardProbeOutcome
	fpVerdict string
	fpTestID  string
	fpDetail  map[string]any
}

// kongGuardPending 是写库失败、等下一轮重新提交的关闭结论。
type kongGuardPending struct {
	rev      int64
	stage    string
	evidence string
}

// KongDowngradeGuard 是降档哨兵。
type KongDowngradeGuard struct {
	cfg      KongDowngradeGuardConfig
	store    KongDowngradeGuardStore
	accounts KongGuardAccountSource
	prober   KongTicketProber
	fper     kongGuardFingerprinter
	sync     func(accountID int64)
	now      func() time.Time
	newOp    func() string
	probeGap time.Duration

	// 以下只在主循环里读写。
	issued      map[string]kongGuardIssued
	seen        map[int64]time.Time
	cursor      time.Time
	accts       map[int64]*kongGuardAcct
	snapshot    map[int64]*Account
	members     map[int64]bool
	impactRun   map[string]time.Time
	pending     map[int64]kongGuardPending
	detects     map[int64]kongGuardJob // 没能启动的检测任务，下一轮重新派发
	lastMaint   time.Time
	floor       time.Time // 启动时的判定下界：更早的行只用来重建票表
	initialized bool
	restored    bool
	issuedReady bool // 启动时的票表重建已完成

	busyMu sync.Mutex
	busy   map[int64]bool

	results chan kongGuardResult
	jobs    chan kongGuardJob
	stop    chan struct{}
	ctx     context.Context // 停止时取消在途的探测与指纹测试
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	once    sync.Once
}

// NewKongDowngradeGuard 创建哨兵。tester 为空（指纹库加载失败）时不能确认，返回错误，调用方不启动它。
func NewKongDowngradeGuard(cfg KongDowngradeGuardConfig, store KongDowngradeGuardStore, accounts KongGuardAccountSource,
	prober KongTicketProber, tester *KongFingerprintTester, syncSnapshot func(accountID int64)) (*KongDowngradeGuard, error) {
	if tester == nil {
		return nil, errors.New("指纹测试不可用，哨兵无法确认")
	}
	if !tester.bank.HasModel(cfg.ProbeModel) {
		return nil, fmt.Errorf("探测模型 %s 不在指纹库里", cfg.ProbeModel)
	}
	return newKongDowngradeGuard(cfg, store, accounts, prober, kongGuardTesterRunner{tester}, syncSnapshot), nil
}

func newKongDowngradeGuard(cfg KongDowngradeGuardConfig, store KongDowngradeGuardStore, accounts KongGuardAccountSource,
	prober KongTicketProber, fper kongGuardFingerprinter, syncSnapshot func(accountID int64)) *KongDowngradeGuard {
	if syncSnapshot == nil {
		syncSnapshot = func(int64) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &KongDowngradeGuard{
		ctx: ctx, cancel: cancel,
		cfg: cfg, store: store, accounts: accounts, prober: prober, fper: fper, sync: syncSnapshot,
		now: time.Now, newOp: func() string { return uuid.NewString() }, probeGap: kongGuardProbeGap,
		issued: map[string]kongGuardIssued{}, seen: map[int64]time.Time{}, accts: map[int64]*kongGuardAcct{},
		snapshot: map[int64]*Account{}, members: map[int64]bool{}, impactRun: map[string]time.Time{}, pending: map[int64]kongGuardPending{}, detects: map[int64]kongGuardJob{},
		busy:    map[int64]bool{},
		results: make(chan kongGuardResult, 64), jobs: make(chan kongGuardJob, 64),
		stop: make(chan struct{}),
	}
}

// Config 返回生效的配置。
func (g *KongDowngradeGuard) Config() KongDowngradeGuardConfig { return g.cfg }

// Start 启动主循环与工作协程。模式为 off 时不启动。
func (g *KongDowngradeGuard) Start() {
	if g == nil || g.cfg.Mode == KongGuardModeOff {
		return
	}
	for i := 0; i < kongGuardConcurrency; i++ {
		g.wg.Add(1)
		go g.worker()
	}
	g.wg.Add(1)
	go g.loop()
	slog.Info("kong downgrade guard: 已启动", "mode", g.cfg.Mode, "group_id", g.cfg.GroupID, "probe_model", g.cfg.ProbeModel,
		"fresh_seconds", int(g.cfg.FreshAge.Seconds()), "ticket_hold_max", g.cfg.TicketHoldMax)
}

// Stop 停止并等待协程退出。
func (g *KongDowngradeGuard) Stop() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.cancel()
		close(g.stop)
	})
	g.wg.Wait()
}

func (g *KongDowngradeGuard) loop() {
	defer g.wg.Done()
	ticker := time.NewTicker(kongGuardTick)
	defer ticker.Stop()
	g.tick()
	for {
		select {
		case <-g.stop:
			return
		case res := <-g.results:
			g.handleResult(res)
		case <-ticker.C:
			g.tick()
		}
	}
}

func (g *KongDowngradeGuard) dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), kongGuardDBTimeout)
}

func (g *KongDowngradeGuard) acct(id int64) *kongGuardAcct {
	a := g.accts[id]
	if a == nil {
		a = &kongGuardAcct{fpStart: map[string]time.Time{}}
		g.accts[id] = a
	}
	return a
}

// restore 从事件表恢复误报冷却与指纹限频。失败返回 false，主循环下一轮再试。
func (g *KongDowngradeGuard) restore() bool {
	ctx, cancel := g.dbCtx()
	defer cancel()
	now := g.now()
	events, err := g.store.RecentEvents(ctx, []string{KongGuardEventFalseAlarm, KongGuardEventFPStart}, now.Add(-kongGuardCooldown))
	if err != nil {
		slog.Warn("kong downgrade guard: 恢复冷却与限频失败，下一轮再试", "error", err)
		return false
	}
	for _, ev := range events {
		a := g.acct(ev.AccountID)
		switch ev.Kind {
		case KongGuardEventFalseAlarm:
			if t := kongGuardParseTime(ev.Detail["cooldown_until"]); t.After(a.cooldown) {
				a.cooldown = t
			}
			if ev.Op != "" && a.knownOp == ev.Op {
				a.knownOp = "" // 这次撤销其实提交了（当时写库返回了错误）
			}
		case KongGuardEventFPStart:
			purpose, _ := ev.Detail["purpose"].(string)
			if ev.CreatedAt.After(a.fpStart[purpose]) {
				a.fpStart[purpose] = ev.CreatedAt
			}
		}
	}
	return true
}

// tick 是一轮：刷新账号快照、处理到期接管与人工解除、扫描用量、重新提交写库失败的关闭、安排探测与重测、补做影响汇总。
// 冷却与限频恢复成功之前什么都不做：它们决定能不能关、能不能测。关闭或误报撤销写库返回错误之后也要重新恢复一次：
// 提交了而响应丢失时，库里已经有新的持有记录或冷却，内存里还没有。
func (g *KongDowngradeGuard) tick() {
	now := g.now()
	if !g.restored {
		if g.restored = g.restore(); !g.restored {
			return
		}
	}
	if !g.refreshSnapshot() {
		return
	}
	if g.cfg.Mode == KongGuardModeOn {
		g.takeOverExpired()
	}
	g.noteReleases()
	refusals := g.scan(now)
	for id, row := range refusals {
		g.onPassiveRefuse(id, row)
	}
	g.retryPending(now)
	g.retryDetects()
	g.patrol(now)
	g.scheduleRetests(now)
	g.runImpacts(now)
	g.maintain(now)
}

// refreshSnapshot 读一次账号与服务分组成员。失败时这一轮什么都不做。
func (g *KongDowngradeGuard) refreshSnapshot() bool {
	ctx, cancel := g.dbCtx()
	defer cancel()
	list, err := g.accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		slog.Warn("kong downgrade guard: 读账号失败", "error", err)
		return false
	}
	snapshot := make(map[int64]*Account, len(list))
	for i := range list {
		a := &list[i]
		if a.IsOpenAIOAuthLike() && !a.IsShadow() && !a.IsOpenAIAgentIdentity() {
			snapshot[a.ID] = a
		}
	}
	members := map[int64]bool{}
	if g.cfg.GroupID > 0 {
		inGroup, err := g.accounts.ListByGroup(ctx, g.cfg.GroupID)
		if err != nil {
			slog.Warn("kong downgrade guard: 读服务分组失败", "group_id", g.cfg.GroupID, "error", err)
			return false
		}
		for i := range inGroup {
			members[inGroup[i].ID] = true
		}
	}
	g.snapshot, g.members = snapshot, members
	if !g.initialized {
		for id, a := range snapshot {
			if h := kongGuardHoldFromExtra(a.Extra); h != nil {
				g.acct(id).knownOp = h.Op
			}
		}
		g.cursor = g.now().Add(-kongGuardStartBack)
		g.floor = g.cursor
		g.initialized = true
	}
	if !g.issuedReady {
		g.issuedReady = g.rebuildIssued()
	}
	return true
}

// rebuildIssued 从用量表回读启动下界之前 60 分钟内的下发票，只重建票表，不据此判定。失败返回 false，下一轮再试；
// 这期间引用这些票的请求不下结论，靠巡检补。
func (g *KongDowngradeGuard) rebuildIssued() bool {
	ctx, cancel := g.dbCtx()
	defer cancel()
	since := g.floor.Add(kongGuardStartBack - kongGuardIssuedKeep)
	ids := g.watched()
	for {
		rows, err := g.store.ScanUsage(ctx, since, ids, kongGuardScanLimit)
		if err != nil {
			slog.Warn("kong downgrade guard: 重建票表失败，下一轮再试", "error", err)
			return false
		}
		for _, row := range rows {
			if row.CreatedAt.After(g.floor) {
				return true
			}
			g.noteIssued(row)
		}
		if len(rows) < kongGuardScanLimit || !rows[len(rows)-1].CreatedAt.After(since) {
			return true
		}
		since = rows[len(rows)-1].CreatedAt
	}
}

// watched 是受监控的账号（快照里的 OpenAI OAuth 账号）。
func (g *KongDowngradeGuard) watched() []int64 {
	ids := make([]int64, 0, len(g.snapshot))
	for id := range g.snapshot {
		ids = append(ids, id)
	}
	return ids
}

func (g *KongDowngradeGuard) noteIssued(row KongGuardUsageRow) {
	if row.ReissuedFP == "" {
		return
	}
	if _, ok := g.snapshot[row.AccountID]; !ok {
		return
	}
	if _, ok := g.issued[row.ReissuedFP]; !ok {
		g.issued[row.ReissuedFP] = kongGuardIssued{accountID: row.AccountID, model: row.Model, at: row.StartedAt()}
	}
}

func (g *KongDowngradeGuard) takeOverExpired() {
	ctx, cancel := g.dbCtx()
	defer cancel()
	ids, err := g.store.TakeOverExpired(ctx)
	if err != nil {
		slog.Warn("kong downgrade guard: 到期接管失败", "error", err)
		return
	}
	for _, id := range ids {
		a := g.acct(id)
		g.event(KongGuardEvent{AccountID: id, Op: a.knownOp, Kind: KongGuardEventHumanRelease, Detail: map[string]any{"reason": "expired"}})
		slog.Warn("kong downgrade guard: 账号到期，关闭转为到期自动暂停", "account_id", id)
		a.knownOp = ""
		if acc := g.snapshot[id]; acc != nil {
			delete(acc.Extra, "kong_downgrade_guard_hold")
		}
	}
}

// noteReleases 对比上一轮的持有记录，发现人或别的流程解除了哨兵的关闭。
func (g *KongDowngradeGuard) noteReleases() {
	for id, a := range g.accts {
		if a.knownOp == "" {
			continue
		}
		acc := g.snapshot[id]
		var h *KongGuardHold
		if acc != nil {
			h = kongGuardHoldFromExtra(acc.Extra)
		}
		if h != nil && h.Op == a.knownOp {
			continue
		}
		g.event(KongGuardEvent{AccountID: id, Op: a.knownOp, Kind: KongGuardEventHumanRelease, Detail: map[string]any{"reason": "schedulable_changed"}})
		slog.Warn("kong downgrade guard: 哨兵的关闭已被人工或其他流程解除", "account_id", id, "op", a.knownOp)
		a.knownOp = ""
		if h != nil {
			a.knownOp = h.Op
		}
	}
	for id, acc := range g.snapshot {
		if h := kongGuardHoldFromExtra(acc.Extra); h != nil {
			g.acct(id).knownOp = h.Op
		}
	}
}

// scan 读新的用量行，更新票表与各账号的证据，返回每个账号这一轮的第一次被动拒绝。
func (g *KongDowngradeGuard) scan(now time.Time) map[int64]KongGuardUsageRow {
	refusals := map[int64]KongGuardUsageRow{}
	ctx, cancel := g.dbCtx()
	defer cancel()
	prev := g.cursor
	since := prev.Add(-kongGuardRescan)
	if since.Before(g.floor) {
		since = g.floor
	}
	rows, err := g.store.ScanUsage(ctx, since, g.watched(), kongGuardScanLimit)
	if err != nil {
		slog.Warn("kong downgrade guard: 扫描用量失败", "error", err)
		return refusals
	}
	for _, row := range rows {
		if row.CreatedAt.After(g.cursor) {
			g.cursor = row.CreatedAt
		}
		if _, dup := g.seen[row.ID]; dup {
			continue
		}
		g.seen[row.ID] = row.CreatedAt
		if _, ok := g.snapshot[row.AccountID]; !ok {
			continue
		}
		verdict := g.judge(row)
		a := g.acct(row.AccountID)
		if row.CreatedAt.After(a.lastTraffic) {
			a.lastTraffic = row.CreatedAt
		}
		if g.cfg.GroupID > 0 && row.GroupID == g.cfg.GroupID && row.Model == g.cfg.ProbeModel && row.OutputTokens > 0 && row.CreatedAt.After(a.lastOutput) {
			a.lastOutput = row.CreatedAt
		}
		switch verdict {
		case kongGuardProbeAccept:
			if s := row.StartedAt(); s.After(a.lastGood) {
				a.lastGood = s
			}
			a.lastVerdict = row.CreatedAt
		case kongGuardProbeRefuse:
			a.lastVerdict = row.CreatedAt
			if _, ok := refusals[row.AccountID]; !ok {
				refusals[row.AccountID] = row
			}
		}
		g.noteIssued(row)
	}
	if len(rows) == kongGuardScanLimit && !g.cursor.After(prev) {
		// 回扫窗口里的行就超过了一批：游标推不动，跳过这段，避免卡在原地。
		g.cursor = prev.Add(kongGuardRescan)
		slog.Warn("kong downgrade guard: 回扫窗口内用量行超过一批，跳过一段", "from", prev, "to", g.cursor)
	}
	g.prune(now)
	return refusals
}

// judge 是被动判据：发出的票是本账号、同一模型、票龄 0–FreshAge 内签发的，响应没有下发新票 → 接受，下发了 → 拒绝；
// 其他情况不下结论。票的签发时刻取签发那一行的开始时刻。
func (g *KongDowngradeGuard) judge(row KongGuardUsageRow) string {
	if row.StateFP == "" {
		return ""
	}
	src, ok := g.issued[row.StateFP]
	if !ok || src.accountID != row.AccountID || src.model != row.Model {
		return ""
	}
	age := row.StartedAt().Sub(src.at)
	if age < 0 || age > g.cfg.FreshAge {
		return ""
	}
	if row.ReissuedFP == "" {
		return kongGuardProbeAccept
	}
	return kongGuardProbeRefuse
}

// prune 清掉不会再被读到的去重记录（早于回扫窗口）与过期的票。
func (g *KongDowngradeGuard) prune(now time.Time) {
	floor := g.cursor.Add(-kongGuardRescan)
	for id, at := range g.seen {
		if at.Before(floor) {
			delete(g.seen, id)
		}
	}
	for fp, t := range g.issued {
		if now.Sub(t.at) > kongGuardIssuedKeep {
			delete(g.issued, fp)
		}
	}
}

// eligible 报告账号是否可以由哨兵关闭：在快照里、在服务分组、可调度、没有持有记录。
func (g *KongDowngradeGuard) eligible(id int64) (*Account, bool) {
	acc := g.snapshot[id]
	if acc == nil || g.cfg.GroupID <= 0 || !g.members[id] || !acc.Schedulable || kongGuardHoldFromExtra(acc.Extra) != nil {
		return acc, false
	}
	return acc, true
}

func (g *KongDowngradeGuard) onPassiveRefuse(id int64, row KongGuardUsageRow) {
	g.event(KongGuardEvent{AccountID: id, Kind: KongGuardEventPassiveRefuse, Detail: map[string]any{
		"usage_id": row.ID, "model": row.Model, "state_fp": row.StateFP, "reissued_fp": row.ReissuedFP,
		"session_id": row.SessionID, "at": row.CreatedAt,
	}})
	acc, ok := g.eligible(id)
	if !ok || g.now().Sub(g.acct(id).lastProbe) < kongGuardPassiveGap {
		return
	}
	g.dispatch(kongGuardJob{accountID: id, rev: kongGuardRevOf(acc.Extra), trigger: "passive"})
}

// patrol 对服务分组里可调度的账号补探测：有流量而最近没有结论的每 ProbeBusy 一次，没有流量的每 ProbeIdle 一次。
func (g *KongDowngradeGuard) patrol(now time.Time) {
	if g.cfg.GroupID <= 0 {
		return
	}
	for id := range g.members {
		acc, ok := g.eligible(id)
		if !ok {
			continue
		}
		a := g.acct(id)
		interval := g.cfg.ProbeIdle
		if now.Sub(a.lastTraffic) <= kongGuardTrafficWindow {
			interval = g.cfg.ProbeBusy
		}
		if now.Sub(a.lastVerdict) < interval || now.Sub(a.lastProbe) < interval {
			continue
		}
		g.dispatch(kongGuardJob{accountID: id, rev: kongGuardRevOf(acc.Extra), trigger: "patrol"})
	}
}

// keepPending 记下一个等下一轮提交的关闭结论。同一版本下已确认降档的结论不被凭票关闭覆盖。
func (g *KongDowngradeGuard) keepPending(id int64, p kongGuardPending) {
	if old, ok := g.pending[id]; ok && old.stage == KongGuardStageConfirmed && old.rev == p.rev {
		return
	}
	g.pending[id] = p
}

// retryPending 重新提交写库失败的关闭，不重测指纹：版本变了、账号不再可关或已不是 on 模式就放弃；凭票关闭要重新
// 过一遍冷却、名额与容量。
func (g *KongDowngradeGuard) retryPending(now time.Time) {
	pending := g.pending
	g.pending = map[int64]kongGuardPending{} // 这一轮再失败的重新记进来，下一轮再试
	for id, p := range pending {
		acc, ok := g.eligible(id)
		if !ok || g.cfg.Mode != KongGuardModeOn || kongGuardRevOf(acc.Extra) != p.rev {
			continue
		}
		if p.stage == KongGuardStageSuspect {
			g.decideHold(id, p.rev, now)
			continue
		}
		g.hold(acc, p.rev, p.stage, p.evidence, now)
	}
}

// kongGuardInGroup 报告账号（现读的）是否在服务分组。
func kongGuardInGroup(acc *Account, groupID int64) bool {
	for _, id := range acc.GroupIDs {
		if id == groupID {
			return true
		}
	}
	return false
}

// scheduleRetests 对待确认、到了重测时刻的账号安排指纹确认。只在 on 模式下：observe 不改持有记录，遗留的记录原样
// 保留，切回 on 后接着处理。
func (g *KongDowngradeGuard) scheduleRetests(now time.Time) {
	if g.cfg.Mode != KongGuardModeOn {
		return
	}
	for id, acc := range g.snapshot {
		h := kongGuardHoldFromExtra(acc.Extra)
		if h == nil || h.Stage != KongGuardStageSuspect {
			continue
		}
		if h.NextFPAt != nil && now.Before(*h.NextFPAt) {
			continue
		}
		g.dispatch(kongGuardJob{accountID: id, rev: kongGuardRevOf(acc.Extra), op: h.Op, fp: true, purpose: kongGuardPurposeConfirm, trigger: "retest"})
	}
}

// claim 占住一个账号：同一账号同一时刻只有一个探测或指纹测试。
func (g *KongDowngradeGuard) claim(id int64) bool {
	g.busyMu.Lock()
	defer g.busyMu.Unlock()
	if g.busy[id] {
		return false
	}
	g.busy[id] = true
	return true
}

// dispatch 把任务交给工作协程。
func (g *KongDowngradeGuard) dispatch(job kongGuardJob) bool {
	if !g.claim(job.accountID) {
		return false
	}
	select {
	case g.jobs <- job:
		return true
	default:
		g.release(job.accountID)
		return false
	}
}

func (g *KongDowngradeGuard) release(id int64) {
	g.busyMu.Lock()
	delete(g.busy, id)
	g.busyMu.Unlock()
}

func (g *KongDowngradeGuard) worker() {
	defer g.wg.Done()
	for {
		select {
		case <-g.stop:
			return
		case job := <-g.jobs:
			res := g.run(job)
			select {
			case g.results <- res:
			case <-g.stop:
				return
			}
		}
	}
}

func (g *KongDowngradeGuard) run(job kongGuardJob) kongGuardResult {
	res := kongGuardResult{job: job, startedAt: g.now()}
	if job.fp {
		res.fpVerdict, res.fpTestID, res.fpDetail = g.fingerprint(job)
		return res
	}
	res.probe = g.probe(g.ctx, job.accountID)
	return res
}

// probe 跑一次主动探测：不带票请求一次拿票，3 秒后带着它再请求一次，看上游是否换发。两次请求都要完整成功才有结论。
func (g *KongDowngradeGuard) probe(parent context.Context, accountID int64) KongGuardProbeOutcome {
	ctx, cancel := context.WithTimeout(parent, kongGuardProbeBudget)
	defer cancel()
	acc, err := g.accounts.GetByID(ctx, accountID)
	if err != nil || acc == nil {
		return KongGuardProbeOutcome{Verdict: kongGuardProbeInconclusive, Detail: "读账号失败"}
	}
	proxyURL, err := g.prober.ResolveProxyURL(ctx, acc.ProxyID)
	if err != nil {
		return KongGuardProbeOutcome{Verdict: kongGuardProbeInconclusive, Detail: err.Error()}
	}
	first, err := g.prober.RunTicketProbe(ctx, acc, proxyURL, g.cfg.ProbeModel, "")
	if err != nil {
		return KongGuardProbeOutcome{Verdict: kongGuardProbeInconclusive, Detail: err.Error()}
	}
	if first.Issued == "" {
		return KongGuardProbeOutcome{Verdict: kongGuardProbeInconclusive, Detail: "第 1 次请求没有下发票"}
	}
	select {
	case <-time.After(g.probeGap):
	case <-ctx.Done():
		return KongGuardProbeOutcome{Verdict: kongGuardProbeInconclusive, Detail: "超时"}
	}
	second, err := g.prober.RunTicketProbe(ctx, acc, proxyURL, g.cfg.ProbeModel, first.Issued)
	if err != nil {
		return KongGuardProbeOutcome{Verdict: kongGuardProbeInconclusive, Detail: err.Error()}
	}
	if second.Issued == "" {
		return KongGuardProbeOutcome{Verdict: kongGuardProbeAccept}
	}
	return KongGuardProbeOutcome{Verdict: kongGuardProbeRefuse, Detail: fmt.Sprintf("票 %s 发回后被换发", kongStateFingerprint(first.Issued))}
}

// fingerprint 跑一次指纹测试：先取得执行权，再写 fp_start（正式记录，写不进就放弃执行权），最后执行。fp_start 写成功
// 之前的失败（有人正在测同一个账号、读账号失败、写不进）都算没开始：不计限频、不计确认次数，下一轮再来。
func (g *KongDowngradeGuard) fingerprint(job kongGuardJob) (string, string, map[string]any) {
	fctx, fcancel := context.WithTimeout(g.ctx, kongGuardFPBudget)
	defer fcancel()
	run, err := g.fper.Begin(fctx, job.accountID, g.cfg.ProbeModel)
	if err != nil {
		if !errors.Is(err, ErrKongFingerprintBusy) {
			slog.Warn("kong downgrade guard: 取指纹测试执行权失败，本轮不测指纹", "account_id", job.accountID, "error", err)
		}
		return kongGuardFPNotStarted, "", map[string]any{"error": err.Error()}
	}
	ctx, cancel := g.dbCtx()
	err = g.store.InsertEvent(ctx, KongGuardEvent{AccountID: job.accountID, Op: job.op, Kind: KongGuardEventFPStart,
		Detail: map[string]any{"purpose": job.purpose, "model": g.cfg.ProbeModel}})
	cancel()
	if err != nil {
		run.Abort()
		slog.Warn("kong downgrade guard: 写 fp_start 失败，本轮不测指纹", "account_id", job.accountID, "error", err)
		return kongGuardFPNotStarted, "", map[string]any{"error": err.Error()}
	}
	result, testID, err := run.Run(fctx)
	if err != nil {
		return kongGuardFPInconclusive, testID, map[string]any{"error": err.Error()}
	}
	verdict := kongGuardClassify(result, g.cfg.ProbeModel)
	detail := map[string]any{"execution": result.Execution, "end_reason": result.EndReason, "verdict": result.Verdict,
		"decided": result.Decided, "parts": result.Parts}
	return verdict, testID, detail
}

// kongGuardClassify 把指纹测试的结论分成 luna / 正常 / 未确认。正常包括指向请求的模型与 astra / 6.1-sol 这一对。
func kongGuardClassify(r *KongFingerprintTestResult, model string) string {
	if r == nil || r.Execution != KongFingerprintExecCompleted {
		return kongGuardFPInconclusive
	}
	decided := strings.TrimSpace(r.Decided)
	if kongGuardLunaModels[decided] {
		return kongGuardFPLuna
	}
	if r.Verdict == KongFingerprintVerdictMatch || decided == model {
		return kongGuardFPNormal
	}
	if decided != "" {
		pairOnly := true
		for _, m := range strings.Split(decided, "|") {
			if !kongGuardProbeModels[strings.TrimSpace(m)] {
				pairOnly = false
			}
		}
		if pairOnly {
			return kongGuardFPNormal
		}
	}
	return kongGuardFPInconclusive
}

// handleResult 先释放账号的忙标记再处理：处理过程中可能要对同一账号接着起确认或检测任务。
func (g *KongDowngradeGuard) handleResult(res kongGuardResult) {
	g.release(res.job.accountID)
	if res.job.fp {
		g.onFingerprint(res)
		return
	}
	g.onProbe(res)
}

func (g *KongDowngradeGuard) onProbe(res kongGuardResult) {
	id := res.job.accountID
	a := g.acct(id)
	a.lastProbe = res.startedAt
	now := g.now()
	g.event(KongGuardEvent{AccountID: id, Kind: KongGuardEventProbe, Detail: map[string]any{
		"verdict": res.probe.Verdict, "detail": res.probe.Detail, "trigger": res.job.trigger, "model": g.cfg.ProbeModel}})
	switch res.probe.Verdict {
	case kongGuardProbeAccept:
		if res.startedAt.After(a.lastGood) {
			a.lastGood = res.startedAt
		}
		a.lastVerdict = now
		return
	case kongGuardProbeRefuse:
		a.lastVerdict = now
	default:
		return
	}
	slog.Warn("kong downgrade guard: 主动探测确认上游不认本账号的票", "account_id", id, "trigger", res.job.trigger)
	g.decideHold(id, res.job.rev, now)
}

// decideHold 在主动探测拒绝之后决定：凭票关闭，或改起检测任务（observe、误报冷却、名额、容量）。
func (g *KongDowngradeGuard) decideHold(id, rev int64, now time.Time) {
	a := g.acct(id)
	acc, ok := g.eligible(id)
	if !ok {
		return
	}
	if g.cfg.Mode != KongGuardModeOn {
		g.startDetect(id, rev, kongGuardPurposeObserve, kongGuardObserveEvery, "observe")
		return
	}
	if p, ok := g.pending[id]; ok && p.stage == KongGuardStageConfirmed && p.rev == rev {
		// 指纹已经确认降档、只是还没写进库：重新提交那个结论，不降成凭票关闭（那要过名额与容量，还会再测一次指纹）。
		g.hold(acc, rev, p.stage, p.evidence, now)
		return
	}
	if !g.restored {
		// 上一次写库的结果不明，冷却与持有记录要等下一轮重新核实：这次的结论先留着。
		g.keepPending(id, kongGuardPending{rev: rev, stage: KongGuardStageSuspect, evidence: "ticket_refused"})
		return
	}
	if now.Before(a.cooldown) {
		g.skip(id, "cooldown")
		g.startDetect(id, rev, kongGuardPurposeDetect, kongGuardDetectEvery, "cooldown")
		return
	}
	if g.suspectHolds() >= g.cfg.TicketHoldMax {
		g.skip(id, "quota")
		g.startDetect(id, rev, kongGuardPurposeDetect, kongGuardDetectMin, "quota")
		return
	}
	if !g.hasCapacity(id, now) {
		g.skip(id, "capacity")
		g.startDetect(id, rev, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
		return
	}
	g.hold(acc, rev, KongGuardStageSuspect, "ticket_refused", now)
}

func (g *KongDowngradeGuard) skip(id int64, reason string) {
	g.event(KongGuardEvent{AccountID: id, Kind: KongGuardEventHoldSkipped, Detail: map[string]any{"reason": reason}})
	slog.Warn("kong downgrade guard: 不凭票关闭，改测指纹", "account_id", id, "reason", reason)
}

// startDetect 起检测任务，按用途限频：同一用途的上一次开始距今不足 every 时不起。派发不出去（账号正忙、队列满）的留到
// 下一轮。
func (g *KongDowngradeGuard) startDetect(id, rev int64, purpose string, every time.Duration, trigger string) {
	a := g.acct(id)
	if every > 0 && g.now().Sub(a.fpStart[purpose]) < every {
		return
	}
	job := kongGuardJob{accountID: id, rev: rev, fp: true, purpose: purpose, trigger: trigger, every: every}
	if !g.dispatch(job) {
		g.detects[id] = job
	}
}

// retryDetects 重新派发上一轮没能启动的检测任务，不等新的拒绝或巡检：版本变了、账号不再可关就放弃，限频照常复核。
func (g *KongDowngradeGuard) retryDetects() {
	detects := g.detects
	g.detects = map[int64]kongGuardJob{}
	for id, job := range detects {
		acc, ok := g.eligible(id)
		if !ok || kongGuardRevOf(acc.Extra) != job.rev {
			continue
		}
		g.startDetect(id, job.rev, job.purpose, job.every, job.trigger)
	}
}

// suspectHolds 数当前处于待确认的持有记录。
func (g *KongDowngradeGuard) suspectHolds() int {
	n := 0
	for _, acc := range g.snapshot {
		if h := kongGuardHoldFromExtra(acc.Extra); h != nil && h.Stage == KongGuardStageSuspect {
			n++
		}
	}
	return n
}

// hasCapacity 报告关掉 id 之后，服务分组里是否还有别的账号能服务探测模型：最近 10 分钟有输出，且此刻（现读账号）
// 仍在服务分组、基础可调度、没有持有记录。这是尽力避免停空，不是保证。
func (g *KongDowngradeGuard) hasCapacity(id int64, now time.Time) bool {
	ctx, cancel := g.dbCtx()
	defer cancel()
	for other := range g.members {
		if other == id {
			continue
		}
		a := g.accts[other]
		if a == nil || now.Sub(a.lastOutput) > kongGuardCapacityWindow {
			continue
		}
		acc, err := g.accounts.GetByID(ctx, other)
		if err != nil || acc == nil {
			continue
		}
		if kongGuardInGroup(acc, g.cfg.GroupID) && acc.IsSchedulable() && kongGuardHoldFromExtra(acc.Extra) == nil {
			return true
		}
	}
	return false
}

// hold 关闭一个账号并写 hold 事件（同一事务）。影响时段的下界是最后一次正常证据，未知时取关闭前 24 小时。
func (g *KongDowngradeGuard) hold(acc *Account, rev int64, stage, evidence string, now time.Time) {
	a := g.acct(acc.ID)
	op := g.newOp()
	h := KongGuardHold{Op: op, Stage: stage, Evidence: evidence, HeldAt: now.UTC()}
	from, unknown := a.lastGood, a.lastGood.IsZero()
	if unknown {
		from = now.Add(-kongGuardUnknownLookback)
	} else {
		lg := a.lastGood.UTC()
		h.LastGoodAt = &lg
	}
	if stage == KongGuardStageSuspect {
		next := now.UTC()
		h.NextFPAt = &next
	}
	ctx, cancel := g.dbCtx()
	defer cancel()
	held, err := g.store.Hold(ctx, KongGuardHoldInput{AccountID: acc.ID, GroupID: g.cfg.GroupID, ExpectedRev: rev, Hold: h,
		Event: KongGuardEvent{AccountID: acc.ID, Op: op, Kind: KongGuardEventHold, Detail: map[string]any{
			"stage": stage, "evidence": evidence, "held_at": now.UTC(), "window_from": from.UTC(), "window_unknown": unknown,
			"group_id": g.cfg.GroupID, "account_name": acc.Name,
		}}})
	if err != nil {
		slog.Error("kong downgrade guard: 关闭失败，下一轮再试", "account_id", acc.ID, "error", err)
		g.skip(acc.ID, "write_failed")
		g.keepPending(acc.ID, kongGuardPending{rev: rev, stage: stage, evidence: evidence})
		g.restored = false
		return
	}
	delete(g.pending, acc.ID)
	if !held {
		g.skip(acc.ID, "condition_changed")
		return
	}
	g.sync(acc.ID)
	a.knownOp = op
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra["kong_downgrade_guard_hold"] = map[string]any{"op": op, "stage": stage}
	acc.Schedulable = false
	slog.Warn("kong downgrade guard: 已关闭账号调度", "account_id", acc.ID, "stage", stage, "evidence", evidence, "op", op,
		"window_from", from.UTC(), "window_unknown", unknown)
	if stage == KongGuardStageSuspect {
		g.dispatch(kongGuardJob{accountID: acc.ID, rev: rev + 1, op: op, fp: true, purpose: kongGuardPurposeConfirm, trigger: "hold"})
	}
}

func (g *KongDowngradeGuard) onFingerprint(res kongGuardResult) {
	id, job := res.job.accountID, res.job
	if res.fpVerdict == kongGuardFPNotStarted {
		// 确认任务有持有记录，到了重测时刻每轮都会再排；检测任务没有，记下来下一轮重新派发。
		if job.purpose != kongGuardPurposeConfirm {
			g.detects[id] = job
		}
		return
	}
	a := g.acct(id)
	a.fpStart[job.purpose] = res.startedAt
	detail := map[string]any{"purpose": job.purpose, "trigger": job.trigger, "verdict": res.fpVerdict, "test_id": res.fpTestID}
	for k, v := range res.fpDetail {
		detail[k] = v
	}
	g.event(KongGuardEvent{AccountID: id, Op: job.op, Kind: KongGuardEventFingerprint, Detail: detail})
	slog.Warn("kong downgrade guard: 指纹测试结束", "account_id", id, "purpose", job.purpose, "verdict", res.fpVerdict, "test_id", res.fpTestID)
	now := g.now()
	switch job.purpose {
	case kongGuardPurposeConfirm:
		g.onConfirm(res, now)
	case kongGuardPurposeDetect:
		if res.fpVerdict == kongGuardFPLuna && g.cfg.Mode == KongGuardModeOn {
			if acc, ok := g.eligible(id); ok {
				g.hold(acc, job.rev, KongGuardStageConfirmed, "fingerprint_luna", now)
			}
		}
	}
}

// onConfirm 提交确认任务的结论。只在 on 模式下：切到 observe 之后到达的结论只记事件。
func (g *KongDowngradeGuard) onConfirm(res kongGuardResult, now time.Time) {
	id, job := res.job.accountID, res.job
	if g.cfg.Mode != KongGuardModeOn {
		return
	}
	ctx, cancel := g.dbCtx()
	defer cancel()
	switch res.fpVerdict {
	case kongGuardFPLuna:
		ok, err := g.store.UpdateHold(ctx, id, job.op, job.rev, map[string]any{"stage": KongGuardStageConfirmed, "fp_test_id": res.fpTestID, "next_fp_at": nil})
		if err != nil {
			slog.Error("kong downgrade guard: 写入确认失败，下一轮重测", "account_id", id, "error", err)
			return
		}
		if ok {
			slog.Warn("kong downgrade guard: 确认降档，保持关闭，交人处理", "account_id", id, "op", job.op)
		}
	case kongGuardFPNormal:
		until := now.Add(kongGuardCooldown).UTC()
		ok, err := g.store.ReleaseFalseAlarm(ctx, id, job.op, job.rev, KongGuardEvent{AccountID: id, Op: job.op, Kind: KongGuardEventFalseAlarm,
			Detail: map[string]any{"cooldown_until": until, "test_id": res.fpTestID}})
		if err != nil {
			slog.Error("kong downgrade guard: 误报撤销失败，下一轮重测", "account_id", id, "error", err)
			g.restored = false
			return
		}
		if ok {
			g.sync(id)
			a := g.acct(id)
			a.cooldown, a.knownOp = until, ""
			slog.Warn("kong downgrade guard: 指纹正常，撤销关闭（误报）", "account_id", id, "op", job.op, "cooldown_until", until)
		}
	default:
		var extra map[string]any
		if acc := g.snapshot[id]; acc != nil {
			extra = acc.Extra
		}
		h := kongGuardHoldFromExtra(extra)
		attempts := 1
		if h != nil {
			attempts = h.FPAttempts + 1
		}
		next := now.Add(kongGuardRetest).UTC()
		if _, err := g.store.UpdateHold(ctx, id, job.op, job.rev, map[string]any{"fp_attempts": attempts, "next_fp_at": next, "fp_test_id": res.fpTestID}); err != nil {
			slog.Error("kong downgrade guard: 写入重测时刻失败", "account_id", id, "error", err)
		}
		if attempts >= 2 {
			slog.Warn("kong downgrade guard: 指纹连续未确认，账号保持关闭，交人处理", "account_id", id, "attempts", attempts)
		}
	}
}

// runImpacts 对关闭满 30 分钟、还没定稿的操作做汇总：首次之后每小时重算，过了截止时刻 30 分钟后那次成功即定稿。
func (g *KongDowngradeGuard) runImpacts(now time.Time) {
	ctx, cancel := g.dbCtx()
	defer cancel()
	holds, err := g.store.PendingImpacts(ctx, kongGuardImpactFirst)
	if err != nil {
		slog.Warn("kong downgrade guard: 读待汇总的关闭失败", "error", err)
		return
	}
	for _, h := range holds {
		heldAt := kongGuardParseTime(h.Detail["held_at"])
		if heldAt.IsZero() {
			heldAt = h.CreatedAt
		}
		cutoff := heldAt.Add(kongGuardImpactCutoff)
		final := !now.Before(cutoff.Add(kongGuardImpactSettle))
		if last, ok := g.impactRun[h.Op]; ok && !final && now.Sub(last) < kongGuardImpactEvery {
			continue
		}
		from := kongGuardParseTime(h.Detail["window_from"])
		if from.IsZero() {
			from = heldAt.Add(-kongGuardUnknownLookback)
		}
		rows, err := g.store.ImpactRows(ctx, h.AccountID, from, heldAt.Add(kongGuardImpactLag), cutoff)
		if err != nil {
			slog.Warn("kong downgrade guard: 汇总影响范围失败", "account_id", h.AccountID, "op", h.Op, "error", err)
			continue
		}
		var total int64
		sessions := make([]KongGuardImpactRow, 0, len(rows))
		var noSession []KongGuardImpactRow // 没有会话的请求（多为网页搜索），每个 API key 一行
		for i := range rows {
			total += rows[i].Count
			if rows[i].SessionID == "" {
				noSession = append(noSession, rows[i])
				continue
			}
			sessions = append(sessions, rows[i])
		}
		detail := map[string]any{
			"window_from": from.UTC(), "held_at": heldAt.UTC(), "cutoff_at": cutoff.UTC(), "computed_at": now.UTC(),
			"finalized": final, "window_unknown": h.Detail["window_unknown"], "total": total, "sessions": sessions,
			"scope": "请求结束晚于时段下界、开始早于关闭后 5 分钟（容纳记账与调度同步的延迟）、结束不晚于截止时刻；定稿时以那次查询已可见的数据为准",
		}
		if len(noSession) > 0 {
			detail["no_session"] = noSession
		}
		if err := g.store.UpsertImpact(ctx, h.AccountID, h.Op, detail); err != nil {
			slog.Warn("kong downgrade guard: 写影响汇总失败", "op", h.Op, "error", err)
			continue
		}
		g.impactRun[h.Op] = now
		if final {
			delete(g.impactRun, h.Op)
		}
	}
}

// maintain 每小时清理一次过期事件。
func (g *KongDowngradeGuard) maintain(now time.Time) {
	if now.Sub(g.lastMaint) < kongGuardMaintainEvery {
		return
	}
	g.lastMaint = now
	ctx, cancel := g.dbCtx()
	defer cancel()
	for i := 0; i < 20; i++ {
		n, err := g.store.DeleteEventsBefore(ctx, now.Add(-kongGuardRetention), 1000)
		if err != nil {
			slog.Warn("kong downgrade guard: 清理过期事件失败", "error", err)
			return
		}
		if n < 1000 {
			return
		}
	}
}

// event 写一条诊断事件，失败只记日志。
func (g *KongDowngradeGuard) event(ev KongGuardEvent) {
	ctx, cancel := g.dbCtx()
	defer cancel()
	if err := g.store.InsertEvent(ctx, ev); err != nil {
		slog.Warn("kong downgrade guard: 写事件失败", "kind", ev.Kind, "account_id", ev.AccountID, "error", err)
	}
}

// ManualProbe 对单个账号立即做一次主动探测并返回结论。探测在调用方的协程里执行、随请求取消，不占工作协程、不排队；
// 结论照常交给主循环判断（服务分组外的账号只记录）。
func (g *KongDowngradeGuard) ManualProbe(ctx context.Context, accountID int64) (KongGuardProbeOutcome, error) {
	if g == nil || g.cfg.Mode == KongGuardModeOff {
		return KongGuardProbeOutcome{}, errors.New("哨兵未启用")
	}
	acc, err := g.accounts.GetByID(ctx, accountID)
	if err != nil || acc == nil || !acc.IsOpenAIOAuthLike() || acc.IsShadow() || acc.IsOpenAIAgentIdentity() {
		return KongGuardProbeOutcome{}, ErrKongFingerprintAccount
	}
	if !g.claim(accountID) {
		return KongGuardProbeOutcome{}, ErrKongGuardBusy
	}
	res := kongGuardResult{job: kongGuardJob{accountID: accountID, rev: kongGuardRevOf(acc.Extra), trigger: "manual"}, startedAt: g.now()}
	res.probe = g.probe(ctx, accountID)
	select {
	case g.results <- res:
	case <-g.stop:
		g.release(accountID)
	}
	return res.probe, nil
}

// ListEvents 读最近的事件。
func (g *KongDowngradeGuard) ListEvents(ctx context.Context, accountID int64, limit int) ([]KongGuardEvent, error) {
	return g.store.ListEvents(ctx, accountID, limit)
}

// kongGuardTesterRunner 用指纹测试器跑测试。
type kongGuardTesterRunner struct{ t *KongFingerprintTester }

func (r kongGuardTesterRunner) Begin(ctx context.Context, accountID int64, model string) (kongGuardFPRun, error) {
	run, err := r.t.Begin(ctx, accountID, model)
	if err != nil {
		return nil, err
	}
	return kongGuardTesterRun{run}, nil
}

type kongGuardTesterRun struct{ run *KongFingerprintRun }

func (r kongGuardTesterRun) Abort() { r.run.Abort() }

// Run 执行测试，收集最终结论。
func (r kongGuardTesterRun) Run(ctx context.Context) (*KongFingerprintTestResult, string, error) {
	var result *KongFingerprintTestResult
	var testID string
	r.run.Execute(ctx, func(ev KongFingerprintTestEvent) {
		if ev.TestID != "" {
			testID = ev.TestID
		}
		if ev.Type == "done" {
			result = ev.Result
		}
	})
	if result == nil {
		return nil, testID, errors.New("指纹测试没有给出结论")
	}
	return result, testID, nil
}

// KongGuardHoldView 是管理端看到的一条持有记录。
type KongGuardHoldView struct {
	AccountID   int64         `json:"account_id"`
	AccountName string        `json:"account_name"`
	Schedulable bool          `json:"schedulable"`
	InGroup     bool          `json:"in_group"`
	Rev         int64         `json:"rev"`
	Hold        KongGuardHold `json:"hold"`
}

// Holds 读当前所有带持有记录的账号。
func (g *KongDowngradeGuard) Holds(ctx context.Context) ([]KongGuardHoldView, error) {
	list, err := g.accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	members := map[int64]bool{}
	if g.cfg.GroupID > 0 {
		inGroup, err := g.accounts.ListByGroup(ctx, g.cfg.GroupID)
		if err != nil {
			return nil, err
		}
		for i := range inGroup {
			members[inGroup[i].ID] = true
		}
	}
	out := []KongGuardHoldView{}
	for i := range list {
		a := &list[i]
		if h := kongGuardHoldFromExtra(a.Extra); h != nil {
			out = append(out, KongGuardHoldView{AccountID: a.ID, AccountName: a.Name, Schedulable: a.Schedulable,
				InGroup: members[a.ID], Rev: kongGuardRevOf(a.Extra), Hold: *h})
		}
	}
	return out, nil
}
