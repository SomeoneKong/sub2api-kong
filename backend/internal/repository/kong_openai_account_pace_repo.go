package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongPaceRepository 是账号消耗节奏的数据库访问（raw SQL，不走 ent，理由同 kong_ticket_repo.go）。
// 决策记录表见 migrations/904_kong_openai_pace_decisions.sql。
type kongPaceRepository struct {
	db *sql.DB
}

// NewKongPaceRepository 创建节奏组件的数据库仓储。
func NewKongPaceRepository(db *sql.DB) service.KongPaceRepository {
	return &kongPaceRepository{db: db}
}

// ListOpenAIOAuthAccounts 读全部 OpenAI OAuth 账号的额度读数与订阅资料。credentials 逐键取、
// 不取整列：同一列里有 access / refresh token。
func (r *kongPaceRepository) ListOpenAIOAuthAccounts(ctx context.Context) ([]service.KongPaceAccountRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id,
		       COALESCE(a.credentials->>'plan_type', ''),
		       a.concurrency,
		       COALESCE(a.credentials->>'subscription_expires_at', ''),
		       COALESCE(a.extra->>'codex_7d_used_percent', ''),
		       COALESCE(a.extra->>'codex_7d_reset_at', ''),
		       COALESCE(a.extra->>'codex_7d_window_minutes', ''),
		       COALESCE(a.extra->>'codex_usage_updated_at', ''),
		       a.last_used_at
		  FROM accounts a
		 WHERE a.platform = $1 AND a.type = $2 AND a.deleted_at IS NULL
		 ORDER BY a.id`, service.PlatformOpenAI, service.AccountTypeOAuth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []service.KongPaceAccountRow
	for rows.Next() {
		var (
			row                                     service.KongPaceAccountRow
			expires, used, resetAt, window, updated string
			lastUsed                                sql.NullTime
		)
		if err := rows.Scan(&row.ID, &row.Plan, &row.Concurrency, &expires, &used, &resetAt, &window, &updated, &lastUsed); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			row.LastUsedAt = &t
		}
		row.SubscriptionExpiresAt = kongPaceParseTime(expires)
		row.UsedPercent = kongPaceParseFloat(used)
		row.ResetAt = kongPaceParseTime(resetAt)
		if f := kongPaceParseFloat(window); f != nil {
			v := int(*f)
			row.WindowMinutes = &v
		}
		row.UsageUpdatedAt = kongPaceParseTime(updated)
		out = append(out, row)
	}
	return out, rows.Err()
}

func kongPaceParseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func kongPaceParseFloat(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func kongPaceNullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func kongPaceNullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func kongPaceNullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	// 传 string 而不是 []byte，理由同 kongMarshalProbs。
	return string(b)
}

const kongPaceDecisionColumns = 17

// InsertDecisions 批量写决策记录。
func (r *kongPaceRepository) InsertDecisions(ctx context.Context, records []service.KongPaceDecisionRecord) error {
	if len(records) == 0 {
		return nil
	}
	var sb strings.Builder
	_, _ = sb.WriteString(`INSERT INTO kong_openai_pace_decisions (created_at, request_id, client_request_id, user_id,
		group_id, model, session_hash, reason, applied, not_applied_reason, config_version, config,
		snapshot_age_ms, rounds, outcome, account_id, attempt_index) VALUES `)
	args := make([]any, 0, len(records)*kongPaceDecisionColumns)
	for i, rec := range records {
		if i > 0 {
			_, _ = sb.WriteString(",")
		}
		_, _ = sb.WriteString("(")
		for j := 0; j < kongPaceDecisionColumns; j++ {
			if j > 0 {
				_, _ = sb.WriteString(",")
			}
			_, _ = fmt.Fprintf(&sb, "$%d", i*kongPaceDecisionColumns+j+1)
		}
		_, _ = sb.WriteString(")")
		snapshotAge := any(rec.SnapshotAgeMs)
		if rec.SnapshotAgeMs < 0 {
			snapshotAge = nil
		}
		attempt := any(rec.AttemptIndex)
		if rec.AttemptIndex < 0 {
			attempt = nil
		}
		args = append(args,
			rec.CreatedAt, kongPaceNullString(rec.RequestID), kongPaceNullString(rec.ClientRequestID), kongPaceNullID(rec.UserID),
			kongPaceNullID(rec.GroupID), kongPaceNullString(rec.Model), kongPaceNullString(rec.SessionHash), rec.Reason,
			rec.Applied, kongPaceNullString(rec.NotAppliedReason), kongPaceNullString(rec.ConfigVersion), kongPaceNullJSON(rec.Config),
			snapshotAge, kongPaceNullJSON(rec.Rounds), rec.Outcome, kongPaceNullID(rec.AccountID), attempt,
		)
	}
	_, err := r.db.ExecContext(ctx, sb.String(), args...)
	return err
}

// DeleteDecisionsBefore 删掉 created_at 早于 cutoff 的一批记录，返回删掉的行数。
func (r *kongPaceRepository) DeleteDecisionsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM kong_openai_pace_decisions WHERE id IN (
		SELECT id FROM kong_openai_pace_decisions WHERE created_at < $1 ORDER BY id LIMIT $2)`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Redis 键（设计文档 4.2）。账号状态每个账号一个键、整体改写，被 LRU 淘汰时窗口与消耗记录同进同出。
const (
	kongPaceAccountKeyPrefix = "kong:pace:openai:account:"
	kongPaceStateKey         = "kong:pace:openai:state"
	kongPaceMetaKey          = "kong:pace:openai:meta"
)

type kongPaceStore struct {
	rdb *redis.Client
}

// NewKongPaceStore 创建节奏组件的 Redis 存取。
func NewKongPaceStore(rdb *redis.Client) service.KongPaceStore {
	return &kongPaceStore{rdb: rdb}
}

// LoadAccountStates 读回全部账号状态（只在启动时调用一次）。
func (s *kongPaceStore) LoadAccountStates(ctx context.Context) (map[int64][]byte, error) {
	var keys []string
	iter := s.rdb.Scan(ctx, 0, kongPaceAccountKeyPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	out := make(map[int64][]byte, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, v := range vals {
		str, ok := v.(string)
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(keys[i], kongPaceAccountKeyPrefix), 10, 64)
		if err != nil {
			continue
		}
		out[id] = []byte(str)
	}
	return out, nil
}

// SaveAccountStates 整体改写各账号的状态，并删掉已不存在的账号。
func (s *kongPaceStore) SaveAccountStates(ctx context.Context, states map[int64][]byte, deleted []int64) error {
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for id, data := range states {
			p.Set(ctx, kongPaceAccountKeyPrefix+strconv.FormatInt(id, 10), data, 0)
		}
		for _, id := range deleted {
			p.Del(ctx, kongPaceAccountKeyPrefix+strconv.FormatInt(id, 10))
		}
		return nil
	})
	return err
}

// PublishState 原子地替换展示用的 state 与 meta，两者都带过期时间：本功能停止运行后自然消失。
func (s *kongPaceStore) PublishState(ctx context.Context, accounts map[int64][]byte, meta []byte, ttl time.Duration) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, kongPaceStateKey)
		if len(accounts) > 0 {
			fields := make(map[string]any, len(accounts))
			for id, data := range accounts {
				fields[strconv.FormatInt(id, 10)] = string(data)
			}
			p.HSet(ctx, kongPaceStateKey, fields)
			p.Expire(ctx, kongPaceStateKey, ttl)
		}
		p.Set(ctx, kongPaceMetaKey, meta, ttl)
		return nil
	})
	return err
}
