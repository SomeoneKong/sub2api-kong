//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDashboardAggregationRepositorySyncGroupUsageRollupsNoopsAtCurrentDate(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)
	retainedFrom := time.Date(2026, 5, 1, 3, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-14", retainedFrom, "Asia/Shanghai"))
	mock.ExpectCommit()

	require.NoError(t, repo.SyncGroupUsageRollups(context.Background(), todayStart))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositorySyncGroupUsageRollupsRebuildsWhenTimezoneChanges(t *testing.T) {
	useGroupUsageRepositoryTestTimezone(t, "America/New_York")

	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)
	retainedFrom := time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-03-09", time.Unix(0, 0).UTC(), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(retainedFrom))
	mock.ExpectExec(`DELETE FROM usage_group_daily_rollups`).
		WithArgs("2026-03-01", "2026-03-01", "2026-03-09").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO usage_group_daily_rollups`).
		WithArgs(retainedFrom, todayStart, "America/New_York").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs("2026-03-09", retainedFrom, "America/New_York").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.SyncGroupUsageRollups(context.Background(), todayStart))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositorySyncGroupUsageRollupsPublishesWatermarkLast(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)
	retainedFrom := time.Date(2026, 5, 1, 3, 0, 0, 0, time.UTC)
	rebuildStart := time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-13", retainedFrom, "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(retainedFrom))
	mock.ExpectExec(`DELETE FROM usage_group_daily_rollups`).
		WithArgs("2026-05-01", "2026-08-13", "2026-08-14").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO usage_group_daily_rollups`).
		WithArgs(rebuildStart, todayStart, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs("2026-08-14", retainedFrom, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.SyncGroupUsageRollups(context.Background(), todayStart))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositorySyncGroupUsageRollupsRejectsFutureWatermark(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)
	retainedFrom := time.Date(2026, 5, 1, 3, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-15", retainedFrom, "Asia/Shanghai"))
	mock.ExpectRollback()

	err := repo.SyncGroupUsageRollups(context.Background(), todayStart)
	require.ErrorContains(t, err, "未来")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryRecomputeRangeInvalidatesGroupRollupsBeforeDashboardRebuild(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	start := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(start, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM usage_dashboard_hourly`).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	err := repo.RecomputeRange(context.Background(), start, end)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryRecomputeRangeRebuildsGroupRollupsBeforeCommit(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	start := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	startDate := service.GroupUsageDate(start)
	rebuildStart, err := service.ParseGroupUsageDate(startDate)
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(start, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	for _, query := range []string{
		`DELETE FROM usage_dashboard_hourly WHERE`,
		`DELETE FROM usage_dashboard_hourly_users WHERE`,
		`DELETE FROM usage_dashboard_daily WHERE`,
		`DELETE FROM usage_dashboard_daily_users WHERE`,
		`INSERT INTO usage_dashboard_hourly_users`,
		`INSERT INTO usage_dashboard_daily_users`,
		`INSERT INTO usage_dashboard_hourly`,
		`INSERT INTO usage_dashboard_daily`,
	} {
		mock.ExpectExec(query).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(startDate, time.Unix(0, 0).UTC(), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(start))
	mock.ExpectExec(`DELETE FROM usage_group_daily_rollups`).
		WithArgs(startDate, startDate, service.GroupUsageDate(todayStart)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO usage_group_daily_rollups`).
		WithArgs(rebuildStart.UTC(), todayStart.UTC(), "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(service.GroupUsageDate(todayStart), start, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.RecomputeRange(context.Background(), start, end))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryCleanupUsageLogsNonPartitionedSkipsInvalidationAndReconciles(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	// 剩下的最早一条在 07-01 10:30（北京时间），边界日 2026-07-01 已发布，需要重算。
	earliest := time.Date(2026, 7, 1, 2, 30, 0, 0, time.UTC)
	boundaryStart, err := service.ParseGroupUsageDate("2026-07-01")
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// 一批不满，删完即止；批次事务只设开关、删行，不碰汇总状态行。
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'on'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)SELECT tableoid, ctid.*ORDER BY created_at ASC, id ASC.*DELETE FROM usage_logs`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'off'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	expectUsageLogsRemainingBefore(mock, cutoff, false)
	// 对账：删旧桶、只重算最早一天（cutoff 也在这一天）、更新 retained_from，closed_before 不动。
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, "2026-07-01", "2026-07-02", boundaryStart, boundaryStart.AddDate(0, 0, 1))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// 水位仍是今天，同步直接返回。
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), earliest, "Asia/Shanghai"))
	mock.ExpectCommit()

	require.NoError(t, repo.CleanupUsageLogs(context.Background(), cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryCleanupUsageLogsPartitionedDropsWithoutInvalidationBeforeReconcile(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	earliest := cutoff.Add(time.Hour)
	boundaryStart, err := service.ParseGroupUsageDate(service.GroupUsageDate(earliest))
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT c.relname`).
		WillReturnRows(sqlmock.NewRows([]string{"relname"}).
			AddRow("usage_logs_202606").
			AddRow("usage_logs_invalid").
			AddRow("usage_logs_202604").
			AddRow("usage_logs_202607"))
	// 整月分区按月份先后直接删除，不锁汇总状态、不降水位。
	mock.ExpectExec(`DROP TABLE IF EXISTS "usage_logs_202604"`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE IF EXISTS "usage_logs_202606"`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// The boundary partition is pruned by exact timestamp. tableoid is required
	// because ctid alone can also identify a retained row in another partition.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'on'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)SELECT tableoid, ctid.*WHERE created_at < \$1.*WHERE \(tableoid, ctid\) IN`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'off'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	expectUsageLogsRemainingBefore(mock, cutoff, false)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, service.GroupUsageDate(earliest),
		service.GroupUsageDate(boundaryStart.AddDate(0, 0, 1)), boundaryStart, boundaryStart.AddDate(0, 0, 1))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), earliest, "Asia/Shanghai"))
	mock.ExpectCommit()

	require.NoError(t, repo.CleanupUsageLogs(context.Background(), cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryCleanupUsageLogsNonPartitionedFailureStillReconciles(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	// 前一轮已提交的批次删到了 07-01 06:00（北京时间 14:00），这一轮第一批就失败。
	earliest := time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	dayStart, err := service.ParseGroupUsageDate("2026-07-01")
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'on'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)SELECT tableoid, ctid.*ORDER BY created_at ASC, id ASC.*DELETE FROM usage_logs`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, "2026-07-01", "2026-07-02", dayStart, dayStart.AddDate(0, 0, 1))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 首批删满、第二批失败：已提交的第一批照样对账，删除没完成，只重算最早一天。
func TestDashboardAggregationRepositoryCleanupUsageLogsLaterBatchFailureReconcilesEarliestDay(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	earliest := time.Date(2026, 5, 20, 3, 0, 0, 0, time.UTC)
	dayStart, err := service.ParseGroupUsageDate("2026-05-20")
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectUsageLogsRetentionBatch(mock, cutoff, usageLogsCleanupBatchSize)
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'on'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)SELECT tableoid, ctid.*DELETE FROM usage_logs`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, "2026-05-20", "2026-05-21", dayStart, dayStart.AddDate(0, 0, 1))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 不足一批但还有过期明细（并发的另一轮清理先删了同一批行）：接着删，确认删完才算完成。
func TestDashboardAggregationRepositoryCleanupUsageLogsBatchesContinueWhileExpiredRowsRemain(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	expectUsageLogsRetentionBatch(mock, cutoff, 0)
	expectUsageLogsRemainingBefore(mock, cutoff, true)
	expectUsageLogsRetentionBatch(mock, cutoff, 3)
	expectUsageLogsRemainingBefore(mock, cutoff, false)

	require.NoError(t, repo.cleanupUsageLogsBatches(context.Background(), cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 整月删掉一个分区后，下一个分区删除失败：默认分区里可能还有更早的行，重算一直延伸到已删分区的上界。
func TestDashboardAggregationRepositoryCleanupUsageLogsPartialPartitionDropReconcilesThroughDroppedMonth(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	dropErr := errors.New("drop partition failed")
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	// 默认分区里留着三月的明细。
	earliest := time.Date(2026, 3, 10, 2, 0, 0, 0, time.UTC)
	rebuildStart, err := service.ParseGroupUsageDate("2026-03-10")
	require.NoError(t, err)
	rebuildEnd, err := service.ParseGroupUsageDate("2026-05-02")
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT c.relname`).
		WillReturnRows(sqlmock.NewRows([]string{"relname"}).
			AddRow("usage_logs_default").
			AddRow("usage_logs_202605").
			AddRow("usage_logs_202604"))
	mock.ExpectExec(`DROP TABLE IF EXISTS "usage_logs_202604"`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE IF EXISTS "usage_logs_202605"`).
		WillReturnError(dropErr)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), earliest, "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, "2026-03-10", "2026-05-02", rebuildStart, rebuildEnd)
	mock.ExpectCommit()

	err = repo.CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, dropErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 探测是否分区就出错：什么都没删，但上一轮已提交的删除照样对账；两个错误都返回。
func TestDashboardAggregationRepositoryCleanupUsageLogsPartitionProbeFailureStillReconciles(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	probeErr := errors.New("probe failed")
	reconcileErr := errors.New("reconcile failed")

	mock.ExpectQuery(`SELECT EXISTS`).WillReturnError(probeErr)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).WillReturnError(reconcileErr)
	mock.ExpectRollback()

	err := repo.CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, probeErr)
	require.ErrorIs(t, err, reconcileErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryCleanupUsageLogsPartitionFailureStopsDeletingAndReconciles(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	dropErr := errors.New("drop partition failed")
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	earliest := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT c.relname`).
		WillReturnRows(sqlmock.NewRows([]string{"relname"}).
			AddRow("usage_logs_202606").
			AddRow("usage_logs_202604"))
	mock.ExpectExec(`DROP TABLE IF EXISTS "usage_logs_202604"`).
		WillReturnError(dropErr)
	// 不再删后面的分区与边界批次，但照样对账；删除没完成，只重算最早一天，不把还没删的旧明细整段重算。
	rebuildStart, err := service.ParseGroupUsageDate(service.GroupUsageDate(earliest))
	require.NoError(t, err)
	rebuildEnd := rebuildStart.AddDate(0, 0, 1)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), earliest, "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, service.GroupUsageDate(earliest), service.GroupUsageDate(rebuildEnd), rebuildStart, rebuildEnd)
	mock.ExpectCommit()

	err = repo.CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, dropErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryReconcileLeavesTimezoneChangeToSync(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := service.GroupUsageTodayStart(time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-14", time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "America/New_York"))
	mock.ExpectCommit()

	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(context.Background(), todayStart, todayStart.AddDate(0, 0, -31)))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 上一轮清理删完之后、对账之前进程退出：这一轮没东西可删，对账照样补上新的最早一天。
func TestDashboardAggregationRepositoryReconcileCatchesUpCommittedDeletes(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := service.GroupUsageTodayStart(time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC))
	earliest := time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC)
	dayStart, err := service.ParseGroupUsageDate("2026-07-10")
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-14", time.Date(2026, 7, 9, 1, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, "2026-07-10", "2026-07-11", dayStart, dayStart.AddDate(0, 0, 1))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	cutoff := time.Date(2026, 7, 9, 22, 0, 0, 0, time.UTC) // 北京时间 07-10 06:00
	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(context.Background(), todayStart, cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 保留边界之前有迟到写入：最早一条明细早于 cutoff 那天，重算范围延伸到 cutoff 那天。
func TestDashboardAggregationRepositoryReconcileExtendsRebuildToCutoffDay(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := service.GroupUsageTodayStart(time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC))
	lateEarliest := time.Date(2026, 6, 20, 4, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC) // 北京时间 07-01 10:00
	rebuildStart, err := service.ParseGroupUsageDate("2026-06-20")
	require.NoError(t, err)
	rebuildEnd, err := service.ParseGroupUsageDate("2026-07-02")
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-14", lateEarliest, "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(lateEarliest))
	// retained_from 没变也照样重算 [06-20, 07-02)。
	expectGroupUsageReconcileRebuild(mock, "2026-06-20", "2026-07-02", rebuildStart, rebuildEnd)
	mock.ExpectCommit()

	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(context.Background(), todayStart, cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryReconcileSkipsUnpublishedEarliestDay(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := service.GroupUsageTodayStart(time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC))
	// 剩下的最早一条在 08-12，而日桶只发布到 08-10 之前：那天的用量还走读取端的尾段，不需要日桶。
	earliest := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-10", time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	mock.ExpectExec(`(?s)DELETE FROM usage_group_daily_rollups.*WHERE bucket_date < \$1::date`).
		WithArgs("2026-08-12").
		WillReturnResult(sqlmock.NewResult(0, 90))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(context.Background(), todayStart, todayStart.AddDate(0, 0, -31)))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationRepositoryReconcileEmptyTableDropsPublishedBuckets(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	todayStart := service.GroupUsageTodayStart(time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow("2026-08-14", time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(nil))
	mock.ExpectExec(`(?s)DELETE FROM usage_group_daily_rollups.*WHERE bucket_date < \$1::date`).
		WithArgs("2026-08-14").
		WillReturnResult(sqlmock.NewResult(0, 103))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(todayStart).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(context.Background(), todayStart, todayStart.AddDate(0, 0, -31)))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 智利 2026-09-06 零点因夏令时被跳过（00:00 -04 直接到 01:00 -03）：重算 09-05 时右端仍是 09-06 的第一个时刻，
// 从 09-06 起算时起点也不落回 09-05。
func TestDashboardAggregationRepositoryReconcileHandlesSkippedMidnight(t *testing.T) {
	useGroupUsageRepositoryTestTimezone(t, "America/Santiago")
	todayStart := service.GroupUsageTodayStart(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC))
	for _, tt := range []struct {
		name         string
		earliest     time.Time
		fromDate     string
		toDate       string
		rebuildStart time.Time
		rebuildEnd   time.Time
	}{
		{
			name:         "最早一天在跳过零点的前一天",
			earliest:     time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC),
			fromDate:     "2026-09-05",
			toDate:       "2026-09-06",
			rebuildStart: time.Date(2026, 9, 5, 4, 0, 0, 0, time.UTC),
			rebuildEnd:   time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC),
		},
		{
			name:         "最早一天就是跳过零点的那天",
			earliest:     time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC),
			fromDate:     "2026-09-06",
			toDate:       "2026-09-07",
			rebuildStart: time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC),
			rebuildEnd:   time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := newDashboardAggregationRepositoryWithSQL(db)

			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
				WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
					AddRow("2026-09-20", time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC), "America/Santiago"))
			mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
				WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(tt.earliest))
			mock.ExpectExec(`(?s)DELETE FROM usage_group_daily_rollups.*WHERE bucket_date < \$1::date`).
				WithArgs(tt.fromDate).
				WillReturnResult(sqlmock.NewResult(0, 30))
			mock.ExpectExec(`(?s)DELETE FROM usage_group_daily_rollups.*WHERE bucket_date >= \$1::date AND bucket_date < \$2::date`).
				WithArgs(tt.fromDate, tt.toDate).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO usage_group_daily_rollups`).
				WithArgs(tt.rebuildStart, tt.rebuildEnd, "America/Santiago").
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
				WithArgs(tt.earliest).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			require.NoError(t, repo.reconcileGroupUsageRollupsWithRetention(context.Background(), todayStart, tt.earliest))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// 进入清理时 ctx 已经取消：删除阶段一步也做不了，但对账用自己的时限照样完成并提交，返回取消的错误。
func TestDashboardAggregationRepositoryCleanupUsageLogsCanceledContextStillReconciles(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)
	earliest := time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	dayStart, err := service.ParseGroupUsageDate("2026-07-01")
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Date(2026, 5, 3, 2, 0, 0, 0, time.UTC), "Asia/Shanghai"))
	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(earliest))
	expectGroupUsageReconcileRebuild(mock, "2026-07-01", "2026-07-02", dayStart, dayStart.AddDate(0, 0, 1))
	mock.ExpectExec(`(?s)UPDATE usage_group_rollup_state.*SET retained_from = \$1`).
		WithArgs(earliest).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = repo.CleanupUsageLogs(ctx, cutoff)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet())
}

