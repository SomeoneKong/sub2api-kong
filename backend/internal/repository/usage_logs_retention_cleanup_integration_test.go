//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 保留清理删掉边界之前的明细后：水位不动，整天被删的日桶删掉，最早一天按剩下的明细重算，retained_from 跟上；
// 不带保留清理开关的删除照旧把水位降到被删的日子。
func TestUsageLogsRetentionCleanupKeepsWatermarkAndReconcilesEarliestDay(t *testing.T) {
	ctx := context.Background()
	tx, repo, todayStart := seedUsageLogsRetentionTest(t, ctx)
	defer func() { _ = tx.Rollback() }()

	cutoff := time.Date(2020, 1, 2, 4, 0, 0, 0, time.UTC) // 北京时间 01-02 12:00
	require.NoError(t, repo.CleanupUsageLogs(ctx, cutoff))
	requireGroupUsageRollupState(t, ctx, tx, "2020-01-05", map[string]float64{
		"2020-01-02": 4, "2020-01-03": 8,
	})
	requireGroupUsageRetainedFrom(t, ctx, tx, time.Date(2020, 1, 2, 12, 0, 0, 0, time.UTC))
	requireGroupUsageTotalCost(t, ctx, tx, todayStart, 12)

	// 回归：普通删除不带开关，照旧失效日桶。
	_, err := tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE id = 4`)
	require.NoError(t, err)
	var closedBefore string
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT closed_before::text FROM usage_group_rollup_state WHERE id = 1
	`).Scan(&closedBefore))
	require.Equal(t, "2020-01-03", closedBefore)
}

// 上一轮的批次已提交、还没对账进程就退出了：下一轮在新的事务里没有东西可删，对账照样把日桶补对。
func TestUsageLogsRetentionCleanupCatchesUpInterruptedRound(t *testing.T) {
	ctx := context.Background()
	seedTx, _, todayStart := seedUsageLogsRetentionTest(t, ctx)
	schema := currentSchema(t, ctx, seedTx)
	require.NoError(t, seedTx.Commit())

	cutoff := time.Date(2020, 1, 2, 4, 0, 0, 0, time.UTC)
	batchTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	deleted, err := deleteUsageLogsRetentionBatch(ctx, batchTx, cutoff)
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted)
	require.NoError(t, batchTx.Commit())

	staleTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	// 只删不对账：日桶还是删之前的样子。
	requireGroupUsageRollupState(t, ctx, staleTx, "2020-01-05", map[string]float64{
		"2020-01-01": 1, "2020-01-02": 6, "2020-01-03": 8,
	})
	require.NoError(t, staleTx.Rollback())

	nextTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	require.NoError(t, setGroupUsageRollupTriggerTimeZone(ctx, nextTx, "Asia/Shanghai"))
	repo := newDashboardAggregationRepositoryWithSQL(nextTx)
	repo.clock = func() time.Time { return time.Date(2020, 1, 5, 0, 30, 0, 0, time.UTC) }
	require.NoError(t, repo.CleanupUsageLogs(ctx, cutoff))
	require.NoError(t, nextTx.Commit())

	checkTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = checkTx.Rollback() }()
	requireGroupUsageRollupState(t, ctx, checkTx, "2020-01-05", map[string]float64{
		"2020-01-02": 4, "2020-01-03": 8,
	})
	requireGroupUsageRetainedFrom(t, ctx, checkTx, time.Date(2020, 1, 2, 12, 0, 0, 0, time.UTC))
	requireGroupUsageTotalCost(t, ctx, checkTx, todayStart, 12)
}

