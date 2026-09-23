//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type kongCatalogTestClock struct {
	mu   sync.Mutex
	t    time.Time
	hook func() // 每次取时刻之前调用（不持时钟锁），用来在组件的某次取时刻处插入并发事件
}

func (c *kongCatalogTestClock) now() time.Time {
	c.mu.Lock()
	hook := c.hook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *kongCatalogTestClock) setHook(hook func()) {
	c.mu.Lock()
	c.hook = hook
	c.mu.Unlock()
}

// kongCatalogAtUnlockedNow 返回时钟钩子：组件第一次在不持锁时取时刻，执行一次 fn。持锁时取的时刻
// （登记、计票等）跳过，fn 因此恰好落在两段加锁之间。
func kongCatalogAtUnlockedNow(c *KongCodexCatalog, fn func()) func() {
	var fired atomic.Bool
	return func() {
		if fired.Load() || !c.mu.TryLock() {
			return
		}
		c.mu.Unlock()
		if fired.CompareAndSwap(false, true) {
			fn()
		}
	}
}

func (c *kongCatalogTestClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type kongCatalogTestGateway struct {
	mu         sync.Mutex
	accounts   []Account
	bodies     map[int64]string
	errs       map[int64]error
	calls      map[int64]int
	block      chan struct{}
	blocks     map[int64]chan struct{}
	running    atomic.Int32
	maxRunning atomic.Int32
}

func newKongCatalogTestGateway(accounts ...Account) *kongCatalogTestGateway {
	return &kongCatalogTestGateway{accounts: accounts, bodies: map[int64]string{}, errs: map[int64]error{}, calls: map[int64]int{}}
}

func (g *kongCatalogTestGateway) setAccounts(accounts ...Account) {
	g.mu.Lock()
	g.accounts = accounts
	g.mu.Unlock()
}

func (g *kongCatalogTestGateway) setBody(id int64, body string) {
	g.mu.Lock()
	g.bodies[id] = body
	g.mu.Unlock()
}

func (g *kongCatalogTestGateway) callCount(id int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[id]
}

func (g *kongCatalogTestGateway) totalCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, n := range g.calls {
		total += n
	}
	return total
}

func (g *kongCatalogTestGateway) kongCodexCatalogCandidates(context.Context, int64) ([]Account, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Account(nil), g.accounts...), nil
}

func (g *kongCatalogTestGateway) kongCodexCatalogRecheck(_ context.Context, account *Account, _ int64) (*Account, string) {
	copied := *account
	return &copied, account.GetCredential("plan_type")
}

func (g *kongCatalogTestGateway) kongCodexCatalogFetch(ctx context.Context, account *Account, _ string) (*OpenAIModelsResponse, error) {
	running := g.running.Add(1)
	defer g.running.Add(-1)
	for {
		peak := g.maxRunning.Load()
		if running <= peak || g.maxRunning.CompareAndSwap(peak, running) {
			break
		}
	}
	g.mu.Lock()
	g.calls[account.ID]++
	body, err, block := g.bodies[account.ID], g.errs[account.ID], g.block
	if perAccount := g.blocks[account.ID]; perAccount != nil {
		block = perAccount
	}
	g.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &OpenAIModelsResponse{Body: []byte(body)}, nil
}

type kongCatalogTestRepo struct {
	mu           sync.Mutex
	catalogs     map[int64]KongCodexCatalogRecord
	nextID       int64
	observations []KongCodexCatalogObservation
	samples      int
	latestCalls  atomic.Int32
	latestDelay  time.Duration
}

func newKongCatalogTestRepo() *kongCatalogTestRepo {
	return &kongCatalogTestRepo{catalogs: map[int64]KongCodexCatalogRecord{}}
}

func (r *kongCatalogTestRepo) EnsureKongCodexCatalog(_ context.Context, record KongCodexCatalogRecord) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.catalogs {
		if existing.SHA256 == record.SHA256 {
			return id, nil
		}
	}
	r.nextID++
	record.ID = r.nextID
	r.catalogs[record.ID] = record
	return record.ID, nil
}

func (r *kongCatalogTestRepo) LoadKongCodexCatalogs(_ context.Context, ids []int64) ([]KongCodexCatalogRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []KongCodexCatalogRecord
	for _, id := range ids {
		if record, ok := r.catalogs[id]; ok {
			out = append(out, record)
		}
	}
	return out, nil
}

