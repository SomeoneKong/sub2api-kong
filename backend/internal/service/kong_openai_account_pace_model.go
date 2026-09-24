package service

import (
	"math"
	"time"
)

// 账号消耗节奏的模型部分：读数判定与记账、进度差、短时惩罚、并发与会话调整、权重。全部是纯函数，
// 公式与取舍见 DESIGN-openai-account-pace.md 第 2、4.3 节。

const (
	// kongPaceStandardWindow 是 7d 窗口的标准长度。上游回报窗口长度为 0（未激活）时，
	// 空闲公式用它代替。
	kongPaceStandardWindow = 7 * 24 * time.Hour
	// kongPaceWindowTolerance 是判定"同一窗口"的重置时刻容差。上游回报的重置时刻有秒级到分钟级的
	// 抖动；自然重置、重置卡重开都让重置时刻往后移一天以上。
	kongPaceWindowTolerance = time.Hour
	// kongPaceRiseRetention 是消耗记录的保留期，覆盖 24h 窗口的起点。
	kongPaceRiseRetention = 25 * time.Hour
)

// KongPaceAccountRow 是每拍从数据库读到的一个 OpenAI OAuth 账号。额度字段为 nil 表示上游没有回报。
type KongPaceAccountRow struct {
	ID                    int64
	Plan                  string
	Concurrency           int
	SubscriptionExpiresAt *time.Time
	UsedPercent           *float64
	ResetAt               *time.Time
	WindowMinutes         *int
	UsageUpdatedAt        *time.Time
	LastUsedAt            *time.Time
}

// hasReading 表示这一行带着一条完整的 7d 读数。
func (r *KongPaceAccountRow) hasReading() bool {
	return r.UsedPercent != nil && r.ResetAt != nil && r.WindowMinutes != nil && r.UsageUpdatedAt != nil
}

// inactiveReading 表示这一行的读数是"7d 窗口未激活"（长度为 0）：此时用量与重置时刻都不看。
func (r *KongPaceAccountRow) inactiveReading() bool {
	return r.WindowMinutes != nil && *r.WindowMinutes <= 0 && r.UsageUpdatedAt != nil
}

// kongPaceRise 是一次已接受的上涨。
type kongPaceRise struct {
	At        time.Time `json:"at"`         // 记账时刻
	Points    float64   `json:"points"`     // 上涨的百分点
	ReadingAt time.Time `json:"reading_at"` // 带来这次上涨的读数时刻
}

// kongPaceAccountState 是一个账号的当前窗口与消耗记录。
//
// 整份存在 Redis 的一个键里、整体改写（设计文档 4.2）：Redis 按 LRU 淘汰时两者同进同出，
// 不会出现"窗口还在、记录没了"的半套状态。
type kongPaceAccountState struct {
	Plan string `json:"plan"`
	// HasWindow 为 false：还没有可用的起点（首次见到时没有读数，或换套餐后等待新读数）。
	HasWindow bool `json:"has_window"`
	// Inactive：最新读数说 7d 窗口未激活，按空闲处理。当前窗口与消耗记录照旧保留，重新激活时
	// 照常判定同一窗口或重置。
	Inactive      bool      `json:"inactive,omitempty"`
	WindowMinutes int       `json:"window_minutes"`
	ResetAt       time.Time `json:"reset_at"`
	MaxUsed       float64   `json:"max_used"`
	// ReadingAt 是已接受读数的更新时刻；SeenReadingAt 是见过的最新读数（含被判为回滚的）。
	ReadingAt     time.Time `json:"reading_at"`
	SeenReadingAt time.Time `json:"seen_reading_at"`
	// AwaitAfter 非零：换套餐后，只接受更新时刻晚于它的读数作为新起点。
	AwaitAfter   time.Time      `json:"await_after,omitempty"`
	ProcessedAt  time.Time      `json:"processed_at"`
	TrackedSince time.Time      `json:"tracked_since"`
	Rises        []kongPaceRise `json:"rises"`
}

// kongPaceObservation 是一次读数判定的结果，供计数与日志。
type kongPaceObservation int