// 分区表：默认分区里有更早的明细，过期的月分区整月删除，边界分区与默认分区里的过期行按批删除；
// 删完之后对账，日桶与累计费用对上剩下的明细。
func TestUsageLogsRetentionCleanupPartitionedWithDefaultPartition(t *testing.T) {
	ctx := context.Background()
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")
	schema := createGroupUsageRollupTriggerTestSchema(t, ctx, true)
	tx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, setGroupUsageRollupTriggerTimeZone(ctx, tx, "Asia/Shanghai"))

	_, err := tx.ExecContext(ctx, `
		CREATE TABLE usage_logs_202001 PARTITION OF usage_logs
			FOR VALUES FROM ('2020-01-01 00:00:00+08') TO ('2020-02-01 00:00:00+08');
		CREATE TABLE usage_logs_202002 PARTITION OF usage_logs
			FOR VALUES FROM ('2020-02-01 00:00:00+08') TO ('2020-03-01 00:00:00+08');
		INSERT INTO groups (id) VALUES (10);
		INSERT INTO users (id) VALUES (1);
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at) VALUES
			(1, 1, 10, 1, TIMESTAMPTZ '2019-12-31 10:00:00+08'),
			(2, 1, 10, 2, TIMESTAMPTZ '2020-01-15 10:00:00+08'),
			(3, 1, 10, 4, TIMESTAMPTZ '2020-02-02 08:00:00+08'),
			(4, 1, 10, 8, TIMESTAMPTZ '2020-02-02 20:00:00+08'),
			(5, 1, 10, 16, TIMESTAMPTZ '2020-02-03 09:00:00+08');
	`)
	require.NoError(t, err)

	todayStart := time.Date(2020, 2, 4, 16, 0, 0, 0, time.UTC)
	repo := newDashboardAggregationRepositoryWithSQL(tx)
	repo.clock = func() time.Time { return time.Date(2020, 2, 5, 0, 30, 0, 0, time.UTC) }
	require.NoError(t, repo.SyncGroupUsageRollups(ctx, todayStart))
	requireGroupUsageRollupState(t, ctx, tx, "2020-02-05", map[string]float64{
		"2019-12-31": 1, "2020-01-15": 2, "2020-02-02": 12, "2020-02-03": 16,
	})

	cutoff := time.Date(2020, 2, 2, 4, 0, 0, 0, time.UTC) // 北京时间 02-02 12:00
	require.NoError(t, repo.CleanupUsageLogs(ctx, cutoff))
	var januaryPartitions int
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pg_class WHERE relname = 'usage_logs_202001' AND relnamespace = current_schema()::regnamespace
	`).Scan(&januaryPartitions))
	require.Zero(t, januaryPartitions, "整月过期的分区要被删掉")
	requireGroupUsageRollupState(t, ctx, tx, "2020-02-05", map[string]float64{
		"2020-02-02": 8, "2020-02-03": 16,
	})
	requireGroupUsageRetainedFrom(t, ctx, tx, time.Date(2020, 2, 2, 12, 0, 0, 0, time.UTC))
	requireGroupUsageTotalCost(t, ctx, tx, todayStart, 24)
}

// 删完之后又来了一条早于保留边界的迟到写入（没有分组，插入触发器不降水位）：最早一条明细不再是删除边界，
// 带 cutoff 的对账把重算延伸到 cutoff 那天，中间被删动的日桶照样对上。
func TestUsageLogsRetentionReconcileCoversLateRowBeforeCutoff(t *testing.T) {
	ctx := context.Background()
	tx, repo, todayStart := seedUsageLogsRetentionTest(t, ctx)
	defer func() { _ = tx.Rollback() }()

	cutoff := time.Date(2020, 1, 2, 4, 0, 0, 0, time.UTC)
	_, err := deleteUsageLogsRetentionBatch(ctx, tx, cutoff)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at)
		VALUES (5, 1, NULL, 16, TIMESTAMPTZ '2019-12-31 10:00:00+08')
	`)
	require.NoError(t, err)

	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(ctx, todayStart, cutoff))
	requireGroupUsageRollupState(t, ctx, tx, "2020-01-05", map[string]float64{
		"2020-01-02": 4, "2020-01-03": 8,
	})
	requireGroupUsageRetainedFrom(t, ctx, tx, time.Date(2019, 12, 31, 2, 0, 0, 0, time.UTC))
	requireGroupUsageTotalCost(t, ctx, tx, todayStart, 12)
}

