package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKongPlanStatsRepository(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewKongPlanStatsRepository(db)
	ctx := context.Background()
	hour := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	row := service.KongPlanStatRow{
		KongPlanStatKey: service.KongPlanStatKey{Hour: hour, AccountID: 2, Layer: "credits", Seg: "credits_room", InUse: "snapshot",
			PlanVersion: 5, PublishSeq: 42},
		Selected: 3, Waited: 1, SessionsSum: 4,
	}

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO kong_dispatch_stats .*ON CONFLICT .*DO UPDATE SET .*GREATEST`).
		WithArgs(hour, int64(2), "credits", "", "credits_room", "snapshot", int64(5), int64(42),
			int64(3), int64(0), int64(1), int64(4), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, repo.UpsertKongPlanStats(ctx, []service.KongPlanStatRow{row}))

	from, to := hour.Add(-time.Hour), hour.Add(time.Hour)
	mock.ExpectQuery(`(?s)SELECT hour, account_id.*FROM kong_dispatch_stats.*WHERE hour >= \$1 AND hour < \$2`).WithArgs(from, to).
		WillReturnRows(sqlmock.NewRows([]string{"hour", "account_id", "layer", "grp", "seg", "in_use", "plan_version", "publish_seq",
			"selected", "confirm_failed", "waited", "sessions_sum", "ahead_limit", "ahead_busy", "ahead_unavailable",
			"dwell_n", "dwell_sum_s", "dwell_max_s"}).
			AddRow(hour, 2, "credits", "", "credits_room", "snapshot", 5, 42, 3, 0, 1, 4, 0, 0, 0, 0, 0, 0))
	rows, err := repo.ListKongPlanStats(ctx, from, to)
	require.NoError(t, err)
	require.Equal(t, []service.KongPlanStatRow{row}, rows)

	mock.ExpectExec(`DELETE FROM kong_dispatch_stats WHERE hour < \$1`).WithArgs(from).WillReturnResult(sqlmock.NewResult(0, 7))
	require.NoError(t, repo.PruneKongPlanStats(ctx, from))
	require.NoError(t, mock.ExpectationsWereMet())
}
