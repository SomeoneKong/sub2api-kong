package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type kongAPIKeyProjectsRepoStub struct {
	service.UsageLogRepository
	filters usagestats.UsageLogFilters
	tz      string
	stats   *service.KongAPIKeyProjectStats
}

func (s *kongAPIKeyProjectsRepoStub) KongGetAPIKeyProjectStats(_ context.Context, filters usagestats.UsageLogFilters, tz string) (*service.KongAPIKeyProjectStats, error) {
	s.filters = filters
	s.tz = tz
	return s.stats, nil
}

func newKongAPIKeyProjectsTestRouter(repo service.UsageLogRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewUsageHandler(service.NewUsageService(repo, nil, nil, nil), nil, nil, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		c.Next()
	})
	router.GET("/usage/dashboard/kong-api-key-projects", handler.KongDashboardAPIKeyProjects)
	return router
}

func TestKongDashboardAPIKeyProjectsScopesToCurrentUser(t *testing.T) {
	repo := &kongAPIKeyProjectsRepoStub{stats: &service.KongAPIKeyProjectStats{
		Coverage: service.KongAPIKeyProjectCoverage{Start: "2026-10-01 00:00", End: "2026-10-02 23:00"},
		APIKeys:  []service.KongAPIKeyProjectKey{{APIKeyID: 3, APIKeyName: "laptop"}},
		Rows: []service.KongAPIKeyProjectRow{{
			APIKeyID: 3, Model: "gpt-6-astra", Tier: "fast", Bucket: "2026-10-01 08:00",
			Requests: 2, InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 1, CacheReadTokens: 30, Cost: 1.5, ActualCost: 1.25,
		}},
	}}
	router := newKongAPIKeyProjectsTestRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/kong-api-key-projects?start_date=2026-10-01&end_date=2026-10-02&timezone=Asia/Singapore", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(42), repo.filters.UserID)
	require.Equal(t, "Asia/Singapore", repo.tz)
	require.NotNil(t, repo.filters.StartTime)
	require.NotNil(t, repo.filters.EndTime)
	// 日期范围按同一个时区解释：新加坡 10-01 00:00 是 UTC 09-30 16:00。
	require.Equal(t, "2026-09-30T16:00:00Z", repo.filters.StartTime.UTC().Format("2006-01-02T15:04:05Z07:00"))
	require.Equal(t, "2026-10-02T16:00:00Z", repo.filters.EndTime.UTC().Format("2006-01-02T15:04:05Z07:00"))

	var body struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, map[string]any{"start": "2026-10-01 00:00", "end": "2026-10-02 23:00"}, body.Data["coverage"])
	require.Equal(t, []any{map[string]any{"api_key_id": float64(3), "api_key_name": "laptop", "deleted": false}}, body.Data["api_keys"])
	require.Equal(t, []any{map[string]any{
		"api_key_id":            float64(3),
		"model":                 "gpt-6-astra",
		"tier":                  "fast",
		"bucket":                "2026-10-01 08:00",
		"requests":              float64(2),
		"input_tokens":          float64(10),
		"output_tokens":         float64(20),
		"cache_creation_tokens": float64(1),
		"cache_read_tokens":     float64(30),
		"cost":                  1.5,
		"actual_cost":           1.25,
	}}, body.Data["rows"])
}

func TestKongDashboardAPIKeyProjectsRejectsInvalidFilter(t *testing.T) {
	repo := &kongAPIKeyProjectsRepoStub{}
	router := newKongAPIKeyProjectsTestRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/kong-api-key-projects?request_type=invalid", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, repo.filters.UserID)
}

type kongUsageRepoWithoutAPIKeyProjects struct {
	service.UsageLogRepository
}

func TestKongDashboardAPIKeyProjectsUnsupportedRepository(t *testing.T) {
	router := newKongAPIKeyProjectsTestRouter(&kongUsageRepoWithoutAPIKeyProjects{})

	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/kong-api-key-projects", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestKongUsageBucketTimezone(t *testing.T) {
	require.Equal(t, "Asia/Singapore", kongUsageBucketTimezone("Asia/Singapore"))
	require.Equal(t, "UTC", kongUsageBucketTimezone("UTC"))
	require.Equal(t, timezone.Name(), kongUsageBucketTimezone(""))
	require.Equal(t, timezone.Name(), kongUsageBucketTimezone("Not/AZone"))
	require.Equal(t, timezone.Name(), kongUsageBucketTimezone("Local"))
}
