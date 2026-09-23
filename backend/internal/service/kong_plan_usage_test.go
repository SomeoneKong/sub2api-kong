//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func kongUsage(fetched int64, windows ...*OpenAIRateLimitWindow) *OpenAIQuotaUsage {
	rl := &OpenAIRateLimit{}
	if len(windows) > 0 {
		rl.PrimaryWindow = windows[0]
	}
	if len(windows) > 1 {
		rl.SecondaryWindow = windows[1]
	}
	return &OpenAIQuotaUsage{FetchedAt: fetched, RateLimit: rl}
}

func kongWindow(used float64, minutes, resetAfter int64) *OpenAIRateLimitWindow {
	return &OpenAIRateLimitWindow{UsedPercent: used, LimitWindowSeconds: minutes * 60, ResetAfterSeconds: resetAfter}
}

func TestKongQuotaUsageUpdates_WindowShapes(t *testing.T) {
	fetched := time.Date(2026, 10, 3, 1, 30, 0, 0, time.UTC)
	at := fetched.Unix()

	// Pro 的正常读数：只有 7d 主窗口，记下结构，读数时刻取查询的源时刻。
	u := kongQuotaUsageUpdates(nil, kongUsage(at, kongWindow(99, 10080, 3600)))
	require.Equal(t, 99.0, u["codex_7d_used_percent"])
	require.Equal(t, 10080, u["codex_7d_window_minutes"])
	require.Equal(t, fetched.Add(time.Hour).Format(time.RFC3339), u["codex_7d_reset_at"])
	require.Equal(t, fetched.Format(time.RFC3339), u["codex_usage_updated_at"])
	require.Equal(t, kongCodexShape7d, u[kongCodexWindowShapeKey])
	require.Contains(t, u, "codex_5h_used_percent", "只有 7d：显式清掉 5h 字段")
	require.Nil(t, u["codex_5h_used_percent"])

	// 平台 reset 后：单主窗口、长度 0。记过只有 7d 的账号记为 7d 未激活，清掉旧的重置时刻与剩余秒数。
	inactive := kongUsage(at, kongWindow(0, 0, 0))
	for name, recorded := range map[string]map[string]any{
		"记过结构":         {kongCodexWindowShapeKey: kongCodexShape7d, "codex_7d_window_minutes": 0},
		"按旧字段推断":       {"codex_7d_window_minutes": 10080, "codex_7d_used_percent": 100},
		"旧的错误转换留下的 5h": {"codex_7d_window_minutes": 10080, "codex_5h_window_minutes": 0, "codex_5h_used_percent": 0},
	} {
		u := kongQuotaUsageUpdates(recorded, inactive)
		require.Equal(t, map[string]any{
			"codex_7d_used_percent":        0.0,
			"codex_7d_window_minutes":      0,
			"codex_7d_reset_at":            nil,
			"codex_7d_reset_after_seconds": nil,
			"codex_usage_updated_at":       fetched.Format(time.RFC3339),
			kongCodexWindowShapeKey:        kongCodexShape7d,
		}, u, name)
	}

	// 判不出窗口身份：不写任何窗口字段，也不推进读数时刻。
	for name, recorded := range map[string]map[string]any{
		"没有记录":   nil,
		"两个窗口":   {kongCodexWindowShapeKey: kongCodexShapeTwo, "codex_7d_window_minutes": 10080},
		"有真实 5h": {"codex_7d_window_minutes": 10080, "codex_5h_window_minutes": 300},
	} {
		require.Nil(t, kongQuotaUsageUpdates(recorded, inactive), name)
	}

	// 两个窗口：按现有归一化，记下结构。
	u = kongQuotaUsageUpdates(nil, kongUsage(at, kongWindow(40, 300, 600), kongWindow(70, 10080, 7200)))
	require.Equal(t, 40.0, u["codex_5h_used_percent"])
	require.Equal(t, 70.0, u["codex_7d_used_percent"])
	require.Equal(t, kongCodexShapeTwo, u[kongCodexWindowShapeKey])

	require.Nil(t, kongQuotaUsageUpdates(nil, &OpenAIQuotaUsage{FetchedAt: at}))
	require.Nil(t, kongQuotaUsageUpdates(nil, kongUsage(at)))
}

// 额度刷新：用量读数与 credits 快照在同一次写入里落下，窗口转换按账号已记的结构。
func TestKongCacheCreditsSnapshotWritesUsage(t *testing.T) {
	ctx := context.Background()
	account := &Account{ID: 200, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{kongCodexWindowShapeKey: kongCodexShape7d, "codex_7d_used_percent": 100.0}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{200: account}}
	svc := &OpenAIQuotaService{accountRepo: repo}
	balance := "800"
	usage := kongUsage(time.Date(2026, 10, 3, 1, 30, 0, 0, time.UTC).Unix(), kongWindow(0, 0, 0))
	usage.Credits = &OpenAICredits{HasCredits: true, Balance: &balance}

	require.NoError(t, svc.CacheCreditsSnapshot(ctx, 200, usage))
	got := repo.extraUpdates[200]
	require.Equal(t, 0, got["codex_7d_window_minutes"])
	require.Contains(t, got, "codex_7d_reset_at")
	require.Nil(t, got["codex_7d_reset_at"])
	require.Contains(t, got, openaiQuotaCreditsKey)
	require.Equal(t, 1, repo.extraUpdateCalls)
}

