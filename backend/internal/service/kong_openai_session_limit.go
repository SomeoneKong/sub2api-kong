package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// OpenAI OAuth 账号的会话数上限（设计见 DESIGN-openai-session-limit.md）。
//
// 上限是软上限，只决定新会话的去向：已有会话（粘性命中、previous_response_id 续接、guardian 亲和、
// WebSocket 后续轮次）一律放行登记；新落点的候选分成"有余量段在前、满额段在后"，满额段只在有余量段用不上
// 时才用（溢出）。登记只刷新或按空闲超时过期，从不主动删除——一个会话会同时有多个请求在途，其中一个失败
// 时别的请求可能还在原账号上执行。
//
// 计数复用上游的 SessionLimitCache（键 session_limit:account:{id}）与账号 extra 的 max_sessions /
// session_idle_timeout_minutes。未设上限的账号不登记、不查询。所有入口对 nil 安全：未装配时全部是空操作。

const (
	// kongSessionUnlimited 是放行登记传给 RegisterSession 的上限：它在上限 ≤ 0 时直接放行、什么也不写。
	kongSessionUnlimited = math.MaxInt32
	// kongSessionStatsInterval 是周期计数日志的间隔。
	kongSessionStatsInterval = 10 * time.Minute
	// kongSessionErrorLogInterval 是 Redis 错误告警的节流间隔。
	kongSessionErrorLogInterval = time.Minute
)

type kongSessionLimit struct {
	cache SessionLimitCache
	now   func() time.Time

	mu           sync.Mutex
	stats        kongSessionStats
	lastLog      time.Time
	lastErrorLog time.Time
}

type kongSessionStats struct {
	placements      int64 // 新落点中有候选设了上限的次数
	roomEmpty       int64 // 其中有余量段为空的次数
	confirmRejected int64 // 确认登记不通过（名额被别的新会话先占）的次数
	overflows       int64 // 落到满额账号的次数
	errors          int64 // Redis 错误次数
}

// SetKongSessionLimitCache 注入会话计数缓存（可选依赖）。装配后调用一次；传 nil 等于不启用。
func (s *OpenAIGatewayService) SetKongSessionLimitCache(cache SessionLimitCache) {
	if cache == nil {
		s.kongSessionLimit = nil
		return
	}
	s.kongSessionLimit = newKongSessionLimit(cache, time.Now)
}

func newKongSessionLimit(cache SessionLimitCache, now func() time.Time) *kongSessionLimit {
	return &kongSessionLimit{cache: cache, now: now, lastLog: now()}
}

// kongSessionLimited 判断账号是否受会话上限约束：OpenAI OAuth 且设了正数上限。
func kongSessionLimited(account *Account) bool {
	return account != nil && account.IsOpenAIOAuth() && account.GetMaxSessions() > 0
}

// kongSessionIdleTimeout 是账号的空闲超时 T：登记、计数与展示共用这一个值。
func kongSessionIdleTimeout(account *Account) time.Duration {
	return time.Duration(account.GetSessionIdleTimeoutMinutes()) * time.Minute
}

type kongSessionCountHashKey struct{}

// KongWithSessionCountHash 把计数用的会话哈希放进上下文。
//
// 计数身份与粘性身份分开：guardian 回退为保住父会话绑定，不带哈希进入旧版选号，计数仍要用请求自己的会话；
// WebSocket 后续轮次不经过选号，续期要用建连选号时的那个会话哈希。
func KongWithSessionCountHash(ctx context.Context, sessionHash string) context.Context {
	if ctx == nil || sessionHash == "" {
		return ctx
	}
	return context.WithValue(ctx, kongSessionCountHashKey{}, sessionHash)
}

// kongSessionCountHash 取计数用的会话哈希：选号参数里有就用它，没有再取上下文里带入的。
func kongSessionCountHash(ctx context.Context, sessionHash string) string {
	if sessionHash != "" || ctx == nil {
		return sessionHash
	}
	v, _ := ctx.Value(kongSessionCountHashKey{}).(string)
	return v
}

// kongSessionTouch 放行登记：不查上限，把会话登记到账号上（没有则加入，有则刷新时刻）。
func (s *OpenAIGatewayService) kongSessionTouch(ctx context.Context, account *Account, sessionHash string) {
	if s == nil || s.kongSessionLimit == nil {
		return
	}
	s.kongSessionLimit.touch(ctx, account, kongSessionCountHash(ctx, sessionHash))
}

// kongSessionRenew 是 WebSocket 每一轮的续期。会话哈希取建连选号时带入上下文的那个；账号配置每轮重新读取：
// 连接活得很久，期间上限或空闲超时可能已经改了。沿用建连时的账号对象，新设的上限对这条连接永远不生效，旧的空闲
// 超时还会按错的窗口清理整个账号的登记。
func (s *OpenAIGatewayService) kongSessionRenew(ctx context.Context, account *Account) {
	if s == nil || s.kongSessionLimit == nil || account == nil || !account.IsOpenAIOAuth() {
		return
	}
	hash := kongSessionCountHash(ctx, "")
	if hash == "" {
		return
	}
	s.kongSessionLimit.touch(ctx, s.kongSessionCurrentAccount(ctx, account), hash)
}

