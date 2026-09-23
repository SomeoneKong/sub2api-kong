//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type kongFakeSlotCall struct {
	kind      KongSlotKind
	id        int64
	requestID string
}

type kongFakeSlotRefresher struct {
	mu      sync.Mutex
	calls   []kongFakeSlotCall
	missing map[string]bool
	err     error
}

func (f *kongFakeSlotRefresher) KongRefreshSlot(_ context.Context, kind KongSlotKind, id int64, requestID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kongFakeSlotCall{kind: kind, id: id, requestID: requestID})
	if f.err != nil {
		return false, f.err
	}
	return !f.missing[requestID], nil
}

type kongFakeLivenessSessions struct {
	mu        sync.Mutex
	idle      map[int64]time.Duration
	idleErr   map[int64]error
	idleCalls map[int64]int
	refreshed []kongLivenessSessionRef
	idles     []time.Duration
	block     chan struct{}
	entered   chan struct{}
}

func newKongFakeLivenessSessions() *kongFakeLivenessSessions {
	return &kongFakeLivenessSessions{idle: map[int64]time.Duration{}, idleErr: map[int64]error{}, idleCalls: map[int64]int{}}
}

func (f *kongFakeLivenessSessions) kongLivenessIdleTimeout(_ context.Context, accountID int64) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idleCalls[accountID]++
	if err := f.idleErr[accountID]; err != nil {
		return 0, err
	}
	return f.idle[accountID], nil
}

func (f *kongFakeLivenessSessions) kongLivenessRefreshSession(_ context.Context, accountID int64, hash string, idle time.Duration) error {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, kongLivenessSessionRef{accountID: accountID, hash: hash})
	f.idles = append(f.idles, idle)
	return nil
}

func (f *kongFakeLivenessSessions) refreshCount(accountID int64, hash string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.refreshed {
		if r.accountID == accountID && r.hash == hash {
			n++
		}
	}
	return n
}

type kongLivenessClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *kongLivenessClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *kongLivenessClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newKongLivenessForTest(slots KongSlotRefresher, sessions kongLivenessSessions) (*KongRequestLiveness, *kongLivenessClock) {
	clock := &kongLivenessClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	return newKongRequestLiveness(slots, sessions, clock.now, time.Hour), clock
}

// drainKongLivenessFinals 在测试里代替工作协程处理结束刷新队列。
func drainKongLivenessFinals(l *KongRequestLiveness) int {
	n := 0
	for {
		select {
		case ref := <-l.finals:
			l.refreshFinal(ref)
			n++
		default:
			return n
		}
	}
}

func TestKongLivenessTrackSkipsWhenNothingToRenew(t *testing.T) {
	l, _ := newKongLivenessForTest(&kongFakeSlotRefresher{}, newKongFakeLivenessSessions())
	if l.track(0, 1, "", 0, "") != nil {
		t.Fatal("既没有并发部分也没有会话，不应登记")
	}
	if l.track(KongSlotAccount, 1, "", 1, "") != nil {
		t.Fatal("没有请求 ID 就没有并发部分，没有哈希就没有会话部分")
	}
	if len(l.entries) != 0 {
		t.Fatalf("entries=%d", len(l.entries))
	}
	var nilLiveness *KongRequestLiveness
	if nilLiveness.track(KongSlotAccount, 1, "r", 1, "h") != nil {
		t.Fatal("nil 续期器不登记")
	}
}

func TestKongLivenessCycleRefreshesSlotsAndSessions(t *testing.T) {
	slots := &kongFakeSlotRefresher{}
	sessions := newKongFakeLivenessSessions()
	sessions.idle[1] = 5 * time.Minute
	sessions.idle[2] = 7 * time.Minute
	l, _ := newKongLivenessForTest(slots, sessions)

	l.track(KongSlotAccount, 1, "r1", 1, "h1")
	l.track(KongSlotAccount, 1, "r2", 1, "h1") // 同一会话的另一个请求
	l.track(KongSlotUser, 9, "r3", 0, "")
	l.track(KongSlotAPIKey, 8, "r4", 0, "")
	l.track(0, 2, "", 2, "h2") // 不限并发的账号槽，只有会话部分
	l.cycle()

	if len(slots.calls) != 4 {
		t.Fatalf("应刷新 4 个并发占用，got %+v", slots.calls)
	}
	if sessions.idleCalls[1] != 1 || sessions.idleCalls[2] != 1 {
		t.Fatalf("每个账号每批只读一次配置：%v", sessions.idleCalls)
	}
	if sessions.refreshCount(1, "h1") != 1 || sessions.refreshCount(2, "h2") != 1 || len(sessions.refreshed) != 2 {
		t.Fatalf("会话按（账号，哈希）去重各刷新一次：%+v", sessions.refreshed)
	}
	for i, ref := range sessions.refreshed {
		want := sessions.idle[ref.accountID]
		if sessions.idles[i] != want {
			t.Fatalf("刷新要用账号当前的空闲超时：got %v want %v", sessions.idles[i], want)
		}
	}
	if l.stats.slotRefreshed != 4 || l.stats.sessionRefresh != 2 {
		t.Fatalf("stats=%+v", l.stats)
	}
}

