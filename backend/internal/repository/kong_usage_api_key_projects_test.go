package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var kongAPIKeyProjectColumns = []string{
	"api_key_id", "api_key_name", "deleted", "model", "tier", "bucket",
	"requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "cost", "actual_cost",
}

func TestKongAPIKeyProjectStatsAppliesFiltersAndScans(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	requestType := int16(service.RequestTypeStream)
	filters := usagestats.UsageLogFilters{
		UserID:      42,
		GroupID:     7,
		RequestType: &requestType,
		StartTime:   &start,
		EndTime:     &end,
	}

	// 时区参数排在筛选参数之后；MATERIALIZED 决定查询计划，缺了不报错、只是慢一倍。
	mock.ExpectQuery(`(?s)WITH r AS MATERIALIZED \(.+created_at AT TIME ZONE \$6::text.+FROM usage_logs\s+WHERE user_id = \$1 AND group_id = \$2 AND .+ AND created_at >= \$4 AND created_at < \$5\s+GROUP BY 1, 2, 3, 4, 5.+LEFT JOIN api_keys k ON k.id = r.api_key_id`).
		WithArgs(int64(42), int64(7), requestType, start, end, "Asia/Singapore").
		WillReturnRows(sqlmock.NewRows(kongAPIKeyProjectColumns).
			AddRow(int64(3), "laptop", false, "gpt-6-astra", "fast", "2026-10-01 08:00", int64(2), int64(10), int64(20), int64(0), int64(30), 1.5, 1.2).
			AddRow(int64(3), "laptop", false, "gpt-6-astra", "standard", "2026-10-01 08:00", int64(5), int64(40), int64(50), int64(1), int64(60), 2.5, 2.2).
			AddRow(int64(9), "old", true, "gpt-6.1-sol", "standard", "2026-10-01 09:00", int64(1), int64(7), int64(8), int64(0), int64(9), 0.5, 0.4))
	mock.ExpectQuery(`(?s)GREATEST\(\$1::timestamptz, MIN\(created_at\)\) AT TIME ZONE \$3::text.+LEAST\(\$2::timestamptz - INTERVAL '1 microsecond', NOW\(\)\) AT TIME ZONE \$3::text.+FROM usage_logs`).
		WithArgs(start, end, "Asia/Singapore").
		WillReturnRows(sqlmock.NewRows([]string{"start", "end"}).AddRow("2026-10-01 08:00", "2026-10-02 07:00"))

	stats, err := repo.KongGetAPIKeyProjectStats(context.Background(), filters, "Asia/Singapore")
	require.NoError(t, err)
	require.Equal(t, service.KongAPIKeyProjectCoverage{Start: "2026-10-01 08:00", End: "2026-10-02 07:00"}, stats.Coverage)
	require.Equal(t, []service.KongAPIKeyProjectKey{
		{APIKeyID: 3, APIKeyName: "laptop"},
		{APIKeyID: 9, APIKeyName: "old", Deleted: true},
	}, stats.APIKeys)
	require.Len(t, stats.Rows, 3)
	require.Equal(t, service.KongAPIKeyProjectRow{
		APIKeyID:            3,
		Model:               "gpt-6-astra",
		Tier:                "standard",
		Bucket:              "2026-10-01 08:00",
		Requests:            5,
		InputTokens:         40,
		OutputTokens:        50,
		CacheCreationTokens: 1,
		CacheReadTokens:     60,
		Cost:                2.5,
		ActualCost:          2.2,
	}, stats.Rows[1])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKongAPIKeyProjectStatsEmptyResultIsNotNil(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	mock.ExpectQuery(`WHERE user_id = \$1 AND created_at >= \$2 AND created_at < \$3`).
		WithArgs(int64(42), start, end, "UTC").
		WillReturnRows(sqlmock.NewRows(kongAPIKeyProjectColumns))
	mock.ExpectQuery(`GREATEST`).
		WithArgs(start, end, "UTC").
		WillReturnRows(sqlmock.NewRows([]string{"start", "end"}).AddRow("2026-10-01 00:00", "2026-10-01 23:00"))

	stats, err := repo.KongGetAPIKeyProjectStats(context.Background(), usagestats.UsageLogFilters{UserID: 42, StartTime: &start, EndTime: &end}, "UTC")
	require.NoError(t, err)
	require.NotNil(t, stats.APIKeys)
	require.Empty(t, stats.APIKeys)
	require.NotNil(t, stats.Rows)
	require.Empty(t, stats.Rows)
	require.Equal(t, "2026-10-01 00:00", stats.Coverage.Start)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKongAPIKeyProjectStatsRequiresTimeRange(t *testing.T) {
	db, _ := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err := repo.KongGetAPIKeyProjectStats(context.Background(), usagestats.UsageLogFilters{UserID: 42, StartTime: &start}, "UTC")
	require.Error(t, err)
}
