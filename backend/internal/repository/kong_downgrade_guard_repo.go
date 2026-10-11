package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// 降档哨兵的持久化：事件表 kong_downgrade_guard_events 与 accounts.extra 里的两个哨兵键（迁移 910）。设计见
// DESIGN-account-downgrade-guard.md。纯 SQL，不走 ent。
//
// 两个哨兵键只有哨兵能写：哨兵的写入在事务里 set_config('kong.guard_write', 'on', true)，迁移 910 的触发器据此
// 放行；其他写入一律还原成写入前的值。schedulable 真正变化时由触发器给版本加一。

// KongDowngradeGuardRepository 实现 service.KongDowngradeGuardStore。
type KongDowngradeGuardRepository struct {
	db *sql.DB
}

// NewKongDowngradeGuardRepository 创建仓储。
func NewKongDowngradeGuardRepository(db *sql.DB) *KongDowngradeGuardRepository {
	return &KongDowngradeGuardRepository{db: db}
}

var _ service.KongDowngradeGuardStore = (*KongDowngradeGuardRepository)(nil)

const (
	kongGuardHoldKey = "kong_downgrade_guard_hold"
	kongGuardRevKey  = "kong_schedulable_rev"
	// kongGuardRevSQL 读账号当前的调度开关版本；缺省或不是数字时为 0。
	kongGuardRevSQL = `(CASE WHEN jsonb_typeof(extra -> 'kong_schedulable_rev') = 'number'
		THEN (extra ->> 'kong_schedulable_rev')::BIGINT ELSE 0 END)`
	// kongGuardExpiredSQL 与 Account 的到期判定同口径：开了到期自动暂停且到期时间已过。
	kongGuardExpiredSQL = `(auto_pause_on_expired AND expires_at IS NOT NULL AND expires_at <= now())`
)

// kongGuardTx 在一个设置了哨兵写入标记的事务里执行 fn。
func (r *KongDowngradeGuardRepository) kongGuardTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin guard tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('kong.guard_write', 'on', true)`); err != nil {
		return fmt.Errorf("set guard write flag: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit guard tx: %w", err)
	}
	return nil
}

// kongGuardNotifyTx 在改动 schedulable 的同一事务里写调度变更通知：提交了就一定有通知，调度器的缓存最终会跟上。
func kongGuardNotifyTx(ctx context.Context, tx *sql.Tx, accountID int64) error {
	if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		return fmt.Errorf("enqueue scheduler outbox for account %d: %w", accountID, err)
	}
	return nil
}

func kongGuardInsertEventTx(ctx context.Context, tx *sql.Tx, ev service.KongGuardEvent) error {
	detail, err := kongGuardDetailJSON(ev.Detail)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kong_downgrade_guard_events (account_id, op, kind, detail) VALUES ($1, $2, $3, $4::jsonb)`,
		ev.AccountID, ev.Op, ev.Kind, string(detail)); err != nil {
		return fmt.Errorf("insert guard event %s: %w", ev.Kind, err)
	}
	return nil
}

func kongGuardDetailJSON(detail map[string]any) ([]byte, error) {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("encode guard event detail: %w", err)
	}
	return b, nil
}