const (
	kongPaceObsNone kongPaceObservation = iota // 没有新读数
	kongPaceObsBaseline
	kongPaceObsSameWindow
	kongPaceObsReset
	kongPaceObsRollback
	kongPaceObsPlanChange
	kongPaceObsInactive // 窗口长度为 0：上游侧未激活
)

// kongPaceObserve 用一行读数推进账号状态（设计文档 4.3）。st 为 nil 表示没有当前窗口（首次见到、
// 或 Redis 数据丢失），返回新建的状态。
func kongPaceObserve(st *kongPaceAccountState, row KongPaceAccountRow, now time.Time) (*kongPaceAccountState, kongPaceObservation) {
	if st == nil {
		st = &kongPaceAccountState{Plan: row.Plan, TrackedSince: now}
		obs := kongPaceObsNone
		switch {
		case row.inactiveReading():
			st.Inactive = true
			obs = kongPaceObsInactive
		case row.hasReading():
			kongPaceSetBaseline(st, row)
			obs = kongPaceObsBaseline
		}
		if row.UsageUpdatedAt != nil {
			st.SeenReadingAt = *row.UsageUpdatedAt
		}
		st.ProcessedAt = now
		return st, obs
	}

	// 记账时刻取"上次处理该账号的时刻"与"读数时刻"中较早的一个：本组件中断过时，缺口期间的
	// 上涨记在缺口开始处，只会少计。
	riseAt := st.ProcessedAt
	defer func() {
		st.ProcessedAt = now
		kongPaceTrimRises(st, now)
	}()

	// 换套餐与读数去重分开判断：套餐资料与额度读数各自更新，读数没变也要识别套餐变化。
	planChanged := row.Plan != st.Plan
	lengthChanged := row.WindowMinutes != nil && *row.WindowMinutes > 0 && st.HasWindow && *row.WindowMinutes != st.WindowMinutes
	if planChanged || lengthChanged {
		st.Plan = row.Plan
		st.HasWindow, st.Inactive = false, false
		st.Rises = nil
		st.TrackedSince = now
		// 只认读数时刻晚于发现变化这一刻的读数：此刻及以前的读数（包括稍后才落库的）可能还是旧套餐的。
		st.AwaitAfter = now
		if row.UsageUpdatedAt != nil && row.UsageUpdatedAt.After(st.SeenReadingAt) {
			st.SeenReadingAt = *row.UsageUpdatedAt
		}
		return st, kongPaceObsPlanChange
	}

	if row.UsageUpdatedAt == nil || !row.UsageUpdatedAt.After(st.SeenReadingAt) || (!row.hasReading() && !row.inactiveReading()) {
		return st, kongPaceObsNone
	}
	st.SeenReadingAt = *row.UsageUpdatedAt
	if !st.AwaitAfter.IsZero() {
		if !row.UsageUpdatedAt.After(st.AwaitAfter) {
			return st, kongPaceObsNone
		}
		st.AwaitAfter = time.Time{}
	}
	if row.inactiveReading() {
		st.Inactive = true
		return st, kongPaceObsInactive
	}

	if riseAt.IsZero() || row.UsageUpdatedAt.Before(riseAt) {
		riseAt = *row.UsageUpdatedAt
	}
	used := *row.UsedPercent
	if !st.HasWindow {
		// 此前已知未激活（用量为 0）：激活后的读数就是这段时间的上涨。
		if st.Inactive && used > 0 {
			st.Rises = append(st.Rises, kongPaceRise{At: riseAt, Points: used, ReadingAt: *row.UsageUpdatedAt})
		}
		kongPaceSetBaseline(st, row)
		return st, kongPaceObsBaseline
	}

	diff := row.ResetAt.Sub(st.ResetAt)
	switch {
	case diff > kongPaceWindowTolerance:
		// 重置：新窗口从 0 起，读数就是新窗口里的上涨；消耗统计跨过重置连续计算。
		if used > 0 {
			st.Rises = append(st.Rises, kongPaceRise{At: riseAt, Points: used, ReadingAt: *row.UsageUpdatedAt})
		}
		st.ResetAt, st.MaxUsed, st.WindowMinutes = *row.ResetAt, used, *row.WindowMinutes
		st.ReadingAt = *row.UsageUpdatedAt
		st.Inactive = false
		return st, kongPaceObsReset
	case diff < -kongPaceWindowTolerance:
		// 回滚：上游短暂回报被取代的旧窗口，整条忽略。
		return st, kongPaceObsRollback
	default:
		if used > st.MaxUsed {
			st.Rises = append(st.Rises, kongPaceRise{At: riseAt, Points: used - st.MaxUsed, ReadingAt: *row.UsageUpdatedAt})
			st.MaxUsed = used
		}
		st.ResetAt = *row.ResetAt
		st.ReadingAt = *row.UsageUpdatedAt
		st.Inactive = false
		return st, kongPaceObsSameWindow
	}
}

