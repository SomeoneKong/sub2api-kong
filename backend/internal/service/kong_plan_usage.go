package service

import (
	"context"
	"time"
)

// 额度接口读数的窗口转换（fork 专有，设计见 DESIGN-account-selection.md 第 1 节"用量读数"）。
//
// 额度接口（/wham/usage）只回报一个主窗口、且长度为 0 时，现有归一化（Normalize）把它当作 5h：平台全体
// reset 后 Pro 账号的读数正是这样，照搬会写成 codex_5h_* = 0，而旧的 codex_7d_used_percent 借新的读数
// 时刻留下（UpdateExtra 是 JSONB 合并）。所以从额度接口写用量的各处都经这里：单主窗口按账号已记的窗口
// 结构判定身份，判不出时不写窗口字段；其余情形仍按现有归一化（buildOpenAIAutoResetUsageUpdates），读数时刻取
// 查询的源时刻。真实请求的响应头里窗口总是已激活的，不经这里。

const (
	// kongCodexWindowShapeKey 记下账号在额度接口里的窗口结构：7d（只有 7d 主窗口）、5h（只有 5h 主窗口）
	// 或 two（两个窗口）。单主窗口长度为 0 时靠它判定身份。
	kongCodexWindowShapeKey = "kong_codex_window_shape"
	kongCodexShape7d        = "7d"
	kongCodexShape5h        = "5h"
	kongCodexShapeTwo       = "two"

	kongCodexShortWindowMax = 360 // 分钟；与 Normalize 的分界一致
)

// kongCodexRecordedShape 返回账号已记的窗口结构。记过 5h 窗口长度为正（真实请求的响应头会更新它，而不更新结构标记，
// 换套餐后两者可能矛盾）就不是只有 7d。没有标记时按旧字段推断：记过的 7d 窗口超过 6 小时、又没有长度为正的 5h 窗口，
// 就是只有 7d（旧的错误转换留下的长度为 0 的 5h 不算）。推断不出返回空串。
func kongCodexRecordedShape(recorded map[string]any) string {
	has5h := parseExtraInt(recorded["codex_5h_window_minutes"]) > 0
	if s, ok := recorded[kongCodexWindowShapeKey].(string); ok && s != "" {
		if s == kongCodexShape7d && has5h {
			return ""
		}
		return s
	}
	if parseExtraInt(recorded["codex_7d_window_minutes"]) > kongCodexShortWindowMax && !has5h {
		return kongCodexShape7d
	}
	return ""
}

// kongQuotaUsageUpdates 把一次额度接口读数换成账号 extra 的 codex_* 写入项。recorded 是账号此刻的 extra。
// 返回 nil 表示这次不写用量：没有窗口，或单主窗口长度为 0 而判不出它属于哪个窗口。
func kongQuotaUsageUpdates(recorded map[string]any, usage *OpenAIQuotaUsage) map[string]any {
	if usage == nil || usage.RateLimit == nil {
		return nil
	}
	primary, secondary := usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow
	if primary == nil && secondary == nil {
		return nil
	}
	fetchedAt := kongQuotaFetchedAt(usage)

	if primary != nil && secondary == nil {
		minutes := int(primary.LimitWindowSeconds / 60)
		switch {
		case minutes > kongCodexShortWindowMax:
			// 只有 7d：换套餐前留下的 5h 字段一并清掉（JSONB 合并不删旧键），否则之后长度为 0 的读数一直判为结构冲突
			updates := buildOpenAIAutoResetUsageUpdates(usage, fetchedAt)
			updates[kongCodexWindowShapeKey] = kongCodexShape7d
			for _, k := range []string{"codex_5h_used_percent", "codex_5h_window_minutes", "codex_5h_reset_at", "codex_5h_reset_after_seconds"} {
				updates[k] = nil
			}
			return updates
		case minutes > 0:
			updates := buildOpenAIAutoResetUsageUpdates(usage, fetchedAt)
			updates[kongCodexWindowShapeKey] = kongCodexShape5h
			return updates
		}
		// 长度为 0：窗口未激活（额度已恢复、等第一个请求起算）。
		if kongCodexRecordedShape(recorded) != kongCodexShape7d {
			return nil
		}
		return map[string]any{
			"codex_7d_used_percent":        primary.UsedPercent,
			"codex_7d_window_minutes":      0,
			"codex_7d_reset_at":            nil,
			"codex_7d_reset_after_seconds": nil,
			"codex_usage_updated_at":       fetchedAt.Format(time.RFC3339),
			kongCodexWindowShapeKey:        kongCodexShape7d,
		}
	}

	updates := buildOpenAIAutoResetUsageUpdates(usage, fetchedAt)
	if primary != nil && secondary != nil {
		updates[kongCodexWindowShapeKey] = kongCodexShapeTwo
	}
	return updates
}

func kongQuotaFetchedAt(usage *OpenAIQuotaUsage) time.Time {
	if usage != nil && usage.FetchedAt > 0 {
		return time.Unix(usage.FetchedAt, 0).UTC()
	}
	return time.Now().UTC()
}

// kongQuotaUsageWrites 读账号此刻的 extra，返回这次额度读数要写入的 codex_* 项。读账号失败时返回错误，不退回
// 按"没有记过"处理：那样会把读数写错身份，或让调用方以为已经写入。
//   - 影子账号（Spark）：QueryUsage 回报的普通窗口是父账号的，影子只认 Spark 窗口，与管理端用量探测同一口径；
//   - 账号已有源时刻更新的读数（这次查询等卡明细期间，真实请求或另一次刷新写过）：不写用量，免得读数倒退。
//     预读与写入之间仍有很窄的交错；被暂停的账号没有请求，在服务的账号由下一次响应纠正。
func kongQuotaUsageWrites(ctx context.Context, repo AccountRepository, accountID int64, usage *OpenAIQuotaUsage) (map[string]any, error) {
	if usage == nil || (usage.RateLimit == nil && len(usage.AdditionalRateLimits) == 0) {
		return nil, nil
	}
	account, err := repo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	fetchedAt := kongQuotaFetchedAt(usage)
	if recorded := parseExtraTime(account.Extra["codex_usage_updated_at"]); recorded.After(fetchedAt) {
		return nil, nil
	}
	if account.IsShadow() {
		return buildCodexSparkWindowExtraUpdates(usage, fetchedAt), nil
	}
	return kongQuotaUsageUpdates(account.Extra, usage), nil
}
