package repository

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 计划组件滚动状态在 Redis 里的部分（fork 专有，设计见 DESIGN-account-selection.md 第 4.1 节）：待处理的余额观测，
// 以及每个账号每个窗口的入层、离层、结算标记与未结算集合。与节奏状态同一个 Redis，提交走节奏组件的事务
// （kong_openai_account_pace_repo.go 的 SaveAccountStates）。
const (
	kongPlanObservationsKey = "kong:plan:obs"
	kongPlanTierKeyPrefix   = "kong:plan:tier:"
	// 标记保留 35 天：窗口 7 天，离层后最多 24h 结算，留足余量。
	kongPlanMarkerTTL = 35 * 24 * time.Hour
)

func kongPlanTierKey(accountID, seq int64, kind string) string {
	return kongPlanTierKeyPrefix + strconv.FormatInt(accountID, 10) + ":" + strconv.FormatInt(seq, 10) + ":" + kind
}

func kongPlanOpenKey(accountID int64) string {
	return kongPlanTierKeyPrefix + strconv.FormatInt(accountID, 10) + ":open"
}

// 入层：标记不存在才写，同时加进未结算集合；已存在时返回已记的时刻与它是否仍未结算。
var kongPlanMarkEnteredScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
  redis.call('SADD', KEYS[2], ARGV[3])
  return {1, ARGV[1], 1}
end
return {0, redis.call('GET', KEYS[1]), redis.call('SISMEMBER', KEYS[2], ARGV[3])}
`)

// 离层：标记不存在才写；返回最终记下的时刻。
var kongPlanMarkLeftScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
  return ARGV[1]
end
return redis.call('GET', KEYS[1])
`)

type kongPlanLedgerStore struct {
	rdb *redis.Client
}

// NewKongPlanLedgerStore 创建计划组件的 Redis 存取。
func NewKongPlanLedgerStore(rdb *redis.Client) service.KongPlanLedgerStore {
	return &kongPlanLedgerStore{rdb: rdb}
}

func (s *kongPlanLedgerStore) PushObservation(ctx context.Context, raw []byte) error {
	return s.rdb.RPush(ctx, kongPlanObservationsKey, raw).Err()
}

func (s *kongPlanLedgerStore) ListObservations(ctx context.Context) ([][]byte, error) {
	vals, err := s.rdb.LRange(ctx, kongPlanObservationsKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(vals))
	for _, v := range vals {
		out = append(out, []byte(v))
	}
	return out, nil
}

func kongPlanParseMarker(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func (s *kongPlanLedgerStore) MarkEntered(ctx context.Context, accountID, seq int64, at time.Time) (bool, time.Time, bool, error) {
	res, err := kongPlanMarkEnteredScript.Run(ctx, s.rdb,
		[]string{kongPlanTierKey(accountID, seq, "entered"), kongPlanOpenKey(accountID)},
		at.UTC().Format(time.RFC3339Nano), kongPlanMarkerTTL.Milliseconds(), strconv.FormatInt(seq, 10)).Slice()
	if err != nil {
		return false, time.Time{}, false, err
	}
	created := len(res) > 0 && res[0] == int64(1)
	enteredAt, ok := kongPlanParseMarker(res[1])
	if !ok {
		enteredAt = at.UTC()
	}
	open := len(res) > 2 && res[2] == int64(1)
	return created, enteredAt, open, nil
}

func (s *kongPlanLedgerStore) MarkLeft(ctx context.Context, accountID, seq int64, at time.Time) (time.Time, error) {
	res, err := kongPlanMarkLeftScript.Run(ctx, s.rdb, []string{kongPlanTierKey(accountID, seq, "left")},
		at.UTC().Format(time.RFC3339Nano), kongPlanMarkerTTL.Milliseconds()).Result()
	if err != nil {
		return time.Time{}, err
	}
	if t, ok := kongPlanParseMarker(res); ok {
		return t, nil
	}
	return at.UTC(), nil
}

// LoadOpenEntries 读回全部未结算的入层（只在启动与重载时调用）。标记已过期的窗口序号从集合里移出。
func (s *kongPlanLedgerStore) LoadOpenEntries(ctx context.Context) (map[int64][]service.KongPlanTierEntry, error) {
	out := map[int64][]service.KongPlanTierEntry{}
	iter := s.rdb.Scan(ctx, 0, kongPlanTierKeyPrefix+"*:open", 200).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	for _, key := range keys {
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(key, kongPlanTierKeyPrefix), ":open"), 10, 64)
		if err != nil {
			continue
		}
		members, err := s.rdb.SMembers(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			seq, err := strconv.ParseInt(m, 10, 64)
			if err != nil {
				continue
			}
			vals, err := s.rdb.MGet(ctx, kongPlanTierKey(id, seq, "entered"), kongPlanTierKey(id, seq, "left")).Result()
			if err != nil {
				return nil, err
			}
			enteredAt, ok := kongPlanParseMarker(vals[0])
			if !ok {
				_ = s.rdb.SRem(ctx, key, m).Err()
				continue
			}
			e := service.KongPlanTierEntry{Seq: seq, EnteredAt: enteredAt}
			if leftAt, ok := kongPlanParseMarker(vals[1]); ok {
				e.LeftAt = &leftAt
			}
			out[id] = append(out[id], e)
		}
	}
	return out, nil
}
