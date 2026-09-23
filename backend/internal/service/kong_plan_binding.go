package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// 粘性会话、续接与 WS 逐轮复核（fork 专有，设计见 DESIGN-account-selection.md 第 2.4、4.3 节）。
//
// 绑定的值只有账号编号，"绑定晚于入层"用标记表达：会话或响应被分到此刻处于 credits 层的账号时，先写标记再写绑定。
// 复用绑定时做两项检查：账号此刻能不能用（定层不是按现状暂停）；账号在 credits 层而没有对应的标记，就是入层之前的
// 绑定，换号一次。账号作为 credits 层被选中、写标记或复用之前都先登记入层（enterCredits）。计划输入未生效时这里全部
// 是空操作。

// KongPlanBindingStore 是会话标记、续接标记与会话绑定条件替换的存储。
type KongPlanBindingStore interface {
	// MarkSession 写会话标记；已有时只续期，标记保留第一次写下的时刻 at。
	MarkSession(ctx context.Context, groupID int64, key string, accountID, seq int64, at time.Time, ttl time.Duration) error
	// TouchSession 续期会话标记，返回标记第一次写下的时刻（读不出时为零值）与标记是否存在。
	TouchSession(ctx context.Context, groupID int64, key string, accountID, seq int64, ttl time.Duration) (time.Time, bool, error)
	MarkResponse(ctx context.Context, groupID int64, responseID string, accountID, seq int64, ttl time.Duration) error
	HasResponse(ctx context.Context, groupID int64, responseID string, accountID, seq int64) (bool, error)
	// ReplaceSessionBinding 在会话绑定的值仍是 from 时改成 to，返回是否改了。
	ReplaceSessionBinding(ctx context.Context, groupID int64, key string, from, to int64, ttl time.Duration) (bool, error)
}

// kongPlanReselectTTL 是"入层换号、旧账号 A"的记录保留多久：选号到最终准入之间的时长，以秒计。
const kongPlanReselectTTL = 10 * time.Minute

// kongPlanReselects 记下因入层换号而跳过旧账号的会话，供最终准入后条件替换绑定。进程内即可：网关单实例，记录丢了
// 也只是下一次请求再换一次。
type kongPlanReselects struct {
	mu sync.Mutex
	m  map[string]kongPlanReselect
}

type kongPlanReselect struct {
	from int64
	at   time.Time
}

var kongPlanPendingReselects = &kongPlanReselects{m: map[string]kongPlanReselect{}}

func kongPlanReselectKey(groupID int64, key string) string {
	return strconv.FormatInt(groupID, 10) + "|" + key
}

func (r *kongPlanReselects) note(groupID int64, key string, from int64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) >= 10000 {
		for k, v := range r.m {
			if now.Sub(v.at) > kongPlanReselectTTL {
				delete(r.m, k)
			}
		}
		if len(r.m) >= 10000 {
			r.m = map[string]kongPlanReselect{}
		}
	}
	r.m[kongPlanReselectKey(groupID, key)] = kongPlanReselect{from: from, at: now}
}

func (r *kongPlanReselects) take(groupID int64, key string, now time.Time) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := kongPlanReselectKey(groupID, key)
	v, ok := r.m[k]
	if !ok {
		return 0, false
	}
	delete(r.m, k)
	return v.from, now.Sub(v.at) <= kongPlanReselectTTL
}

// kongPlanActive 返回计划输入此刻生效时的运行时与时刻。
func kongPlanActive() (*kongPlanRuntime, time.Time, bool) {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return nil, time.Time{}, false
	}
	now := rt.now()
	return rt, now, rt.store.current().inUse(now) != KongPlanInUseLegacy
}

// kongPlanAccount 读取账号当前的状态（调度快照优先）。账号已不存在时返回 ErrAccountNotFound，其余读取失败原样返回。
func (s *OpenAIGatewayService) kongPlanAccount(ctx context.Context, id int64) (*Account, error) {
	var (
		acc *Account
		err error
	)
	switch {
	case s.schedulerSnapshot != nil:
		acc, err = s.schedulerSnapshot.GetAccount(ctx, id)
	case s.accountRepo != nil:
		acc, err = s.accountRepo.GetByID(ctx, id)
	default:
		return nil, errors.New("kong plan: no account source")
	}
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, ErrAccountNotFound
	}
	return acc, nil
}

