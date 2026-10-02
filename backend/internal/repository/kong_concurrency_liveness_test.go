package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// kongLivenessRedis 让 TIME 返回的时刻与键的过期时间一起推进：槽位按分数判定过期，键按 EXPIRE 过期。
type kongLivenessRedis struct {
	server *miniredis.Miniredis
	now    time.Time
}

func (r *kongLivenessRedis) advance(d time.Duration) {
	r.now = r.now.Add(d)
	r.server.SetTime(r.now)
	r.server.FastForward(d)
}

func newKongLivenessTestCache(t *testing.T) (*kongLivenessRedis, service.ConcurrencyCache, service.KongSlotRefresher) {
	t.Helper()
	redisServer := &kongLivenessRedis{server: miniredis.RunT(t), now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	redisServer.server.SetTime(redisServer.now)
	client := redis.NewClient(&redis.Options{Addr: redisServer.server.Addr()})
	cache := NewConcurrencyCache(client, 1, 60)
	refresher, ok := cache.(service.KongSlotRefresher)
	require.True(t, ok)
	return redisServer, cache, refresher
}

func TestKongRefreshSlotKeepsLongRequestCounted(t *testing.T) {
	redisServer, cache, refresher := newKongLivenessTestCache(t)
	ctx := context.Background()

	for _, id := range []string{"long", "short"} {
		acquired, err := cache.AcquireAccountSlot(ctx, 10, 5, id)
		require.NoError(t, err)
		require.True(t, acquired)
	}
	redisServer.advance(50 * time.Second)
	present, err := refresher.KongRefreshSlot(ctx, service.KongSlotAccount, 10, "long")
	require.NoError(t, err)
	require.True(t, present)

	// 距占用已 100 秒，超过 60 秒的 TTL；刷新过的仍被计数，没刷新的已过期。
	redisServer.advance(50 * time.Second)
	count, err := cache.GetAccountConcurrency(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Greater(t, redisServer.server.TTL(accountSlotKey(10)), time.Duration(0))
}

func TestKongRefreshSlotDoesNotReviveReleasedSlot(t *testing.T) {
	_, cache, refresher := newKongLivenessTestCache(t)
	ctx := context.Background()

	acquired, err := cache.AcquireAccountSlot(ctx, 10, 5, "req")
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, cache.ReleaseAccountSlot(ctx, 10, "req"))

	present, err := refresher.KongRefreshSlot(ctx, service.KongSlotAccount, 10, "req")
	require.NoError(t, err)
	require.False(t, present)
	count, err := cache.GetAccountConcurrency(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestKongRefreshSlotCoversUserAndAPIKeySlots(t *testing.T) {
	redisServer, cache, refresher := newKongLivenessTestCache(t)
	ctx := context.Background()
	apiKeys, ok := cache.(service.APIKeyConcurrencyCache)
	require.True(t, ok)

	acquired, err := cache.AcquireUserSlot(ctx, 20, 5, "user-req")
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, apiKeys.TrackAPIKeySlot(ctx, 30, "key-req"))

	redisServer.advance(50 * time.Second)
	present, err := refresher.KongRefreshSlot(ctx, service.KongSlotUser, 20, "user-req")
	require.NoError(t, err)
	require.True(t, present)
	present, err = refresher.KongRefreshSlot(ctx, service.KongSlotAPIKey, 30, "key-req")
	require.NoError(t, err)
	require.True(t, present)

	redisServer.advance(50 * time.Second)
	userCount, err := cache.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, 1, userCount)
	keyCounts, err := apiKeys.GetAPIKeyConcurrencyBatch(ctx, []int64{30})
	require.NoError(t, err)
	require.Equal(t, 1, keyCounts[30])

	_, err = refresher.KongRefreshSlot(ctx, service.KongSlotKind(0), 1, "x")
	require.Error(t, err)
}

// 脚本缓存被清空（例如 Redis 重启）后，刷新自行重新加载脚本。
func TestKongRefreshSlotRecoversAfterScriptFlush(t *testing.T) {
	redisServer, cache, refresher := newKongLivenessTestCache(t)
	ctx := context.Background()

	acquired, err := cache.AcquireAccountSlot(ctx, 10, 5, "req")
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = refresher.KongRefreshSlot(ctx, service.KongSlotAccount, 10, "req")
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: redisServer.server.Addr()})
	require.NoError(t, client.ScriptFlush(ctx).Err())
	present, err := refresher.KongRefreshSlot(ctx, service.KongSlotAccount, 10, "req")
	require.NoError(t, err)
	require.True(t, present)
}
