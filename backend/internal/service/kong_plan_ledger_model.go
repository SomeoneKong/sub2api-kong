package service

import (
	"math"
	"time"
)

// 计划组件的余额记账（fork 专有，设计见 DESIGN-account-selection.md 第 4.2 节第 5 步）：软线把 credits 消耗也算进去。
// 本文件是纯函数，推进者（节奏组件的刷新协程）按顺序调用；边车的余额账本用同一组规则，两边跑同一份夹具
// testdata/kong_plan/balance_timelines.json。

const (
	// kongPlanUnknownHold 是"未知"标记的有效期：此后 24h 内账号视同到线。
	kongPlanUnknownHold = 24 * time.Hour
	// kongPlanSettleMax 是离层后最多等多久结算：一直没有有数值的观测时，满 24h 也结算。
	kongPlanSettleMax = 24 * time.Hour
)

// kongPlanObservation 是一次余额观测。Balance 为 nil：查询成功但上游没给余额数值。
// ID 只为让待处理列表里的每一条都是唯一的值，推进者按原值精确删除。
type kongPlanObservation struct {
	ID        string    `json:"id"`
	AccountID int64     `json:"account_id"`
	At        time.Time `json:"at"`
	Balance   *float64  `json:"balance"`
}

// KongPlanTierEntry 是账号在一个窗口里的入层：入层时刻、离层时刻（还没离层为 nil）与是否已结算。
type KongPlanTierEntry struct {
	Seq       int64      `json:"window_seq"`
	EnteredAt time.Time  `json:"entered_at"`
	LeftAt    *time.Time `json:"left_at"`
	Settled   bool       `json:"settled,omitempty"`
}

func kongPlanMarkUnknown(st *kongPaceAccountState, at time.Time) {
	if until := at.Add(kongPlanUnknownHold); until.After(st.UnknownUntil) {
		st.UnknownUntil = until
	}
}

// kongPlanApplyObservation 用一次观测推进账号的余额记账。hasOpen：账号此刻有未结算的入层。
//   - 只对源查询时刻比基点新、带余额数值的观测计算差额；同一次下降不会记两次。不看账号当时在哪一层。
//   - 下降量 ÷ perPoint 记为消耗，记在观测的源查询时刻；perPoint 未知时改记一个"未知"。余额上升不记消耗。
//   - 没有数值的观测不推进基点。还没有基点时，首个有数值的观测只建立基点；此前已经入层的，这段消耗无法还原，记"未知"。
func kongPlanApplyObservation(st *kongPaceAccountState, obs kongPlanObservation, perPoint *float64, hasOpen bool) {
	if obs.Balance == nil {
		return
	}
	v := *obs.Balance
	switch {
	case st.Balance == nil:
		if hasOpen {
			kongPlanMarkUnknown(st, obs.At)
		}
	case !obs.At.After(st.Balance.At):
		return
	default:
		if drop := st.Balance.Value - v; drop > 0 {
			points := math.NaN()
			if perPoint != nil && *perPoint > 0 {
				points = drop / *perPoint
			}
			if math.IsNaN(points) || math.IsInf(points, 0) {
				kongPlanMarkUnknown(st, obs.At) // 换算率未知，或小到换算结果溢出
			} else {
				kongPaceAddRise(st, kongPaceRise{At: obs.At, Points: points, ReadingAt: obs.At, Credits: true})
			}
		}
	}
	st.Balance = &kongPlanBalancePoint{At: obs.At, Value: v}
}

// kongPlanSettleDue 判断一次入层能不能结算：已经离层，并且离层之后有数值的观测已经记进基点，或者离层满 24h。
// 没有数值的观测不推进基点，也就不提前结算、不延长这 24h。
func kongPlanSettleDue(e KongPlanTierEntry, st *kongPaceAccountState, now time.Time) bool {
	if e.Settled || e.LeftAt == nil {
		return false
	}
	if st != nil && st.Balance != nil && st.Balance.At.After(*e.LeftAt) {
		return true
	}
	return !now.Before(e.LeftAt.Add(kongPlanSettleMax))
}

// kongPlanAtLine 判断账号是否视同到线：有未结算的入层（不论哪个窗口），或者"未知"标记还在有效期内。
func kongPlanAtLine(entries []KongPlanTierEntry, st *kongPaceAccountState, now time.Time) bool {
	for _, e := range entries {
		if !e.Settled {
			return true
		}
	}
	return st != nil && now.Before(st.UnknownUntil)
}