// kongPlanStickyTTL 是会话标记的有效期：不短于两种粘性绑定的有效期，命中时续期。
func (s *OpenAIGatewayService) kongPlanStickyTTL(ttl time.Duration) time.Duration {
	for _, d := range []time.Duration{openaiStickySessionTTL, s.openAIWSSessionStickyTTL()} {
		if d > ttl {
			ttl = d
		}
	}
	return ttl
}

// kongPlanBindTTL 与 BindStickySession 用的有效期相同。
func (s *OpenAIGatewayService) kongPlanBindTTL() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return openaiStickySessionTTL
}

// kongPlanMarkSession 在写会话绑定之前调用：账号此刻在 credits 层时先登记入层、再写会话标记，任一步写不进去就返回
// 错误，调用方不写这份绑定。读不到账号时判不了层，同样不写。key 是绑定用的粘性键。
func (s *OpenAIGatewayService) kongPlanMarkSession(ctx context.Context, groupID *int64, key string, accountID int64, ttl time.Duration) error {
	rt, now, ok := kongPlanActive()
	if !ok || rt.bindings == nil || key == "" {
		return nil
	}
	acc, err := s.kongPlanAccount(ctx, accountID)
	if err != nil {
		kongPlanWarn("kong plan: 读不到账号，这次不绑定", "account_id", accountID, "error", err)
		return fmt.Errorf("kong plan: account: %w", err)
	}
	if !acc.IsOpenAIOAuth() {
		return nil
	}
	res := rt.tier(ctx, acc, now)
	if res.Layer != kongPlanLayerCredits {
		rt.observeLayer(ctx, acc, res, now)
		return nil
	}
	if err := rt.enterCredits(ctx, acc, res, now); err != nil {
		return fmt.Errorf("kong plan: entry: %w", err)
	}
	if err := rt.bindings.MarkSession(ctx, derefGroupID(groupID), key, accountID, res.WindowSeq, now, s.kongPlanStickyTTL(ttl)); err != nil {
		kongPlanWarn("kong plan: 会话标记写入失败，这次不绑定", "account_id", accountID, "error", err)
		return fmt.Errorf("kong plan: session marker: %w", err)
	}
	return nil
}

// kongPlanGuardianOwner 是复用检查要看的会话：guardian 亲和复用的是父会话的绑定，按父会话的哈希检查。
func kongPlanGuardianOwner(ctx context.Context, sessionHash string, preserve bool) string {
	if preserve {
		if affinity, ok := openAIGuardianParentAffinityFromContext(ctx); ok && affinity.currentSessionHash != "" {
			return affinity.currentSessionHash
		}
	}
	return sessionHash
}

// kongPlanReuseSticky 是复用会话绑定前的两项检查，返回 false 时不复用、也不删绑定，调用方按"绑定账号暂不可用"
// 落到正常选号。ownerHash 是绑定所有者的会话哈希；reselect 为真时记下"入层换号、旧账号"，供最终准入后条件替换
// 绑定（guardian 不改父绑定，传 false）。
func (s *OpenAIGatewayService) kongPlanReuseSticky(ctx context.Context, groupID *int64, ownerHash string, account *Account, reselect bool) bool {
	rt, now, ok := kongPlanActive()
	if !ok || account == nil || !account.IsOpenAIOAuth() {
		return true
	}
	res := rt.tier(ctx, account, now)
	switch res.Layer {
	case kongPlanLayerPaused:
		return false
	case kongPlanLayerCredits:
		key := s.openAISessionCacheKey(ownerHash)
		if rt.bindings != nil && key != "" {
			markedAt, found, err := rt.bindings.TouchSession(ctx, derefGroupID(groupID), key, account.ID, res.WindowSeq, s.kongPlanStickyTTL(0))
			if err != nil {
				kongPlanWarn("kong plan: 会话标记读取失败，照常复用绑定", "account_id", account.ID, "error", err)
			} else if !found {
				if reselect {
					kongPlanPendingReselects.note(derefGroupID(groupID), key, account.ID, now)
				}
				return false
			}
			if rt.enterCredits(ctx, account, res, now) != nil {
				return false
			}
			rt.recordDwell(account.ID, markedAt, now)
			return true
		}
		return rt.enterCredits(ctx, account, res, now) == nil
	}
	rt.observeLayer(ctx, account, res, now)
	return true
}

