package service

import (
	"testing"
	"time"
)

// 票的 TTL，用于构造时间线。
const kongTestTicketTTL = 3600 * time.Second

func kongTestBase() time.Time {
	return time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
}

func kongAt(seconds int) time.Time {
	return kongTestBase().Add(time.Duration(seconds) * time.Second)
}

func kongAtPtr(seconds int) *time.Time {
	t := kongAt(seconds)
	return &t
}

// 「预取失败之后不能立即重来」——这是设计里最容易被直觉推翻的一条：一次刚刚接触过出口的失败
// 已经把静默重新计时了，此时距「静默重新满 M」比长期闲置后的冷启动更远，而不是更近。
//
// 用默认参数复现设计 §4.3 的反例：请求在刷新窗口末尾才到达，预取失败之后拒服约 30 分钟，
// 不是「那个请求多等几十秒」。
func TestKongScheduleRetryAfterPrefetchFailure(t *testing.T) {
	params := KongDefaultTicketParams()

	// t=3595 出口最后一次活动（取票本身有耗时），t=3610 判定失败并进入冷却。
	lastEgress := kongAtPtr(3595)
	lastCooldown := kongAtPtr(3610)

	allowed := KongNextFetchAllowedAt(lastEgress, lastCooldown, params)
	wantAllowed := kongAt(5410) // max(3595+1800, 3610+1800)
	if !allowed.Equal(wantAllowed) {
		t.Fatalf("下一次可取票时刻 = %v, want %v", allowed, wantAllowed)
	}

	// 票在 t=3600 过期，此后的请求应当被拒，且拒服原因是窗口未开。
	in := KongScheduleInput{
		Now:            kongAt(3601),
		Mode:           KongTicketModeFull,
		Egress:         KongTicketEgressProxy,
		EgressUsable:   true,
		AccountReady:   true,
		LastEgressUsed: lastEgress,
		LastCooldownAt: lastCooldown,
		Params:         params,
	}
	decision := KongDecideTicketAction(in)
	if decision.Action != KongActionDeny {
		t.Fatalf("Action = %q, want deny", decision.Action)
	}
	if decision.DenyReason != KongDenyWindowClosed {
		t.Errorf("DenyReason = %q, want %q", decision.DenyReason, KongDenyWindowClosed)
	}
	if !decision.RetryAfter.Equal(wantAllowed) {
		t.Errorf("RetryAfter = %v, want %v", decision.RetryAfter, wantAllowed)
	}

	// 从票过期到能再次尝试，是 30 分 10 秒的拒服；再加上取票与验证的耗时才是完整的中断。
	outage := wantAllowed.Sub(kongAt(3600))
	if outage < 30*time.Minute {
		t.Errorf("拒服时长 %v 少于 30 分钟，与设计 §4.3 的反例不符", outage)
	}
}

// 「C 取成与 M 相同的值」不等于「失败后不额外加时」：两者起算点不同，C 从失败判定时刻起算、
// M 从最后活动时刻起算，实际期限仍比单纯的静默门槛晚了 F − A。
func TestKongScheduleCooldownEqualsIdleStillAddsTime(t *testing.T) {
	params := KongDefaultTicketParams()
	if params.VerifyFailCooldown != params.TicketFetchMinIdle {
		t.Fatalf("本用例假定默认 C == M，实际 C=%v M=%v", params.VerifyFailCooldown, params.TicketFetchMinIdle)
	}

	lastEgress := kongAtPtr(1000)   // A
	lastCooldown := kongAtPtr(1030) // F：失败判定晚于最后活动 30 秒

	allowed := KongNextFetchAllowedAt(lastEgress, lastCooldown, params)
	idleOnly := lastEgress.Add(params.TicketFetchMinIdle)
	if !allowed.After(idleOnly) {
		t.Fatalf("期限 %v 应当晚于纯静默门槛 %v", allowed, idleOnly)
	}
	if got, want := allowed.Sub(idleOnly), 30*time.Second; got != want {
		t.Errorf("额外等待 %v, want %v（即 F − A）", got, want)
	}
}

// 从未使用过该出口、也从未冷却时，窗口是开着的——冷启动不该被凭空推迟。
func TestKongScheduleFreshEgressAllowsFetch(t *testing.T) {
	params := KongDefaultTicketParams()
	if allowed := KongNextFetchAllowedAt(nil, nil, params); !allowed.IsZero() {
		t.Errorf("从未用过的出口不应有等待期，得到 %v", allowed)
	}
	decision := KongDecideTicketAction(KongScheduleInput{
		Now:          kongAt(0),
		Mode:         KongTicketModeFull,
		Egress:       KongTicketEgressDirect,
		EgressUsable: true,
		AccountReady: true,
		Params:       params,
	})
	if decision.Action != KongActionFetch {
		t.Errorf("Action = %q, want fetch", decision.Action)
	}
}

