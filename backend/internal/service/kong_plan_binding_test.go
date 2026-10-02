//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 复用绑定时的两项检查、会话与续接标记、入层换号的条件替换、WS 逐轮复核。

type kongPlanFakeBindings struct {
	sessions  map[string]time.Duration
	markedAt  map[string]time.Time
	responses map[string]time.Duration
	replaced  []string
	markErr   error
	touchErr  error
}

func newKongPlanFakeBindings() *kongPlanFakeBindings {
	return &kongPlanFakeBindings{sessions: map[string]time.Duration{}, responses: map[string]time.Duration{}}
}

func kongPlanMarkerKey(groupID int64, key string, accountID, seq int64) string {
	return fmt.Sprintf("%d|%s|%d|%d", groupID, key, accountID, seq)
}

func (f *kongPlanFakeBindings) MarkSession(_ context.Context, groupID int64, key string, accountID, seq int64, at time.Time, ttl time.Duration) error {
	if f.markErr != nil {
		return f.markErr
	}
	k := kongPlanMarkerKey(groupID, key, accountID, seq)
	f.sessions[k] = ttl
	if f.markedAt == nil {
		f.markedAt = map[string]time.Time{}
	}
	if _, ok := f.markedAt[k]; !ok {
		f.markedAt[k] = at
	}
	return nil
}

func (f *kongPlanFakeBindings) TouchSession(_ context.Context, groupID int64, key string, accountID, seq int64, ttl time.Duration) (time.Time, bool, error) {
	if f.touchErr != nil {
		return time.Time{}, false, f.touchErr
	}
	k := kongPlanMarkerKey(groupID, key, accountID, seq)
	if _, ok := f.sessions[k]; !ok {
		return time.Time{}, false, nil
	}
	f.sessions[k] = ttl
	return f.markedAt[k], true, nil
}

func (f *kongPlanFakeBindings) MarkResponse(ctx context.Context, groupID int64, responseID string, accountID, seq int64, ttl time.Duration) error {
	if f.markErr != nil {
		return f.markErr
	}
	if err := ctx.Err(); err != nil {
		return err // 与 Redis 客户端一样，上下文已取消就写不进
	}
	f.responses[kongPlanMarkerKey(groupID, responseID, accountID, seq)] = ttl
	return nil
}

func (f *kongPlanFakeBindings) HasResponse(_ context.Context, groupID int64, responseID string, accountID, seq int64) (bool, error) {
	_, ok := f.responses[kongPlanMarkerKey(groupID, responseID, accountID, seq)]
	return ok, nil
}

func (f *kongPlanFakeBindings) ReplaceSessionBinding(_ context.Context, groupID int64, key string, from, to int64, _ time.Duration) (bool, error) {
	f.replaced = append(f.replaced, fmt.Sprintf("%d|%s|%d->%d", groupID, key, from, to))
	return true, nil
}

// newKongPlanBindingHarness：账号 1 在 credits 层（窗口序号 4），账号 2 在第一层；都不设会话上限。
func newKongPlanBindingHarness(t *testing.T, bindings map[string]int64) (*kongPlanSelHarness, *kongPlanFakeBindings) {
	t.Helper()
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 0, 99), th.selAccount(2, 5, 0, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.creditsEntry(1, 1)+"]", 1)
	for k, v := range bindings {
		h.sess.sticky.sessionBindings[k] = v
	}
	fake := newKongPlanFakeBindings()
	h.rt.bindings = fake
	kongPlanPendingReselects.m = map[string]kongPlanReselect{}
	return h, fake
}

func TestKongPlanSticky_BindingBeforeEntryReselectsOnce(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	h.sess.sticky.sessionBindings = map[string]int64{"openai:s1": 1}

	sel := h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(2), sel.Account.ID, "入层之前的绑定不复用，第一层的账号先于 credits")
	require.Equal(t, int64(2), h.sess.sticky.sessionBindings["openai:s1"], "第 2 层按新账号绑定")
	from, ok := kongPlanPendingReselects.take(1, "openai:s1", h.now)
	require.True(t, ok)
	require.Equal(t, int64(1), from, "记下入层换号的旧账号")

	// 最终准入后的绑定：条件替换旧绑定
	kongPlanPendingReselects.note(1, "openai:s1", 1, h.now)
	group := int64(1)
	require.NoError(t, h.sess.svc.BindStickySessionAfterProfitAdmission(context.Background(), &group, "s1", 2))
	require.Equal(t, []string{"1|openai:s1|1->2"}, fake.replaced)
	_, ok = kongPlanPendingReselects.take(1, "openai:s1", h.now)
	require.False(t, ok, "记录用过即删")
}

func TestKongPlanSticky_MarkerKeepsBinding(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	h.sess.sticky.sessionBindings = map[string]int64{"openai:s1": 1}
	fake.sessions[kongPlanMarkerKey(1, "openai:s1", 1, 4)] = time.Minute

	sel := h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(1), sel.Account.ID, "入层之后的绑定照常复用")
	require.Greater(t, fake.sessions[kongPlanMarkerKey(1, "openai:s1", 1, 4)], time.Minute, "命中时续期标记")

	// 新窗口序号是新的入层
	h.setPace(1, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 5,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	sel = h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(2), sel.Account.ID)
}

func TestKongPlanSticky_ReselectBackToSameAccountWritesMarker(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	h.sess.svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{h.selAccount(1, 1, 0, 99)}}
	h.sess.sticky.sessionBindings = map[string]int64{"openai:s1": 1}

	sel := h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(1), sel.Account.ID)
	require.Contains(t, fake.sessions, kongPlanMarkerKey(1, "openai:s1", 1, 4), "重新选回 credits 层的账号时先写标记")
	require.Len(t, h.ledger.Entries(1), 1)

	sel = h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(1), sel.Account.ID, "之后保持")
}

