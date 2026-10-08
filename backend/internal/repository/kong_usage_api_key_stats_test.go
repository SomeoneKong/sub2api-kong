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

var kongAPIKeyStatsColumns = []string{"api_key_id", "api_key_name", "deleted", "requests", "total_tokens", "cost", "actual_cost"}

func TestKongAPIKeyStatsAppliesUsageFiltersAndScans(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	requestType := int16(service.RequestTypeStream)
	billingType := int8(0)
	filters := usagestats.UsageLogFilters{
		UserID:            42,
		GroupID:           7,
		Model:             "gpt-6-sol",
		ModelFilterSource: usagestats.ModelSourceRequested,
		RequestType:       &requestType,
		BillingType:       &billingType,
		StartTime:         &start,
		EndTime:           &end,
	}

	mock.ExpectQuery(`(?s)FROM usage_logs\s+WHERE user_id = \$1 AND group_id = \$2 AND .+ = \$3 AND .+ AND billing_type = \$5 AND created_at >= \$6 AND created_at < \$7\s+GROUP BY api_key_id\s+\) s\s+LEFT JOIN api_keys k ON k.id = s.api_key_id\s+ORDER BY s.total_tokens DESC, s.api_key_id`).
		WithArgs(int64(42), int64(7), "gpt-6-sol", requestType, int16(0), start, end).
		WillReturnRows(sqlmock.NewRows(kongAPIKeyStatsColumns).
			AddRow(int64(3), "laptop", false, int64(10), int64(5000), 1.5, 1.2).
			AddRow(int64(9), "old", true, int64(2), int64(300), 0.2, 0.1))

	stats, err := repo.KongGetAPIKeyStatsWithUsageFilters(context.Background(), filters)
	require.NoError(t, err)
	require.Len(t, stats, 2)
	require.Equal(t, int64(3), stats[0].APIKeyID)
	require.Equal(t, "laptop", stats[0].APIKeyName)
	require.False(t, stats[0].Deleted)
	require.Equal(t, int64(10), stats[0].Requests)
	require.Equal(t, int64(5000), stats[0].TotalTokens)
	require.Equal(t, 1.5, stats[0].Cost)
	require.Equal(t, 1.2, stats[0].ActualCost)
	require.True(t, stats[1].Deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKongAPIKeyStatsEmptyResultIsNotNil(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	mock.ExpectQuery(`WHERE user_id = \$1 AND created_at >= \$2 AND created_at < \$3`).
		WithArgs(int64(42), start, end).
		WillReturnRows(sqlmock.NewRows(kongAPIKeyStatsColumns))

	stats, err := repo.KongGetAPIKeyStatsWithUsageFilters(context.Background(), usagestats.UsageLogFilters{UserID: 42, StartTime: &start, EndTime: &end})
	require.NoError(t, err)
	require.NotNil(t, stats)
	require.Empty(t, stats)
	require.NoError(t, mock.ExpectationsWereMet())
}
