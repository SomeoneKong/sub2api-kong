//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKongPickResetCreditByExpiry(t *testing.T) {
	cands := []openAIAutoResetCreditCandidate{
		{ID: "a", ExpiresAt: "2026-10-22T20:33:24.129003Z"},
		{ID: "b", ExpiresAt: "2026-11-01T00:00:00Z"},
		{ID: "", ExpiresAt: "2026-12-01T00:00:00Z"},
	}
	exact, _ := time.Parse(time.RFC3339, "2026-10-22T20:33:24.129003Z")
	got, err := kongPickResetCreditByExpiry(cands, exact)
	require.NoError(t, err)
	require.Equal(t, "a", got.ID)

	// 调用方丢了微秒也能按秒匹配到同一张
	second, _ := time.Parse(time.RFC3339, "2026-10-23T04:33:24+08:00")
	got, err = kongPickResetCreditByExpiry(cands, second)
	require.NoError(t, err)
	require.Equal(t, "a", got.ID)

	_, err = kongPickResetCreditByExpiry(cands, exact.Add(time.Hour))
	require.Error(t, err)

	noID, _ := time.Parse(time.RFC3339, "2026-12-01T00:00:00Z")
	_, err = kongPickResetCreditByExpiry(cands, noID)
	require.Error(t, err)

	// 同一秒两张：整秒请求不猜，报冲突；带小数秒的请求精确命中仍唯一
	dup := append(cands, openAIAutoResetCreditCandidate{ID: "c", ExpiresAt: "2026-10-22T20:33:24.500000Z"})
	_, err = kongPickResetCreditByExpiry(dup, second)
	require.Error(t, err)
	got, err = kongPickResetCreditByExpiry(dup, exact)
	require.NoError(t, err)
	require.Equal(t, "a", got.ID)

	// 带小数秒的请求找不到精确那张（已被别的操作用掉）：不落到同一秒的另一张
	rest := []openAIAutoResetCreditCandidate{{ID: "c", ExpiresAt: "2026-10-22T20:33:24.500000Z"}}
	_, err = kongPickResetCreditByExpiry(rest, exact)
	require.Error(t, err)

	// 整秒请求：恰好整秒的那张之外同一秒还有一张，也按歧义拒绝
	whole := []openAIAutoResetCreditCandidate{
		{ID: "a", ExpiresAt: "2026-10-22T20:33:24Z"},
		{ID: "b", ExpiresAt: "2026-10-22T20:33:24.500000Z"},
	}
	_, err = kongPickResetCreditByExpiry(whole, second)
	require.Error(t, err)
	got, err = kongPickResetCreditByExpiry(whole[:1], second)
	require.NoError(t, err)
	require.Equal(t, "a", got.ID)
}

func TestKongScrubResetCreditID(t *testing.T) {
	r := &OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1,
		Credit: &OpenAIQuotaResetCredit{ID: "credit-1", Status: "redeemed", ExpiresAt: "2026-10-22T20:33:24Z", RedeemedAt: "2026-09-30T02:00:00Z"}}
	got := kongScrubResetCreditID(r)
	require.Empty(t, got.Credit.ID)
	require.Equal(t, "redeemed", got.Credit.Status)
	require.Equal(t, "2026-10-22T20:33:24Z", got.Credit.ExpiresAt)
	require.Equal(t, 1, got.WindowsReset)
	require.Equal(t, "credit-1", r.Credit.ID) // 原结果不被改动

	got = kongScrubResetCreditID(&OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1})
	require.Nil(t, got.Credit)
}

func TestKongResetCreditCandidates(t *testing.T) {
	_, err := kongResetCreditCandidates(nil)
	require.Error(t, err)
	_, err = kongResetCreditCandidates(&OpenAIQuotaUsage{})
	require.Error(t, err)

	// 可用 0 张：拒绝
	_, err = kongResetCreditCandidates(&OpenAIQuotaUsage{RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 0}})
	require.Error(t, err)

	// 声明两张可用、只解析出一张：明细不全，拒绝（另一张到期未知，匹配不保证唯一）
	one := []openAIAutoResetCreditCandidate{{ID: "a", ExpiresAt: "2026-10-22T20:33:24Z"}}
	_, err = kongResetCreditCandidates(&OpenAIQuotaUsage{RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 2}, autoResetCandidates: one})
	require.Error(t, err)

	got, err := kongResetCreditCandidates(&OpenAIQuotaUsage{RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 1}, autoResetCandidates: one})
	require.NoError(t, err)
	require.Len(t, got, 1)
}
