package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Codex 模型目录的版本存档与多数对外（DESIGN-codex-catalog-consensus.md）。
//
// 网关从纳管账号（合格的 OpenAI OAuth 账号）拉到的目录按全文存成版本，各账号看到哪个版本记成观测；
// 给下游的对外版本由分组内最高一档账号的观测计票决定（规则在 kong_codex_catalog_vote.go）。纳管账号
// 的一切目录拉取都经这里的准入与任务：同一账号同一客户端版本 10 分钟最多一次。

// KongCodexCatalogConsensusEnv 是开关：不设置为开，false 关闭，写错按关闭处理并记错误日志。
const KongCodexCatalogConsensusEnv = "KONG_CODEX_CATALOG_CONSENSUS"

const (
	// kongCodexCatalogActiveWindow 内有业务请求的账号才会被采样。
	kongCodexCatalogActiveWindow = 15 * time.Minute
	// kongCodexCatalogAttemptGap 是同一账号同一客户端版本两次拉取的最小间隔，成功失败都算。
	kongCodexCatalogAttemptGap = 10 * time.Minute
	// kongCodexCatalogFreshWindow 内采到的观测算新鲜票。
	kongCodexCatalogFreshWindow = 60 * time.Minute
	// 冷启动最多拉几个账号、前台最多等多久（从进入入口算起，含按需加载）。
	kongCodexCatalogColdAccounts = 3
	kongCodexCatalogColdBudget   = 2500 * time.Millisecond
	// 全局同时进行的拉取任务数与每个任务的超时。
	kongCodexCatalogTaskSlots   = 3
	kongCodexCatalogTaskTimeout = 15 * time.Second
	// 最多登记几个客户端版本；满额时可移除多久没有请求的。
	kongCodexCatalogMaxClients = 8
	kongCodexCatalogClientIdle = 24 * time.Hour
	kongCodexCatalogLoadRetry  = time.Minute
	// kongCodexCatalogLoadTimeout 是按需加载读库的超时；加载不随某个请求取消，等待方各自按预算等。
	kongCodexCatalogLoadTimeout = 5 * time.Second
	kongCodexCatalogLabelPrefix = 12
)

func kongParseCodexCatalogConsensus(value string, present bool) (bool, error) {
	if !present {
		return true, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s 必须是 true / false，得到 %q", KongCodexCatalogConsensusEnv, value)
	}
	return enabled, nil
}

// KongCodexCatalogRecord 是一个目录版本：原始正文与从中解析出的模型名。
type KongCodexCatalogRecord struct {
	ID     int64
	SHA256 string
	Slugs  []string
	Body   []byte
}

// KongCodexCatalogObservation 是某账号在某客户端版本下的一段观测；记一次采样时首末时刻相同。
type KongCodexCatalogObservation struct {
	AccountID     int64
	ClientVersion string
	CatalogID     int64
	PlanType      string
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
}

// KongCodexCatalogRepository 是存档的数据库访问（实现在 repository/kong_codex_catalog_repo.go）。
type KongCodexCatalogRepository interface {
	// EnsureKongCodexCatalog 按 sha256 插入版本，已存在就返回已有的 id。
	EnsureKongCodexCatalog(ctx context.Context, record KongCodexCatalogRecord) (int64, error)
	// LoadKongCodexCatalogs 取指定版本的正文与模型名。
	LoadKongCodexCatalogs(ctx context.Context, ids []int64) ([]KongCodexCatalogRecord, error)
	// LatestKongCodexCatalogObservations 取某客户端版本下每个账号的最新一段。
	LatestKongCodexCatalogObservations(ctx context.Context, clientVersion string) ([]KongCodexCatalogObservation, error)
	// RecordKongCodexCatalogSample 记一次采样：与该账号该客户端版本的最新一段同版本同套餐就续上，否则插新段。
	RecordKongCodexCatalogSample(ctx context.Context, sample KongCodexCatalogObservation) error
}

// kongCodexCatalogGateway 是组件用到的网关能力，测试里替换。
type kongCodexCatalogGateway interface {
	// kongCodexCatalogCandidates 返回分组里空模型名选号会接受、且合格的账号（调度快照上的精简账号）。
	kongCodexCatalogCandidates(ctx context.Context, groupID int64) ([]Account, error)
	// kongCodexCatalogRecheck 按库里的最新状态复核并补全账号，另返回库里当前的 plan_type；不合格返回 nil。
	kongCodexCatalogRecheck(ctx context.Context, account *Account, groupID int64) (*Account, string)
	// kongCodexCatalogFetch 绕过账号级缓存向上游拉目录。
	kongCodexCatalogFetch(ctx context.Context, account *Account, clientVersion string) (*OpenAIModelsResponse, error)
}

type kongCodexCatalogVersion struct {
	id     int64 // 库里的 id；还没写进库时为 0
	sha    string
	slugs  []string
	models map[string]struct{}
	body   []byte
}