func kongPaceSetBaseline(st *kongPaceAccountState, row KongPaceAccountRow) {
	st.HasWindow, st.Inactive = true, false
	st.WindowMinutes = *row.WindowMinutes
	st.ResetAt = *row.ResetAt
	st.MaxUsed = *row.UsedPercent
	st.ReadingAt = *row.UsageUpdatedAt
}

func kongPaceTrimRises(st *kongPaceAccountState, now time.Time) {
	cutoff := now.Add(-kongPaceRiseRetention)
	i := 0
	for i < len(st.Rises) && st.Rises[i].At.Before(cutoff) {
		i++
	}
	if i > 0 {
		st.Rises = append([]kongPaceRise(nil), st.Rises[i:]...)
	}
}

// kongPaceIncrease 返回记账时刻落在 (now − d, now] 内的上涨之和。
func kongPaceIncrease(st *kongPaceAccountState, now time.Time, d time.Duration) float64 {
	if st == nil {
		return 0
	}
	cutoff := now.Add(-d)
	var sum float64
	for _, r := range st.Rises {
		if r.At.After(cutoff) && !r.At.After(now) {
			sum += r.Points
		}
	}
	return sum
}

// kongPaceProgress 是一个账号在某一时刻的额度进度。
type kongPaceProgress struct {
	Running        bool      // 窗口在跑
	Used           float64   // U
	WindowStart    time.Time // S
	Deadline       time.Time // D
	DeadlineSource string    // reset / subscription / ""
	TimeProgress   float64   // τ
	Gap            float64   // Δ
}

// kongPaceComputeProgress 计算进度差（设计文档 2.1–2.2）。既没有可用窗口、也不知道是否未激活的账号
// 按进度正常处理（Δ = 0）。
func kongPaceComputeProgress(st *kongPaceAccountState, subscriptionExpiresAt *time.Time, now time.Time) kongPaceProgress {
	if st == nil || (!st.HasWindow && !st.Inactive) {
		return kongPaceProgress{}
	}
	window := time.Duration(st.WindowMinutes) * time.Minute
	if !st.Inactive && st.WindowMinutes > 0 && st.ResetAt.After(now) {
		start := st.ResetAt.Add(-window)
		p := kongPaceProgress{Running: true, Used: st.MaxUsed, WindowStart: start, Deadline: st.ResetAt, DeadlineSource: "reset"}
		if subscriptionExpiresAt != nil && subscriptionExpiresAt.After(start) && subscriptionExpiresAt.After(now) && subscriptionExpiresAt.Before(st.ResetAt) {
			p.Deadline, p.DeadlineSource = *subscriptionExpiresAt, "subscription"
		}
		span := p.Deadline.Sub(start)
		if span > 0 {
			p.TimeProgress = kongPaceClamp(float64(now.Sub(start))/float64(span), 0, 1)
		}
		p.Gap = 100*p.TimeProgress - p.Used
		return p
	}

	// 空闲：额度不随时间流失，但订阅到期会截掉下一个窗口的一部分。
	if st.Inactive || window <= 0 {
		window = kongPaceStandardWindow
	}
	p := kongPaceProgress{}
	if subscriptionExpiresAt != nil && subscriptionExpiresAt.After(now) {
		remaining := subscriptionExpiresAt.Sub(now)
		if remaining < window {
			p.Gap = 100 * (1 - float64(remaining)/float64(window))
			p.Deadline, p.DeadlineSource = *subscriptionExpiresAt, "subscription"
		}
	}
	return p
}