// InsertEvent 写一条事件。
func (r *KongDowngradeGuardRepository) InsertEvent(ctx context.Context, ev service.KongGuardEvent) error {
	detail, err := kongGuardDetailJSON(ev.Detail)
	if err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO kong_downgrade_guard_events (account_id, op, kind, detail) VALUES ($1, $2, $3, $4::jsonb)`,
		ev.AccountID, ev.Op, ev.Kind, string(detail)); err != nil {
		return fmt.Errorf("insert guard event %s: %w", ev.Kind, err)
	}
	return nil
}

// ScanUsage 读 accountIDs 这些账号上 created_at 不早于 since 的 Responses 用量行（不含 compact），按 created_at、id
// 排序，最多 limit 行。账号在限量之前过滤：别的账号的流量不能挤占批量上限。模型取实际上送的（有模型映射时与请求的
// 模型不同），票与容量都按它算。
func (r *KongDowngradeGuardRepository) ScanUsage(ctx context.Context, since time.Time, accountIDs []int64, limit int) ([]service.KongGuardUsageRow, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, created_at, account_id, COALESCE(group_id, 0), COALESCE(api_key_id, 0),
		       COALESCE(NULLIF(upstream_model, ''), model),
		       COALESCE(duration_ms, 0), COALESCE(output_tokens, 0), COALESCE(session_id, ''),
		       COALESCE(kong_request_features ->> 'state_fp', ''), COALESCE(kong_request_features ->> 'reissued_fp', '')
		FROM usage_logs
		WHERE created_at >= $1
		  AND account_id = ANY($2)
		  AND inbound_endpoint LIKE '%responses%'
		  AND inbound_endpoint NOT LIKE '%compact%'
		ORDER BY created_at, id
		LIMIT $3`, since.UTC(), pq.Array(accountIDs), limit)
	if err != nil {
		return nil, fmt.Errorf("scan usage for guard: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]service.KongGuardUsageRow, 0, 256)
	for rows.Next() {
		var row service.KongGuardUsageRow
		if err := rows.Scan(&row.ID, &row.CreatedAt, &row.AccountID, &row.GroupID, &row.APIKeyID, &row.Model,
			&row.DurationMs, &row.OutputTokens, &row.SessionID, &row.StateFP, &row.ReissuedFP); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// kongGuardInGroupSQL 判断账号仍在服务分组。
const kongGuardInGroupSQL = `EXISTS (SELECT 1 FROM account_groups g WHERE g.account_id = accounts.id AND g.group_id = $%d)`

// Hold 凭条件关闭一个账号并写入持有记录与 hold 事件（同一事务）。条件不成立返回 false。
func (r *KongDowngradeGuardRepository) Hold(ctx context.Context, in service.KongGuardHoldInput) (bool, error) {
	hold, err := json.Marshal(in.Hold)
	if err != nil {
		return false, fmt.Errorf("encode hold: %w", err)
	}
	held := false
	err = r.kongGuardTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE accounts
			SET schedulable = FALSE,
			    extra = COALESCE(extra, '{}'::jsonb) || jsonb_build_object('`+kongGuardHoldKey+`', $2::jsonb),
			    updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			  AND schedulable = TRUE
			  AND NOT (COALESCE(extra, '{}'::jsonb) ? '`+kongGuardHoldKey+`')
			  AND `+kongGuardRevSQL+` = $3
			  AND `+fmt.Sprintf(kongGuardInGroupSQL, 4),
			in.AccountID, string(hold), in.ExpectedRev, in.GroupID)
		if err != nil {
			return fmt.Errorf("hold account %d: %w", in.AccountID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		held = true
		if err := kongGuardNotifyTx(ctx, tx, in.AccountID); err != nil {
			return err
		}
		return kongGuardInsertEventTx(ctx, tx, in.Event)
	})
	if err != nil {
		return false, err
	}
	return held, nil
}

// UpdateHold 把 patch 并进持有记录；只在持有记录的 op 仍是这一次、版本仍等于 expectedRev 时生效。
func (r *KongDowngradeGuardRepository) UpdateHold(ctx context.Context, accountID int64, op string, expectedRev int64, patch map[string]any) (bool, error) {
	body, err := json.Marshal(patch)
	if err != nil {
		return false, fmt.Errorf("encode hold patch: %w", err)
	}
	updated := false
	err = r.kongGuardTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE accounts
			SET extra = jsonb_set(extra, '{`+kongGuardHoldKey+`}', (extra -> '`+kongGuardHoldKey+`') || $3::jsonb),
			    updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			  AND extra -> '`+kongGuardHoldKey+`' ->> 'op' = $2
			  AND `+kongGuardRevSQL+` = $4`,
			accountID, op, string(body), expectedRev)
		if err != nil {
			return fmt.Errorf("update hold of account %d: %w", accountID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		updated = n > 0
		return nil
	})
	return updated, err
}

// ReleaseFalseAlarm 撤销待确认阶段的关闭并写 false_alarm 事件（同一事务）。条件：持有记录的 op 仍是这一次、
// 仍是待确认，版本未变，账号 active 且未到期。
func (r *KongDowngradeGuardRepository) ReleaseFalseAlarm(ctx context.Context, accountID int64, op string, expectedRev int64, ev service.KongGuardEvent) (bool, error) {
	released := false
	err := r.kongGuardTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE accounts
			SET schedulable = TRUE,
			    extra = extra - '`+kongGuardHoldKey+`',
			    updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			  AND extra -> '`+kongGuardHoldKey+`' ->> 'op' = $2
			  AND extra -> '`+kongGuardHoldKey+`' ->> 'stage' = '`+service.KongGuardStageSuspect+`'
			  AND `+kongGuardRevSQL+` = $3
			  AND status = 'active'
			  AND NOT `+kongGuardExpiredSQL,
			accountID, op, expectedRev)
		if err != nil {
			return fmt.Errorf("release account %d: %w", accountID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		released = true
		if err := kongGuardNotifyTx(ctx, tx, accountID); err != nil {
			return err
		}
		return kongGuardInsertEventTx(ctx, tx, ev)
	})
	return released, err
}

// TakeOverExpired 让到期的账号接管哨兵的关闭：删掉持有记录、版本加一，保持关闭。只处理已到期且仍带持有记录的
// 账号；删掉记录后不会再被选中，版本不会反复增加。返回被接管的账号。
func (r *KongDowngradeGuardRepository) TakeOverExpired(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := r.kongGuardTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			UPDATE accounts
			SET extra = (extra - '`+kongGuardHoldKey+`') || jsonb_build_object('`+kongGuardRevKey+`', `+kongGuardRevSQL+` + 1),
			    updated_at = now()
			WHERE deleted_at IS NULL
			  AND COALESCE(extra, '{}'::jsonb) ? '`+kongGuardHoldKey+`'
			  AND `+kongGuardExpiredSQL+`
			RETURNING id`)
		if err != nil {
			return fmt.Errorf("take over expired holds: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// RecentEvents 读 since 之后指定种类的事件（按时间升序）。
func (r *KongDowngradeGuardRepository) RecentEvents(ctx context.Context, kinds []string, since time.Time) ([]service.KongGuardEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, created_at, account_id, op, kind, detail
		FROM kong_downgrade_guard_events
		WHERE kind = ANY($1) AND created_at >= $2
		ORDER BY created_at, id`, pq.Array(kinds), since.UTC())
	if err != nil {
		return nil, fmt.Errorf("read guard events: %w", err)
	}
	return kongGuardScanEvents(rows)
}

// ListEvents 读最近的事件，accountID 为 0 时不限账号。
func (r *KongDowngradeGuardRepository) ListEvents(ctx context.Context, accountID int64, limit int) ([]service.KongGuardEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, created_at, account_id, op, kind, detail
		FROM kong_downgrade_guard_events
		WHERE ($1 = 0 OR account_id = $1)
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list guard events: %w", err)
	}
	return kongGuardScanEvents(rows)
}

func kongGuardScanEvents(rows *sql.Rows) ([]service.KongGuardEvent, error) {
	defer func() { _ = rows.Close() }()
	var out []service.KongGuardEvent
	for rows.Next() {
		var ev service.KongGuardEvent
		var detail []byte
		if err := rows.Scan(&ev.ID, &ev.CreatedAt, &ev.AccountID, &ev.Op, &ev.Kind, &detail); err != nil {
			return nil, fmt.Errorf("scan guard event: %w", err)
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &ev.Detail); err != nil {
				return nil, fmt.Errorf("decode guard event %d: %w", ev.ID, err)
			}
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// PendingImpacts 返回关闭满 minAge、影响汇总还没定稿的 hold 事件。
func (r *KongDowngradeGuardRepository) PendingImpacts(ctx context.Context, minAge time.Duration) ([]service.KongGuardEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT h.id, h.created_at, h.account_id, h.op, h.kind, h.detail
		FROM kong_downgrade_guard_events h
		LEFT JOIN kong_downgrade_guard_events i ON i.kind = 'impact' AND i.op = h.op
		WHERE h.kind = 'hold' AND h.op <> ''
		  AND h.created_at <= now() - make_interval(secs => $1)
		  AND (i.id IS NULL OR COALESCE((i.detail ->> 'finalized')::BOOLEAN, FALSE) = FALSE)
		ORDER BY h.created_at`, minAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list pending impacts: %w", err)
	}
	return kongGuardScanEvents(rows)
}

// ImpactRows 汇总一个账号上请求区间与 [from, holdAt) 重叠、且结束不晚于 cutoff 的用量行，按 API key 与会话分组。
func (r *KongDowngradeGuardRepository) ImpactRows(ctx context.Context, accountID int64, from, holdAt, cutoff time.Time) ([]service.KongGuardImpactRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(u.api_key_id, 0), COALESCE(k.name, ''), COALESCE(u.session_id, ''),
		       MIN(u.created_at - make_interval(secs => COALESCE(u.duration_ms, 0) / 1000.0)), MAX(u.created_at), COUNT(*)
		FROM usage_logs u
		LEFT JOIN api_keys k ON k.id = u.api_key_id
		WHERE u.account_id = $1
		  AND u.created_at > $2
		  AND u.created_at <= $4
		  AND u.created_at - make_interval(secs => COALESCE(u.duration_ms, 0) / 1000.0) < $3
		GROUP BY 1, 2, 3
		ORDER BY 4`, accountID, from.UTC(), holdAt.UTC(), cutoff.UTC())
	if err != nil {
		return nil, fmt.Errorf("impact rows of account %d: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []service.KongGuardImpactRow
	for rows.Next() {
		var row service.KongGuardImpactRow
		if err := rows.Scan(&row.APIKeyID, &row.APIKeyName, &row.SessionID, &row.First, &row.Last, &row.Count); err != nil {
			return nil, fmt.Errorf("scan impact row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpsertImpact 写入或覆盖一个关闭操作的影响汇总。已定稿的不再覆盖。
func (r *KongDowngradeGuardRepository) UpsertImpact(ctx context.Context, accountID int64, op string, detail map[string]any) error {
	body, err := kongGuardDetailJSON(detail)
	if err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO kong_downgrade_guard_events (account_id, op, kind, detail)
		VALUES ($1, $2, 'impact', $3::jsonb)
		ON CONFLICT (op) WHERE kind = 'impact'
		DO UPDATE SET detail = EXCLUDED.detail
		WHERE COALESCE((kong_downgrade_guard_events.detail ->> 'finalized')::BOOLEAN, FALSE) = FALSE`,
		accountID, op, string(body)); err != nil {
		return fmt.Errorf("upsert impact %s: %w", op, err)
	}
	return nil
}

// DeleteEventsBefore 删除一批早于 cutoff 的事件，返回删除的行数。hold 事件要等它的影响汇总定稿才删（汇总靠它驱动），
// 未定稿的影响汇总不删。
func (r *KongDowngradeGuardRepository) DeleteEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM kong_downgrade_guard_events WHERE id IN (
			SELECT e.id FROM kong_downgrade_guard_events e
			WHERE e.created_at < $1
			  AND NOT (e.kind = 'impact' AND COALESCE((e.detail ->> 'finalized')::BOOLEAN, FALSE) = FALSE)
			  AND NOT (e.kind = 'hold' AND NOT EXISTS (
				SELECT 1 FROM kong_downgrade_guard_events i
				WHERE i.kind = 'impact' AND i.op = e.op
				  AND COALESCE((i.detail ->> 'finalized')::BOOLEAN, FALSE) = TRUE))
			ORDER BY e.created_at
			LIMIT $2)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete guard events before cutoff: %w", err)
	}
	return res.RowsAffected()
}