func (v *kongCodexCatalogVersion) label() string {
	if v.id > 0 {
		return "v" + strconv.FormatInt(v.id, 10)
	}
	return v.sha[:kongCodexCatalogLabelPrefix]
}

type kongCodexCatalogSegment struct {
	sha       string
	plan      string
	firstSeen time.Time
	lastSeen  time.Time
}

type kongCodexCatalogCurrent struct {
	sha  string
	tier int
}

// kongCodexCatalogClient 是一个已登记的客户端版本的状态。
type kongCodexCatalogClient struct {
	lastRequest time.Time
	loaded      bool
	loading     chan struct{} // 正在加载时非空，加载结束时关闭
	loadRetryAt time.Time
	segments    map[int64]*kongCodexCatalogSegment // 账号 → 最新一段
	attempts    map[int64]time.Time                // 账号 → 上次尝试拉取
	current     map[int64]*kongCodexCatalogCurrent // 分组 → 对外版本
}

type kongCodexCatalogTaskKey struct {
	accountID     int64
	clientVersion string
}

// kongCodexCatalogTask 是一次拉取。done 关闭后 body / err 只读：body 是成功时拉到的原始正文。
type kongCodexCatalogTask struct {
	done chan struct{}
	sha  string
	body []byte
	err  error
}

type kongCodexCatalogAdmit int

const (
	kongCodexCatalogStarted kongCodexCatalogAdmit = iota + 1
	kongCodexCatalogJoined
	kongCodexCatalogCooling
	kongCodexCatalogNoSlot
	kongCodexCatalogStopped
)

// KongCodexCatalog 是版本存档组件。
type KongCodexCatalog struct {
	enabled bool
	repo    KongCodexCatalogRepository
	gw      kongCodexCatalogGateway
	now     func() time.Time

	slots   chan struct{}
	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// 以下两个只给测试用：冷启动发起任务之后、开始等待之前调用；准入开始阻塞等名额之前调用。
	coldStartWaiting func()
	slotWaiting      func()

	mu       sync.Mutex
	stopped  bool
	clients  map[string]*kongCodexCatalogClient
	versions map[string]*kongCodexCatalogVersion // sha → 版本；只留被观测或对外版本引用的
	tasks    map[kongCodexCatalogTaskKey]*kongCodexCatalogTask
	// changed 在任务发起或结束时关闭并换新，等名额的一方据此重新走准入。
	changed chan struct{}
}

// NewKongCodexCatalog 建存档组件；开关按 KONG_CODEX_CATALOG_CONSENSUS 读取。
func NewKongCodexCatalog(repo KongCodexCatalogRepository, gateway *OpenAIGatewayService) *KongCodexCatalog {
	value, present := os.LookupEnv(KongCodexCatalogConsensusEnv)
	enabled, err := kongParseCodexCatalogConsensus(value, present)
	if err != nil {
		slog.Error("kong codex catalog: 目录存档已关闭", "error", err)
	}
	var gw kongCodexCatalogGateway
	if gateway != nil {
		gw = gateway
	}
	return newKongCodexCatalog(enabled, repo, gw, time.Now)
}

func newKongCodexCatalog(enabled bool, repo KongCodexCatalogRepository, gw kongCodexCatalogGateway, now func() time.Time) *KongCodexCatalog {
	ctx, cancel := context.WithCancel(context.Background())
	return &KongCodexCatalog{
		enabled:  enabled && gw != nil,
		repo:     repo,
		gw:       gw,
		now:      now,
		slots:    make(chan struct{}, kongCodexCatalogTaskSlots),
		baseCtx:  ctx,
		cancel:   cancel,
		clients:  make(map[string]*kongCodexCatalogClient),
		versions: make(map[string]*kongCodexCatalogVersion),
		tasks:    make(map[kongCodexCatalogTaskKey]*kongCodexCatalogTask),
		changed:  make(chan struct{}),
	}
}

// Stop 取消并等待全部拉取任务，之后不再起新任务。
func (c *KongCodexCatalog) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
}

// KongCodexCatalogResult 是调度器分支取对外版本的结果。
type KongCodexCatalogResult struct {
	// Manifest 非空表示已选出对外版本，是一份独立副本，可以直接交给后续的合并与折算。
	Manifest *OpenAIModelsResponse
	// AccountID 是最近一次看到对外版本的投票人，供运维记录。
	AccountID int64
	// Fallback 交给原调度循环；nil 表示这个请求不纳管。
	Fallback *KongCodexCatalogFallback
}

// KongCodexCatalogFallback 让原调度循环对纳管账号的拉取也走组件的准入与任务。
type KongCodexCatalogFallback struct {
	c             *KongCodexCatalog
	groupID       int64
	clientVersion string
}

