package service

import "time"

// 生效上限（fork 专有，设计见 DESIGN-account-selection.md 第 4.2 节第 6 步）：计划输入（快照或兜底）生效、账号的条目
// 带覆盖值时用覆盖值，否则用账号自身的设置；热度限速生效时再在这个值上取小（kong_plan_hot.go）。作用于旧版选号路径上
// 全部的抢槽、排队、续接、会话登记与 WS 续期；不迁移已有会话，降到在途数以下时只阻止后续抢槽与登记。

// KongEffectiveConcurrency 返回账号此刻生效的并发上限。
func KongEffectiveConcurrency(account *Account) int {
	if account == nil {
		return 0
	}
	return kongEffectiveConcurrencyByID(account.ID, account.Concurrency)
}

// KongWSTurnConcurrency 是 WS 后续轮次与同账号重试抢槽用的并发上限。计划输入生效时按此刻的生效值：建连时排队计划给的
// 值可能已含覆盖，撤销覆盖之后不能再沿用。未生效时用建连时定下的值，与上游相同。
func KongWSTurnConcurrency(account *Account, connected int) int {
	if account == nil || !kongPlanInEffect() {
		return connected
	}
	return KongEffectiveConcurrency(account)
}

// kongEffectiveLimits 返回账号此刻生效的并发与会话上限：条目与热度取自同一份计划状态，交接（换发布）发生在两次读取之间
// 也不会拼出两份发布各一半的结果。
func kongEffectiveLimits(id int64, ownConcurrency, ownSessions int) (concurrency, sessions int) {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return ownConcurrency, ownSessions
	}
	now := rt.now()
	st := rt.store.current()
	return kongEffectiveLimitsOf(st, rt.hotEffect(st, id, now), id, ownConcurrency, ownSessions, now)
}

// kongEffectiveLimitsOf 按给定的计划状态与由它求出的热度 h 算两个生效上限。
func kongEffectiveLimitsOf(st *kongPlanState, h kongHotEffect, id int64, ownConcurrency, ownSessions int, now time.Time) (concurrency, sessions int) {
	concurrency, sessions = ownConcurrency, ownSessions
	if e := st.entryFor(id, st.inUse(now)); e != nil {
		if e.MaxConcurrency != nil {
			concurrency = *e.MaxConcurrency
		}
		if e.MaxSessions != nil {
			sessions = *e.MaxSessions
		}
	}
	return h.concurrency(concurrency), h.sessions(sessions)
}

func kongEffectiveConcurrencyByID(id int64, own int) int {
	c, _ := kongEffectiveLimits(id, own, 0)
	return c
}

// kongEffectiveMaxSessions 返回账号此刻生效的会话上限，0 表示不限。
func kongEffectiveMaxSessions(account *Account) int {
	_, s := kongEffectiveLimits(account.ID, account.Concurrency, account.GetMaxSessions())
	return s
}

// kongEffectiveLoadFactor 是负载率的分母：账号设了负载因子就用它，否则取生效并发上限（同 Account.EffectiveLoadFactor）。
func kongEffectiveLoadFactor(account *Account) int {
	if account != nil && account.LoadFactor != nil && *account.LoadFactor > 0 {
		return *account.LoadFactor
	}
	if c := KongEffectiveConcurrency(account); c > 0 {
		return c
	}
	return 1
}