// kongExplicitSchedulableSQL 是"显式的可调度操作"的前置：一条语句里删掉持有记录、版本加一，并写入人给的 schedulable。
// schedulable 本身不变时触发器察觉不到这类操作（例如人把已经关闭的账号再设一次 false），所以单独做；连同人给的值一起
// 写，是为了撤销所有权与写入之间没有空隙——否则哨兵可能在空隙里按新版本关闭，人随后那次"值没变"的写入撤不掉它。
// 上游随后的写入写的是同一个值。
//
// CTE 里先设置哨兵写入标记（UPDATE 要等它求值），UPDATE 全部执行完后外层查询再清掉标记，在调用方的事务里执行时
// 标记也不会留给后续语句。
const kongExplicitSchedulableSQL = `
	WITH kong_guard_flag AS (SELECT set_config('kong.guard_write', 'on', true) AS v),
	kong_guard_upd AS (
		UPDATE accounts
		SET schedulable = $2,
		    extra = (COALESCE(extra, '{}'::jsonb) - '` + kongGuardHoldKey + `')
		            || jsonb_build_object('` + kongGuardRevKey + `', ` + kongGuardRevSQL + ` + 1)
		FROM kong_guard_flag
		WHERE accounts.id = ANY($1) AND accounts.deleted_at IS NULL AND kong_guard_flag.v = 'on'
		RETURNING 1
	)
	SELECT set_config('kong.guard_write', '', true) FROM (SELECT COUNT(*) FROM kong_guard_upd) AS kong_guard_n`