// kongCodexCatalogClientVersion 解析有效的客户端版本：为空时沿用现有语义取规范版本，再校验；不合法返回空。
func kongCodexCatalogClientVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = CodexCanonicalClientVersion()
	}
	return NormalizeCodexClientVersion(raw)
}

// Serve 为 (分组, 客户端版本) 取对外版本（2.4），选不出时做冷启动（2.5 前三步），并顺带发起补采样（2.3）。
// start 是请求进入 handler 的时刻，冷启动的前台预算从它起算。
func (c *KongCodexCatalog) Serve(ctx context.Context, group *Group, rawClientVersion string, start time.Time) KongCodexCatalogResult {
	if c == nil || !c.enabled || group == nil || group.Platform != PlatformOpenAI || group.CodexModelsManifestConfig.Enabled {
		return KongCodexCatalogResult{}
	}
	clientVersion := kongCodexCatalogClientVersion(rawClientVersion)
	if clientVersion == "" {
		return KongCodexCatalogResult{}
	}
	budgetCtx, cancel := context.WithDeadline(ctx, start.Add(kongCodexCatalogColdBudget))
	defer cancel()

	c.mu.Lock()
	registered := !c.stopped && c.registerLocked(clientVersion, c.now())
	c.mu.Unlock()
	if !registered {
		return KongCodexCatalogResult{}
	}
	result := KongCodexCatalogResult{Fallback: &KongCodexCatalogFallback{c: c, groupID: group.ID, clientVersion: clientVersion}}

	c.ensureLoaded(budgetCtx, clientVersion)
	accounts, err := c.gw.kongCodexCatalogCandidates(budgetCtx, group.ID)
	if err != nil {
		slog.Warn("kong codex catalog: 取候选账号失败，走原循环", "group_id", group.ID, "client_version", clientVersion, "error", err)
		return result
	}
	voters, tier := kongCodexCatalogVoters(accounts)
	version, accountID := c.decide(clientVersion, group.ID, voters, tier, false)
	if len(voters) == 0 {
		return result
	}
	if version == nil {
		c.coldStart(budgetCtx, clientVersion, group.ID, voters, tier)
		version, accountID = c.decide(clientVersion, group.ID, voters, tier, true)
	}
	// 补采只在请求还在时发起；冷启动预算用完不影响。
	c.sample(ctx, clientVersion, group.ID, voters, tier)
	if version == nil {
		slog.Info("kong codex catalog: 没有选票，走原循环", "group_id", group.ID, "client_version", clientVersion, "voters", len(voters))
		return result
	}
	result.Manifest = kongCodexCatalogResponse(version.body)
	result.AccountID = accountID
	return result
}

// registerLocked 登记客户端版本；满额又没有可移除的就返回 false，这个请求走原循环。
func (c *KongCodexCatalog) registerLocked(clientVersion string, now time.Time) bool {
	if client := c.clients[clientVersion]; client != nil {
		client.lastRequest = now
		return true
	}
	if len(c.clients) >= kongCodexCatalogMaxClients {
		oldest := ""
		for cv, client := range c.clients {
			if now.Sub(client.lastRequest) < kongCodexCatalogClientIdle {
				continue
			}
			if oldest == "" || client.lastRequest.Before(c.clients[oldest].lastRequest) {
				oldest = cv
			}
		}
		if oldest == "" {
			return false
		}
		delete(c.clients, oldest)
		c.pruneLocked()
		slog.Info("kong codex catalog: 移除久未请求的客户端版本", "client_version", oldest)
	}
	c.clients[clientVersion] = &kongCodexCatalogClient{
		lastRequest: now,
		segments:    make(map[int64]*kongCodexCatalogSegment),
		attempts:    make(map[int64]time.Time),
		current:     make(map[int64]*kongCodexCatalogCurrent),
	}
	return true
}