// kongPlanReplaceBinding 是最终准入后写会话绑定处的挂点：这个会话因入层换号跳过了旧账号、最终落到另一个账号时，
// 先写新账号的会话标记，再把绑定从旧账号条件替换为新账号（值仍是旧账号才改）。返回 true 表示已处理，调用方不再写。
func (s *OpenAIGatewayService) kongPlanReplaceBinding(ctx context.Context, groupID *int64, sessionHash string, accountID int64) bool {
	rt, now, ok := kongPlanActive()
	if !ok || rt.bindings == nil {
		return false
	}
	key := s.openAISessionCacheKey(sessionHash)
	if key == "" {
		return false
	}
	from, ok := kongPlanPendingReselects.take(derefGroupID(groupID), key, now)
	if !ok || from == accountID {
		return false
	}
	ttl := s.kongPlanBindTTL()
	if err := s.kongPlanMarkSession(ctx, groupID, key, accountID, ttl); err != nil {
		return true
	}
	replaced, err := rt.bindings.ReplaceSessionBinding(ctx, derefGroupID(groupID), key, from, accountID, ttl)
	if err != nil {
		kongPlanWarn("kong plan: 入层换号的绑定替换失败", "from", from, "to", accountID, "error", err)
		return false
	}
	return replaced
}

// kongPlanReuseResponse 是续接复用前的两项检查：入层之前产出的响应（账号在 credits 层而没有续接标记）跳过，
// 不删也不替换——换到别的账号本来就接不上。
func (s *OpenAIGatewayService) kongPlanReuseResponse(ctx context.Context, groupID int64, responseID string, account *Account) bool {
	rt, now, ok := kongPlanActive()
	if !ok || account == nil || !account.IsOpenAIOAuth() {
		return true
	}
	res := rt.tier(ctx, account, now)
	switch res.Layer {
	case kongPlanLayerPaused:
		return false
	case kongPlanLayerCredits:
		found := true
		if rt.bindings != nil {
			var err error
			found, err = rt.bindings.HasResponse(ctx, groupID, normalizeOpenAIWSResponseID(responseID), account.ID, res.WindowSeq)
			if err != nil {
				kongPlanWarn("kong plan: 续接标记读取失败，照常续接", "account_id", account.ID, "error", err)
				found = true
			}
		}
		return found && rt.enterCredits(ctx, account, res, now) == nil
	}
	rt.observeLayer(ctx, account, res, now)
	return true
}

// kongPlanKeepStore 在计划输入生效时不删除续接绑定：账号到阈值又没有 credits 资格时，旧版选号的复用路径会删掉绑定，
// 资格恢复后响应就再也接不上了。改为只跳过；绑定有 TTL，真正失效的账号随 TTL 清掉。
type kongPlanKeepStore struct {
	OpenAIWSStateStore
}

func (s kongPlanKeepStore) DeleteResponseAccount(ctx context.Context, groupID int64, responseID string) error {
	if _, _, ok := kongPlanActive(); ok {
		return nil
	}
	return s.OpenAIWSStateStore.DeleteResponseAccount(ctx, groupID, responseID)
}

// kongPlanKeepResponseBindings 是续接复用入口的挂点：计划组件装配后包一层，删除时按此刻是否生效决定。
func kongPlanKeepResponseBindings(store OpenAIWSStateStore) OpenAIWSStateStore {
	if store == nil || kongPlanRuntimeNow() == nil {
		return store
	}
	return kongPlanKeepStore{OpenAIWSStateStore: store}
}