func (r *kongCatalogTestRepo) LatestKongCodexCatalogObservations(_ context.Context, clientVersion string) ([]KongCodexCatalogObservation, error) {
	r.latestCalls.Add(1)
	time.Sleep(r.latestDelay)
	r.mu.Lock()
	defer r.mu.Unlock()
	latest := map[int64]KongCodexCatalogObservation{}
	for _, o := range r.observations {
		if o.ClientVersion == clientVersion && !o.LastSeenAt.Before(latest[o.AccountID].LastSeenAt) {
			latest[o.AccountID] = o
		}
	}
	var out []KongCodexCatalogObservation
	for _, o := range latest {
		out = append(out, o)
	}
	return out, nil
}

func (r *kongCatalogTestRepo) RecordKongCodexCatalogSample(_ context.Context, sample KongCodexCatalogObservation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples++
	latest := -1
	for i, o := range r.observations {
		if o.AccountID == sample.AccountID && o.ClientVersion == sample.ClientVersion &&
			(latest < 0 || !o.LastSeenAt.Before(r.observations[latest].LastSeenAt)) {
			latest = i
		}
	}
	if latest >= 0 && r.observations[latest].CatalogID == sample.CatalogID && r.observations[latest].PlanType == sample.PlanType {
		r.observations[latest].LastSeenAt = sample.LastSeenAt
		return nil
	}
	r.observations = append(r.observations, sample)
	return nil
}

func (r *kongCatalogTestRepo) segmentCount(accountID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, o := range r.observations {
		if o.AccountID == accountID {
			n++
		}
	}
	return n
}

const kongCatalogFullBody = `{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6-astra"},{"slug":"gpt-6-luna"}]}`
const kongCatalogFreeBody = `{"models":[{"slug":"gpt-6-luna"}]}`
const kongCatalogPlusBody = `{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6-luna"}]}`

func kongCatalogSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func kongCatalogActiveAccount(id int64, plan string, now time.Time, usedAgo time.Duration) Account {
	account := kongCatalogTestAccount(id, plan)
	used := now.Add(-usedAgo)
	account.LastUsedAt = &used
	return account
}

type kongCatalogFixture struct {
	clock *kongCatalogTestClock
	gw    *kongCatalogTestGateway
	repo  *kongCatalogTestRepo
	c     *KongCodexCatalog
	group *Group
}

func newKongCatalogFixture(t *testing.T, accounts ...Account) *kongCatalogFixture {
	t.Helper()
	clock := &kongCatalogTestClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	gw := newKongCatalogTestGateway(accounts...)
	repo := newKongCatalogTestRepo()
	c := newKongCodexCatalog(true, repo, gw, clock.now)
	t.Cleanup(c.Stop)
	return &kongCatalogFixture{clock: clock, gw: gw, repo: repo, c: c, group: &Group{ID: 10, Platform: PlatformOpenAI}}
}

func (f *kongCatalogFixture) serve(t *testing.T) KongCodexCatalogResult {
	t.Helper()
	result := f.c.Serve(context.Background(), f.group, "0.160.1", time.Now())
	f.waitIdle(t)
	return result
}

func (f *kongCatalogFixture) waitIdle(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.c.mu.Lock()
		defer f.c.mu.Unlock()
		return len(f.c.tasks) == 0
	}, 3*time.Second, 5*time.Millisecond)
}

func TestKongCodexCatalogColdStartServesAgreedVersion(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "pro", now, 2*time.Minute),
		kongCatalogActiveAccount(3, "prolite", now, 3*time.Minute))
	for id := int64(1); id <= 3; id++ {
		f.gw.setBody(id, kongCatalogFullBody)
	}

	result := f.serve(t)
	require.NotNil(t, result.Manifest)
	require.Equal(t, kongCatalogFullBody, string(result.Manifest.Body))
	require.Equal(t, CodexModelsManifestETag([]byte(kongCatalogFullBody)), result.Manifest.ETag)
	require.NotZero(t, result.AccountID)
	require.NotNil(t, result.Fallback)
	require.Len(t, f.repo.catalogs, 1)
	for id := int64(1); id <= 3; id++ {
		require.Equal(t, 1, f.gw.callCount(id), "冷启动与补采样合起来每个账号只拉一次")
		require.Equal(t, 1, f.repo.segmentCount(id))
	}

	// 冷却期内再来请求：直接用内存里的对外版本，不再拉上游。
	f.clock.advance(5 * time.Minute)
	again := f.serve(t)
	require.Equal(t, kongCatalogFullBody, string(again.Manifest.Body))
	require.Equal(t, 3, f.gw.totalCalls())

	// 过了 10 分钟，活跃账号重新采样，同版本只续上最新一段。
	f.clock.advance(6 * time.Minute)
	for i := range f.gw.accounts {
		used := f.clock.now().Add(-time.Minute)
		f.gw.accounts[i].LastUsedAt = &used
	}
	f.serve(t)
	require.Equal(t, 6, f.gw.totalCalls())
	require.Equal(t, 1, f.repo.segmentCount(1))
	require.Equal(t, 6, f.repo.samples)
}