// ensureLoaded 在客户端版本登记后第一次用到时，从库里取它下每个账号的最新一段与引用的版本。同一客户端
// 版本同时只有一次加载，在后台进行，用自己的超时，不随某个请求取消；每个请求只按自己的预算等它。读库
// 失败时一分钟后再试，期间按内存里已有的观测处理。
func (c *KongCodexCatalog) ensureLoaded(ctx context.Context, clientVersion string) {
	c.mu.Lock()
	client := c.clients[clientVersion]
	if client == nil || client.loaded || c.now().Before(client.loadRetryAt) {
		c.mu.Unlock()
		return
	}
	if c.repo == nil {
		client.loaded = true
		c.mu.Unlock()
		return
	}
	done := client.loading
	if done == nil {
		if c.stopped {
			c.mu.Unlock()
			return
		}
		done = make(chan struct{})
		client.loading = done
		c.wg.Add(1)
		go c.load(clientVersion, client, done)
	}
	c.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *KongCodexCatalog) load(clientVersion string, client *kongCodexCatalogClient, done chan struct{}) {
	defer c.wg.Done()
	loadCtx, cancel := context.WithTimeout(c.baseCtx, kongCodexCatalogLoadTimeout)
	rows, records, err := c.loadFromRepository(loadCtx, clientVersion)
	cancel()

	c.mu.Lock()
	defer func() {
		c.mu.Unlock()
		close(done)
	}()
	client.loading = nil
	if c.clients[clientVersion] != client {
		return
	}
	if err != nil {
		client.loadRetryAt = c.now().Add(kongCodexCatalogLoadRetry)
		slog.Warn("kong codex catalog: 读取历史观测失败", "client_version", clientVersion, "error", err)
		return
	}
	byID := make(map[int64]*kongCodexCatalogVersion, len(records))
	for _, record := range records {
		version := c.versions[record.SHA256]
		if version == nil {
			version = newKongCodexCatalogVersion(record.SHA256, record.Slugs, record.Body)
			c.versions[record.SHA256] = version
		}
		version.id = record.ID
		byID[record.ID] = version
	}
	for _, row := range rows {
		version := byID[row.CatalogID]
		if version == nil {
			continue
		}
		if _, ok := client.segments[row.AccountID]; !ok {
			client.segments[row.AccountID] = &kongCodexCatalogSegment{
				sha: version.sha, plan: row.PlanType, firstSeen: row.FirstSeenAt, lastSeen: row.LastSeenAt,
			}
		}
		if _, ok := client.attempts[row.AccountID]; !ok {
			client.attempts[row.AccountID] = row.LastSeenAt
		}
	}
	client.loaded = true
	c.pruneLocked()
}

func (c *KongCodexCatalog) loadFromRepository(ctx context.Context, clientVersion string) ([]KongCodexCatalogObservation, []KongCodexCatalogRecord, error) {
	rows, err := c.repo.LatestKongCodexCatalogObservations(ctx, clientVersion)
	if err != nil || len(rows) == 0 {
		return rows, nil, err
	}
	ids := make([]int64, 0, len(rows))
	seen := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.CatalogID]; !ok {
			seen[row.CatalogID] = struct{}{}
			ids = append(ids, row.CatalogID)
		}
	}
	records, err := c.repo.LoadKongCodexCatalogs(ctx, ids)
	return rows, records, err
}

// decide 按投票人的最新观测计票，更新并返回对外版本与最近一次看到它的投票人。返回的是版本本身：
// 解锁之后别的请求可能已经换掉对外版本并清理它的正文，按 sha 再查会落空。
func (c *KongCodexCatalog) decide(clientVersion string, groupID int64, voters []Account, tier int, cold bool) (*kongCodexCatalogVersion, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	client := c.clients[clientVersion]
	if client == nil {
		return nil, 0
	}
	now := c.now()
	votes := make([]kongCodexCatalogVote, 0, len(voters))
	for i := range voters {
		segment := client.segments[voters[i].ID]
		if segment == nil || c.versions[segment.sha] == nil {
			continue
		}
		votes = append(votes, kongCodexCatalogVote{accountID: voters[i].ID, sha: segment.sha, lastSeen: segment.lastSeen})
	}

	current := ""
	previous := client.current[groupID]
	tierChanged := false
	if previous != nil {
		if previous.tier != tier {
			// C 是上一档的投票人选出的：即使新的一档还选不出，也不能留着它，否则档位恢复时会绕过重选。
			tierChanged = true
			delete(client.current, groupID)
			c.pruneLocked()
			slog.Info("kong codex catalog: 档位变化，清掉对外版本", "group_id", groupID, "client_version", clientVersion,
				"from_tier", previous.tier, "to_tier", tier)
		} else if c.versions[previous.sha] != nil {
			current = previous.sha
		}
	}
	chosen, basis := kongCodexCatalogDecide(current, votes, len(voters), now, c.modelsLocked)
	if chosen == "" {
		return nil, 0
	}
	version := c.versions[chosen]
	if chosen != current {
		client.current[groupID] = &kongCodexCatalogCurrent{sha: chosen, tier: tier}
		c.logCurrentChangeLocked(clientVersion, groupID, previous, chosen, basis, tierChanged, cold, votes, now)
		c.pruneLocked()
	}
	var accountID int64
	var latest time.Time
	for _, v := range votes {
		if v.sha == chosen && v.lastSeen.After(latest) {
			accountID, latest = v.accountID, v.lastSeen
		}
	}
	return version, accountID
}

func (c *KongCodexCatalog) modelsLocked(sha string) map[string]struct{} {
	if version := c.versions[sha]; version != nil {
		return version.models
	}
	return nil
}