func TestKongLivenessCountsMissingAndErrors(t *testing.T) {
	slots := &kongFakeSlotRefresher{missing: map[string]bool{"gone": true}}
	l, _ := newKongLivenessForTest(slots, nil)
	l.track(KongSlotAccount, 1, "gone", 0, "")
	l.track(KongSlotAccount, 1, "here", 0, "")
	l.cycle()
	if l.stats.slotMissing != 1 || l.stats.slotRefreshed != 1 {
		t.Fatalf("stats=%+v", l.stats)
	}
	slots.err = errors.New("redis down")
	l.cycle()
	if l.stats.slotErrors != 2 {
		t.Fatalf("Redis 出错要计数：%+v", l.stats)
	}
}

func TestKongLivenessReleaseQueuesOneFinalRefresh(t *testing.T) {
	slots := &kongFakeSlotRefresher{}
	sessions := newKongFakeLivenessSessions()
	sessions.idle[1] = 5 * time.Minute
	l, _ := newKongLivenessForTest(slots, sessions)

	release := l.track(KongSlotAccount, 1, "r1", 1, "h1")
	release()
	release()
	if len(l.entries) != 0 {
		t.Fatal("释放后应注销")
	}
	if n := drainKongLivenessFinals(l); n != 1 {
		t.Fatalf("重复释放只排一次结束刷新，got %d", n)
	}
	if sessions.refreshCount(1, "h1") != 1 {
		t.Fatalf("结束刷新应刷新会话：%+v", sessions.refreshed)
	}
	l.cycle()
	if len(slots.calls) != 0 || sessions.refreshCount(1, "h1") != 1 {
		t.Fatalf("注销后周期续期不再碰它：slots=%+v sessions=%+v", slots.calls, sessions.refreshed)
	}

	l.track(KongSlotUser, 9, "r2", 0, "")()
	if n := drainKongLivenessFinals(l); n != 0 {
		t.Fatalf("没有会话部分的登记项不排结束刷新，got %d", n)
	}
}

func TestKongLivenessSameSessionRequestsAreIndependent(t *testing.T) {
	sessions := newKongFakeLivenessSessions()
	sessions.idle[1] = 5 * time.Minute
	l, _ := newKongLivenessForTest(&kongFakeSlotRefresher{}, sessions)
	first := l.track(KongSlotAccount, 1, "r1", 1, "h1")
	l.track(KongSlotAccount, 1, "r2", 1, "h1")
	first()
	drainKongLivenessFinals(l)
	l.cycle()
	if sessions.refreshCount(1, "h1") != 2 {
		t.Fatalf("一个请求结束不能停掉另一个请求的续期：%+v", sessions.refreshed)
	}
}

func TestKongLivenessIdleTimeoutFallsBackToLastKnown(t *testing.T) {
	sessions := newKongFakeLivenessSessions()
	sessions.idleErr[1] = errors.New("snapshot unavailable")
	l, _ := newKongLivenessForTest(nil, sessions)
	l.track(0, 1, "", 1, "h1")

	l.cycle()
	if len(sessions.refreshed) != 0 || l.stats.sessionSkipped != 1 || l.stats.idleErrors != 1 {
		t.Fatalf("从没读到过空闲超时时跳过并计数：refreshed=%+v stats=%+v", sessions.refreshed, l.stats)
	}

	delete(sessions.idleErr, 1)
	sessions.idle[1] = 5 * time.Minute
	l.cycle()
	sessions.idleErr[1] = errors.New("snapshot unavailable")
	l.cycle()
	if len(sessions.idles) != 2 || sessions.idles[1] != 5*time.Minute {
		t.Fatalf("读不到时沿用最近一次读到的值：%v", sessions.idles)
	}
}