// expectUsageLogsRetentionBatch 期望一个自开事务的保留批次，删掉 affected 行。
func expectUsageLogsRetentionBatch(mock sqlmock.Sqlmock, cutoff time.Time, affected int64) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'on'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)SELECT tableoid, ctid.*DELETE FROM usage_logs`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, affected))
	mock.ExpectExec(`SET LOCAL sub2api\.usage_logs_retention_cleanup = 'off'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
}

// expectUsageLogsRemainingBefore 期望不足一批之后的剩余检查。
func expectUsageLogsRemainingBefore(mock sqlmock.Sqlmock, cutoff time.Time, remaining bool) {
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM usage_logs WHERE created_at < \$1\)`).
		WithArgs(cutoff).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(remaining))
}

// expectGroupUsageReconcileRebuild 期望对账删掉 fromDate 之前的日桶，并重算 [fromDate, toDate)。
func expectGroupUsageReconcileRebuild(mock sqlmock.Sqlmock, fromDate, toDate string, rebuildStart, rebuildEnd time.Time) {
	mock.ExpectExec(`(?s)DELETE FROM usage_group_daily_rollups.*WHERE bucket_date < \$1::date`).
		WithArgs(fromDate).
		WillReturnResult(sqlmock.NewResult(0, 59))
	mock.ExpectExec(`(?s)DELETE FROM usage_group_daily_rollups.*WHERE bucket_date >= \$1::date AND bucket_date < \$2::date`).
		WithArgs(fromDate, toDate).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO usage_group_daily_rollups`).
		WithArgs(rebuildStart.UTC(), rebuildEnd.UTC(), "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func setGroupUsageRollupTestTimezone(t *testing.T) {
	t.Helper()
	useGroupUsageRepositoryTestTimezone(t, "Asia/Shanghai")
}