func (c *KongCodexCatalog) logCurrentChangeLocked(
	clientVersion string, groupID int64, previous *kongCodexCatalogCurrent, chosen string,
	basis kongCodexCatalogBasis, tierChanged, cold bool, votes []kongCodexCatalogVote, now time.Time,
) {
	switch {
	case cold:
		basis = "冷启动"
	case tierChanged:
		basis = "档位变化"
	}
	fresh := make(map[string]int)
	for _, v := range votes {
		if now.Sub(v.lastSeen) <= kongCodexCatalogFreshWindow {
			fresh[v.sha]++
		}
	}
	to := c.versions[chosen]
	attrs := []any{
		"group_id", groupID, "client_version", clientVersion, "basis", string(basis),
		"to", to.label(), "to_models", len(to.models), "to_fresh", fresh[chosen],
	}
	if previous != nil {
		if from := c.versions[previous.sha]; from != nil {
			missing, added := kongCodexCatalogModelDiff(from.models, to.models)
			attrs = append(attrs, "from", from.label(), "from_models", len(from.models), "from_fresh", fresh[previous.sha],
				"missing", missing, "added", added)
		}
	}
	slog.Info("kong codex catalog: 对外版本变化", attrs...)
}

// coldStart 拉最高一档里最多 3 个最近有业务的账号，等到凑够 2 份一致、任务都结束或预算用完。
// 等名额与等结果并行推进：等名额期间别的候选已经完成的，也计入"两份一致"的判断。
func (c *KongCodexCatalog) coldStart(ctx context.Context, clientVersion string, groupID int64, voters []Account, tier int) {
	start := c.now()
	active := make([]Account, 0, len(voters))
	for i := range voters {
		if kongCodexCatalogActive(&voters[i], start) {
			active = append(active, voters[i])
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].LastUsedAt.After(*active[j].LastUsedAt) })
	if len(active) > kongCodexCatalogColdAccounts {
		active = active[:kongCodexCatalogColdAccounts]
	}
	// 冷启动返回时取消：辅助协程不再等名额、不再等结果；已经起了的任务照常做完、照常入档。
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan struct{}, len(active))
	pending := 0
	for i := range active {
		account := active[i]
		task, admit := c.obtain(waitCtx, clientVersion, groupID, &account, tier, false)
		switch admit {
		case kongCodexCatalogStarted, kongCodexCatalogJoined:
			pending++
			go func() {
				select {
				case <-task.done:
				case <-waitCtx.Done():
				}
				events <- struct{}{}
			}()
		case kongCodexCatalogNoSlot:
			pending++
			go func() {
				if waited, _ := c.obtain(waitCtx, clientVersion, groupID, &account, tier, true); waited != nil {
					select {
					case <-waited.done:
					case <-waitCtx.Done():
					}
				}
				events <- struct{}{}
			}()
		}
	}
	if c.coldStartWaiting != nil {
		c.coldStartWaiting()
	}
	for pending > 0 && !c.coldAgreed(clientVersion, active) {
		select {
		case <-events:
			pending--
		case <-waitCtx.Done():
			return
		}
	}
}

// coldAgreed 判断冷启动的候选里是否已有两个账号看到同一个版本。走到冷启动说明候选在计票那一刻都还没有
// 观测，所以它们现在的观测都是这之后拿到的，包括冷启动记下起点之前就已完成的任务。
func (c *KongCodexCatalog) coldAgreed(clientVersion string, candidates []Account) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	client := c.clients[clientVersion]
	if client == nil {
		return false
	}
	agree := make(map[string]int, len(candidates))
	for i := range candidates {
		segment := client.segments[candidates[i].ID]
		if segment == nil {
			continue
		}
		agree[segment.sha]++
		if agree[segment.sha] >= 2 {
			return true
		}
	}
	return false
}

// sample 为最高一档里最近有业务、能通过准入的账号起新任务；拿不到名额就留给下次。ctx 是请求的上下文：
// 下游已经放弃就不再起新任务，已经起了的任务不受影响。
func (c *KongCodexCatalog) sample(ctx context.Context, clientVersion string, groupID int64, voters []Account, tier int) {
	now := c.now()
	for i := range voters {
		if kongCodexCatalogActive(&voters[i], now) {
			c.obtain(ctx, clientVersion, groupID, &voters[i], tier, false)
		}
	}
}

func kongCodexCatalogActive(account *Account, now time.Time) bool {
	return account.LastUsedAt != nil && now.Sub(*account.LastUsedAt) <= kongCodexCatalogActiveWindow
}