func TestKongLivenessLongHeldWarnsOnceAndKeepsRenewing(t *testing.T) {
	slots := &kongFakeSlotRefresher{}
	l, clock := newKongLivenessForTest(slots, nil)
	l.track(KongSlotAccount, 1, "r1", 0, "")
	clock.advance(kongLivenessLongHeld)
	l.cycle()
	if entries := kongLivenessEntrySnapshot(l); len(entries) != 1 || !entries[0].warned {
		t.Fatalf("满阈值要告警：%+v", entries)
	}
	l.cycle() // 第一批已输出并清零周期计数
	if l.stats.longHeldWarning != 0 {
		t.Fatalf("长时登记只告警一次：%+v", l.stats)
	}
	if len(slots.calls) != 2 {
		t.Fatalf("告警不改变行为，照常续期：%+v", slots.calls)
	}
}

func TestKongLivenessFinalQueueFullDropsWithoutBlocking(t *testing.T) {
	l, _ := newKongLivenessForTest(nil, newKongFakeLivenessSessions())
	for i := 0; i < kongLivenessFinalQueueSize+1; i++ {
		l.track(0, 1, "", 1, "h")()
	}
	if l.stats.finalDropped != 1 {
		t.Fatalf("队列满时丢弃并计数：%+v", l.stats)
	}
}

// 工作协程被会话刷新卡住时，释放照样立即返回。
func TestKongLivenessReleaseDoesNotWaitForWorker(t *testing.T) {
	sessions := newKongFakeLivenessSessions()
	sessions.idle[1] = 5 * time.Minute
	sessions.block = make(chan struct{})
	sessions.entered = make(chan struct{}, 1)
	clock := &kongLivenessClock{t: time.Now()}
	l := newKongRequestLiveness(&kongFakeSlotRefresher{}, sessions, clock.now, 10*time.Millisecond)
	l.track(0, 1, "", 1, "h1")
	release := l.track(KongSlotAccount, 2, "r2", 1, "h1")
	l.Start()
	defer func() {
		close(sessions.block)
		l.Stop()
	}()

	select {
	case <-sessions.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("工作协程没有开始续期")
	}
	done := make(chan struct{})
	go func() {
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("释放在等工作协程")
	}
}

func TestKongLivenessStopWithoutStart(t *testing.T) {
	l, _ := newKongLivenessForTest(nil, nil)
	stopped := make(chan struct{})
	go func() {
		l.Stop()
		l.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("未启动时 Stop 不应阻塞")
	}
	var nilLiveness *KongRequestLiveness
	nilLiveness.Start()
	nilLiveness.Stop()
}

// kongTrackFakeCache 记录 ConcurrencyService 写进 Redis 的占用与释放。
type kongTrackFakeCache struct {
	ConcurrencyCache
	mu       sync.Mutex
	released []string
}

func (c *kongTrackFakeCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}

func (c *kongTrackFakeCache) ReleaseAccountSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, "account:"+requestID)
	return nil
}

func (c *kongTrackFakeCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}

func (c *kongTrackFakeCache) ReleaseUserSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, "user:"+requestID)
	return nil
}

func (c *kongTrackFakeCache) TrackAPIKeySlot(context.Context, int64, string) error { return nil }

func (c *kongTrackFakeCache) ReleaseAPIKeySlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, "api_key:"+requestID)
	return nil
}

func (c *kongTrackFakeCache) GetAPIKeyConcurrencyBatch(context.Context, []int64) (map[int64]int, error) {
	return nil, nil
}

func kongLivenessEntrySnapshot(l *KongRequestLiveness) []kongLivenessEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]kongLivenessEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, *e)
	}
	return out
}