// kongSessionCurrentAccount 读取账号当前的配置（调度快照优先）。读不到时沿用传入的对象并计一次错误——续期不能
// 因为读配置失败而中断当前这一轮。
func (s *OpenAIGatewayService) kongSessionCurrentAccount(ctx context.Context, account *Account) *Account {
	var (
		current *Account
		err     error
	)
	switch {
	case s.schedulerSnapshot != nil:
		current, err = s.schedulerSnapshot.GetAccount(ctx, account.ID)
	case s.accountRepo != nil:
		current, err = s.accountRepo.GetByID(ctx, account.ID)
	default:
		return account
	}
	if err != nil || current == nil {
		if err == nil {
			err = errors.New("account not found")
		}
		s.kongSessionLimit.fail(account.ID, err)
		return account
	}
	return current
}

func (l *kongSessionLimit) touch(ctx context.Context, account *Account, hash string) {
	if l == nil || hash == "" || !kongSessionLimited(account) {
		return
	}
	if _, err := l.cache.RegisterSession(ctx, account.ID, hash, kongSessionUnlimited, kongSessionIdleTimeout(account)); err != nil {
		l.fail(account.ID, err)
	}
}

// kongSessionGate 是一次新落点的会话分段：进入旧版选号第 2 层时建立，第 2、3 层共用。nil 表示不生效
// （未装配、没有计数身份、候选都未设上限，或计数查询失败时失败开放）。
type kongSessionGate struct {
	limit *kongSessionLimit
	hash  string
	// ids 是全部候选（按候选顺序），counts 是分段时查得的设了上限的候选的活跃会话数，交给节奏算会话占用。
	ids    []int64
	counts map[int64]int
	// full 是设了上限且已满的候选：分段时已满的，以及之后确认登记没通过（名额被别的新会话先占）的。
	full map[int64]bool
	// roomCount 是有余量段的候选数（未设上限或未满）。降到 0 时第 2 层直接用满额段，即溢出。
	roomCount int
}

