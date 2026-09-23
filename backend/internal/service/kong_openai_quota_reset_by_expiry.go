package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 按到期时间指定要消耗的重置卡（fork 专有，设计见 DESIGN-openai-plan-reset-by-expiry.md）。
//
// 上游卡 ID 不出服务层（见 openAIAutoResetCreditCandidate 的说明），所以外部调用方只能用它看得到的
// 到期时间指定卡；这里现查一遍额度与卡明细、把到期时间换成卡 ID，再走已有的定向兑换。redeem_request_id 原样
// 交给上游做幂等：调用方超时后用同一个 id 重试，不会消耗第二张——上游还没兑换就照常兑换；已兑换的话那张卡
// 不再可用，重试按 409 返回，拿不回首次的结果，调用方要按卡数与用量对账。

// KongResetCreditByExpiry 消耗到期时间为 expiresAt 的那张重置卡。卡明细走 QueryUsage（含 agent identity 失效的
// 恢复重试与上游超时）；明细不全（可用张数多于解析出的卡）、找不到、多张同秒到期、或那张卡没有上游 ID 都按 409 返回，
// 不会退而消耗别的卡。
func (s *OpenAIQuotaService) KongResetCreditByExpiry(ctx context.Context, accountID int64, expiresAt, redeemRequestID string) (*OpenAIQuotaResetResult, error) {
	expiresAt = strings.TrimSpace(expiresAt)
	redeemRequestID = strings.TrimSpace(redeemRequestID)
	if expiresAt == "" || redeemRequestID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KONG_OPENAI_QUOTA_RESET_BY_EXPIRY_INVALID", "expires_at and redeem_request_id are required")
	}
	want, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "KONG_OPENAI_QUOTA_RESET_BY_EXPIRY_INVALID", "expires_at must be RFC3339")
	}
	usage, err := s.QueryUsage(ctx, accountID)
	if err != nil {
		return nil, err
	}
	candidates, err := kongResetCreditCandidates(usage)
	if err != nil {
		return nil, err
	}
	candidate, err := kongPickResetCreditByExpiry(candidates, want)
	if err != nil {
		return nil, err
	}
	result, err := s.resetCredit(ctx, accountID, candidate.ID, redeemRequestID, true)
	if err != nil || result == nil {
		return result, err
	}
	return kongScrubResetCreditID(result), nil
}

// kongScrubResetCreditID 返回去掉已兑换卡 ID 的副本，其余元数据照旧：卡 ID 不出服务层。
func kongScrubResetCreditID(r *OpenAIQuotaResetResult) *OpenAIQuotaResetResult {
	out := *r
	if r.Credit != nil {
		credit := *r.Credit
		credit.ID = ""
		out.Credit = &credit
	}
	return &out
}

// kongResetCreditCandidates 从额度查询结果里取可选的卡：可用张数为 0 或明细少于可用张数时拒绝——
// 少了的那张到期时间未知，无法保证匹配是唯一的（与自动用卡的 selectOpenAIAutoResetCandidate 同一口径）。
func kongResetCreditCandidates(usage *OpenAIQuotaUsage) ([]openAIAutoResetCreditCandidate, error) {
	if usage == nil || usage.RateLimitResetCredits == nil {
		return nil, infraerrors.New(http.StatusBadGateway, "KONG_OPENAI_QUOTA_RESET_CREDIT_DETAILS_UNAVAILABLE", "reset credit details are unavailable")
	}
	available := usage.RateLimitResetCredits.AvailableCount
	if available <= 0 {
		return nil, infraerrors.Conflict("OPENAI_AUTO_RESET_NO_CREDIT", "no reset credit is available")
	}
	if len(usage.autoResetCandidates) < available {
		return nil, infraerrors.Conflict("OPENAI_AUTO_RESET_CREDIT_DETAILS_INCOMPLETE", "reset credit details are incomplete")
	}
	return usage.autoResetCandidates, nil
}

// kongPickResetCreditByExpiry 在卡明细里找到期时间为 want 的那张。want 带小数秒时只认精确相等；want 是整秒时
// 认到期落在这一秒里的全部卡（含恰好整秒的那张）——调用方可能把到期时间截到了秒，但不能因此落到同一秒里的
// 另一张卡上，所以这一秒里多于一张就按歧义拒绝。
func kongPickResetCreditByExpiry(candidates []openAIAutoResetCreditCandidate, want time.Time) (openAIAutoResetCreditCandidate, error) {
	wholeSecond := want.Nanosecond() == 0
	var matched []openAIAutoResetCreditCandidate
	for _, c := range candidates {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(c.ExpiresAt))
		if err != nil {
			continue
		}
		if (wholeSecond && t.Truncate(time.Second).Equal(want)) || (!wholeSecond && t.Equal(want)) {
			matched = append(matched, c)
		}
	}
	switch {
	case len(matched) == 0:
		return openAIAutoResetCreditCandidate{}, infraerrors.Conflict("KONG_OPENAI_QUOTA_RESET_CREDIT_NOT_FOUND", "no reset credit expires at the given time")
	case len(matched) > 1:
		return openAIAutoResetCreditCandidate{}, infraerrors.Conflict("KONG_OPENAI_QUOTA_RESET_CREDIT_AMBIGUOUS", "more than one reset credit expires at the given time")
	case strings.TrimSpace(matched[0].ID) == "":
		return openAIAutoResetCreditCandidate{}, infraerrors.Conflict("OPENAI_AUTO_RESET_CREDIT_ID_MISSING", "the matched reset credit has no official id")
	}
	return matched[0], nil
}