func TestConcurrencyServiceKongTrack(t *testing.T) {
	cache := &kongTrackFakeCache{}
	svc := NewConcurrencyService(cache)
	l, _ := newKongLivenessForTest(&kongFakeSlotRefresher{}, newKongFakeLivenessSessions())
	svc.SetKongRequestLiveness(l)
	ctx := KongWithSessionCountHash(context.Background(), "h1")

	t.Run("account slot carries the session", func(t *testing.T) {
		result, err := svc.AcquireAccountSlot(ctx, 1, 5)
		if err != nil || !result.Acquired {
			t.Fatalf("acquire: %+v %v", result, err)
		}
		entries := kongLivenessEntrySnapshot(l)
		if len(entries) != 1 || entries[0].kind != KongSlotAccount || entries[0].accountID != 1 || entries[0].hash != "h1" || entries[0].requestID == "" {
			t.Fatalf("entries=%+v", entries)
		}
		result.ReleaseFunc()
		if len(kongLivenessEntrySnapshot(l)) != 0 || len(cache.released) != 1 || drainKongLivenessFinals(l) != 1 {
			t.Fatalf("释放要注销、排结束刷新、做原有的释放：released=%v", cache.released)
		}
	})

	t.Run("unlimited account slot renews only the session", func(t *testing.T) {
		result, err := svc.AcquireAccountSlot(ctx, 2, 0)
		if err != nil || !result.Acquired {
			t.Fatalf("acquire: %+v %v", result, err)
		}
		entries := kongLivenessEntrySnapshot(l)
		if len(entries) != 1 || entries[0].kind != 0 || entries[0].accountID != 2 {
			t.Fatalf("不限并发时只登记会话部分：%+v", entries)
		}
		result.ReleaseFunc()
		if len(kongLivenessEntrySnapshot(l)) != 0 || drainKongLivenessFinals(l) != 1 {
			t.Fatal("释放后注销并排结束刷新")
		}

		result, _ = svc.AcquireAccountSlot(context.Background(), 2, 0)
		if len(kongLivenessEntrySnapshot(l)) != 0 {
			t.Fatal("不限并发又没有会话时不登记")
		}
		result.ReleaseFunc()
	})

	t.Run("user and api key slots carry no session", func(t *testing.T) {
		result, err := svc.AcquireUserSlot(ctx, 9, 5)
		if err != nil || !result.Acquired {
			t.Fatalf("acquire: %+v %v", result, err)
		}
		releaseKey := svc.TrackAPIKeySlot(ctx, 8)
		entries := kongLivenessEntrySnapshot(l)
		if len(entries) != 2 {
			t.Fatalf("entries=%+v", entries)
		}
		for _, e := range entries {
			if e.accountID != 0 || (e.kind != KongSlotUser && e.kind != KongSlotAPIKey) {
				t.Fatalf("用户槽与 API Key 占用不带会话：%+v", e)
			}
		}
		result.ReleaseFunc()
		releaseKey()
		if len(kongLivenessEntrySnapshot(l)) != 0 || drainKongLivenessFinals(l) != 0 {
			t.Fatal("释放后注销，且不排结束刷新")
		}
	})

	t.Run("without liveness the release is unchanged", func(t *testing.T) {
		plain := NewConcurrencyService(cache)
		result, _ := plain.AcquireAccountSlot(ctx, 1, 5)
		before := len(cache.released)
		result.ReleaseFunc()
		if len(cache.released) != before+1 {
			t.Fatal("未装配续期器时释放照旧")
		}
	})
}

// RefreshSession 与 Redis 脚本一致：只刷新已存在的成员，不加入。
func (f *kongFakeSessionCache) RefreshSession(_ context.Context, accountID int64, hash string, _ time.Duration) error {
	if f.has(accountID, hash) {
		f.put(accountID, hash)
	}
	return nil
}

// 选号先抢槽、再确认会话名额：确认被拒的账号，续期与结束刷新都不能把会话计入。
func TestKongLivenessDoesNotRegisterRejectedAttempt(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
	l := newKongRequestLiveness(nil, h.svc, func() time.Time { return *h.clock }, time.Hour)
	h.svc.concurrencyService.SetKongRequestLiveness(l)
	h.cache.stealOnRegister[1] = "thief"

	ctx := KongWithSessionCountHash(context.Background(), "s-new")
	selection := h.selectAccount(t, ctx, "s-new")
	if selection.Account.ID != 2 {
		t.Fatalf("1 的最后一个名额被抢，应换到 2，got %d", selection.Account.ID)
	}
	drainKongLivenessFinals(l)
	*h.clock = h.clock.Add(time.Minute)
	l.cycle()
	if h.cache.has(1, "s-new") {
		t.Fatalf("被拒的账号上不能出现这个会话：%v", h.cache.sessions)
	}
	if got := h.cache.sessions[2]["s-new"]; !got.Equal(*h.clock) {
		t.Fatalf("实际接纳的账号要被续期到当前时刻：got %v want %v", got, *h.clock)
	}

	selection.ReleaseFunc()
	*h.clock = h.clock.Add(time.Minute)
	drainKongLivenessFinals(l)
	if got := h.cache.sessions[2]["s-new"]; !got.Equal(*h.clock) {
		t.Fatalf("结束刷新从释放时刻起计：got %v want %v", got, *h.clock)
	}
}