// 保留清理的批次事务不锁汇总状态行：它没提交时，另一个连接照样能写入新明细。
func TestUsageLogsRetentionCleanupBatchDoesNotBlockUsageInserts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	schema := seedUsageLogsRetentionLockTest(t, ctx)
	cleanupTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = cleanupTx.Rollback() }()
	deleted, err := deleteUsageLogsRetentionBatch(ctx, cleanupTx, time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	requireUsageLogInsertNotBlocked(t, ctx, schema, 3, "保留清理的批次事务未提交时，新明细的写入不能等它")
	require.NoError(t, cleanupTx.Commit())
}

// 对账事务持有状态行的 FOR NO KEY UPDATE 并已重算日桶、改写 retained_from：它没提交时，另一个连接照样能写入新明细。
func TestUsageLogsRetentionReconcileDoesNotBlockUsageInserts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")

	schema := seedUsageLogsRetentionLockTest(t, ctx)
	cutoff := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	batchTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	deleted, err := deleteUsageLogsRetentionBatch(ctx, batchTx, cutoff)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	require.NoError(t, batchTx.Commit())

	reconcileTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = reconcileTx.Rollback() }()
	repo := newDashboardAggregationRepositoryWithSQL(reconcileTx)
	todayStart := service.GroupUsageTodayStart(time.Now())
	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(ctx, todayStart, cutoff))
	// 对账确实动了：01-01 的日桶删掉、01-03 重算，retained_from 推进，closed_before 不变。
	requireGroupUsageRollupState(t, ctx, reconcileTx, service.GroupUsageDate(todayStart), map[string]float64{"2020-01-03": 2})
	requireGroupUsageRetainedFrom(t, ctx, reconcileTx, time.Date(2020, 1, 3, 2, 0, 0, 0, time.UTC))

	requireUsageLogInsertNotBlocked(t, ctx, schema, 3, "对账事务未提交时，新明细的写入不能等它")
	require.NoError(t, reconcileTx.Commit())
}

// seedUsageLogsRetentionTest 建表、写入四条分组 10 的明细并发布日桶（北京时间今天 = 2020-01-05）。
// 返回的事务由调用方回滚；repo 在这个事务里执行。
func seedUsageLogsRetentionTest(t *testing.T, ctx context.Context) (*sql.Tx, *dashboardAggregationRepository, time.Time) {
	t.Helper()
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")
	now := time.Date(2020, 1, 5, 0, 30, 0, 0, time.UTC) // 北京时间 01-05 08:30
	todayStart := time.Date(2020, 1, 4, 16, 0, 0, 0, time.UTC)
	schema := createGroupUsageRollupTriggerTestSchema(t, ctx, false)
	tx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	require.NoError(t, setGroupUsageRollupTriggerTimeZone(ctx, tx, "Asia/Shanghai"))

	_, err := tx.ExecContext(ctx, `
		INSERT INTO groups (id) VALUES (10);
		INSERT INTO users (id) VALUES (1);
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at) VALUES
			(1, 1, 10, 1, TIMESTAMPTZ '2020-01-01 10:00:00+08'),
			(2, 1, 10, 2, TIMESTAMPTZ '2020-01-02 08:00:00+08'),
			(3, 1, 10, 4, TIMESTAMPTZ '2020-01-02 20:00:00+08'),
			(4, 1, 10, 8, TIMESTAMPTZ '2020-01-03 09:00:00+08');
	`)
	require.NoError(t, err)

	repo := newDashboardAggregationRepositoryWithSQL(tx)
	repo.clock = func() time.Time { return now }
	require.NoError(t, repo.SyncGroupUsageRollups(ctx, todayStart))
	requireGroupUsageRollupState(t, ctx, tx, "2020-01-05", map[string]float64{
		"2020-01-01": 1, "2020-01-02": 6, "2020-01-03": 8,
	})
	return tx, repo, todayStart
}

