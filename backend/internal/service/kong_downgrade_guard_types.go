package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// 降档哨兵的类型、持久化接口与配置。设计见 DESIGN-account-downgrade-guard.md。

// 哨兵模式。
const (
	KongGuardModeOff     = "off"
	KongGuardModeObserve = "observe"
	KongGuardModeOn      = "on"
)

// 持有记录的阶段。
const (
	KongGuardStageSuspect   = "suspect"
	KongGuardStageConfirmed = "confirmed"
)

// 事件种类。hold、false_alarm、fp_start 是恢复控制状态所需的正式记录，impact 由续跑任务保证最终写入，其余是诊断事件。
const (
	KongGuardEventPassiveRefuse = "passive_refuse"
	KongGuardEventProbe         = "probe"
	KongGuardEventFPStart       = "fp_start"
	KongGuardEventFingerprint   = "fingerprint"
	KongGuardEventHold          = "hold"
	KongGuardEventHoldSkipped   = "hold_skipped"
	KongGuardEventFalseAlarm    = "false_alarm"
	KongGuardEventImpact        = "impact"
	KongGuardEventHumanRelease  = "human_release"
)

// 指纹测试的用途：限频按用途分开计。
const (
	kongGuardPurposeConfirm = "confirm"
	kongGuardPurposeDetect  = "detect"
	kongGuardPurposeObserve = "observe"
)

// KongGuardUsageRow 是扫描循环读到的一行用量。
type KongGuardUsageRow struct {
	ID           int64
	CreatedAt    time.Time
	AccountID    int64
	GroupID      int64
	APIKeyID     int64
	Model        string
	DurationMs   int64
	OutputTokens int64
	SessionID    string
	StateFP      string
	ReissuedFP   string
}

// StartedAt 是请求的开始时刻：用量行在请求结束时写入，created_at 是结束时刻。
func (r KongGuardUsageRow) StartedAt() time.Time {
	return r.CreatedAt.Add(-time.Duration(r.DurationMs) * time.Millisecond)
}

// KongGuardHold 是 accounts.extra.kong_downgrade_guard_hold 的内容，只在哨兵持有关闭期间存在。
type KongGuardHold struct {
	Op         string     `json:"op"`
	Stage      string     `json:"stage"`
	Evidence   string     `json:"evidence"`
	HeldAt     time.Time  `json:"held_at"`
	LastGoodAt *time.Time `json:"last_good_at,omitempty"`
	FPAttempts int        `json:"fp_attempts"`
	NextFPAt   *time.Time `json:"next_fp_at,omitempty"`
	FPTestID   string     `json:"fp_test_id,omitempty"`
}

// KongGuardHoldInput 是一次关闭的提交。
type KongGuardHoldInput struct {
	AccountID   int64
	GroupID     int64
	ExpectedRev int64
	Hold        KongGuardHold
	Event       KongGuardEvent
}

// KongGuardEvent 是 kong_downgrade_guard_events 的一行。
type KongGuardEvent struct {
	ID        int64          `json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	AccountID int64          `json:"account_id"`
	Op        string         `json:"op"`
	Kind      string         `json:"kind"`
	Detail    map[string]any `json:"detail"`
}

// KongGuardImpactRow 是影响汇总里按 API key 与会话聚合的一行。
type KongGuardImpactRow struct {
	APIKeyID   int64     `json:"api_key_id"`
	APIKeyName string    `json:"api_key_name"`
	SessionID  string    `json:"session_id"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	Count      int64     `json:"count"`
}

// KongDowngradeGuardStore 是哨兵的持久化面（repository/kong_downgrade_guard_repo.go）。
type KongDowngradeGuardStore interface {
	InsertEvent(ctx context.Context, ev KongGuardEvent) error
	ScanUsage(ctx context.Context, since time.Time, accountIDs []int64, limit int) ([]KongGuardUsageRow, error)
	Hold(ctx context.Context, in KongGuardHoldInput) (bool, error)
	UpdateHold(ctx context.Context, accountID int64, op string, expectedRev int64, patch map[string]any) (bool, error)
	ReleaseFalseAlarm(ctx context.Context, accountID int64, op string, expectedRev int64, ev KongGuardEvent) (bool, error)
	TakeOverExpired(ctx context.Context) ([]int64, error)
	RecentEvents(ctx context.Context, kinds []string, since time.Time) ([]KongGuardEvent, error)
	ListEvents(ctx context.Context, accountID int64, limit int) ([]KongGuardEvent, error)
	PendingImpacts(ctx context.Context, minAge time.Duration) ([]KongGuardEvent, error)
	ImpactRows(ctx context.Context, accountID int64, from, holdAt, cutoff time.Time) ([]KongGuardImpactRow, error)
	UpsertImpact(ctx context.Context, accountID int64, op string, detail map[string]any) error
	DeleteEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// KongGuardAccountSource 是哨兵读账号的面（AccountRepository 的子集）。
type KongGuardAccountSource interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
	ListByGroup(ctx context.Context, groupID int64) ([]Account, error)
}