// 抢到槽之后、确认之前正好续期一次：只刷新已存在的成员，不能让确认误判为已有会话。
func TestKongLivenessRefreshBeforeConfirmDoesNotAdmit(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1)}, stubConcurrencyCache{}, nil)
	h.cache.put(1, "other")
	l := newKongRequestLiveness(nil, h.svc, func() time.Time { return *h.clock }, time.Hour)
	l.track(0, 1, "", 1, "s-new")
	l.cycle()
	if h.cache.has(1, "s-new") {
		t.Fatalf("续期不能加入还没被接纳的会话：%v", h.cache.sessions)
	}
}

func TestKongLivenessIdleTimeoutReadsCurrentAccount(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 2)}, stubConcurrencyCache{}, nil)
	idle, err := h.svc.kongLivenessIdleTimeout(context.Background(), 1)
	if err != nil || idle != 15*time.Minute {
		t.Fatalf("idle=%v err=%v", idle, err)
	}
	if _, err := h.svc.kongLivenessIdleTimeout(context.Background(), 99); err == nil {
		t.Fatal("读不到账号要报错")
	}
}

// kongBlockingSlotRefresher 让第一次刷新一直阻塞到上下文被取消，用来验证停止会放弃本批剩余的调用。
type kongBlockingSlotRefresher struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	exitErr error // 首个调用退出时上下文的错误
}

func (f *kongBlockingSlotRefresher) KongRefreshSlot(ctx context.Context, _ KongSlotKind, _ int64, _ string) (bool, error) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if first {
		f.entered <- struct{}{}
		<-ctx.Done()
		f.mu.Lock()
		f.exitErr = ctx.Err()
		f.mu.Unlock()
		return false, ctx.Err()
	}
	return true, nil
}

func TestKongLivenessStopAbandonsRemainingBatch(t *testing.T) {
	slots := &kongBlockingSlotRefresher{entered: make(chan struct{}, 1)}
	sessions := newKongFakeLivenessSessions()
	sessions.idle[1] = 5 * time.Minute
	clock := &kongLivenessClock{t: time.Now()}
	l := newKongRequestLiveness(slots, sessions, clock.now, 10*time.Millisecond)
	for i := 0; i < 20; i++ {
		l.track(KongSlotAccount, int64(i+1), "r", 0, "")
	}
	release := l.track(0, 1, "", 1, "h1")
	l.Start()

	select {
	case <-slots.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("工作协程没有开始续期")
	}
	release() // 结束刷新排进队列，但停止后不应再处理
	stopped := make(chan struct{})
	go func() {
		l.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 应取消正在进行的调用并尽快返回")
	}
	slots.mu.Lock()
	calls, exitErr := slots.calls, slots.exitErr
	slots.mu.Unlock()
	if !errors.Is(exitErr, context.Canceled) {
		t.Fatalf("正在进行的调用应被 Stop 取消，而不是等到超时：%v", exitErr)
	}
	if calls != 1 {
		t.Fatalf("停止后不能再发起本批剩余的调用，got %d 次", calls)
	}
	if n := len(sessions.refreshed); n != 0 {
		t.Fatalf("停止后不再处理会话刷新与结束刷新，got %d", n)
	}
}

// kongGatedLivenessSessions 只让指定会话的刷新阻塞，并在每次刷新后发信号。
type kongGatedLivenessSessions struct {
	*kongFakeLivenessSessions
	blockHash string
	gate      chan struct{}
	entered   chan struct{}
	notify    chan kongLivenessSessionRef
}

func (f *kongGatedLivenessSessions) kongLivenessRefreshSession(ctx context.Context, accountID int64, hash string, idle time.Duration) error {
	if hash == f.blockHash {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		<-f.gate
	}
	err := f.kongFakeLivenessSessions.kongLivenessRefreshSession(ctx, accountID, hash, idle)
	select {
	case f.notify <- kongLivenessSessionRef{accountID: accountID, hash: hash}:
	default:
	}
	return err
}

