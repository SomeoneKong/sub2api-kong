//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type kongPlanFakeLive struct {
	loads       map[int64]*AccountLoadInfo
	loadErr     error
	sessions    map[int64]int
	sessionErr  error
	loadReq     []AccountWithConcurrency
	sessionIDs  []int64
	idle        map[int64]time.Duration
	sessionCall int
}

func (f *kongPlanFakeLive) KongPlanLoads(_ context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	f.loadReq = accounts
	return f.loads, f.loadErr
}

func (f *kongPlanFakeLive) KongPlanSessions(_ context.Context, ids []int64, idle map[int64]time.Duration) (map[int64]int, error) {
	f.sessionCall++
	f.sessionIDs, f.idle = ids, idle
	return f.sessions, f.sessionErr
}

// 账号 1 有会话上限（自定义空闲超时 9 分钟），账号 2 不限。
func kongPlanLiveFixture() ([]KongPlanAccountView, map[int64]*Account) {
	views := []KongPlanAccountView{{ID: 1, MaxSessions: 3, MaxConcurrency: 8}, {ID: 2, MaxSessions: 0, MaxConcurrency: 10}}
	accounts := map[int64]*Account{
		1: {ID: 1, Extra: map[string]any{"session_idle_timeout_minutes": 9}},
		2: {ID: 2},
	}
	return views, accounts
}

func TestKongPlanAttachLive_Values(t *testing.T) {
	views, accounts := kongPlanLiveFixture()
	f := &kongPlanFakeLive{
		loads:    map[int64]*AccountLoadInfo{1: {AccountID: 1, CurrentConcurrency: 0, WaitingCount: 0}, 2: {AccountID: 2, CurrentConcurrency: 4, WaitingCount: 2}},
		sessions: map[int64]int{1: 0},
	}
	kongPlanAttachLive(context.Background(), f, views, accounts)

	require.Equal(t, []AccountWithConcurrency{{ID: 1, MaxConcurrency: 8}, {ID: 2, MaxConcurrency: 10}}, f.loadReq)
	require.Equal(t, []int64{1}, f.sessionIDs, "只读会话上限生效的账号")
	require.Equal(t, map[int64]time.Duration{1: 9 * time.Minute}, f.idle)

	require.NotNil(t, views[0].Live)
	require.Equal(t, 0, views[0].Live.Concurrency)
	require.NotNil(t, views[0].Live.Sessions, "读到的 0 是 0，不是读不到")
	require.Equal(t, 0, *views[0].Live.Sessions)
	require.Equal(t, &KongPlanLiveView{Concurrency: 4, Waiting: 2}, views[1].Live, "不限会话的账号 sessions 为 nil")
}

func TestKongPlanAttachLive_LoadFailures(t *testing.T) {
	views, accounts := kongPlanLiveFixture()
	f := &kongPlanFakeLive{loadErr: errors.New("redis down"), sessions: map[int64]int{1: 2}}
	kongPlanAttachLive(context.Background(), f, views, accounts)
	require.Nil(t, views[0].Live)
	require.Nil(t, views[1].Live)
	require.Zero(t, f.sessionCall, "负载读不到时不再读会话")

	views, accounts = kongPlanLiveFixture()
	f = &kongPlanFakeLive{loads: map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 1}, 2: nil}, sessions: map[int64]int{1: 2}}
	kongPlanAttachLive(context.Background(), f, views, accounts)
	require.Equal(t, 1, views[0].Live.Concurrency)
	require.Nil(t, views[1].Live, "结果里没有这个账号（或为 nil）就是读不到")
}

func TestKongPlanAttachLive_SessionFailures(t *testing.T) {
	cases := map[string]*kongPlanFakeLive{
		"报错":      {sessionErr: errors.New("redis down"), sessions: map[int64]int{1: 2}},
		"整批空结果":   {sessions: map[int64]int{}},
		"结果为 nil": {},
		"缺这一项":    {sessions: map[int64]int{2: 5}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			views, accounts := kongPlanLiveFixture()
			f.loads = map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 3, WaitingCount: 1}, 2: {}}
			kongPlanAttachLive(context.Background(), f, views, accounts)
			require.Equal(t, &KongPlanLiveView{Concurrency: 3, Waiting: 1}, views[0].Live, "只有会话为 nil，不按 0 算")
			require.Equal(t, &KongPlanLiveView{}, views[1].Live)
		})
	}
}