func TestKongPlanSticky_MarkerWriteFailureSkipsBinding(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	h.sess.svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{h.selAccount(1, 1, 0, 99)}}
	fake.markErr = errors.New("redis down")

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID)
	_, bound := h.sess.sticky.sessionBindings["openai:s-new"]
	require.False(t, bound, "标记写不进去就不写这份绑定")

	// 第一层的账号不写标记，绑定照常
	fake.markErr = nil
	h.sess.svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{h.selAccount(2, 5, 0, 50)}}
	fake.markErr = errors.New("redis down")
	sel = h.sess.selectAccount(t, context.Background(), "s-2")
	require.Equal(t, int64(2), sel.Account.ID)
	require.Equal(t, int64(2), h.sess.sticky.sessionBindings["openai:s-2"])
}

func TestKongPlanSticky_PausedAccountNotReused(t *testing.T) {
	h, _ := newKongPlanBindingHarness(t, nil)
	h.sess.sticky.sessionBindings = map[string]int64{"openai:s1": 1}
	h.control(t, 20, `"shadow"`, "[1]", 0)

	sel := h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(2), sel.Account.ID, "到阈值而不能用 credits：此刻不可服务，不复用")
	_, ok := kongPlanPendingReselects.take(1, "openai:s1", h.now)
	require.False(t, ok, "不可服务不是入层换号")
}

func TestKongPlanSticky_GuardianChecksParentAndNeverNotes(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	ctx := context.WithValue(context.Background(), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent"})
	require.Equal(t, "parent", kongPlanGuardianOwner(ctx, "legacy-hash", true))
	require.Equal(t, "own", kongPlanGuardianOwner(ctx, "own", false))

	acc := h.selAccount(1, 1, 0, 99)
	require.False(t, h.sess.svc.kongPlanReuseSticky(ctx, nil, "parent", &acc, false))
	_, ok := kongPlanPendingReselects.take(0, "openai:parent", h.now)
	require.False(t, ok, "guardian 不改父绑定，不记换号")
	fake.sessions[kongPlanMarkerKey(0, "openai:parent", 1, 4)] = time.Minute
	require.True(t, h.sess.svc.kongPlanReuseSticky(ctx, nil, "parent", &acc, false), "父会话入层之后的绑定，guardian 跟着走")

	fake.touchErr = errors.New("redis down")
	delete(fake.sessions, kongPlanMarkerKey(0, "openai:parent", 1, 4))
	require.True(t, h.sess.svc.kongPlanReuseSticky(ctx, nil, "parent", &acc, false), "标记读不到时照常复用")
}

func TestKongPlanResponse_MarkerBeforeBindingAndCheck(t *testing.T) {
	h, fake := newKongPlanBindingHarness(t, nil)
	credits, sub := h.selAccount(1, 1, 0, 99), h.selAccount(2, 5, 0, 50)
	svc := h.sess.svc

	require.False(t, svc.kongPlanReuseResponse(context.Background(), 7, "resp_1", &credits), "入层之前产出的响应不续接")
	svc.kongPlanMarkResponse(context.Background(), 7, "resp_1", &credits, time.Hour)
	require.Equal(t, time.Hour, fake.responses[kongPlanMarkerKey(7, "resp_1", 1, 4)])
	require.True(t, svc.kongPlanReuseResponse(context.Background(), 7, "resp_1", &credits))

	svc.kongPlanMarkResponse(context.Background(), 7, "resp_2", &sub, time.Hour)
	require.NotContains(t, fake.responses, kongPlanMarkerKey(7, "resp_2", 2, 0), "第一层的账号不写续接标记")
	require.True(t, svc.kongPlanReuseResponse(context.Background(), 7, "resp_2", &sub))

	h.control(t, 21, `"shadow"`, "[1]", 0)
	require.False(t, svc.kongPlanReuseResponse(context.Background(), 7, "resp_1", &credits), "此刻不可服务")
}

func TestKongPlanWS_TurnRecheck(t *testing.T) {
	h, _ := newKongPlanBindingHarness(t, nil)
	svc := h.sess.svc
	ctx := context.Background()
	sub := h.selAccount(2, 5, 0, 50)

	st := svc.KongPlanWSBegin(ctx, &sub)
	require.True(t, svc.KongPlanWSTurnAllowed(ctx, &sub, st))
	// 连接期间账号到了阈值、进了 credits 层：建连时不在 credits 层，请重连
	h.sess.svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{h.selAccount(1, 1, 0, 99), h.selAccount(2, 5, 0, 99)}}
	h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	h.publish(t, 1, "["+h.creditsEntry(1, 1)+","+h.creditsEntry(2, 1)+"]")
	h.control(t, 22, `"on"`, "[1,2]", 0)
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &sub, st))

	credits := h.selAccount(1, 1, 0, 99)
	st = svc.KongPlanWSBegin(ctx, &credits)
	require.True(t, svc.KongPlanWSTurnAllowed(ctx, &credits, st))
	h.setPace(1, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 5,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &credits, st), "窗口序号变了")

	h.control(t, 23, `"on"`, "[2]", 0)
	st = svc.KongPlanWSBegin(ctx, &sub)
	require.False(t, svc.KongPlanWSTurnAllowed(ctx, &credits, KongPlanWSState{credits: true, seq: 5}), "移出白名单：此刻不可服务")

	kongPlanRT.Store(nil)
	require.True(t, svc.KongPlanWSTurnAllowed(ctx, &credits, st), "计划输入未生效时不复核")
}