// 真实工作循环里：周期续期卡住时，经包装的释放照样立即完成；结束刷新随后恰好执行一次，注销项不再参与后续周期。
func TestKongLivenessWrappedReleaseWithRunningWorker(t *testing.T) {
	cache := &kongTrackFakeCache{}
	svc := NewConcurrencyService(cache)
	sessions := &kongGatedLivenessSessions{
		kongFakeLivenessSessions: newKongFakeLivenessSessions(),
		blockHash:                "blocker",
		gate:                     make(chan struct{}),
		entered:                  make(chan struct{}, 1),
		notify:                   make(chan kongLivenessSessionRef, 64),
	}
	sessions.idle[1] = 5 * time.Minute
	sessions.idle[2] = 5 * time.Minute
	slots := &kongFakeSlotRefresher{}
	clock := &kongLivenessClock{t: time.Now()}
	l := newKongRequestLiveness(slots, sessions, clock.now, 10*time.Millisecond)
	svc.SetKongRequestLiveness(l)
	l.track(0, 1, "", 1, "blocker")
	result, err := svc.AcquireAccountSlot(KongWithSessionCountHash(context.Background(), "target"), 2, 5)
	if err != nil || !result.Acquired {
		t.Fatalf("acquire: %+v %v", result, err)
	}
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(func() { close(sessions.gate) }) }
	// 清理按注册的逆序执行：先解开闸门，再停止，测试中途失败时工作协程也能退出。
	t.Cleanup(l.Stop)
	t.Cleanup(unlock)
	l.Start()

	select {
	case <-sessions.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("工作协程没有卡在周期续期上")
	}
	released := make(chan struct{})
	go func() {
		result.ReleaseFunc()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("释放在等工作协程")
	}
	cache.mu.Lock()
	if len(cache.released) != 1 {
		t.Fatalf("原有释放要立即执行：%v", cache.released)
	}
	cache.mu.Unlock()
	before := sessions.refreshCount(2, "target")
	slots.mu.Lock()
	slotCalls := len(slots.calls)
	slots.mu.Unlock()

	unlock()
	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for !cond() {
			select {
			case <-sessions.notify:
			case <-deadline:
				t.Fatal(msg)
			}
		}
	}
	waitFor(func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.stats.finalRefreshes == 1
	}, "结束刷新没有执行")
	// 卡住的那一批在释放前已取出快照，放行后可能再刷新它一次，然后才是结束刷新。
	after := sessions.refreshCount(2, "target")
	if after != before+1 && after != before+2 {
		t.Fatalf("放行后最多是本批一次加结束刷新一次：before=%d after=%d", before, after)
	}
	blockerSeen := sessions.refreshCount(1, "blocker")
	waitFor(func() bool { return sessions.refreshCount(1, "blocker") >= blockerSeen+2 }, "后续周期没有继续")
	if got := sessions.refreshCount(2, "target"); got != after {
		t.Fatalf("结束刷新之后不再续期：got %d want %d", got, after)
	}
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if len(slots.calls) != slotCalls {
		t.Fatalf("释放后不再刷新它的并发占用：%+v", slots.calls[slotCalls:])
	}
}

func TestKongTrackReleaseSemantics(t *testing.T) {
	svc := NewConcurrencyService(&kongTrackFakeCache{})
	l, _ := newKongLivenessForTest(&kongFakeSlotRefresher{}, newKongFakeLivenessSessions())
	svc.SetKongRequestLiveness(l)
	ctx := KongWithSessionCountHash(context.Background(), "h1")

	t.Run("nil original release", func(t *testing.T) {
		release := svc.kongTrack(ctx, KongSlotAccount, 1, "r-nil", nil)
		release()
		if len(kongLivenessEntrySnapshot(l)) != 0 || drainKongLivenessFinals(l) != 1 {
			t.Fatal("原释放为 nil 时照样注销并排一次结束刷新")
		}
	})

	t.Run("repeated and concurrent release", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		release := svc.kongTrack(ctx, KongSlotAccount, 1, "r-many", func() {
			mu.Lock()
			calls++
			mu.Unlock()
		})
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				release()
			}()
		}
		wg.Wait()
		release()
		if n := drainKongLivenessFinals(l); n != 1 {
			t.Fatalf("注销与结束刷新只发生一次，got %d", n)
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 11 {
			t.Fatalf("原释放的调用次数保持原样（每次都调用），got %d", calls)
		}
	})
}