// kongPaceLinePenalty 是一个窗口的短时惩罚：start 以下不罚，start 到 full 线性增加到 qmax，
// 到 full 罚满并到线。
func kongPaceLinePenalty(x float64, line KongPaceSoftLine, qmax float64) (float64, bool) {
	switch {
	case x < line.Start:
		return 0, false
	case x >= line.Full:
		return qmax, true
	default:
		return qmax * ((x - line.Start) / (line.Full - line.Start)), false
	}
}

// kongPaceShortPenalty 返回 q₆、q₂₄、Q = max(q₆, q₂₄) 与是否到线。取较大者而不相加：24h 的消耗
// 已经包含这 6h。
func kongPaceShortPenalty(inc6, inc24 float64, plan KongPacePlan, qmax float64) (q6, q24, q float64, atLine bool) {
	var l6, l24 bool
	q6, l6 = kongPaceLinePenalty(inc6, plan.SoftLine.H6, qmax)
	q24, l24 = kongPaceLinePenalty(inc24, plan.SoftLine.H24, qmax)
	return q6, q24, math.Max(q6, q24), l6 || l24
}

// kongPaceConcurrencyAdjust 返回并发占用调整 C（设计文档 2.5）。peak 是近 N 分钟"在途 + 排队"的
// 峰值（已含本次读到的值）；recentlyUsed 表示最近使用时刻在 N 分钟以内。关闭时恒为 0，空闲加分也不给。
func kongPaceConcurrencyAdjust(peak, limit int, recentlyUsed bool, c KongPaceConcurrency) float64 {
	if !c.Enabled {
		return 0
	}
	if peak <= 0 {
		if recentlyUsed {
			return 0
		}
		return -c.IdleBonusPP
	}
	if limit <= 0 {
		return c.PenaltyPP.Full
	}
	if float64(peak) <= c.BusyRatio*float64(limit) {
		return 0
	}
	switch free := limit - peak; {
	case free >= 2:
		return c.PenaltyPP.Free2Plus
	case free == 1:
		return c.PenaltyPP.Free1
	default:
		return c.PenaltyPP.Full
	}
}

// kongPaceSessionUse 是一个候选的会话占用输入：会话上限在第 2 层入口查得的活跃会话数与账号的会话上限。
// Full 表示会话上限已把它记为满额（分段时已满，或之后确认登记没通过），占用按 1 计。Active 为 nil 表示入口
// 没有它的计数（入口时未设上限，确认登记时才按新设的上限被记为满额）。
type kongPaceSessionUse struct {
	Active *int
	Limit  int
	Full   bool
}

// kongPaceSessionAdjust 返回会话占用调整 M = Mmax × min(1, 活跃会话数 ÷ 上限)（设计文档 2.6）。
// 关闭时为 0；记为满额时为 Mmax；没有计数或上限无效时为 0。
func kongPaceSessionAdjust(u kongPaceSessionUse, s KongPaceSessions) float64 {
	switch {
	case !s.Enabled:
		return 0
	case u.Full:
		return s.MaxPenaltyPP
	case u.Active == nil || u.Limit <= 0:
		return 0
	}
	return s.MaxPenaltyPP * math.Min(1, float64(*u.Active)/float64(u.Limit))
}

// kongPaceWeight 是 w = K × 2^((min(Δ, G) − O − Q − C − M) ÷ H)。上限只截进度差，让位量、短时惩罚与
// 两项占用调整都在截顶之后扣。
func kongPaceWeight(gap float64, plan KongPacePlan, q, c, m float64, cfg *KongPaceConfig) float64 {
	score := math.Min(gap, cfg.GapCapPP) - plan.YieldPP - q - c - m
	return plan.Capacity * math.Exp2(score/cfg.GapDoublingPP)
}

func kongPaceClamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