// KongDowngradeGuardConfig 是哨兵的配置，来自环境变量。
type KongDowngradeGuardConfig struct {
	Mode          string
	GroupID       int64 // 0 表示没配置服务分组：只判定、记录，不关调度也不巡检
	FreshAge      time.Duration
	ProbeBusy     time.Duration
	ProbeIdle     time.Duration
	ProbeModel    string
	TicketHoldMax int
}

// kongGuardProbeModels 是探测与指纹确认可用的模型：判据只在这两个模型上验证过，且它们不属于 luna 档。
var kongGuardProbeModels = map[string]bool{"gpt-6.1-sol": true, "gpt-6-astra": true}

// kongGuardLunaModels 是指纹结论里算作 luna 档的模型。
var kongGuardLunaModels = map[string]bool{"gpt-5.6-luna": true, "gpt-6-luna": true}

// KongDowngradeGuardConfigFromEnv 读配置。取值不合法的数值项记警告并用缺省值；探测模型不合法返回错误，
// 调用方据此不启动哨兵。
func KongDowngradeGuardConfigFromEnv() (KongDowngradeGuardConfig, error) {
	cfg := KongDowngradeGuardConfig{
		Mode:          KongGuardModeObserve,
		FreshAge:      3000 * time.Second,
		ProbeBusy:     10 * time.Minute,
		ProbeIdle:     60 * time.Minute,
		ProbeModel:    "gpt-6.1-sol",
		TicketHoldMax: 2,
	}
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("KONG_DOWNGRADE_GUARD"))); v {
	case "":
	case KongGuardModeOff, KongGuardModeObserve, KongGuardModeOn:
		cfg.Mode = v
	default:
		return cfg, fmt.Errorf("KONG_DOWNGRADE_GUARD=%q 不是 off / observe / on", v)
	}
	if v := strings.TrimSpace(os.Getenv("KONG_DOWNGRADE_GUARD_GROUP_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return cfg, fmt.Errorf("KONG_DOWNGRADE_GUARD_GROUP_ID=%q 不是正整数", v)
		}
		cfg.GroupID = id
	}
	kongGuardEnvInt("KONG_DOWNGRADE_GUARD_FRESH_SECONDS", 60, 3500, func(n int) { cfg.FreshAge = time.Duration(n) * time.Second })
	kongGuardEnvInt("KONG_DOWNGRADE_GUARD_PROBE_BUSY_MINUTES", 1, 1440, func(n int) { cfg.ProbeBusy = time.Duration(n) * time.Minute })
	kongGuardEnvInt("KONG_DOWNGRADE_GUARD_PROBE_IDLE_MINUTES", 1, 1440, func(n int) { cfg.ProbeIdle = time.Duration(n) * time.Minute })
	kongGuardEnvInt("KONG_DOWNGRADE_GUARD_TICKET_HOLD_MAX", 1, 100, func(n int) { cfg.TicketHoldMax = n })
	if v := strings.TrimSpace(os.Getenv("KONG_DOWNGRADE_GUARD_PROBE_MODEL")); v != "" {
		cfg.ProbeModel = v
	}
	if !kongGuardProbeModels[cfg.ProbeModel] {
		return cfg, fmt.Errorf("KONG_DOWNGRADE_GUARD_PROBE_MODEL=%q 只能是 gpt-6.1-sol 或 gpt-6-astra", cfg.ProbeModel)
	}
	return cfg, nil
}

func kongGuardEnvInt(name string, lo, hi int, set func(int)) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		slog.Warn("kong downgrade guard: 配置取值不合法，用缺省值", "name", name, "value", v, "min", lo, "max", hi)
		return
	}
	set(n)
}

// kongGuardHoldFromExtra 读账号 extra 里的持有记录；没有时返回 nil。
func kongGuardHoldFromExtra(extra map[string]any) *KongGuardHold {
	raw, ok := extra["kong_downgrade_guard_hold"].(map[string]any)
	if !ok {
		return nil
	}
	h := &KongGuardHold{}
	h.Op, _ = raw["op"].(string)
	h.Stage, _ = raw["stage"].(string)
	h.Evidence, _ = raw["evidence"].(string)
	h.FPTestID, _ = raw["fp_test_id"].(string)
	if n, ok := raw["fp_attempts"].(float64); ok {
		h.FPAttempts = int(n)
	}
	h.HeldAt = kongGuardParseTime(raw["held_at"])
	if t := kongGuardParseTime(raw["last_good_at"]); !t.IsZero() {
		h.LastGoodAt = &t
	}
	if t := kongGuardParseTime(raw["next_fp_at"]); !t.IsZero() {
		h.NextFPAt = &t
	}
	if h.Op == "" {
		return nil
	}
	return h
}

// kongGuardRevOf 读账号 extra 里的调度开关版本；缺省或不是数字时为 0。
func kongGuardRevOf(extra map[string]any) int64 {
	switch v := extra["kong_schedulable_rev"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func kongGuardParseTime(v any) time.Time {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