func TestKongCodexCatalogOnlyFetchesActiveTopTierAccounts(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, 2*time.Hour), // 最高档但闲置
		kongCatalogActiveAccount(2, "free", now, time.Minute),
		kongCatalogActiveAccount(3, "free", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	f.gw.setBody(2, kongCatalogFreeBody)
	f.gw.setBody(3, kongCatalogFreeBody)

	result := f.serve(t)
	require.Nil(t, result.Manifest, "最高档没有活跃账号时不拉闲置账号，也不转去拉低档账号")
	require.NotNil(t, result.Fallback)
	require.Zero(t, f.gw.totalCalls())
}

func TestKongCodexCatalogSingleDegradedAccountKeepsVersion(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "pro", now, time.Minute),
		kongCatalogActiveAccount(3, "pro", now, time.Minute))
	for id := int64(1); id <= 3; id++ {
		f.gw.setBody(id, kongCatalogFullBody)
	}
	require.Equal(t, kongCatalogFullBody, string(f.serve(t).Manifest.Body))

	// 16 分钟后账号 1 掉档（凭据仍是 pro），只有它还在跑业务，被采到缩水版；另两个账号已闲置。
	f.clock.advance(16 * time.Minute)
	f.gw.setBody(1, kongCatalogFreeBody)
	used := f.clock.now().Add(-time.Minute)
	f.gw.accounts[0].LastUsedAt = &used
	f.serve(t)
	require.Equal(t, 4, f.gw.totalCalls())
	require.Equal(t, 2, f.repo.segmentCount(1), "换版本开新段")

	result := f.serve(t)
	require.Equal(t, kongCatalogFullBody, string(result.Manifest.Body), "单个账号掉档不改变对外版本")
}

func TestKongCodexCatalogFallbackUsesTaskAndCooldown(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t)
	f.clock.t = now
	result := f.serve(t) // 没有合格账号：登记客户端版本，返回回退句柄
	require.Nil(t, result.Manifest)
	fallback := result.Fallback
	require.NotNil(t, fallback)

	failing := kongCatalogActiveAccount(1, "pro", now, 2*time.Hour)
	retryable := &codexModelsManifestUpstreamError{err: errors.New("upstream 503"), retryable: true, statusCode: http.StatusServiceUnavailable}
	f.gw.errs[1] = retryable
	manifest, excluded, err := fallback.fetch(context.Background(), &failing)
	require.Nil(t, manifest)
	require.False(t, excluded)
	require.True(t, IsRetryableCodexModelsManifestError(err))

	manifest, excluded, err = fallback.fetch(context.Background(), &failing)
	require.NoError(t, err)
	require.Nil(t, manifest)
	require.True(t, excluded, "冷却中又没有观测：排除")
	require.Equal(t, 1, f.gw.callCount(1))

	working := kongCatalogActiveAccount(2, "free", now, 2*time.Hour)
	f.gw.setBody(2, kongCatalogFreeBody)
	manifest, excluded, err = fallback.fetch(context.Background(), &working)
	require.NoError(t, err)
	require.False(t, excluded)
	require.Equal(t, kongCatalogFreeBody, string(manifest.Body))

	// 冷却中但有观测：用它的存档正文，不再拉上游；响应是独立副本。
	manifest.Body[0] = 'X'
	again, excluded, err := fallback.fetch(context.Background(), &working)
	require.NoError(t, err)
	require.False(t, excluded)
	require.Equal(t, kongCatalogFreeBody, string(again.Body))
	require.Equal(t, 1, f.gw.callCount(2))
}

