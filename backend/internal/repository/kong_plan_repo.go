package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongPlanRepository 是账号选择与 credits 的数据库访问（raw SQL，不走 ent，理由同 kong_ticket_repo.go）。
// 表见 migrations/907_kong_plan_dispatch.sql。
type kongPlanRepository struct {
	db *sql.DB
}

// NewKongPlanRepository 创建计划组件的数据库仓储。
func NewKongPlanRepository(db *sql.DB) service.KongPlanRepository {
	return &kongPlanRepository{db: db}
}

// LoadKongPlan 在一个只读事务里读出全部行与调用方声明，得到同一时刻的一致状态。
func (r *kongPlanRepository) LoadKongPlan(ctx context.Context) (_ []service.KongPlanRow, _ *service.KongPoolCaller, err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT kind, content FROM kong_plan_dispatch ORDER BY kind`)
	if err != nil {
		return nil, nil, err
	}
	var out []service.KongPlanRow
	for rows.Next() {
		var row service.KongPlanRow
		if err := rows.Scan(&row.Kind, &row.Content); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		out = append(out, row)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var (
		caller   service.KongPoolCaller
		desired  sql.NullFloat64
		apiKeyID sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT state, desired_pp_per_hour, until_at, note, api_key_id, updated_at
		  FROM kong_pool_caller WHERE id = 1`).
		Scan(&caller.State, &desired, &caller.Until, &caller.Note, &apiKeyID, &caller.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, nil, tx.Commit()
	case err != nil:
		return nil, nil, err
	}
	if desired.Valid {
		v := desired.Float64
		caller.DesiredPPPerHour = &v
	}
	if apiKeyID.Valid {
		v := apiKeyID.Int64
		caller.APIKeyID = &v
	}
	caller.Until, caller.UpdatedAt = caller.Until.UTC(), caller.UpdatedAt.UTC()
	return out, &caller, tx.Commit()
}

// SaveKongPlanRows 在一个事务里写入给定的行。
func (r *kongPlanRepository) SaveKongPlanRows(ctx context.Context, rows ...service.KongPlanRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range rows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO kong_plan_dispatch (kind, generation, seq, revision, sha256, content, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, now())
			ON CONFLICT (kind) DO UPDATE SET
			    generation = EXCLUDED.generation,
			    seq        = EXCLUDED.seq,
			    revision   = EXCLUDED.revision,
			    sha256     = EXCLUDED.sha256,
			    content    = EXCLUDED.content,
			    updated_at = EXCLUDED.updated_at`,
			row.Kind, kongPlanNullInt(row.Generation), kongPlanNullInt(row.Seq), kongPlanNullInt(row.Revision),
			sql.NullString{String: row.SHA256, Valid: row.SHA256 != ""}, string(row.Content))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveKongPoolCaller 覆盖写入调用方声明。
func (r *kongPlanRepository) SaveKongPoolCaller(ctx context.Context, c service.KongPoolCaller) error {
	var desired sql.NullFloat64
	if c.DesiredPPPerHour != nil {
		desired = sql.NullFloat64{Float64: *c.DesiredPPPerHour, Valid: true}
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO kong_pool_caller (id, state, desired_pp_per_hour, until_at, note, api_key_id, updated_at)
		VALUES (1, $1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
		    state               = EXCLUDED.state,
		    desired_pp_per_hour = EXCLUDED.desired_pp_per_hour,
		    until_at            = EXCLUDED.until_at,
		    note                = EXCLUDED.note,
		    api_key_id          = EXCLUDED.api_key_id,
		    updated_at          = EXCLUDED.updated_at`,
		c.State, desired, c.Until, c.Note, kongPlanNullInt(c.APIKeyID), c.UpdatedAt)
	return err
}

// KongPlanAccountKinds 返回给定编号中存在且未删除的账号的平台与类型。
func (r *kongPlanRepository) KongPlanAccountKinds(ctx context.Context, ids []int64) (map[int64]service.KongPlanAccountKind, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, platform, type FROM accounts
		 WHERE id = ANY($1) AND deleted_at IS NULL`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]service.KongPlanAccountKind, len(ids))
	for rows.Next() {
		var (
			id   int64
			kind service.KongPlanAccountKind
		)
		if err := rows.Scan(&id, &kind.Platform, &kind.Type); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

func kongPlanNullInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}
