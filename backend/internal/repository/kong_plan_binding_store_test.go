//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestKongPlanBindingStore(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewKongPlanBindingStore(rdb)

	t0 := time.Unix(1790000000, 0)
	_, found, err := s.TouchSession(ctx, 3, "openai:h", 7, 4, time.Hour)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, s.MarkSession(ctx, 3, "openai:h", 7, 4, t0, time.Hour))
	require.Equal(t, time.Hour, mr.TTL("kong:plan:sess:3:openai:h:7:4"))
	require.NoError(t, s.MarkSession(ctx, 3, "openai:h", 7, 4, t0.Add(time.Hour), 3*time.Hour))
	require.Equal(t, 3*time.Hour, mr.TTL("kong:plan:sess:3:openai:h:7:4"), "再写只续期")
	markedAt, found, err := s.TouchSession(ctx, 3, "openai:h", 7, 4, 2*time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, markedAt.Equal(t0), "保留第一次写下的时刻")
	require.Equal(t, 2*time.Hour, mr.TTL("kong:plan:sess:3:openai:h:7:4"), "命中时续期")
	_, found, err = s.TouchSession(ctx, 3, "openai:h", 7, 5, time.Hour)
	require.NoError(t, err)
	require.False(t, found, "别的窗口序号是另一份标记")
	require.NoError(t, mr.Set("kong:plan:sess:3:openai:old:7:4", "1"))
	markedAt, found, err = s.TouchSession(ctx, 3, "openai:old", 7, 4, time.Hour)
	require.NoError(t, err)
	require.True(t, found, "早期版本写下的标记照样命中")
	require.True(t, markedAt.IsZero(), "读不出时刻")

	require.NoError(t, s.MarkResponse(ctx, 3, "resp_1", 7, 4, 30*time.Minute))
	ok, err := s.HasResponse(ctx, 3, "resp_1", 7, 4)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 30*time.Minute, mr.TTL("kong:plan:resp:3:resp_1:7:4"))
	ok, err = s.HasResponse(ctx, 3, "resp_1", 8, 4)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, mr.Set(buildSessionKey(3, "openai:h"), "7"))
	replaced, err := s.ReplaceSessionBinding(ctx, 3, "openai:h", 7, 9, time.Hour)
	require.NoError(t, err)
	require.True(t, replaced)
	v, err := mr.Get(buildSessionKey(3, "openai:h"))
	require.NoError(t, err)
	require.Equal(t, "9", v)
	require.Equal(t, time.Hour, mr.TTL(buildSessionKey(3, "openai:h")))

	replaced, err = s.ReplaceSessionBinding(ctx, 3, "openai:h", 7, 11, time.Hour)
	require.NoError(t, err)
	require.False(t, replaced, "值已不是旧账号时不改")
	v, _ = mr.Get(buildSessionKey(3, "openai:h"))
	require.Equal(t, "9", v)
}
