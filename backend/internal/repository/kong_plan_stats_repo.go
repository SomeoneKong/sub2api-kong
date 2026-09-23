package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongPlanStatsRepository 是决策记录的数据库访问（raw SQL），表见 migrations/907_kong_plan_dispatch.sql。
type kongPlanStatsRepository struct {
	db *sql.DB
}

// NewKongPlanStatsRepository 创建决策记录的仓储。
func NewKongPlanStatsRepository(db *sql.DB) service.KongPlanStatsRepository {
	return &kongPlanStatsRepository{db: db}
}

// UpsertKongPlanStats 在一个事务里把各行累加进表。
func (r *kongPlanStatsRepository) UpsertKongPlanStats(ctx context.Context, rows []service.KongPlanStatRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range rows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO kong_dispatch_stats (hour, account_id, layer, grp, seg, in_use, plan_version, publish_seq,
			    selected, confirm_failed, waited, sessions_sum, ahead_limit, ahead_busy, ahead_unavailable,
			    dwell_n, dwell_sum_s, dwell_max_s)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
			ON CONFLICT (hour, account_id, layer, grp, seg, in_use, plan_version, publish_seq) DO UPDATE SET
			    selected          = kong_dispatch_stats.selected + EXCLUDED.selected,
			    confirm_failed    = kong_dispatch_stats.confirm_failed + EXCLUDED.confirm_failed,
			    waited            = kong_dispatch_stats.waited + EXCLUDED.waited,
			    sessions_sum      = kong_dispatch_stats.sessions_sum + EXCLUDED.sessions_sum,
			    ahead_limit       = kong_dispatch_stats.ahead_limit + EXCLUDED.ahead_limit,
			    ahead_busy        = kong_dispatch_stats.ahead_busy + EXCLUDED.ahead_busy,
			    ahead_unavailable = kong_dispatch_stats.ahead_unavailable + EXCLUDED.ahead_unavailable,
			    dwell_n           = kong_dispatch_stats.dwell_n + EXCLUDED.dwell_n,
			    dwell_sum_s       = kong_dispatch_stats.dwell_sum_s + EXCLUDED.dwell_sum_s,
			    dwell_max_s       = GREATEST(kong_dispatch_stats.dwell_max_s, EXCLUDED.dwell_max_s)`,
			row.Hour, row.AccountID, row.Layer, row.Group, row.Seg, row.InUse, row.PlanVersion, row.PublishSeq,
			row.Selected, row.ConfirmFailed, row.Waited, row.SessionsSum, row.AheadLimit, row.AheadBusy, row.AheadUnavailable,
			row.DwellN, row.DwellSumS, row.DwellMaxS)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListKongPlanStats 按小时与维度排序返回 [from, to) 内的行。
func (r *kongPlanStatsRepository) ListKongPlanStats(ctx context.Context, from, to time.Time) ([]service.KongPlanStatRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT hour, account_id, layer, grp, seg, in_use, plan_version, publish_seq,
		       selected, confirm_failed, waited, sessions_sum, ahead_limit, ahead_busy, ahead_unavailable,
		       dwell_n, dwell_sum_s, dwell_max_s
		  FROM kong_dispatch_stats
		 WHERE hour >= $1 AND hour < $2
		 ORDER BY hour, account_id, layer, grp, seg, in_use, plan_version, publish_seq`, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []service.KongPlanStatRow{}
	for rows.Next() {
		var x service.KongPlanStatRow
		if err := rows.Scan(&x.Hour, &x.AccountID, &x.Layer, &x.Group, &x.Seg, &x.InUse, &x.PlanVersion, &x.PublishSeq,
			&x.Selected, &x.ConfirmFailed, &x.Waited, &x.SessionsSum, &x.AheadLimit, &x.AheadBusy, &x.AheadUnavailable,
			&x.DwellN, &x.DwellSumS, &x.DwellMaxS); err != nil {
			return nil, err
		}
		x.Hour = x.Hour.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

// PruneKongPlanStats 删掉 before 之前的行。
func (r *kongPlanStatsRepository) PruneKongPlanStats(ctx context.Context, before time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM kong_dispatch_stats WHERE hour < $1`, before)
	return err
}