// seedUsageLogsRetentionLockTest 建表并提交两条明细与它们的日桶，水位视为已发布到今天、时区与配置一致。
func seedUsageLogsRetentionLockTest(t *testing.T, ctx context.Context) string {
	t.Helper()
	schema := createGroupUsageRollupTriggerTestSchema(t, ctx, false)
	seedTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	_, err := seedTx.ExecContext(ctx, `
		INSERT INTO groups (id) VALUES (10);
		INSERT INTO users (id) VALUES (1);
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at) VALUES
			(1, 1, 10, 1, TIMESTAMPTZ '2020-01-01 10:00:00+08'),
			(2, 1, 10, 2, TIMESTAMPTZ '2020-01-03 10:00:00+08');
		INSERT INTO usage_group_daily_rollups (bucket_date, group_id, actual_cost, computed_at) VALUES
			(DATE '2020-01-01', 10, 1, NOW()),
			(DATE '2020-01-03', 10, 2, NOW());
		UPDATE usage_group_rollup_state
		SET closed_before = (CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Shanghai')::date,
			retained_from = TIMESTAMPTZ '2020-01-01 10:00:00+08',
			timezone_name = 'Asia/Shanghai'
		WHERE id = 1;
	`)
	require.NoError(t, err)
	require.NoError(t, seedTx.Commit())
	return schema
}

// currentSchema 返回事务里 search_path 指向的隔离 schema。
func currentSchema(t *testing.T, ctx context.Context, tx *sql.Tx) string {
	t.Helper()
	var schema string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))
	return schema
}

// requireUsageLogInsertNotBlocked 在另一个连接上以 1 秒的锁等待上限写入一条今天的明细，必须成功。
// 会话时区与水位所用的上海时区一致，插入触发器才会把它判为今天、不去降水位。
func requireUsageLogInsertNotBlocked(t *testing.T, ctx context.Context, schema string, id int64, msg string) {
	t.Helper()
	insertTx := beginGroupUsageRollupTriggerTestTx(t, ctx, schema)
	defer func() { _ = insertTx.Rollback() }()
	require.NoError(t, setGroupUsageRollupTriggerTimeZone(ctx, insertTx, "Asia/Shanghai"))
	_, err := insertTx.ExecContext(ctx, `SET LOCAL lock_timeout = '1s'`)
	require.NoError(t, err)
	_, err = insertTx.ExecContext(ctx, `
		INSERT INTO usage_logs (id, user_id, group_id, actual_cost, created_at)
		VALUES ($1, 1, 10, 4, CURRENT_TIMESTAMP)
	`, id)
	require.NoError(t, err, msg)
	require.NoError(t, insertTx.Commit())
}

// requireGroupUsageRollupState 断言水位与分组 10 的全部日桶（日期 → 费用）。
func requireGroupUsageRollupState(t *testing.T, ctx context.Context, tx *sql.Tx, closedBefore string, buckets map[string]float64) {
	t.Helper()
	var gotClosedBefore string
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT closed_before::text FROM usage_group_rollup_state WHERE id = 1
	`).Scan(&gotClosedBefore))
	require.Equal(t, closedBefore, gotClosedBefore)

	rows, err := tx.QueryContext(ctx, `
		SELECT bucket_date::text, actual_cost::float8
		FROM usage_group_daily_rollups
		WHERE group_id = 10
		ORDER BY bucket_date
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string]float64{}
	for rows.Next() {
		var date string
		var cost float64
		require.NoError(t, rows.Scan(&date, &cost))
		got[date] = cost
	}
	require.NoError(t, rows.Err())
	require.Equal(t, buckets, got)
}

func requireGroupUsageRetainedFrom(t *testing.T, ctx context.Context, tx *sql.Tx, want time.Time) {
	t.Helper()
	var retainedFrom time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT retained_from FROM usage_group_rollup_state WHERE id = 1
	`).Scan(&retainedFrom))
	require.True(t, retainedFrom.Equal(want), retainedFrom)
}

// requireGroupUsageTotalCost 断言读取端给出的分组 10 累计费用。
func requireGroupUsageTotalCost(t *testing.T, ctx context.Context, tx *sql.Tx, todayStart time.Time, want float64) {
	t.Helper()
	summary, err := newUsageLogRepositoryWithSQL(nil, tx).GetAllGroupUsageSummary(ctx, todayStart)
	require.NoError(t, err)
	require.Len(t, summary, 1)
	require.InDelta(t, want, summary[0].TotalCost, 0.0000001)
}