// kongPlanMarkResponse 在写响应绑定之前调用：账号此刻在 credits 层时登记入层、写续接标记。写失败只记日志，绑定照常写。
// 按最新读数定层（长连接持有的账号对象不随额度刷新更新）；读不到账号时不写标记。响应完成后客户端常常立刻断开，
// 写入用脱离请求取消、有限超时的上下文，与 HTTP 的响应绑定同一写法。
func (s *OpenAIGatewayService) kongPlanMarkResponse(ctx context.Context, groupID int64, responseID string, account *Account, ttl time.Duration) {
	rt, now, ok := kongPlanActive()
	id := normalizeOpenAIWSResponseID(responseID)
	if !ok || rt.bindings == nil || account == nil || !account.IsOpenAIOAuth() || id == "" {
		return
	}
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithTimeout(base, openAIWSStateStoreRedisTimeout)
	defer cancel()
	current, err := s.kongPlanAccount(ctx, account.ID)
	if err != nil {
		kongPlanWarn("kong plan: 读不到账号，不写续接标记", "account_id", account.ID, "error", err)
		return
	}
	res := rt.tier(ctx, current, now)
	if res.Layer != kongPlanLayerCredits {
		rt.observeLayer(ctx, current, res, now)
		return
	}
	if rt.enterCredits(ctx, current, res, now) != nil {
		return
	}
	if err := rt.bindings.MarkResponse(ctx, groupID, id, account.ID, res.WindowSeq, normalizeOpenAIWSTTL(ttl)); err != nil {
		kongPlanWarn("kong plan: 续接标记写入失败，绑定照常写", "account_id", account.ID, "error", err)
	}
}

// KongPlanWSState 是 WS 建连时记下的定层。
type KongPlanWSState struct {
	credits bool
	seq     int64
}

// KongPlanWSBegin 在 WS 建连选定账号后调用，记下账号是否在 credits 层与窗口序号。credits 层的账号登记入层失败时
// 按不在 credits 层记下，逐轮复核随即以"请重连"关闭。
func (s *OpenAIGatewayService) KongPlanWSBegin(ctx context.Context, account *Account) KongPlanWSState {
	rt, now, ok := kongPlanActive()
	if !ok || account == nil || !account.IsOpenAIOAuth() {
		return KongPlanWSState{}
	}
	res := rt.tier(ctx, account, now)
	rt.observeLayer(ctx, account, res, now)
	credits := res.Layer == kongPlanLayerCredits && rt.enterCredits(ctx, account, res, now) == nil
	return KongPlanWSState{credits: credits, seq: res.WindowSeq}
}

// KongPlanWSTurnAllowed 是 WS 逐轮复核：账号此刻已不可服务（暂停、限流、人工停用、移出白名单、余额到保底、到作废时刻、
// 到阈值而不能用 credits），或者此刻在 credits 层、而建连时不在或窗口序号不同，返回 false，调用方以"请重连"关闭。
// 读取账号当前的状态：连接活得很久，建连时的读数早已过时。
func (s *OpenAIGatewayService) KongPlanWSTurnAllowed(ctx context.Context, account *Account, st KongPlanWSState) bool {
	rt, now, ok := kongPlanActive()
	if !ok || account == nil || !account.IsOpenAIOAuth() {
		return true
	}
	current, err := s.kongPlanAccount(ctx, account.ID)
	switch {
	case errors.Is(err, ErrAccountNotFound):
		return false
	case err != nil:
		current = account // 临时读取失败：按建连时的对象判断
	}
	if !rt.serviceable(ctx, current, now) {
		return false
	}
	res := rt.tier(ctx, current, now)
	switch res.Layer {
	case kongPlanLayerPaused:
		return false
	case kongPlanLayerCredits:
		return st.credits && st.seq == res.WindowSeq && rt.enterCredits(ctx, current, res, now) == nil
	}
	rt.observeLayer(ctx, current, res, now)
	return true
}