// kongExplicitSchedulable 在显式的可调度操作（单个开关、批量更新带 schedulable）写入之前调用；schedulable 为 nil（这次
// 操作不带可调度）时什么都不做。失败返回错误，调用方让这次操作失败。
//
// 批量更新在它自己的事务里调用，与写入、调度变更通知一起提交或回滚。单个开关传进来的是连接池：这里自己开事务，把
// 调度变更通知与开关一起提交——开关已经在这一步写下，上游随后的写入即使失败，调度器的缓存也会跟上。
func kongExplicitSchedulable(ctx context.Context, exec sqlExecutor, ids []int64, schedulable *bool) error {
	if schedulable == nil || exec == nil || len(ids) == 0 {
		return nil
	}
	db, standalone := exec.(*sql.DB)
	if !standalone {
		if _, err := exec.ExecContext(ctx, kongExplicitSchedulableSQL, pq.Array(ids), *schedulable); err != nil {
			return fmt.Errorf("revoke downgrade guard ownership: %w", err)
		}
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("revoke downgrade guard ownership: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, kongExplicitSchedulableSQL, pq.Array(ids), *schedulable); err != nil {
		return fmt.Errorf("revoke downgrade guard ownership: %w", err)
	}
	for _, id := range ids {
		if err := kongGuardNotifyTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("revoke downgrade guard ownership: %w", err)
	}
	return nil
}

// kongEditSchedulable 决定管理后台普通编辑写回的 schedulable：编辑开始时读到的调度开关版本与锁行后库里的不一致，
// 说明期间有人或哨兵动过开关，沿用库里的值，不用编辑开始时读到的旧值回写。调用方已在同一事务里锁住这一行。
func kongEditSchedulable(ctx context.Context, client *dbent.Client, account *service.Account, schedulable bool) bool {
	if client == nil || account == nil || account.ID <= 0 {
		return schedulable
	}
	rows, err := client.QueryContext(ctx,
		`SELECT schedulable, `+kongGuardRevSQL+` FROM accounts WHERE id = $1 AND deleted_at IS NULL`, account.ID)
	if err != nil {
		kongGuardLogf("edit schedulable read failed: account=%d err=%v", account.ID, err)
		return schedulable
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return schedulable
	}
	var dbSchedulable bool
	var dbRev int64
	if err := rows.Scan(&dbSchedulable, &dbRev); err != nil {
		kongGuardLogf("edit schedulable scan failed: account=%d err=%v", account.ID, err)
		return schedulable
	}
	if kongGuardRevFromExtra(account.Extra) != dbRev {
		return dbSchedulable
	}
	return schedulable
}

// kongGuardRevFromExtra 读 extra 里的调度开关版本；缺省或不是数字时为 0。
func kongGuardRevFromExtra(extra map[string]any) int64 {
	switch v := extra[kongGuardRevKey].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// KongGuardSnapshotSyncer 返回关闭或撤销提交之后立即刷新单账号调度快照的函数（独立的短期限）。调度变更通知已在
// 那次提交的事务里写入，这里失败也只是等通知被消费。
func KongGuardSnapshotSyncer(repo service.AccountRepository) func(accountID int64) {
	return func(accountID int64) {
		if ar, ok := repo.(*accountRepository); ok {
			ar.syncSchedulerAccountSnapshotDetached(context.Background(), accountID)
		}
	}
}

func kongGuardLogf(format string, args ...any) {
	logger.LegacyPrintf("repository.kong_downgrade_guard", "[KongDowngradeGuard] "+format, args...)
}