// kongSessionGateBegin 批量查询候选的活跃会话数并分段。pace 是同一次第 2 层的节奏决策：它按分段取会话占用，
// 并把满额账号记进决策记录。
func (s *OpenAIGatewayService) kongSessionGateBegin(ctx context.Context, sessionHash string, candidates []*Account, pace *kongPaceDecision) *kongSessionGate {
	if s == nil || s.kongSessionLimit == nil {
		return nil
	}
	l := s.kongSessionLimit
	hash := kongSessionCountHash(ctx, sessionHash)
	if hash == "" {
		return nil
	}
	ids := make([]int64, 0, len(candidates))
	timeouts := make(map[int64]time.Duration, len(candidates))
	for _, acc := range candidates {
		if kongSessionLimited(acc) {
			ids = append(ids, acc.ID)
			timeouts[acc.ID] = kongSessionIdleTimeout(acc)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// 批量查询里单个账号失败时，结果里没有它、整体仍返回 nil 错误；缺项不能当成 0，按查询失败处理。
	counts, err := l.cache.GetActiveSessionCountBatch(ctx, ids, timeouts)
	if err == nil {
		for _, id := range ids {
			if _, ok := counts[id]; !ok {
				err = fmt.Errorf("active session count missing for account %d", id)
				break
			}
		}
	}
	if err != nil {
		l.fail(0, err)
		return nil
	}
	g := &kongSessionGate{limit: l, hash: hash, ids: make([]int64, 0, len(candidates)), counts: counts, full: make(map[int64]bool)}
	for _, acc := range candidates {
		g.ids = append(g.ids, acc.ID)
		if kongSessionLimited(acc) && counts[acc.ID] >= acc.GetMaxSessions() {
			g.full[acc.ID] = true
			continue
		}
		g.roomCount++
	}
	l.record(func(st *kongSessionStats) {
		st.placements++
		if g.roomCount == 0 {
			st.roomEmpty++
		}
	})
	pace.attachSessions(g)
	return g
}

// paceUse 返回节奏计算会话占用用的输入：设了上限、或已被记为满额的候选才有，其余为 nil（会话占用调整为 0）。
// 记为满额的账号不一定有入口计数：候选快照里还没有上限、确认登记时按库里新设的上限被拒的账号，同样按占满计。
func (g *kongSessionGate) paceUse(account *Account) *kongPaceSessionUse {
	if g == nil || account == nil {
		return nil
	}
	u := &kongPaceSessionUse{Limit: account.GetMaxSessions(), Full: g.full[account.ID]}
	if count, ok := g.counts[account.ID]; ok && kongSessionLimited(account) {
		u.Active = &count
	}
	if u.Active == nil && !u.Full {
		return nil
	}
	return u
}

// fullIDs 返回当前记为满额的账号（按候选顺序）。
func (g *kongSessionGate) fullIDs() []int64 {
	if g == nil || len(g.full) == 0 {
		return nil
	}
	out := make([]int64, 0, len(g.full))
	for _, id := range g.ids {
		if g.full[id] {
			out = append(out, id)
		}
	}
	return out
}

// overflow 表示有余量的账号已一个都没有，第 2 层用的是满额账号。
func (g *kongSessionGate) overflow() bool {
	return g != nil && g.roomCount == 0
}

// preferRoomLoads 用于第 2 层：有余量段非空时只留有余量的候选；为空时原样返回，此时用的就是满额段（溢出）。
func (g *kongSessionGate) preferRoomLoads(available []accountWithLoad) []accountWithLoad {
	return kongSessionPreferRoom(g, available, func(item accountWithLoad) int64 { return item.account.ID })
}

// preferRoomAccounts 与 preferRoomLoads 相同，用于第 2 层负载读取失败的降级分支。
func (g *kongSessionGate) preferRoomAccounts(accounts []*Account) []*Account {
	return kongSessionPreferRoom(g, accounts, func(acc *Account) int64 { return acc.ID })
}

func kongSessionPreferRoom[T any](g *kongSessionGate, items []T, accountID func(T) int64) []T {
	if g == nil || len(g.full) == 0 || g.roomCount == 0 {
		return items
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		if !g.full[accountID(item)] {
			out = append(out, item)
		}
	}
	return out
}

// roomFirst 用于第 3 层：有余量的候选在前、满额的在后，各自保持原顺序；最后把设了上限的有余量候选再排一遍。
//
// 末尾那一遍是确认登记的兜底：有余量的账号可能在确认时才被别的新会话抢满，此时它已被遍历过。确认没通过的账号
// 会被记为满额，第二次遇到时按溢出放行，所以名额竞争不会让本来能服务的请求落空；第一次复核就不可用的账号，第二次
// 照样过不了复核。
func (g *kongSessionGate) roomFirst(accounts []*Account) []*Account {
	if g == nil {
		return accounts
	}
	out := make([]*Account, 0, len(accounts)*2)
	var full, retry []*Account
	for _, acc := range accounts {
		if g.full[acc.ID] {
			full = append(full, acc)
			continue
		}
		out = append(out, acc)
		if kongSessionLimited(acc) {
			retry = append(retry, acc)
		}
	}
	return append(append(out, full...), retry...)
}

// confirm 在选定账号之后登记会话。有余量的账号做带上限的原子登记，名额已被别的新会话先占时把它记为满额并返回
// false，调用方换下一个候选；满额的账号即溢出，放行登记。Redis 出错时失败开放。
func (g *kongSessionGate) confirm(ctx context.Context, account *Account) bool {
	if g == nil || !kongSessionLimited(account) {
		return true
	}
	l := g.limit
	if g.full[account.ID] {
		l.touch(ctx, account, g.hash)
		l.record(func(st *kongSessionStats) { st.overflows++ })
		slog.Warn("kong session limit: 有余量的账号都用不上，新会话落到满额账号",
			"account_id", account.ID, "max_sessions", account.GetMaxSessions())
		return true
	}
	allowed, err := l.cache.RegisterSession(ctx, account.ID, g.hash, account.GetMaxSessions(), kongSessionIdleTimeout(account))
	if err != nil {
		l.fail(account.ID, err)
		return true
	}
	if !allowed {
		g.full[account.ID] = true
		g.roomCount--
		l.record(func(st *kongSessionStats) { st.confirmRejected++ })
	}
	return allowed
}

func (l *kongSessionLimit) fail(accountID int64, err error) {
	l.record(func(st *kongSessionStats) { st.errors++ })
	l.mu.Lock()
	now := l.now()
	due := now.Sub(l.lastErrorLog) >= kongSessionErrorLogInterval
	if due {
		l.lastErrorLog = now
	}
	l.mu.Unlock()
	if due {
		slog.Warn("kong session limit: 会话计数不可用，按不限制处理", "account_id", accountID, "error", err)
	}
}

// record 更新计数，并按间隔输出一行汇总（借选号触发，不另开协程）。
func (l *kongSessionLimit) record(update func(*kongSessionStats)) {
	l.mu.Lock()
	update(&l.stats)
	now := l.now()
	if now.Sub(l.lastLog) < kongSessionStatsInterval {
		l.mu.Unlock()
		return
	}
	st := l.stats
	l.stats = kongSessionStats{}
	l.lastLog = now
	l.mu.Unlock()
	slog.Info("kong session limit: 周期计数",
		"placements", st.placements, "room_empty", st.roomEmpty, "confirm_rejected", st.confirmRejected,
		"overflows", st.overflows, "errors", st.errors)
}
