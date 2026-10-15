package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// groupUsageRollupSnapshot 是一次汇总水位的快照。
// closedBefore / retainedFrom 在水位无效时退化为 1970-01-01，
// 与此前 SQL 里 CASE WHEN valid 的语义一一对应。
type groupUsageRollupSnapshot struct {
	valid        bool
	closedBefore string    // date，历史日桶的右开边界
	retainedDate string    // retained_from 在服务端时区内的日期，历史日桶的左闭边界
	tailStart    time.Time // 尾段（还没进日桶的那部分 usage_logs）的起点
}

// readGroupUsageRollupSnapshot 读一次汇总水位并在 Go 侧判定有效性。
//
// 判定条件与原先 SQL 里的 state_values CTE 完全一致：
// 恰好一行、时区名与当前服务端配置相同、且水位不在未来。
func (r *usageLogRepository) readGroupUsageRollupSnapshot(ctx context.Context, timezoneName, todayDate string) (groupUsageRollupSnapshot, error) {
	epoch := time.Unix(0, 0).UTC()
	invalid := groupUsageRollupSnapshot{
		closedBefore: "1970-01-01",
		retainedDate: "1970-01-01",
		tailStart:    epoch,
	}

	var rowCount int
	var closedBefore sql.NullString
	var retainedFrom sql.NullTime
	var stateTimezone sql.NullString
	if err := scanSingleRow(ctx, r.sql, `
		SELECT
			COUNT(*),
			MAX(closed_before)::text,
			MAX(retained_from),
			MAX(timezone_name)
		FROM usage_group_rollup_state
		WHERE id = 1
	`, nil, &rowCount, &closedBefore, &retainedFrom, &stateTimezone); err != nil {
		return groupUsageRollupSnapshot{}, fmt.Errorf("读取分组用量汇总水位: %w", err)
	}
	if rowCount != 1 || !closedBefore.Valid || !retainedFrom.Valid ||
		!stateTimezone.Valid || stateTimezone.String != timezoneName ||
		closedBefore.String > todayDate {
		return invalid, nil
	}

	tailStart, err := service.ParseGroupUsageDate(closedBefore.String)
	if err != nil {
		// 水位值解析不出来，按无效处理：宁可多扫一次，也不要算错钱。
		return invalid, nil //nolint:nilerr // 与上面的 valid 判定同语义，均降级为全量重算
	}
	return groupUsageRollupSnapshot{
		valid:        true,
		closedBefore: closedBefore.String,
		retainedDate: service.GroupUsageDate(retainedFrom.Time),
		tailStart:    tailStart.UTC(),
	}, nil
}