// obtain 是准入（2.3）：已有任务就返回它；冷却中不拉；否则拿到名额后起新任务并记下尝试时刻。
// requireTier 大于 0 时，任务复核后还要求账号仍在这一档。wait 为 false 时拿不到名额就放弃；为 true 时
// 等名额期间任务一有发起或结束就重新走准入，同一账号的任务出现了就加入它，不再干等名额。
func (c *KongCodexCatalog) obtain(ctx context.Context, clientVersion string, groupID int64, account *Account, requireTier int, wait bool) (*kongCodexCatalogTask, kongCodexCatalogAdmit) {
	key := kongCodexCatalogTaskKey{accountID: account.ID, clientVersion: clientVersion}
	for {
		if ctx.Err() != nil {
			return nil, kongCodexCatalogNoSlot
		}
		c.mu.Lock()
		if task, admit, decided := c.admitLocked(key); decided {
			c.mu.Unlock()
			return task, admit
		}
		changed := c.changed
		c.mu.Unlock()

		select {
		case c.slots <- struct{}{}:
		default:
			if !wait {
				return nil, kongCodexCatalogNoSlot
			}
			if c.slotWaiting != nil {
				c.slotWaiting()
			}
			select {
			case c.slots <- struct{}{}:
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, kongCodexCatalogNoSlot
			case <-c.baseCtx.Done():
				return nil, kongCodexCatalogNoSlot
			}
		}

		c.mu.Lock()
		if task, admit, decided := c.admitLocked(key); decided {
			c.mu.Unlock()
			<-c.slots
			return task, admit
		}
		if ctx.Err() != nil {
			// 等名额期间调用方已经放弃：不建任务、不记尝试。
			c.mu.Unlock()
			<-c.slots
			return nil, kongCodexCatalogNoSlot
		}
		task := &kongCodexCatalogTask{done: make(chan struct{})}
		c.tasks[key] = task
		if client := c.clients[clientVersion]; client != nil {
			client.attempts[account.ID] = c.now()
		}
		c.notifyLocked()
		c.wg.Add(1)
		c.mu.Unlock()
		go c.run(key, task, *account, groupID, requireTier)
		return task, kongCodexCatalogStarted
	}
}

// notifyLocked 唤醒等名额的一方重新走准入。
func (c *KongCodexCatalog) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *KongCodexCatalog) admitLocked(key kongCodexCatalogTaskKey) (*kongCodexCatalogTask, kongCodexCatalogAdmit, bool) {
	if c.stopped {
		return nil, kongCodexCatalogStopped, true
	}
	if task := c.tasks[key]; task != nil {
		return task, kongCodexCatalogJoined, true
	}
	if client := c.clients[key.clientVersion]; client != nil {
		if last, ok := client.attempts[key.accountID]; ok && c.now().Sub(last) < kongCodexCatalogAttemptGap {
			return nil, kongCodexCatalogCooling, true
		}
	}
	return nil, 0, false
}

func (c *KongCodexCatalog) run(key kongCodexCatalogTaskKey, task *kongCodexCatalogTask, account Account, groupID int64, requireTier int) {
	defer c.wg.Done()
	ctx, cancel := context.WithTimeout(c.baseCtx, kongCodexCatalogTaskTimeout)
	sha, body, err := c.fetchAndRecord(ctx, key.clientVersion, &account, groupID, requireTier)
	cancel()
	<-c.slots
	c.mu.Lock()
	task.sha, task.body, task.err = sha, body, err
	delete(c.tasks, key)
	c.notifyLocked()
	c.mu.Unlock()
	close(task.done)
}

var errKongCodexCatalogIneligible = &codexModelsManifestUpstreamError{
	err:       infraerrors.New(http.StatusBadGateway, "KONG_CODEX_CATALOG_ACCOUNT_INELIGIBLE", "account is no longer eligible for Codex catalog fetches"),
	retryable: true,
}

func (c *KongCodexCatalog) fetchAndRecord(ctx context.Context, clientVersion string, account *Account, groupID int64, requireTier int) (string, []byte, error) {
	full, plan := c.gw.kongCodexCatalogRecheck(ctx, account, groupID)
	if full == nil {
		return "", nil, errKongCodexCatalogIneligible
	}
	plan = strings.TrimSpace(plan)
	if requireTier > 0 && kongCodexCatalogPlanTier(plan) != requireTier {
		return "", nil, errKongCodexCatalogIneligible
	}
	response, err := c.gw.kongCodexCatalogFetch(ctx, full, clientVersion)
	if err != nil {
		if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
			slog.Warn("kong codex catalog: 拉取失败", "account_id", full.ID, "client_version", clientVersion, "error", err)
		}
		return "", nil, err
	}
	if response == nil || len(response.Body) == 0 {
		return "", nil, &codexModelsManifestUpstreamError{
			err:       infraerrors.New(http.StatusBadGateway, "KONG_CODEX_CATALOG_EMPTY", "Codex catalog fetch returned no body"),
			retryable: true,
		}
	}
	body := append([]byte(nil), response.Body...)
	slugs, err := kongCodexCatalogSlugs(body)
	if err != nil {
		return "", nil, &codexModelsManifestUpstreamError{
			err:       infraerrors.Newf(http.StatusBadGateway, "KONG_CODEX_CATALOG_INVALID", "decode Codex catalog: %v", err),
			retryable: true,
		}
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	c.observe(ctx, clientVersion, full.ID, plan, sha, slugs, body)
	return sha, body, nil
}

