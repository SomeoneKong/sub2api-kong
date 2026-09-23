//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newKongPlanLedgerStoreTest(t *testing.T) (*kongPlanLedgerStore, *kongPaceStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &kongPlanLedgerStore{rdb: rdb}, &kongPaceStore{rdb: rdb}, mr
}

func TestKongPlanLedgerStore_MarkersAndCommit(t *testing.T) {
	ctx := context.Background()
	ls, ps, mr := newKongPlanLedgerStoreTest(t)
	t0 := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)

	created, at, open, err := ls.MarkEntered(ctx, 1, 3, t0)
	require.NoError(t, err)
	require.True(t, created)
	require.True(t, open)
	require.Equal(t, t0, at)
	created, at, open, err = ls.MarkEntered(ctx, 1, 3, t0.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, created, "同一窗口只记一次")
	require.True(t, open)
	require.Equal(t, t0, at, "返回第一次记下的时刻")
	ttl := mr.TTL(kongPlanTierKey(1, 3, "entered"))
	require.Greater(t, ttl, 30*24*time.Hour)

	left, err := ls.MarkLeft(ctx, 1, 3, t0.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, t0.Add(2*time.Hour), left)
	left, err = ls.MarkLeft(ctx, 1, 3, t0.Add(5*time.Hour))
	require.NoError(t, err)
	require.Equal(t, t0.Add(2*time.Hour), left)

	entries, err := ls.LoadOpenEntries(ctx)
	require.NoError(t, err)
	require.Len(t, entries[1], 1)
	require.Equal(t, int64(3), entries[1][0].Seq)
	require.Equal(t, t0.Add(2*time.Hour), *entries[1][0].LeftAt)

	require.NoError(t, ls.PushObservation(ctx, []byte(`{"id":"a"}`)))
	require.NoError(t, ls.PushObservation(ctx, []byte(`{"id":"b"}`)))
	require.NoError(t, ps.SaveAccountStates(ctx, map[int64][]byte{1: []byte(`{"s":1}`)}, nil, service.KongPaceCommitExtra{
		DoneObservations: [][]byte{[]byte(`{"id":"a"}`)},
		Settlements:      []service.KongPlanSettlement{{AccountID: 1, Seq: 3, At: t0.Add(3 * time.Hour)}},
	}))
	obs, err := ls.ListObservations(ctx)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte(`{"id":"b"}`)}, obs, "按原值删除已处理的观测")
	entries, err = ls.LoadOpenEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, entries[1], "结算后移出未结算集合")
	require.True(t, mr.Exists(kongPlanTierKey(1, 3, "settled")))

	// 已结算的窗口再次入层：不重新打开
	created, _, open, err = ls.MarkEntered(ctx, 1, 3, t0.Add(4*time.Hour))
	require.NoError(t, err)
	require.False(t, created)
	require.False(t, open)

	// 标记过期而集合里还留着序号：读回时移出
	_, err = mr.SAdd(kongPlanOpenKey(2), "9")
	require.NoError(t, err)
	entries, err = ls.LoadOpenEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, entries[2])
	members, _ := mr.SMembers(kongPlanOpenKey(2))
	require.Empty(t, members)
}
