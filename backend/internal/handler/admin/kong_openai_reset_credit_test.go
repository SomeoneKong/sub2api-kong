//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type kongOpenAIQuotaByExpiryStub struct {
	*openAIQuotaWorkflowStub
	byExpiryCalls int
	lastExpiresAt string
	lastRequestID string
}

func (s *kongOpenAIQuotaByExpiryStub) KongResetCreditByExpiry(_ context.Context, _ int64, expiresAt, redeemRequestID string) (*service.OpenAIQuotaResetResult, error) {
	s.byExpiryCalls++
	s.lastExpiresAt, s.lastRequestID = expiresAt, redeemRequestID
	return s.resetResult, s.resetErr
}

func performKongResetByExpiry(t *testing.T, handler *OpenAIOAuthHandler, body string) (int, openAIQuotaResetEnvelope) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/admin/openai/accounts/:id/kong-reset-quota", handler.KongResetQuotaByExpiry)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/openai/accounts/42/kong-reset-quota", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	var envelope openAIQuotaResetEnvelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return recorder.Code, envelope
}

func TestKongResetQuotaByExpiry_RejectsMissingFields(t *testing.T) {
	quota := &kongOpenAIQuotaByExpiryStub{openAIQuotaWorkflowStub: successfulOpenAIQuotaWorkflowStub()}
	handler := &OpenAIOAuthHandler{adminService: recoveredAccountStub(), quotaService: quota, rateLimitService: &openAIAccountStateRecovererStub{}}

	status, _ := performKongResetByExpiry(t, handler, `{"expires_at": "2026-10-22T20:33:24Z"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Zero(t, quota.byExpiryCalls)
}

func TestKongResetQuotaByExpiry_RequiresCapableService(t *testing.T) {
	handler := &OpenAIOAuthHandler{adminService: recoveredAccountStub(), quotaService: successfulOpenAIQuotaWorkflowStub(), rateLimitService: &openAIAccountStateRecovererStub{}}

	status, _ := performKongResetByExpiry(t, handler, `{"expires_at": "2026-10-22T20:33:24Z", "redeem_request_id": "op-1"}`)
	require.Equal(t, http.StatusBadRequest, status)
}

func TestKongResetQuotaByExpiry_RunsPostProcessLikeResetQuota(t *testing.T) {
	quota := &kongOpenAIQuotaByExpiryStub{openAIQuotaWorkflowStub: successfulOpenAIQuotaWorkflowStub()}
	recoverer := &openAIAccountStateRecovererStub{}
	handler := &OpenAIOAuthHandler{adminService: recoveredAccountStub(), quotaService: quota, rateLimitService: recoverer}

	status, envelope := performKongResetByExpiry(t, handler, `{"expires_at": "2026-10-22T20:33:24.129003Z", "redeem_request_id": "op-1"}`)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, quota.byExpiryCalls)
	require.Equal(t, "2026-10-22T20:33:24.129003Z", quota.lastExpiresAt)
	require.Equal(t, "op-1", quota.lastRequestID)
	require.Zero(t, quota.resetCalls) // 不走无参的 ResetCredit
	require.Equal(t, 1, recoverer.calls)
	require.Equal(t, 1, envelope.Data.WindowsReset)
	require.True(t, envelope.Data.CacheRefreshed)
	require.NotNil(t, envelope.Data.Account)
}
