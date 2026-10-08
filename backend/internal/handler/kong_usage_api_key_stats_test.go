package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type kongAPIKeyStatsRepoStub struct {
	service.UsageLogRepository
	filters usagestats.UsageLogFilters
	stats   []service.KongAPIKeyStat
}

func (s *kongAPIKeyStatsRepoStub) KongGetAPIKeyStatsWithUsageFilters(_ context.Context, filters usagestats.UsageLogFilters) ([]service.KongAPIKeyStat, error) {
	s.filters = filters
	return s.stats, nil
}

func newKongAPIKeyStatsTestRouter(repo service.UsageLogRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewUsageHandler(service.NewUsageService(repo, nil, nil, nil), nil, nil, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		c.Next()
	})
	router.GET("/usage/dashboard/kong-api-key-stats", handler.KongDashboardAPIKeyStats)
	return router
}

func TestKongDashboardAPIKeyStatsScopesToCurrentUser(t *testing.T) {
	repo := &kongAPIKeyStatsRepoStub{stats: []service.KongAPIKeyStat{
		{APIKeyID: 3, APIKeyName: "laptop", Requests: 10, TotalTokens: 5000, Cost: 1.5, ActualCost: 1.2},
		{APIKeyID: 9, APIKeyName: "old", Deleted: true, Requests: 2, TotalTokens: 300, Cost: 0.2, ActualCost: 0.1},
	}}
	router := newKongAPIKeyStatsTestRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/kong-api-key-stats?start_date=2026-10-01&end_date=2026-10-02&group_id=7&model=gpt-6-sol&request_type=stream&timezone=UTC", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(42), repo.filters.UserID)
	require.Equal(t, int64(7), repo.filters.GroupID)
	require.Equal(t, "gpt-6-sol", repo.filters.Model)
	require.NotNil(t, repo.filters.RequestType)
	require.Equal(t, int16(service.RequestTypeStream), *repo.filters.RequestType)
	require.NotNil(t, repo.filters.StartTime)
	require.NotNil(t, repo.filters.EndTime)
	require.Equal(t, "2026-10-01T00:00:00Z", repo.filters.StartTime.UTC().Format("2006-01-02T15:04:05Z07:00"))
	require.Equal(t, "2026-10-03T00:00:00Z", repo.filters.EndTime.UTC().Format("2006-01-02T15:04:05Z07:00"))

	var body struct {
		Data map[string][]map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	apiKeys := body.Data["api_keys"]
	require.Len(t, apiKeys, 2)
	require.Equal(t, map[string]any{
		"api_key_id":   float64(3),
		"api_key_name": "laptop",
		"deleted":      false,
		"requests":     float64(10),
		"total_tokens": float64(5000),
		"cost":         1.5,
		"actual_cost":  1.2,
	}, apiKeys[0])
	require.Equal(t, true, apiKeys[1]["deleted"])
}

func TestKongDashboardAPIKeyStatsRejectsInvalidFilter(t *testing.T) {
	repo := &kongAPIKeyStatsRepoStub{}
	router := newKongAPIKeyStatsTestRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/kong-api-key-stats?request_type=invalid", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, repo.filters.UserID)
}

type kongUsageRepoWithoutAPIKeyStats struct {
	service.UsageLogRepository
}

func TestKongDashboardAPIKeyStatsUnsupportedRepository(t *testing.T) {
	router := newKongAPIKeyStatsTestRouter(&kongUsageRepoWithoutAPIKeyStats{})

	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/kong-api-key-stats", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
}