func (r *usageLogRepository) getAllGroupUsageSummaryFromRollups(ctx context.Context, todayStart time.Time) (results []usagestats.GroupUsageSummary, err error) {
	todayStart = service.GroupUsageTodayStart(todayStart)
	yesterdayStart := service.GroupUsageYesterdayStart(todayStart)
	timezoneName := service.GroupUsageTimezoneName()
	todayDate := service.GroupUsageDate(todayStart)
	yesterdayDate := service.GroupUsageDate(yesterdayStart)

	state, err := r.readGroupUsageRollupSnapshot(ctx, timezoneName, todayDate)
	if err != nil {
		return nil, err
	}

	// 尾段起点必须以**查询参数**的形式给进来。
	//
	// 此前它是在同一条 SQL 里由 state CTE 算出、再 CROSS JOIN 给 tail 用的，
	// 于是 created_at 的下界对 planner 来说是个运行期才知道的值：既进不了
	// index cond，也估不准选择性，只能退化成 usage_logs 全表扫。
	// 生产实测（1721 万行 / PostgreSQL 17）：
	//   Seq Scan on usage_logs … rows=17212870, Rows Removed by Join Filter: 17178082
	//   Execution Time: 48720 ms
	// 换成参数之后走 idx_usage_logs_created_at：
	//   Index Scan … Index Cond: (created_at >= $7)
	//   Execution Time: 34 ms
	const query = `
		WITH historical AS (
			SELECT
				rollup.group_id,
				COALESCE(SUM(rollup.actual_cost), 0) AS actual_cost,
				COALESCE(SUM(rollup.actual_cost) FILTER (
					WHERE rollup.bucket_date = $3::date
				), 0) AS yesterday_cost
			FROM usage_group_daily_rollups rollup
			WHERE $4::boolean
				AND rollup.bucket_date >= $5::date
				AND rollup.bucket_date < $6::date
			GROUP BY rollup.group_id
		),
		tail AS (
			SELECT
				ul.group_id,
				COALESCE(SUM(ul.actual_cost), 0) AS actual_cost,
				COALESCE(SUM(ul.actual_cost) FILTER (WHERE ul.created_at >= $1), 0) AS today_cost,
				COALESCE(SUM(ul.actual_cost) FILTER (
					WHERE ul.created_at >= $2
						AND ul.created_at < $1
				), 0) AS yesterday_cost
			FROM usage_logs ul
			WHERE ul.created_at >= $7
			GROUP BY ul.group_id
		)
		SELECT
			g.id AS group_id,
			COALESCE(historical.actual_cost, 0) + COALESCE(tail.actual_cost, 0) AS total_cost,
			COALESCE(tail.today_cost, 0) AS today_cost,
			COALESCE(historical.yesterday_cost, 0) + COALESCE(tail.yesterday_cost, 0) AS yesterday_cost
		FROM groups g
		LEFT JOIN historical ON historical.group_id = g.id
		LEFT JOIN tail ON tail.group_id = g.id
		ORDER BY g.id
	`

	rows, err := r.sql.QueryContext(
		ctx,
		query,
		todayStart,
		yesterdayStart,
		yesterdayDate,
		state.valid,
		state.retainedDate,
		state.closedBefore,
		state.tailStart,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			results = nil
		}
	}()

	results = make([]usagestats.GroupUsageSummary, 0)
	for rows.Next() {
		var row usagestats.GroupUsageSummary
		if err := rows.Scan(&row.GroupID, &row.TotalCost, &row.TodayCost, &row.YesterdayCost); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// SyncGroupUsageRollups 将服务端配置时区今日以前的用量发布为分组日桶。
func (r *dashboardAggregationRepository) SyncGroupUsageRollups(ctx context.Context, todayStart time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	todayStart = service.GroupUsageTodayStart(todayStart)
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		txRepo := newDashboardAggregationRepositoryWithSQL(tx)
		if err := txRepo.syncGroupUsageRollupsInTx(ctx, todayStart); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.syncGroupUsageRollupsInTx(ctx, todayStart)
}

func (r *dashboardAggregationRepository) syncGroupUsageRollupsInTx(ctx context.Context, todayStart time.Time) error {
	var closedBefore string
	var previousRetainedFrom time.Time
	var stateTimezoneName string
	if err := scanSingleRow(ctx, r.sql, `
		SELECT closed_before::text, retained_from, timezone_name
		FROM usage_group_rollup_state
		WHERE id = 1
		FOR UPDATE
	`, nil, &closedBefore, &previousRetainedFrom, &stateTimezoneName); err != nil {
		return fmt.Errorf("读取分组用量汇总水位: %w", err)
	}

	todayDate := service.GroupUsageDate(todayStart)
	timezoneName := service.GroupUsageTimezoneName()
	timezoneChanged := stateTimezoneName != timezoneName
	var closedTime time.Time
	if !timezoneChanged {
		var err error
		closedTime, err = service.ParseGroupUsageDate(closedBefore)
		if err != nil {
			return fmt.Errorf("解析分组用量汇总水位 %q: %w", closedBefore, err)
		}
		todayDateTime, err := service.ParseGroupUsageDate(todayDate)
		if err != nil {
			return err
		}
		if closedTime.After(todayDateTime) {
			return fmt.Errorf("分组用量汇总水位位于未来: %s", closedBefore)
		}
		if closedBefore == todayDate {
			return nil
		}
	}

	var earliest sql.NullTime
	if err := scanSingleRow(ctx, r.sql, "SELECT MIN(created_at) FROM usage_logs", nil, &earliest); err != nil {
		return fmt.Errorf("读取最早用量记录: %w", err)
	}
	retainedFrom := todayStart
	if earliest.Valid {
		retainedFrom = earliest.Time.UTC()
	}
	retainedDate := service.GroupUsageDate(retainedFrom)
	retainedDateTime, err := service.ParseGroupUsageDate(retainedDate)
	if err != nil {
		return err
	}
	rebuildStartDate := retainedDate
	if !timezoneChanged && closedTime.After(retainedDateTime) {
		rebuildStartDate = closedBefore
	}
	rebuildStart, err := service.ParseGroupUsageDate(rebuildStartDate)
	if err != nil {
		return err
	}

	if _, err := r.sql.ExecContext(ctx, `
		DELETE FROM usage_group_daily_rollups
		WHERE bucket_date < $1::date
			OR (bucket_date >= $2::date AND bucket_date < $3::date)
			OR bucket_date >= $3::date
	`, retainedDate, rebuildStartDate, todayDate); err != nil {
		return fmt.Errorf("清理分组用量日桶: %w", err)
	}

	if _, err := r.sql.ExecContext(ctx, groupUsageDailyRollupsRebuildSQL,
		rebuildStart.UTC(), todayStart.UTC(), timezoneName); err != nil {
		return fmt.Errorf("重建分组用量日桶: %w", err)
	}

	if _, err := r.sql.ExecContext(ctx, `
		UPDATE usage_group_rollup_state
		SET closed_before = $1::date,
			retained_from = $2,
			timezone_name = $3,
			updated_at = NOW()
		WHERE id = 1
	`, todayDate, retainedFrom, timezoneName); err != nil {
		return fmt.Errorf("更新分组用量汇总水位: %w", err)
	}
	return nil
}

// groupUsageDailyRollupsRebuildSQL 按服务端时区把 [$1, $2) 的明细聚合成分组日桶并写入（覆盖已有的桶）。
const groupUsageDailyRollupsRebuildSQL = `
	INSERT INTO usage_group_daily_rollups (bucket_date, group_id, actual_cost, computed_at)
	SELECT
		(created_at AT TIME ZONE $3::text)::date AS bucket_date,
		group_id,
		COALESCE(SUM(actual_cost), 0) AS actual_cost,
		NOW()
	FROM usage_logs
	WHERE group_id IS NOT NULL
		AND created_at >= $1
		AND created_at < $2
	GROUP BY 1, 2
	ON CONFLICT (bucket_date, group_id)
	DO UPDATE SET
		actual_cost = EXCLUDED.actual_cost,
		computed_at = EXCLUDED.computed_at
`

// reconcileGroupUsageRollupsWithRetention 在保留清理之后让日桶跟上剩下的明细：删掉最早一条明细那天之前的日桶，
// 重算从那天到 deletedThrough 那天（取较晚者）之间已发布的日桶，并把 retained_from 推进到最早一条明细；
// closed_before 不动。
//
// 保留清理删的是早于 cutoff 的最早一段明细，之后的日桶没有变化，不需要像降低水位那样把整个保留期重建一遍。
// deletedThrough 是这一轮的删除可能涉及的最晚时刻（见 deleteExpiredUsageLogs）：全部删完时是 cutoff，这时
// 保留边界之前的迟到写入会让最早一条明细早于实际的删除边界，而这段范围里只剩这些迟到写入与 cutoff 那天剩下的
// 明细；没删完时不延伸到 cutoff，避免把尚未删除的旧明细整段重算。零值表示只重算最早一天。
//
// 状态行只加 FOR NO KEY UPDATE：与同步、迟到写入降水位互斥，但与插入触发器的 FOR KEY SHARE 兼容，
// 对账期间新明细照常写入。
func (r *dashboardAggregationRepository) reconcileGroupUsageRollupsWithRetention(ctx context.Context, todayStart, deletedThrough time.Time) error {
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		txRepo := newDashboardAggregationRepositoryWithSQL(tx)
		if err := txRepo.reconcileGroupUsageRollupsWithRetentionInTx(ctx, todayStart, deletedThrough); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.reconcileGroupUsageRollupsWithRetentionInTx(ctx, todayStart, deletedThrough)
}

func (r *dashboardAggregationRepository) reconcileGroupUsageRollupsWithRetentionInTx(ctx context.Context, todayStart, deletedThrough time.Time) error {
	var closedBefore string
	var previousRetainedFrom time.Time
	var stateTimezoneName string
	if err := scanSingleRow(ctx, r.sql, `
		SELECT closed_before::text, retained_from, timezone_name
		FROM usage_group_rollup_state
		WHERE id = 1
		FOR NO KEY UPDATE
	`, nil, &closedBefore, &previousRetainedFrom, &stateTimezoneName); err != nil {
		return fmt.Errorf("读取分组用量汇总水位: %w", err)
	}
	timezoneName := service.GroupUsageTimezoneName()
	if stateTimezoneName != timezoneName {
		// 时区变了，同步会整体重建日桶，这里不动。
		return nil
	}

	var earliest sql.NullTime
	if err := scanSingleRow(ctx, r.sql, "SELECT MIN(created_at) FROM usage_logs", nil, &earliest); err != nil {
		return fmt.Errorf("读取最早用量记录: %w", err)
	}
	retainedFrom := service.GroupUsageTodayStart(todayStart)
	if earliest.Valid {
		retainedFrom = earliest.Time.UTC()
	}

	// 重算 [最早一天, min(lastDate 的次日, closed_before))；closed_before 及以后的用量由读取端直接汇总明细。
	// 日期都是 YYYY-MM-DD，可直接按字符串比较。
	retainedDate := service.GroupUsageDate(retainedFrom)
	lastDate := retainedDate
	if !deletedThrough.IsZero() {
		if throughDate := service.GroupUsageDate(deletedThrough); throughDate > lastDate {
			lastDate = throughDate
		}
	}
	rebuildStart, err := groupUsageDayStart(retainedDate)
	if err != nil {
		return err
	}
	rebuildEndDate, err := nextGroupUsageDate(lastDate)
	if err != nil {
		return err
	}
	var rebuildEnd time.Time
	if closedBefore < rebuildEndDate {
		// 截在 closed_before 时与读取端一样按它解析出的时刻切开日桶与尾段，避免两边重复或遗漏。
		rebuildEndDate = closedBefore
		rebuildEnd, err = service.ParseGroupUsageDate(closedBefore)
		if err != nil {
			return fmt.Errorf("解析分组用量汇总水位 %q: %w", closedBefore, err)
		}
	} else if rebuildEnd, err = groupUsageDayStart(rebuildEndDate); err != nil {
		return err
	}

	if _, err := r.sql.ExecContext(ctx, `
		DELETE FROM usage_group_daily_rollups
		WHERE bucket_date < $1::date
	`, retainedDate); err != nil {
		return fmt.Errorf("清理最早一天之前的分组用量日桶: %w", err)
	}
	if retainedDate < rebuildEndDate {
		if _, err := r.sql.ExecContext(ctx, `
			DELETE FROM usage_group_daily_rollups
			WHERE bucket_date >= $1::date AND bucket_date < $2::date
		`, retainedDate, rebuildEndDate); err != nil {
			return fmt.Errorf("清理待重算的分组用量日桶: %w", err)
		}
		if _, err := r.sql.ExecContext(ctx, groupUsageDailyRollupsRebuildSQL,
			rebuildStart.UTC(), rebuildEnd.UTC(), timezoneName); err != nil {
			return fmt.Errorf("重算最早一天起的分组用量日桶: %w", err)
		}
	}

	if !retainedFrom.Equal(previousRetainedFrom) {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE usage_group_rollup_state
			SET retained_from = $1,
				updated_at = NOW()
			WHERE id = 1
		`, retainedFrom); err != nil {
			return fmt.Errorf("更新分组用量保留起点: %w", err)
		}
	}
	return nil
}

// nextGroupUsageDate 返回日历上的下一天（YYYY-MM-DD）。按日期算，不经过时区里的时刻，免得夏令时把结果落回同一天。
func nextGroupUsageDate(date string) (string, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", err
	}
	return day.AddDate(0, 0, 1).Format("2006-01-02"), nil
}

// groupUsageDayStart 返回配置时区里 date 那一天的第一个时刻。零点因夏令时被跳过时，time.Date 会把它落到前一天，
// 这时取那次切换的时刻。
func groupUsageDayStart(date string) (time.Time, error) {
	start, err := service.ParseGroupUsageDate(date)
	if err != nil {
		return time.Time{}, err
	}
	if service.GroupUsageDate(start) != date {
		if _, end := start.ZoneBounds(); !end.IsZero() {
			start = end
		}
	}
	return start, nil
}

func lockGroupUsageRollupState(ctx context.Context, tx *sql.Tx) error {
	var id int16
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM usage_group_rollup_state
		WHERE id = 1
		FOR UPDATE
	`).Scan(&id); err != nil {
		return fmt.Errorf("锁定分组用量汇总水位: %w", err)
	}
	return nil
}

func invalidateGroupUsageRollupsAt(ctx context.Context, tx *sql.Tx, affectedAt time.Time) error {
	timezoneName := service.GroupUsageTimezoneName()
	_, err := tx.ExecContext(ctx, `
		UPDATE usage_group_rollup_state
		SET closed_before = LEAST(
			closed_before,
			($1::timestamptz AT TIME ZONE $2::text)::date
		),
			updated_at = NOW()
		WHERE id = 1
	`, affectedAt.UTC(), timezoneName)
	return err
}