// observe 把一次成功的拉取记进库与内存：同版本同套餐续上最新一段，否则开新段。先写库拿到版本 id，
// 再在一段锁里取出或新建版本并更新观测，版本不会在两步之间被清理掉。客户端版本已被移除时只写库。
func (c *KongCodexCatalog) observe(ctx context.Context, clientVersion string, accountID int64, plan, sha string, slugs []string, body []byte) {
	c.mu.Lock()
	var catalogID int64
	if version := c.versions[sha]; version != nil {
		catalogID = version.id
	}
	c.mu.Unlock()
	if catalogID == 0 && c.repo != nil {
		id, err := c.repo.EnsureKongCodexCatalog(ctx, KongCodexCatalogRecord{SHA256: sha, Slugs: slugs, Body: body})
		if err != nil {
			slog.Warn("kong codex catalog: 写入版本失败", "sha256", sha, "error", err)
		} else {
			catalogID = id
		}
	}

	now := c.now()
	c.mu.Lock()
	if client := c.clients[clientVersion]; client != nil {
		version := c.versions[sha]
		if version == nil {
			version = newKongCodexCatalogVersion(sha, slugs, body)
			c.versions[sha] = version
		}
		if catalogID > 0 {
			version.id = catalogID
		}
		segment := client.segments[accountID]
		if segment != nil && segment.sha == sha && segment.plan == plan {
			segment.lastSeen = now
		} else {
			client.segments[accountID] = &kongCodexCatalogSegment{sha: sha, plan: plan, firstSeen: now, lastSeen: now}
			c.logSegmentChangeLocked(clientVersion, accountID, plan, segment, sha)
			c.pruneLocked()
		}
	}
	c.mu.Unlock()

	if catalogID > 0 && c.repo != nil {
		err := c.repo.RecordKongCodexCatalogSample(ctx, KongCodexCatalogObservation{
			AccountID: accountID, ClientVersion: clientVersion, CatalogID: catalogID, PlanType: plan,
			FirstSeenAt: now, LastSeenAt: now,
		})
		if err != nil {
			slog.Warn("kong codex catalog: 写入观测失败", "account_id", accountID, "client_version", clientVersion, "error", err)
		}
	}
}

func (c *KongCodexCatalog) logSegmentChangeLocked(clientVersion string, accountID int64, plan string, previous *kongCodexCatalogSegment, sha string) {
	to := c.versions[sha]
	attrs := []any{
		"account_id", accountID, "client_version", clientVersion, "plan", plan,
		"to", to.label(), "to_models", len(to.models),
	}
	if previous != nil {
		if from := c.versions[previous.sha]; from != nil {
			missing, added := kongCodexCatalogModelDiff(from.models, to.models)
			attrs = append(attrs, "from", from.label(), "from_models", len(from.models), "from_plan", previous.plan,
				"missing", missing, "added", added)
		}
	}
	slog.Info("kong codex catalog: 账号观测换段", attrs...)
}

// pruneLocked 丢掉不再被任何观测或对外版本引用的版本正文；库里不删。
func (c *KongCodexCatalog) pruneLocked() {
	referenced := make(map[string]struct{}, len(c.versions))
	for _, client := range c.clients {
		for _, segment := range client.segments {
			referenced[segment.sha] = struct{}{}
		}
		for _, current := range client.current {
			referenced[current.sha] = struct{}{}
		}
	}
	for sha := range c.versions {
		if _, ok := referenced[sha]; !ok {
			delete(c.versions, sha)
		}
	}
}

// fetch 是原调度循环对纳管账号的拉取（2.5 第 4 步）。excluded 为 true 表示冷却中又没有观测。
func (f *KongCodexCatalogFallback) fetch(ctx context.Context, account *Account) (*OpenAIModelsResponse, bool, error) {
	c := f.c
	task, admit := c.obtain(ctx, f.clientVersion, f.groupID, account, 0, true)
	switch admit {
	case kongCodexCatalogStarted, kongCodexCatalogJoined:
		select {
		case <-task.done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		if task.err != nil {
			return nil, false, task.err
		}
		return kongCodexCatalogResponse(task.body), false, nil
	case kongCodexCatalogCooling:
		c.mu.Lock()
		var body []byte
		if client := c.clients[f.clientVersion]; client != nil {
			if segment := client.segments[account.ID]; segment != nil {
				if version := c.versions[segment.sha]; version != nil {
					body = version.body
				}
			}
		}
		c.mu.Unlock()
		if body == nil {
			return nil, true, nil
		}
		return kongCodexCatalogResponse(body), false, nil
	default:
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, infraerrors.New(http.StatusServiceUnavailable, "KONG_CODEX_CATALOG_STOPPED", "Codex catalog fetches are stopping")
	}
}

func newKongCodexCatalogVersion(sha string, slugs []string, body []byte) *kongCodexCatalogVersion {
	models := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		models[slug] = struct{}{}
	}
	return &kongCodexCatalogVersion{sha: sha, slugs: slugs, models: models, body: body}
}

