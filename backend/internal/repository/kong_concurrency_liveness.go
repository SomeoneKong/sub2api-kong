package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 请求存活续期的并发槽刷新（设计见 DESIGN-request-liveness.md）。

// kongRefreshSlotScript 只刷新仍存在的成员：成员在才把分数改为 Redis 当前时刻并刷新键的过期时间；
// 不在（已释放或已过期）则什么也不做，所以释放之后迟到的续期不会让占用复活。
// KEYS[1] = 槽位有序集合键，ARGV[1] = TTL（秒），ARGV[2] = 请求 ID。返回 {成员是否还在, Redis 当前秒}。
var kongRefreshSlotScript = redis.NewScript(`
	redis.replicate_commands()
	local key = KEYS[1]
	local ttl = tonumber(ARGV[1])
	local member = ARGV[2]
	local now = tonumber(redis.call('TIME')[1])
	if redis.call('ZSCORE', key, member) == false then
		return {0, now}
	end
	redis.call('ZADD', key, now, member)
	redis.call('EXPIRE', key, ttl)
	return {1, now}
`)

// KongRefreshSlot 刷新一个在途占用。账号槽与用户槽另刷新活跃索引，与获取时一致。
func (c *concurrencyCache) KongRefreshSlot(ctx context.Context, kind service.KongSlotKind, id int64, requestID string) (bool, error) {
	var key, indexKey string
	switch kind {
	case service.KongSlotAccount:
		key, indexKey = accountSlotKey(id), accountActiveIndexKey
	case service.KongSlotUser:
		key, indexKey = userSlotKey(id), userActiveIndexKey
	case service.KongSlotAPIKey:
		key = apiKeySlotKey(id)
	default:
		return false, fmt.Errorf("kong liveness: unknown slot kind %d", kind)
	}
	present, now, err := runScriptInt64Pair(ctx, c.rdb, kongRefreshSlotScript, []string{key}, c.slotTTLSeconds, requestID)
	if err != nil {
		return false, err
	}
	if present == 1 && indexKey != "" {
		c.touchActiveIndexAt(ctx, indexKey, id, now+int64(c.slotTTLSeconds))
	}
	return present == 1, nil
}