// 结构标记与真实的 5h 窗口矛盾（换套餐后真实请求更新了 5h，标记没变）：判不出身份，不改写任何窗口字段。
func TestKongQuotaUsageUpdates_ShapeConflictsWith5h(t *testing.T) {
	recorded := map[string]any{kongCodexWindowShapeKey: kongCodexShape7d, "codex_7d_window_minutes": 10080, "codex_5h_window_minutes": 300}
	require.Nil(t, kongQuotaUsageUpdates(recorded, kongUsage(time.Now().Unix(), kongWindow(0, 0, 0))))
}

func TestKongQuotaUsageUpdates_TwoWindowsToSingle7dThenReset(t *testing.T) {
	at := time.Date(2026, 10, 3, 1, 30, 0, 0, time.UTC).Unix()
	merge := func(extra, updates map[string]any) {
		for k, v := range updates { // 与 UpdateExtra 的 JSONB 合并相同：未写的旧键留着，写 nil 的变成 null
			extra[k] = v
		}
	}
	extra := map[string]any{}
	merge(extra, kongQuotaUsageUpdates(extra, kongUsage(at, kongWindow(40, 300, 600), kongWindow(70, 10080, 3600))))
	require.Equal(t, kongCodexShapeTwo, extra[kongCodexWindowShapeKey])
	// 换成只有 7d 的套餐：旧的 5h 字段被清掉
	merge(extra, kongQuotaUsageUpdates(extra, kongUsage(at+60, kongWindow(99, 10080, 3600))))
	require.Equal(t, kongCodexShape7d, kongCodexRecordedShape(extra))
	// 平台 reset：单主窗口、长度 0 → 7d 未激活，清掉旧的重置时刻
	u := kongQuotaUsageUpdates(extra, kongUsage(at+120, kongWindow(0, 0, 0)))
	require.NotNil(t, u)
	require.Equal(t, 0, u["codex_7d_window_minutes"])
	require.Nil(t, u["codex_7d_reset_at"])
	merge(extra, u)
	// 之后真实响应又写出长度为正的 5h：仍判为结构冲突
	extra["codex_5h_window_minutes"] = 300
	require.Nil(t, kongQuotaUsageUpdates(extra, kongUsage(at+180, kongWindow(0, 0, 0))))
}

func TestKongQuotaUsageWrites_ReadFailureAndNewerReading(t *testing.T) {
	ctx := context.Background()
	fetched := time.Date(2026, 10, 3, 1, 30, 0, 0, time.UTC)
	usage := kongUsage(fetched.Unix(), kongWindow(0, 0, 0))

	// 读账号失败：返回错误，不退回按"没有记过"处理。
	_, err := kongQuotaUsageWrites(ctx, &stubQuotaAccountRepo{}, 200, usage)
	require.Error(t, err)
	// 没有窗口的读数不需要读账号。
	updates, err := kongQuotaUsageWrites(ctx, &stubQuotaAccountRepo{}, 200, &OpenAIQuotaUsage{FetchedAt: fetched.Unix()})
	require.NoError(t, err)
	require.Nil(t, updates)

	// 账号已有源时刻更新的读数：不写，免得读数倒退。
	account := &Account{ID: 200, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		kongCodexWindowShapeKey: kongCodexShape7d, "codex_usage_updated_at": fetched.Add(time.Minute).Format(time.RFC3339)}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{200: account}}
	updates, err = kongQuotaUsageWrites(ctx, repo, 200, usage)
	require.NoError(t, err)
	require.Nil(t, updates)
	account.Extra["codex_usage_updated_at"] = fetched.Add(-time.Minute).Format(time.RFC3339)
	updates, err = kongQuotaUsageWrites(ctx, repo, 200, usage)
	require.NoError(t, err)
	require.Equal(t, 0, updates["codex_7d_window_minutes"])
}

// 影子账号（Spark）：QueryUsage 回报的普通窗口是父账号的，影子只写 Spark 窗口；没有 Spark 数据时不写。
func TestKongQuotaUsageWrites_ShadowUsesSparkWindow(t *testing.T) {
	ctx := context.Background()
	parent := int64(100)
	shadow := &Account{ID: 201, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent, Extra: map[string]any{}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{201: shadow}}
	usage := kongUsage(time.Now().Unix(), kongWindow(10, 300, 600), kongWindow(20, 10080, 7200))

	updates, err := kongQuotaUsageWrites(ctx, repo, 201, usage)
	require.NoError(t, err)
	require.Nil(t, updates, "没有 Spark 数据时影子不写父账号的普通窗口")

	usage.AdditionalRateLimits = []OpenAIAdditionalRateLimit{{MeteredFeature: "codex_bengalfox",
		RateLimit: &OpenAIRateLimit{PrimaryWindow: kongWindow(100, 300, 600), SecondaryWindow: kongWindow(95, 10080, 7200)}}}
	updates, err = kongQuotaUsageWrites(ctx, repo, 201, usage)
	require.NoError(t, err)
	require.Equal(t, 100.0, updates["codex_5h_used_percent"])
	require.Equal(t, 95.0, updates["codex_7d_used_percent"])
	require.NotContains(t, updates, kongCodexWindowShapeKey)
}