// kongCodexCatalogResponse 从只读的存档正文做一份独立的响应：正文另行复制，基础 ETag 按正文算。
func kongCodexCatalogResponse(body []byte) *OpenAIModelsResponse {
	copied := append([]byte(nil), body...)
	return &OpenAIModelsResponse{Body: copied, ETag: codexModelsManifestBodyETag(copied)}
}

func kongCodexCatalogSlugs(body []byte) ([]string, error) {
	var envelope struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(envelope.Models))
	for _, model := range envelope.Models {
		if slug := strings.TrimSpace(model.Slug); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

// ---- 网关侧 ----

type kongCodexCatalogBypassKey struct{}

// kongCodexCatalogBypassCache 判断请求是否来自存档的拉取任务：是就不经账号级缓存。
func kongCodexCatalogBypassCache(ctx context.Context) bool {
	bypass, _ := ctx.Value(kongCodexCatalogBypassKey{}).(bool)
	return bypass
}

// SetKongCodexCatalog 注入版本存档组件（可选依赖）；传 nil 等于不启用。
func (s *OpenAIGatewayService) SetKongCodexCatalog(c *KongCodexCatalog) {
	if s != nil {
		s.kongCodexCatalog = c
	}
}

// StopKongCodexCatalog 在进程退出时取消并等待拉取任务。
func (s *OpenAIGatewayService) StopKongCodexCatalog() {
	if s != nil && s.kongCodexCatalog != nil {
		s.kongCodexCatalog.Stop()
	}
}

// KongServeCodexCatalog 为调度器分支取对外版本；未注入组件时返回空结果。start 是请求进入 handler 的时刻。
func (s *OpenAIGatewayService) KongServeCodexCatalog(ctx context.Context, group *Group, rawClientVersion string, start time.Time) KongCodexCatalogResult {
	if s == nil {
		return KongCodexCatalogResult{}
	}
	return s.kongCodexCatalog.Serve(ctx, group, rawClientVersion, start)
}

// KongCodexCatalogNow 给 handler 记请求进入的时刻（冷启动预算的起点）。
func KongCodexCatalogNow() time.Time { return time.Now() }

// KongFetchCodexModelsManifest 是原调度循环的拉取：纳管账号交给存档组件的任务，其余照原样拉取。
// excluded 为 true 时调用方排除这个账号、继续选号，不计换号次数。
func (s *OpenAIGatewayService) KongFetchCodexModelsManifest(ctx context.Context, fallback *KongCodexCatalogFallback, account *Account, clientVersion string) (*OpenAIModelsResponse, bool, error) {
	if fallback == nil || !kongCodexCatalogEligible(account) {
		manifest, err := s.FetchCodexModelsManifest(ctx, account, clientVersion, "")
		return manifest, false, err
	}
	return fallback.fetch(ctx, account)
}

func (s *OpenAIGatewayService) kongCodexCatalogCandidates(ctx context.Context, groupID int64) ([]Account, error) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	id := groupID
	accounts, err := s.listSchedulableAccounts(ctx, &id, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	// 与旧版选号的读库复核同口径：分组归属与分组的隐私要求在这里补上，快照里的分组桶可能比单账号缓存旧。
	requiresPrivacy := s.openAIGroupRequiresPrivacySet(ctx, &id)
	out := make([]Account, 0, len(accounts))
	for i := range accounts {
		fresh := s.resolveFreshSchedulableOpenAIAccountBeforeProfit(ctx, &accounts[i], PlatformOpenAI, "", false, "")
		if fresh == nil || !kongCodexCatalogEligible(fresh) || !s.openAIAccountMatchesSchedulingGroup(fresh, &id) {
			continue
		}
		if requiresPrivacy && !fresh.IsPrivacySet() {
			continue
		}
		out = append(out, *fresh)
	}
	return out, nil
}

// kongCodexCatalogRecheck 先按库里的最新状态复核（含分组归属），再从调度快照补全令牌与代理；套餐取库里的。
func (s *OpenAIGatewayService) kongCodexCatalogRecheck(ctx context.Context, account *Account, groupID int64) (*Account, string) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	id := groupID
	latest := s.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, account, &id, PlatformOpenAI, "", false, "")
	if latest == nil || !kongCodexCatalogEligible(latest) {
		return nil, ""
	}
	full, err := s.hydrateSelectedAccount(ctx, latest)
	if err != nil || !kongCodexCatalogEligible(full) {
		return nil, ""
	}
	return full, latest.GetCredential("plan_type")
}

func (s *OpenAIGatewayService) kongCodexCatalogFetch(ctx context.Context, account *Account, clientVersion string) (*OpenAIModelsResponse, error) {
	return s.FetchCodexModelsManifest(context.WithValue(ctx, kongCodexCatalogBypassKey{}, true), account, clientVersion, "")
}