func TestKongCodexCatalogResponsesAreIndependentCopies(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t, kongCatalogActiveAccount(1, "pro", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	first := f.serve(t)
	first.Manifest.Body = []byte(`{}`)
	first.Manifest.NotModified = true
	second := f.serve(t)
	require.Equal(t, kongCatalogFullBody, string(second.Manifest.Body))
	require.False(t, second.Manifest.NotModified)
}

func TestKongCodexCatalogRestoresFromRepository(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "pro", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	f.gw.setBody(2, kongCatalogFullBody)
	f.serve(t)
	require.Equal(t, 2, f.gw.totalCalls())

	// 重启：新组件、同一个库。第一次请求从库里恢复观测与正文，冷却期内不拉上游。
	restarted := newKongCodexCatalog(true, f.repo, f.gw, f.clock.now)
	t.Cleanup(restarted.Stop)
	f.clock.advance(3 * time.Minute)
	result := restarted.Serve(context.Background(), f.group, "0.160.1", time.Now())
	require.NotNil(t, result.Manifest)
	require.Equal(t, kongCatalogFullBody, string(result.Manifest.Body))
	require.Equal(t, 2, f.gw.totalCalls())
}

func TestKongCodexCatalogTierChangeReelects(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "plus", now, time.Minute),
		kongCatalogActiveAccount(3, "plus", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	f.gw.setBody(2, kongCatalogPlusBody)
	f.gw.setBody(3, kongCatalogPlusBody)
	require.Equal(t, kongCatalogFullBody, string(f.serve(t).Manifest.Body))
	require.Zero(t, f.gw.callCount(2), "低档账号不采样")

	// pro 账号不再可调度：降到 plus 档，清掉对外版本，在新的一档里冷启动重选。
	f.gw.setAccounts(f.gw.accounts[1], f.gw.accounts[2])
	require.Equal(t, kongCatalogPlusBody, string(f.serve(t).Manifest.Body))
}

func TestKongCodexCatalogClientVersionRegistration(t *testing.T) {
	require.Equal(t, CodexCanonicalClientVersion(), kongCodexCatalogClientVersion(""))
	require.Equal(t, "0.160.1", kongCodexCatalogClientVersion(" 0.160.1 "))
	require.Empty(t, kongCodexCatalogClientVersion("not a version!"))

	f := newKongCatalogFixture(t)
	require.Nil(t, f.c.Serve(context.Background(), f.group, "not a version!", time.Now()).Fallback, "不合法的版本不纳管")
	require.Nil(t, f.c.Serve(context.Background(), &Group{ID: 1, Platform: PlatformComposite}, "0.160.1", time.Now()).Fallback)
	pinned := &Group{ID: 2, Platform: PlatformOpenAI, CodexModelsManifestConfig: GroupCodexModelsManifestConfig{Enabled: true}}
	require.Nil(t, f.c.Serve(context.Background(), pinned, "0.160.1", time.Now()).Fallback)

	for i := 0; i < kongCodexCatalogMaxClients; i++ {
		require.NotNil(t, f.c.Serve(context.Background(), f.group, "0.160."+string(rune('0'+i)), time.Now()).Fallback)
	}
	require.Nil(t, f.c.Serve(context.Background(), f.group, "0.161.0", time.Now()).Fallback, "满额又没有可移除的：走原循环")
	f.clock.advance(kongCodexCatalogClientIdle)
	require.NotNil(t, f.c.Serve(context.Background(), f.group, "0.161.0", time.Now()).Fallback, "移除 24 小时没有请求的")
	f.c.mu.Lock()
	registered := len(f.c.clients)
	f.c.mu.Unlock()
	require.Equal(t, kongCodexCatalogMaxClients, registered)
}

func TestKongCodexCatalogTaskSlotsAndStop(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var accounts []Account
	for id := int64(1); id <= 5; id++ {
		accounts = append(accounts, kongCatalogActiveAccount(id, "pro", now, time.Duration(id)*time.Minute))
	}
	f := newKongCatalogFixture(t, accounts...)
	f.gw.block = make(chan struct{})
	for id := int64(1); id <= 5; id++ {
		f.gw.setBody(id, kongCatalogFullBody)
	}

	start := time.Now()
	result := f.c.Serve(context.Background(), f.group, "0.160.1", time.Now())
	require.Nil(t, result.Manifest)
	require.Less(t, time.Since(start), kongCodexCatalogColdBudget+time.Second, "前台等待有上限")
	require.LessOrEqual(t, f.gw.maxRunning.Load(), int32(kongCodexCatalogTaskSlots))

	done := make(chan struct{})
	go func() {
		f.c.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 应取消并等待在途任务")
	}
	_, admit := f.c.obtain(context.Background(), "0.160.1", f.group.ID, &accounts[4], 1, false)
	require.Equal(t, kongCodexCatalogStopped, admit)
}

func TestKongCodexCatalogFetchCachedBypassesCache(t *testing.T) {
	s := &OpenAIGatewayService{}
	request := openAIModelsRequest{url: "https://chatgpt.com/backend-api/codex/models?client_version=0.160.1", accountID: 7}
	var calls int
	fetch := func(context.Context, string) (*OpenAIModelsResponse, error) {
		calls++
		return &OpenAIModelsResponse{Body: []byte(kongCatalogFreeBody), ETag: `"e"`, upstreamETag: `"e"`}, nil
	}
	bypass := context.WithValue(context.Background(), kongCodexCatalogBypassKey{}, true)
	for i := 0; i < 2; i++ {
		_, err := s.fetchCachedOpenAIModels(bypass, request, fetch, "")
		require.NoError(t, err)
	}
	require.Equal(t, 2, calls, "采样任务每次都向上游拉")
	cached, _ := s.openAIModelsCache.get(buildOpenAIModelsCacheKey(request), time.Now())
	require.Nil(t, cached, "采样任务不写缓存")

	for i := 0; i < 2; i++ {
		_, err := s.fetchCachedOpenAIModels(context.Background(), request, fetch, "")
		require.NoError(t, err)
	}
	require.Equal(t, 3, calls, "不带标记时照旧走缓存")
}

func TestKongParseCodexCatalogConsensus(t *testing.T) {
	enabled, err := kongParseCodexCatalogConsensus("", false)
	require.NoError(t, err)
	require.True(t, enabled)
	enabled, err = kongParseCodexCatalogConsensus(" false ", true)
	require.NoError(t, err)
	require.False(t, enabled)
	enabled, err = kongParseCodexCatalogConsensus("maybe", true)
	require.Error(t, err)
	require.False(t, enabled)
	require.True(t, strings.Contains(err.Error(), KongCodexCatalogConsensusEnv))
}

func TestKongCodexCatalogDisabledIsInert(t *testing.T) {
	c := newKongCodexCatalog(false, nil, newKongCatalogTestGateway(), time.Now)
	t.Cleanup(c.Stop)
	require.Equal(t, KongCodexCatalogResult{}, c.Serve(context.Background(), &Group{ID: 1, Platform: PlatformOpenAI}, "0.160.1", time.Now()))
	var nilCatalog *KongCodexCatalog
	require.Equal(t, KongCodexCatalogResult{}, nilCatalog.Serve(context.Background(), &Group{ID: 1, Platform: PlatformOpenAI}, "0.160.1", time.Now()))
	nilCatalog.Stop()
	require.Equal(t, kongCatalogSHA(kongCatalogFullBody), kongCatalogSHA(kongCatalogFullBody))
}

func (f *kongCatalogFixture) hasCurrent(clientVersion string) bool {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	client := f.c.clients[clientVersion]
	return client != nil && client.current[f.group.ID] != nil
}

func (f *kongCatalogFixture) hasVersion(sha string) bool {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	return f.c.versions[sha] != nil
}

func TestKongCodexCatalogTierChangeWithoutVotesDropsCurrent(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	pro := kongCatalogActiveAccount(1, "pro", now, time.Minute)
	plus := kongCatalogActiveAccount(2, "plus", now, 2*time.Hour) // 闲置，冷启动不拉
	f := newKongCatalogFixture(t, pro, plus)
	f.gw.setBody(1, kongCatalogFullBody)
	require.Equal(t, kongCatalogFullBody, string(f.serve(t).Manifest.Body))

	// pro 暂不可用：降到 plus 档，新的一档选不出，也要清掉 pro 档选出的对外版本。
	f.gw.setAccounts(plus)
	result := f.serve(t)
	require.Nil(t, result.Manifest)
	require.False(t, f.hasCurrent("0.160.1"))

	// pro 恢复：按全部最新观测首次选出，而不是沿用旧的对外版本。
	f.gw.setAccounts(pro, plus)
	require.Equal(t, kongCatalogFullBody, string(f.serve(t).Manifest.Body))
	require.True(t, f.hasCurrent("0.160.1"))
}

func TestKongCodexCatalogPrunesReplacedVersion(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "pro", now, time.Minute),
		kongCatalogActiveAccount(3, "pro", now, time.Minute))
	for id := int64(1); id <= 3; id++ {
		f.gw.setBody(id, kongCatalogPlusBody)
	}
	require.Equal(t, kongCatalogPlusBody, string(f.serve(t).Manifest.Body))

	// 上游新增模型：三个活跃账号重采都看到新版本，对外版本切过去，旧正文不再被引用就清掉。
	f.clock.advance(11 * time.Minute)
	for i := range f.gw.accounts {
		used := f.clock.now().Add(-time.Minute)
		f.gw.accounts[i].LastUsedAt = &used
		f.gw.setBody(f.gw.accounts[i].ID, kongCatalogFullBody)
	}
	f.serve(t)
	require.Equal(t, kongCatalogFullBody, string(f.serve(t).Manifest.Body))
	require.False(t, f.hasVersion(kongCatalogSHA(kongCatalogPlusBody)))
	require.True(t, f.hasVersion(kongCatalogSHA(kongCatalogFullBody)))
}