func TestKongScheduleDecisions(t *testing.T) {
	params := KongDefaultTicketParams()
	base := KongScheduleInput{
		Now:          kongAt(1000),
		Mode:         KongTicketModeFull,
		Egress:       KongTicketEgressProxy,
		EgressUsable: true,
		AccountReady: true,
		Params:       params,
	}

	cases := []struct {
		name       string
		mutate     func(*KongScheduleInput)
		wantAction string
		wantReason string
	}{
		{
			name: "有票且离过期还远 → 直接注入",
			mutate: func(in *KongScheduleInput) {
				in.CurrentExpiresAt = kongAt(1000 + 3000)
				in.CurrentTicketID = 7
			},
			wantAction: KongActionInject,
		},
		{
			name: "进入刷新窗口 → 注入当前票并异步预取",
			mutate: func(in *KongScheduleInput) {
				in.CurrentExpiresAt = kongAt(1000 + 200) // 剩 200s < refresh_before 300s
				in.CurrentTicketID = 7
			},
			wantAction: KongActionInjectAndPrefetch,
		},
		{
			name: "刷新窗口内但出口不可用 → 仍用当前票，不拒服",
			mutate: func(in *KongScheduleInput) {
				in.CurrentExpiresAt = kongAt(1000 + 200)
				in.CurrentTicketID = 7
				in.EgressUsable = false
			},
			wantAction: KongActionInject,
		},
		{
			name: "刷新窗口内且预取已在路上 → 用当前票，不重复发起预取",
			mutate: func(in *KongScheduleInput) {
				in.CurrentExpiresAt = kongAt(1000 + 200)
				in.CurrentTicketID = 7
				in.InflightTask = true
			},
			wantAction: KongActionInject,
		},
		{
			name: "无票但有在途任务 → 等它，不另起一个",
			mutate: func(in *KongScheduleInput) {
				in.InflightTask = true
			},
			wantAction: KongActionWait,
		},
		{
			name: "无票且有候选 → 先验候选（不消耗出口静默）",
			mutate: func(in *KongScheduleInput) {
				in.HasCandidate = true
				in.CandidateID = 42
			},
			wantAction: KongActionVerifyCandidate,
		},
		{
			name: "无票、无候选、配置为 none → 没有票源可用",
			mutate: func(in *KongScheduleInput) {
				in.Egress = KongTicketEgressNone
			},
			wantAction: KongActionDeny,
			wantReason: KongDenyNoTicketSource,
		},
		{
			name: "无票且出口失效 → 拒服而不是退回直连",
			mutate: func(in *KongScheduleInput) {
				in.EgressUsable = false
			},
			wantAction: KongActionDeny,
			wantReason: KongDenyEgressUnusable,
		},
		{
			name: "账号不可调度 → 不发起任何主动请求",
			mutate: func(in *KongScheduleInput) {
				in.AccountReady = false
			},
			wantAction: KongActionDeny,
			wantReason: KongDenyAccountUnready,
		},
		{
			name: "账号不可调度时，连缓存候选也不验（验证同样是主动请求）",
			mutate: func(in *KongScheduleInput) {
				in.AccountReady = false
				in.HasCandidate = true
				in.CandidateID = 42
			},
			wantAction: KongActionDeny,
			wantReason: KongDenyAccountUnready,
		},
		{
			name: "observe 模式不参与注入",
			mutate: func(in *KongScheduleInput) {
				in.Mode = KongTicketModeObserve
			},
			wantAction: KongActionDeny,
			wantReason: KongDenyModeNotFull,
		},
		{
			name: "票已过期等同于无票",
			mutate: func(in *KongScheduleInput) {
				in.CurrentExpiresAt = kongAt(999)
				in.CurrentTicketID = 7
				in.HasCandidate = true
				in.CandidateID = 42
			},
			wantAction: KongActionVerifyCandidate,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			c.mutate(&in)
			got := KongDecideTicketAction(in)
			if got.Action != c.wantAction {
				t.Fatalf("Action = %q, want %q（原因 %q）", got.Action, c.wantAction, got.DenyReason)
			}
			if c.wantReason != "" && got.DenyReason != c.wantReason {
				t.Errorf("DenyReason = %q, want %q", got.DenyReason, c.wantReason)
			}
		})
	}
}

func TestKongTicketParamsValidate(t *testing.T) {
	ok := KongDefaultTicketParams()
	if err := ok.Validate(kongTestTicketTTL); err != nil {
		t.Fatalf("默认参数应当合法: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*KongTicketParams)
	}{
		{"refresh_before 为零", func(p *KongTicketParams) { p.RefreshBefore = 0 }},
		{"refresh_before 不小于 TTL", func(p *KongTicketParams) { p.RefreshBefore = kongTestTicketTTL }},
		{"min_ticket_age 不小于 TTL 会让被动票永远无法验证", func(p *KongTicketParams) { p.MinTicketAge = kongTestTicketTTL }},
		{"observe 间隔为零", func(p *KongTicketParams) { p.ObserveProbeInterval = 0 }},
		{"冷却为负", func(p *KongTicketParams) { p.VerifyFailCooldown = -time.Second }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := KongDefaultTicketParams()
			c.mutate(&p)
			if err := p.Validate(kongTestTicketTTL); err == nil {
				t.Error("应当被拒绝")
			}
		})
	}

	// 「预取被静默门槛推迟」本身不是错误配置：推迟之后仍可能在票过期前完成。
	delayed := KongDefaultTicketParams()
	delayed.TicketFetchMinIdle = kongTestTicketTTL - delayed.RefreshBefore + 100*time.Second
	if err := delayed.Validate(kongTestTicketTTL); err != nil {
		t.Errorf("M 大于可用续期间隔不等于必然空窗，不应拒绝: %v", err)
	}
}

func TestKongObserveShouldProbe(t *testing.T) {
	params := KongDefaultTicketParams()
	now := kongAt(10000)

	if !KongObserveShouldProbe(now, true, nil, params) {
		t.Error("从未探测过时应当探测")
	}
	if KongObserveShouldProbe(now, false, nil, params) {
		t.Error("账号不可调度时不应探测——那时测出的档位不代表正常状态")
	}
	recent := now.Add(-params.ObserveProbeInterval + time.Second)
	if KongObserveShouldProbe(now, true, &recent, params) {
		t.Error("未满最小间隔时不应探测")
	}
	old := now.Add(-params.ObserveProbeInterval)
	if !KongObserveShouldProbe(now, true, &old, params) {
		t.Error("恰好满最小间隔时应当探测")
	}
}