func TestKongPlanAttachLive_NoReaderOrNoSessionAccounts(t *testing.T) {
	views, accounts := kongPlanLiveFixture()
	kongPlanAttachLive(context.Background(), nil, views, accounts)
	require.Nil(t, views[0].Live)

	views = []KongPlanAccountView{{ID: 2}}
	f := &kongPlanFakeLive{loads: map[int64]*AccountLoadInfo{2: {CurrentConcurrency: 1}}}
	kongPlanAttachLive(context.Background(), f, views, accounts)
	require.Zero(t, f.sessionCall, "没有会话上限生效的账号时不读会话")
	require.Equal(t, &KongPlanLiveView{Concurrency: 1}, views[0].Live)
}

func TestKongPlanLiveView_JSON(t *testing.T) {
	zero := 0
	b, err := json.Marshal(KongPlanAccountView{ID: 1, Live: &KongPlanLiveView{Sessions: &zero}})
	require.NoError(t, err)
	require.Contains(t, string(b), `"live":{"concurrency":0,"waiting":0,"sessions":0}`)

	b, err = json.Marshal(KongPlanAccountView{ID: 1, Live: &KongPlanLiveView{Concurrency: 2}})
	require.NoError(t, err)
	require.Contains(t, string(b), `"live":{"concurrency":2,"waiting":0,"sessions":null}`)

	b, err = json.Marshal(KongPlanAccountView{ID: 1})
	require.NoError(t, err)
	require.Contains(t, string(b), `"live":null`, "读不到时字段照样输出为 null")
}

func TestKongPlanLiveReader_Unwired(t *testing.T) {
	var s *OpenAIGatewayService
	_, err := s.KongPlanLoads(context.Background(), nil)
	require.ErrorIs(t, err, errKongPlanLiveUnavailable)
	_, err = s.KongPlanSessions(context.Background(), nil, nil)
	require.ErrorIs(t, err, errKongPlanLiveUnavailable)

	s = &OpenAIGatewayService{}
	_, err = s.KongPlanLoads(context.Background(), nil)
	require.ErrorIs(t, err, errKongPlanLiveUnavailable)
	_, err = s.KongPlanSessions(context.Background(), nil, nil)
	require.ErrorIs(t, err, errKongPlanLiveUnavailable)
}

// 经 KongPlanAccounts 走一遍：会话按视图算出的生效上限决定要不要读——账号自身不限、计划条目给了上限也读，空闲超时用账号自己的。
func TestKongPlanAccounts_Live(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.publish(t, 1, `[{"id": 1, "tier": "normal", "max_sessions": 2}, {"id": 2, "tier": "normal"}]`)
	planLimited := h.account(10, 0)
	planLimited.Extra["session_idle_timeout_minutes"] = 9
	unlimited := h.account(10, 0)
	unlimited.ID = 2
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{}}
	f := &kongPlanFakeLive{
		loads:    map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 1}, 2: {CurrentConcurrency: 2, WaitingCount: 1}},
		sessions: map[int64]int{1: 1},
	}
	views, err := KongPlanAccounts(context.Background(), &kongPlanListRepo{stubQuotaAccountRepo: repo, list: []Account{*unlimited, *planLimited}}, f)
	require.NoError(t, err)
	require.Len(t, views, 2)
	require.Zero(t, planLimited.GetMaxSessions(), "账号自身不限")
	require.Equal(t, 2, views[0].MaxSessions, "上限来自计划条目")
	require.Equal(t, []int64{1}, f.sessionIDs)
	require.Equal(t, map[int64]time.Duration{1: 9 * time.Minute}, f.idle)
	require.Equal(t, 1, *views[0].Live.Sessions)
	require.Equal(t, &KongPlanLiveView{Concurrency: 2, Waiting: 1}, views[1].Live)
}