func TestKongCodexCatalogColdStartStopsOnceTwoAgree(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	a := kongCatalogActiveAccount(1, "pro", now, time.Minute)
	b := kongCatalogActiveAccount(2, "pro", now, 2*time.Minute)
	c := kongCatalogActiveAccount(3, "pro", now, 3*time.Minute)
	other := kongCatalogActiveAccount(9, "pro", now, time.Hour) // 不在候选里，只占名额
	f := newKongCatalogFixture(t, a, b, c)
	releaseA, releaseB, releaseC, releaseOther := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.gw.blocks = map[int64]chan struct{}{1: releaseA, 2: releaseB, 3: releaseC, 9: releaseOther}
	t.Cleanup(func() {
		for _, ch := range []chan struct{}{releaseA, releaseOther} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})
	for _, id := range []int64{1, 2, 3, 9} {
		f.gw.setBody(id, kongCatalogFullBody)
	}
	f.c.mu.Lock()
	f.c.registerLocked("0.160.1", f.clock.now())
	f.c.clients["0.160.1"].loaded = true
	f.c.mu.Unlock()
	for _, account := range []Account{b, c, other} {
		task, admit := f.c.obtain(context.Background(), "0.160.1", f.group.ID, &account, 0, false)
		require.Equal(t, kongCodexCatalogStarted, admit)
		require.NotNil(t, task)
	}
	waiting := make(chan struct{})
	f.c.coldStartWaiting = func() { close(waiting) }

	type served struct {
		result  KongCodexCatalogResult
		elapsed time.Duration
	}
	out := make(chan served, 1)
	go func() {
		start := time.Now()
		result := f.c.Serve(context.Background(), f.group, "0.160.1", start)
		out <- served{result, time.Since(start)}
	}()
	<-waiting // 冷启动已发起：B、C 在跑，A 在等名额
	close(releaseB)
	close(releaseC)
	got := <-out
	require.NotNil(t, got.result.Manifest, "B、C 一致后就该下发")
	require.Less(t, got.elapsed, time.Second, "不等还没完成的 A，也不等到预算用完")
	select {
	case <-releaseA:
		t.Fatal("A 不应已被放行")
	default:
	}
}

