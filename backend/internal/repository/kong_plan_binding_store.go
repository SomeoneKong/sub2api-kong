package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 会话标记、续接标记与绑定的条件替换（fork 专有，设计见 DESIGN-account-selection.md 第 4.3 节）。
//
// 会话标记 kong:plan:sess:{分组}:{粘性键}:{账号}:{窗口序号} 表示这个会话是在账号处于 credits 层之后分到它的；
// 续接标记 kong:plan:resp:{分组}:{响应 ID}:{账号}:{窗口序号} 表示这个响应是在账号处于 credits 层之后产出的。
// 判断只看键是否存在；会话标记的值是第一次写下的时刻（Unix 秒），决策记录据此算会话的停留时长。

type kongPlanBindingStore struct {
	rdb *redis.Client
}

// NewKongPlanBindingStore 创建标记存储。
func NewKongPlanBindingStore(rdb *redis.Client) service.KongPlanBindingStore {
	return &kongPlanBindingStore{rdb: rdb}
}

func kongPlanSessionMarkerKey(groupID int64, key string, accountID, seq int64) string {
	return fmt.Sprintf("kong:plan:sess:%d:%s:%d:%d", groupID, key, accountID, seq)
}

func kongPlanResponseMarkerKey(groupID int64, responseID string, accountID, seq int64) string {
	return fmt.Sprintf("kong:plan:resp:%d:%s:%d:%d", groupID, responseID, accountID, seq)
}

// kongPlanMarkSessionScript 写会话标记：不存在时写下时刻，已存在时只续期、保留第一次的时刻。
var kongPlanMarkSessionScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
	return 1
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 0
`)

// kongPlanTouchSessionScript 续期会话标记并返回它的值；不存在时返回 nil。
var kongPlanTouchSessionScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return v
`)

func (s *kongPlanBindingStore) MarkSession(ctx context.Context, groupID int64, key string, accountID, seq int64, at time.Time, ttl time.Duration) error {
	return kongPlanMarkSessionScript.Run(ctx, s.rdb, []string{kongPlanSessionMarkerKey(groupID, key, accountID, seq)},
		strconv.FormatInt(at.Unix(), 10), ttl.Milliseconds()).Err()
}

func (s *kongPlanBindingStore) TouchSession(ctx context.Context, groupID int64, key string, accountID, seq int64, ttl time.Duration) (time.Time, bool, error) {
	v, err := kongPlanTouchSessionScript.Run(ctx, s.rdb, []string{kongPlanSessionMarkerKey(groupID, key, accountID, seq)},
		ttl.Milliseconds()).Text()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	// 早期版本写的值是 1，读不出时刻
	if sec, perr := strconv.ParseInt(v, 10, 64); perr == nil && sec > 1 {
		return time.Unix(sec, 0), true, nil
	}
	return time.Time{}, true, nil
}

func (s *kongPlanBindingStore) MarkResponse(ctx context.Context, groupID int64, responseID string, accountID, seq int64, ttl time.Duration) error {
	return s.rdb.Set(ctx, kongPlanResponseMarkerKey(groupID, responseID, accountID, seq), 1, ttl).Err()
}

func (s *kongPlanBindingStore) HasResponse(ctx context.Context, groupID int64, responseID string, accountID, seq int64) (bool, error) {
	n, err := s.rdb.Exists(ctx, kongPlanResponseMarkerKey(groupID, responseID, accountID, seq)).Result()
	return n > 0, err
}

// kongPlanReplaceBindingScript 在绑定的值仍是旧账号时改成新账号。
var kongPlanReplaceBindingScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
	return 1
end
return 0
`)

func (s *kongPlanBindingStore) ReplaceSessionBinding(ctx context.Context, groupID int64, key string, from, to int64, ttl time.Duration) (bool, error) {
	n, err := kongPlanReplaceBindingScript.Run(ctx, s.rdb, []string{buildSessionKey(groupID, key)},
		strconv.FormatInt(from, 10), strconv.FormatInt(to, 10), ttl.Milliseconds()).Int()
	return n == 1, err
}
