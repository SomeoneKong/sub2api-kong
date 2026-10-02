package service

// 生效上限（fork 专有，设计见 DESIGN-account-selection.md 第 4.2 节第 6 步）：计划输入（快照或兜底）生效、账号的条目
// 带覆盖值时用覆盖值，否则用账号自身的设置。作用于旧版选号路径上全部的抢槽、排队、续接、会话登记与 WS 续期；
// 不迁移已有会话，降到在途数以下时只阻止后续抢槽与登记。

// kongPlanEntryNow 返回账号在此刻所用输入里的条目；计划输入未生效或输入里没有这个账号时为 nil。
func kongPlanEntryNow(id int64) *KongPlanEntry {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return nil
	}
	st := rt.store.current()
	return st.entryFor(id, st.inUse(rt.now()))
}

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

func kongEffectiveConcurrencyByID(id int64, own int) int {
	if e := kongPlanEntryNow(id); e != nil && e.MaxConcurrency != nil {
		return *e.MaxConcurrency
	}
	return own
}

// kongEffectiveMaxSessions 返回账号此刻生效的会话上限，0 表示不限。
func kongEffectiveMaxSessions(account *Account) int {
	if e := kongPlanEntryNow(account.ID); e != nil && e.MaxSessions != nil {
		return *e.MaxSessions
	}
	return account.GetMaxSessions()
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