func TestKongCodexCatalogFirstLoadRespectsBudget(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t, kongCatalogActiveAccount(1, "pro", now, time.Minute), kongCatalogActiveAccount(2, "pro", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	f.gw.setBody(2, kongCatalogFullBody)
	f.serve(t)

	f.repo.latestDelay = 300 * time.Millisecond
	f.repo.latestCalls.Store(0)
	restarted := newKongCodexCatalog(true, f.repo, f.gw, f.clock.now)
	t.Cleanup(restarted.Stop)
	// 进入 handler 已经过去 2.45 秒：首个请求只剩 50 毫秒，读库慢也要按预算返回。
	start := time.Now()
	result := restarted.Serve(context.Background(), f.group, "0.160.1", start.Add(-kongCodexCatalogColdBudget+50*time.Millisecond))
	require.Less(t, time.Since(start), 200*time.Millisecond)
	require.Nil(t, result.Manifest)

	// 后台加载照常完成，后面的请求直接用；整个过程只读一次库。
	require.Eventually(t, func() bool {
		return restarted.Serve(context.Background(), f.group, "0.160.1", time.Now()).Manifest != nil
	}, 2*time.Second, 20*time.Millisecond)
	require.Equal(t, int32(1), f.repo.latestCalls.Load())
}

func TestKongCodexCatalogObtainSkipsCanceledCaller(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	account := kongCatalogActiveAccount(1, "pro", now, time.Minute)
	f := newKongCatalogFixture(t, account)
	f.c.mu.Lock()
	f.c.registerLocked("0.160.1", f.clock.now())
	f.c.mu.Unlock()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	task, admit := f.c.obtain(canceled, "0.160.1", f.group.ID, &account, 0, true)
	require.Nil(t, task)
	require.Equal(t, kongCodexCatalogNoSlot, admit)
	f.c.mu.Lock()
	_, attempted := f.c.clients["0.160.1"].attempts[account.ID]
	f.c.mu.Unlock()
	require.False(t, attempted, "已放弃的调用方不记尝试")
	require.Zero(t, f.gw.totalCalls())
}

func TestKongCodexCatalogConcurrentFirstLoadQueriesOnce(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t, kongCatalogActiveAccount(1, "pro", now, time.Minute), kongCatalogActiveAccount(2, "pro", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	f.gw.setBody(2, kongCatalogFullBody)
	f.serve(t)

	f.repo.latestDelay = 50 * time.Millisecond
	f.repo.latestCalls.Store(0)
	restarted := newKongCodexCatalog(true, f.repo, f.gw, f.clock.now)
	t.Cleanup(restarted.Stop)
	var wg sync.WaitGroup
	results := make([]KongCodexCatalogResult, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = restarted.Serve(context.Background(), f.group, "0.160.1", time.Now())
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), f.repo.latestCalls.Load(), "同一客户端版本同时只加载一次")
	for _, result := range results {
		require.NotNil(t, result.Manifest)
	}
}

type kongCatalogCandidateRepo struct {
	AccountRepository
	accounts []Account
}

func (r kongCatalogCandidateRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

func (r kongCatalogCandidateRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, ErrAccountNotFound
}

func TestKongCodexCatalogCandidatesCheckGroupMembership(t *testing.T) {
	inGroup := kongCatalogTestAccount(1, "plus")
	inGroup.GroupIDs = []int64{10}
	moved := kongCatalogTestAccount(2, "pro") // 分组桶还没重建，账号已移出本组
	moved.GroupIDs = []int64{11}
	apiKey := kongCatalogTestAccount(3, "")
	apiKey.Type = AccountTypeAPIKey
	apiKey.GroupIDs = []int64{10}
	s := &OpenAIGatewayService{accountRepo: kongCatalogCandidateRepo{accounts: []Account{inGroup, moved, apiKey}}}

	candidates, err := s.kongCodexCatalogCandidates(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, kongCatalogTestIDs(candidates))
	voters, tier := kongCodexCatalogVoters(candidates)
	require.Equal(t, 2, tier, "已移组的 pro 不能占据最高档")
	require.Equal(t, []int64{1}, kongCatalogTestIDs(voters))
}

func TestKongCodexCatalogWaiterJoinsTaskStartedMeanwhile(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t)
	f.c.mu.Lock()
	f.c.registerLocked("0.160.1", f.clock.now())
	f.c.mu.Unlock()
	// 三个名额都被别的账号占着。
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.gw.blocks = map[int64]chan struct{}{21: release, 22: release, 23: release}
	for _, id := range []int64{21, 22, 23} {
		busy := kongCatalogActiveAccount(id, "pro", now, time.Minute)
		_, admit := f.c.obtain(context.Background(), "0.160.1", f.group.ID, &busy, 0, false)
		require.Equal(t, kongCodexCatalogStarted, admit)
	}

	// X 的回退请求在等名额；期间 X 的任务由别处发起（这里直接登记一个），它应加入这个任务而不是干等名额。
	x := kongCatalogActiveAccount(5, "pro", now, time.Minute)
	type fetched struct {
		manifest *OpenAIModelsResponse
		err      error
	}
	out := make(chan fetched, 1)
	fallback := &KongCodexCatalogFallback{c: f.c, groupID: f.group.ID, clientVersion: "0.160.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	waiting := make(chan struct{})
	var once sync.Once
	f.c.slotWaiting = func() { once.Do(func() { close(waiting) }) }
	go func() {
		manifest, _, err := fallback.fetch(ctx, &x)
		out <- fetched{manifest, err}
	}()
	<-waiting // X 已经卡在等名额上

	// 照真实任务的样子：发起时记尝试，结束前写入观测，结束时通知。
	key := kongCodexCatalogTaskKey{accountID: x.ID, clientVersion: "0.160.1"}
	task := &kongCodexCatalogTask{done: make(chan struct{})}
	f.c.mu.Lock()
	f.c.tasks[key] = task
	f.c.clients["0.160.1"].attempts[x.ID] = f.clock.now()
	f.c.notifyLocked()
	f.c.mu.Unlock()
	sha := kongCatalogSHA(kongCatalogFullBody)
	f.c.observe(context.Background(), "0.160.1", x.ID, "pro", sha, []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna"}, []byte(kongCatalogFullBody))
	f.c.mu.Lock()
	task.sha, task.body = sha, []byte(kongCatalogFullBody)
	delete(f.c.tasks, key)
	f.c.notifyLocked()
	f.c.mu.Unlock()
	close(task.done)

	select {
	case got := <-out:
		require.NoError(t, got.err)
		require.Equal(t, kongCatalogFullBody, string(got.manifest.Body))
	case <-time.After(time.Second):
		t.Fatal("等名额的一方应加入同一账号的任务，不等到超时")
	}
	require.Zero(t, f.gw.callCount(x.ID), "没有另起任务")
}

func TestKongCodexCatalogServeKeepsChosenVersionPrunedMeanwhile(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "pro", now, time.Minute))
	f.gw.setBody(1, kongCatalogPlusBody)
	f.gw.setBody(2, kongCatalogPlusBody)
	require.Equal(t, kongCatalogPlusBody, string(f.serve(t).Manifest.Body))

	// 这个请求计票选出 Plus 之后，别的请求让两个投票人都看到 Full，把对外版本换过去并清掉 Plus 的正文。
	voters, tier := kongCodexCatalogVoters(f.gw.accounts)
	full := []byte(kongCatalogFullBody)
	slugs, err := kongCodexCatalogSlugs(full)
	require.NoError(t, err)
	f.clock.setHook(kongCatalogAtUnlockedNow(f.c, func() {
		for _, id := range []int64{1, 2} {
			f.c.observe(context.Background(), "0.160.1", id, "pro", kongCatalogSHA(kongCatalogFullBody), slugs, full)
		}
		f.c.decide("0.160.1", f.group.ID, voters, tier, false)
	}))
	result := f.c.Serve(context.Background(), f.group, "0.160.1", time.Now())
	f.clock.setHook(nil)

	require.False(t, f.hasVersion(kongCatalogSHA(kongCatalogPlusBody)), "Plus 的正文已被清理")
	require.NotNil(t, result.Manifest, "选定的版本随后被清理，也照样下发，不退回原循环")
	require.Equal(t, kongCatalogPlusBody, string(result.Manifest.Body))
	require.True(t, f.hasCurrent("0.160.1"))
}

func TestKongCodexCatalogColdStartCountsResultsBeforeItStarts(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	a := kongCatalogActiveAccount(1, "pro", now, time.Minute)
	b := kongCatalogActiveAccount(2, "pro", now, 2*time.Minute)
	c := kongCatalogActiveAccount(3, "pro", now, 3*time.Minute)
	f := newKongCatalogFixture(t, a, b, c)
	releaseA, releaseB, releaseC := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.gw.blocks = map[int64]chan struct{}{1: releaseA, 2: releaseB, 3: releaseC}
	t.Cleanup(func() { close(releaseA) })
	for _, id := range []int64{1, 2, 3} {
		f.gw.setBody(id, kongCatalogFullBody)
	}
	f.c.mu.Lock()
	f.c.registerLocked("0.160.1", f.clock.now())
	f.c.clients["0.160.1"].loaded = true
	f.c.mu.Unlock()
	for _, account := range []Account{b, c} {
		_, admit := f.c.obtain(context.Background(), "0.160.1", f.group.ID, &account, 0, false)
		require.Equal(t, kongCodexCatalogStarted, admit)
	}

	// 计票时 B、C 还没结果；冷启动记下起点之前它们完成且一致，之后时钟往前走。
	f.clock.setHook(kongCatalogAtUnlockedNow(f.c, func() {
		close(releaseB)
		close(releaseC)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			f.c.mu.Lock()
			n := len(f.c.tasks)
			f.c.mu.Unlock()
			if n == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		f.clock.advance(time.Second)
	}))
	start := time.Now()
	result := f.c.Serve(context.Background(), f.group, "0.160.1", start)
	elapsed := time.Since(start)
	f.clock.setHook(nil)

	require.NotNil(t, result.Manifest)
	require.Equal(t, kongCatalogFullBody, string(result.Manifest.Body))
	require.Less(t, elapsed, time.Second, "两份一致已经在手，不等 A，也不等到预算用完")
	f.c.mu.Lock()
	_, running := f.c.tasks[kongCodexCatalogTaskKey{accountID: a.ID, clientVersion: "0.160.1"}]
	f.c.mu.Unlock()
	require.True(t, running, "A 的任务照常在后台跑")
}

func TestKongCodexCatalogSampleFollowsRequestContext(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := newKongCatalogFixture(t,
		kongCatalogActiveAccount(1, "pro", now, time.Minute),
		kongCatalogActiveAccount(2, "pro", now, time.Minute))
	f.gw.setBody(1, kongCatalogFullBody)
	f.gw.setBody(2, kongCatalogFullBody)
	require.Equal(t, kongCatalogFullBody, string(f.serve(t).Manifest.Body))

	f.clock.advance(11 * time.Minute)
	for i := range f.gw.accounts {
		used := f.clock.now().Add(-time.Minute)
		f.gw.accounts[i].LastUsedAt = &used
	}
	calls := f.gw.totalCalls()

	// 下游已经放弃：不再起补采，也不占用准入。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.c.Serve(ctx, f.group, "0.160.1", time.Now())
	f.waitIdle(t)
	require.Equal(t, calls, f.gw.totalCalls())
	f.c.mu.Lock()
	attempted := f.c.clients["0.160.1"].attempts[1]
	f.c.mu.Unlock()
	require.Equal(t, now, attempted, "准入时刻还是上一次的")

	// 只是冷启动预算用完、请求还在：照常补采。
	result := f.c.Serve(context.Background(), f.group, "0.160.1", time.Now().Add(-time.Minute))
	f.waitIdle(t)
	require.Equal(t, kongCatalogFullBody, string(result.Manifest.Body))
	require.Equal(t, calls+2, f.gw.totalCalls())
}
