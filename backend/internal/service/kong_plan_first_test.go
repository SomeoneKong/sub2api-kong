//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 优先承接新会话（DESIGN-routing-priority.md 第 4.2、10 节）：契约字段、前置位置的确认、确认时改判的去向、选号途中换快照。

func (h *kongPlanTierHarness) firstUntil(d time.Duration) string {
	return h.now.Add(d).UTC().Format(time.RFC3339)
}

// firstEntry 是带优先承接截止的条目；credits 给了级别时带 credits。
func (h *kongPlanTierHarness) firstEntry(id int64, level int) string {
	e := `{"id": ` + strconv.FormatInt(id, 10) + `, "tier": "normal", "first_until": "` + h.firstUntil(time.Hour) + `"`
	if level > 0 {
		e += `, "credits": {"level": ` + strconv.Itoa(level) + `, "expires_at": "` + h.firstUntil(30*24*time.Hour) + `"}`
	}
	return e + "}"
}

func kongPlanTestLoads(accounts ...*Account) []accountWithLoad {
	out := make([]accountWithLoad, len(accounts))
	for i, a := range accounts {
		out[i] = accountWithLoad{account: a, loadInfo: &AccountLoadInfo{AccountID: a.ID}}
	}
	return out
}

// kongPlanNextActive 推进游标到 id 下一个要尝试的位置；没有了返回 nil。
func kongPlanNextActive(p *kongPlanSelection, id int64) *kongPlanSlot {
	for p.cursor < len(p.slots) {
		sid := p.slots[p.cursor].ID
		if p.next(sid) && p.cur != nil && sid == id {
			return p.cur
		}
	}
	return nil
}

func TestKongPlanFirst_Contract(t *testing.T) {
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body := func(fallback, snapshot string) string {
		return `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [` + fallback + `],
			"snapshot": {"expires_at": "` + at.Format(time.RFC3339) + `", "accounts": [` + snapshot + `]}}`
	}
	req, err := ParseKongPlanDispatch(strings.NewReader(body(`{"id": 1, "tier": "normal"}`,
		`{"id": 1, "tier": "normal", "first_until": "`+at.In(time.FixedZone("BJ", 8*3600)).Format(time.RFC3339)+`"}`)))
	require.NoError(t, err)
	require.NotNil(t, req.Snapshot.Accounts[0].FirstUntil)
	require.True(t, req.Snapshot.Accounts[0].FirstUntil.Equal(at), "带时区的时刻转成 UTC")
	require.Equal(t, time.UTC, req.Snapshot.Accounts[0].FirstUntil.Location())

	plain, err := ParseKongPlanDispatch(strings.NewReader(body(`{"id": 1, "tier": "normal"}`, `{"id": 1, "tier": "normal"}`)))
	require.NoError(t, err)
	null, err := ParseKongPlanDispatch(strings.NewReader(body(`{"id": 1, "tier": "normal"}`, `{"id": 1, "tier": "normal", "first_until": null}`)))
	require.NoError(t, err)
	require.Equal(t, plain.SHA256, null.SHA256, "没有截止时内容指纹与不认这个字段的版本相同")
	require.NotEqual(t, plain.SHA256, req.SHA256)

	_, err = ParseKongPlanDispatch(strings.NewReader(body(`{"id": 1, "tier": "normal", "first_until": "`+at.Format(time.RFC3339)+`"}`, ``)))
	require.ErrorContains(t, err, "first_until", "兜底条目不能带")
	_, err = ParseKongPlanDispatch(strings.NewReader(body(``, `{"id": 1, "tier": "normal", "first_until": "明天"}`)))
	require.ErrorContains(t, err, "first_until")
}

func TestKongPlanFirst_SelectsFirstUntilSessionsFull(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.firstEntry(2, 0)+"]")

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(2), sel.Account.ID, "前置账号先于原优先级更高的账号")
	require.Equal(t, []int{1}, h.sess.cache.registerCaps, "前置位置按生效上限登记")

	h.sess.cache.registerCaps = nil
	sel = h.sess.selectAccount(t, context.Background(), "s-next")
	// 2 此刻已有 s-new：会话满，不再前置，按原段排在 1 的余量之后
	require.Equal(t, int64(1), sel.Account.ID)
	require.Equal(t, []int{1}, h.sess.cache.registerCaps)
}

func TestKongPlanFirst_RegisterFailureGoesToGrace(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.firstEntry(2, 0)+"]")
	h.sess.cache.stealOnRegister[2] = "thief"

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID, "前置位置名额被抢：2 失去余量机会降到宽限段，1 的余量位置在前")
	require.Equal(t, []int{1, 1}, h.sess.cache.registerCaps)
}

func TestKongPlanFirst_RetierAtFirstStaysInFirst(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[2]", 0)
	h.publish(t, 1, "["+h.firstEntry(2, 1)+"]")
	h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	other := h.selAccount(1, 1, 0, 50)
	listed, fresh := h.selAccount(2, 2, 0, 98), h.selAccount(2, 2, 0, 99)
	ctx := context.Background()
	p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&other, &listed}, nil, nil)
	var tried []string
	for _, item := range p.sequenceLoads(kongPlanTestLoads(&other, &listed)) {
		if !p.next(item.account.ID) {
			continue
		}
		tried = append(tried, strconv.FormatInt(item.account.ID, 10)+":"+p.cur.Key.Seg.String())
		cur := item.account
		if cur.ID == 2 {
			cur = &fresh
		}
		if p.confirm(ctx, nil, cur) {
			break
		}
	}
	require.Equal(t, []string{"2:first", "2:first"}, tried, "前置位置上改判为 credits 层：在前置段里它的改判位置继续，先于 1 的余量位置")
	require.Len(t, h.ledger.Entries(2), 1, "按 credits 层选中前登记入层")
}

func TestKongPlanFirst_RetierAfterLosingRoomGoesToCreditsOverflow(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[2]", 0)
	h.publish(t, 1, "["+h.firstEntry(2, 1)+"]")
	h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	listed, fresh := h.selAccount(2, 1, 1, 98), h.selAccount(2, 1, 1, 99)
	cache := newKongFakeSessionCache(func() time.Time { return kongSessionT0 })
	cache.stealOnRegister[2] = "thief"
	g := &kongSessionGate{limit: newKongSessionLimit(cache, func() time.Time { return kongSessionT0 }), hash: "s",
		counts: map[int64]int{2: 0}, full: map[int64]bool{}}
	ctx := context.Background()
	p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&listed}, g, nil)
	p.sequenceLoads(kongPlanTestLoads(&listed))

	s := kongPlanNextActive(p, 2)
	require.NotNil(t, s)
	require.Equal(t, kongPlanSegFirst, s.Key.Seg)
	require.False(t, p.confirm(ctx, g, &listed), "前置位置名额被抢")
	require.Equal(t, kongPlanSegGrace, p.minSeg[2])

	s = kongPlanNextActive(p, 2)
	require.NotNil(t, s)
	require.Equal(t, kongPlanSegGrace, s.Key.Seg, "前置段里的改判位置没有激活，下一个是宽限段")
	require.False(t, p.confirm(ctx, g, &fresh), "宽限位置上才复核到 credits 层")
	require.Equal(t, kongPlanSegCreditsOverflow, p.minSeg[2], "已失去余量机会：到 credits 溢出段，不回到前置或 credits 有余量段")

	s = kongPlanNextActive(p, 2)
	require.NotNil(t, s)
	require.Equal(t, kongPlanSegCreditsOverflow, s.Key.Seg)
	require.True(t, p.confirm(ctx, g, &fresh))
	require.Len(t, h.ledger.Entries(2), 1)
}

func TestKongPlanFirst_SnapshotChangeKeepsRoundSettings(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[]", 0)
	h.publish(t, 1, "["+h.firstEntry(2, 0)+"]")
	a1, a2 := h.selAccount(1, 1, 0, 50), h.selAccount(2, 2, 0, 50)
	ctx := context.Background()
	p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&a1, &a2}, nil, nil)
	first := func(p *kongPlanSelection) []int64 {
		var ids []int64
		for _, s := range p.slots {
			if s.Key.Seg == kongPlanSegFirst {
				ids = append(ids, s.ID)
			}
		}
		return ids
	}
	p.sequenceLoads(kongPlanTestLoads(&a1, &a2))
	require.Equal(t, []int64{2}, first(p))

	h.publish(t, 1, `[{"id": 2, "tier": "normal"}]`)
	p.sequenceLoads(kongPlanTestLoads(&a1, &a2))
	require.Equal(t, []int64{2}, first(p), "本次选号沿用开始时的排序设置")

	q := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&a1, &a2}, nil, nil)
	q.sequenceLoads(kongPlanTestLoads(&a1, &a2))
	require.Empty(t, first(q), "下一次选号按新快照")

	h.publish(t, 1, `[{"id": 2, "tier": "normal", "first_until": "`+h.firstUntil(-time.Minute)+`"}]`)
	q = (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&a1, &a2}, nil, nil)
	q.sequenceLoads(kongPlanTestLoads(&a1, &a2))
	require.Empty(t, first(q), "截止已过不前置")
}

func TestKongPlanFirst_AccountsRouting(t *testing.T) {
	h := newKongPlanTierHarness(t)
	soon, past := h.firstUntil(time.Hour), h.firstUntil(-time.Minute)
	h.publish(t, 1, `[{"id": 1, "tier": "normal", "max_sessions": 2, "first_until": "`+soon+`"},
		{"id": 2, "tier": "normal", "first_until": "`+soon+`"}, {"id": 3, "tier": "normal", "first_until": "`+past+`"},
		{"id": 4, "tier": "normal"}, {"id": 5, "tier": "standby", "first_until": "`+soon+`"},
		{"id": 6, "tier": "normal", "paused": true, "first_until": "`+soon+`"}, {"id": 7, "tier": "normal", "first_until": "`+soon+`"}]`)
	var list []Account
	for id := int64(1); id <= 7; id++ {
		a := h.account(10, 0)
		a.ID, a.Concurrency = id, 4
		list = append(list, *a)
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{}}
	loads := map[int64]*AccountLoadInfo{}
	for id := int64(1); id <= 6; id++ {
		loads[id] = &AccountLoadInfo{CurrentConcurrency: 1}
	}
	f := &kongPlanFakeLive{loads: loads, sessions: map[int64]int{1: 2}}
	views, err := KongPlanAccounts(context.Background(), &kongPlanListRepo{stubQuotaAccountRepo: repo, list: list}, f)
	require.NoError(t, err)
	got := map[int64]string{}
	for _, v := range views {
		require.NotNil(t, v.Routing)
		require.False(t, v.Routing.JudgedAt.IsZero())
		got[v.ID] = v.Routing.FirstReason
		if v.Routing.FirstInEffect {
			got[v.ID] = "in_effect"
		}
	}
	require.Equal(t, map[int64]string{1: "sessions_full", 2: "in_effect", 3: "expired", 4: "", 5: "standby", 6: "held", 7: "busy"}, got)
	require.Nil(t, views[3].Routing.FirstUntil, "条目没有截止时为 null")
	require.NotNil(t, views[1].Routing.FirstUntil)

	b, err := json.Marshal(views[3])
	require.NoError(t, err)
	require.Contains(t, string(b), `"routing":{"first_until":null,"first_in_effect":false,"judged_at":`)
}

func TestKongPlanFirst_RetierKeepsStartTierAcrossRounds(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.control(t, 2, `"on"`, "[2]", 0)
	h.publish(t, 1, "["+h.firstEntry(2, 1)+"]")
	h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	listed, fresh := h.selAccount(2, 1, 0, 98), h.selAccount(2, 1, 0, 99)
	ctx := context.Background()
	p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&listed}, nil, nil)
	p.sequenceLoads(kongPlanTestLoads(&listed))
	require.NotNil(t, kongPlanNextActive(p, 2))
	// 选号途中快照把它改成备用档：本次选号仍按开始时的档位与前置
	standby := strings.Replace(h.firstEntry(2, 1), `"tier": "normal"`, `"tier": "standby"`, 1)
	h.publish(t, 1, "["+standby+"]")
	require.False(t, p.confirm(ctx, nil, &fresh))
	require.Equal(t, KongPlanTierNormal, p.facts[2].Tier.Entry.Tier)
	p.sequenceLoads(kongPlanTestLoads(&fresh)) // 刷新负载后的下一轮
	s := kongPlanNextActive(p, 2)
	require.NotNil(t, s)
	require.Equal(t, kongPlanSegFirst, s.Key.Seg, "下一轮仍在前置段")

	q := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&fresh}, nil, nil)
	q.sequenceLoads(kongPlanTestLoads(&fresh))
	s = kongPlanNextActive(q, 2)
	require.NotNil(t, s)
	require.NotEqual(t, kongPlanSegFirst, s.Key.Seg, "下一次选号按新快照的备用档")
}

func TestKongPlanFirst_LostFirstRemembersAcrossRounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grace string
		at    kongPlanSeg
	}{
		{"降到宽限后在负载未知的一轮改判", "1", kongPlanSegGrace},
		{"宽限为 0、降到溢出后在负载未知的一轮改判", "0", kongPlanSegOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newKongPlanTierHarness(t)
			h.control(t, 2, `"on"`, "[2]", 0)
			entry := strings.Replace(h.firstEntry(2, 1), `"tier": "normal"`, `"tier": "normal", "grace": `+tc.grace, 1)
			h.publish(t, 1, "["+entry+"]")
			h.setPace(2, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
				Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
			listed, fresh := h.selAccount(2, 1, 1, 98), h.selAccount(2, 1, 1, 99)
			cache := newKongFakeSessionCache(func() time.Time { return kongSessionT0 })
			cache.stealOnRegister[2] = "thief"
			g := &kongSessionGate{limit: newKongSessionLimit(cache, func() time.Time { return kongSessionT0 }), hash: "s",
				counts: map[int64]int{2: 0}, full: map[int64]bool{}}
			ctx := context.Background()
			p := (&OpenAIGatewayService{}).kongPlanBegin(ctx, []*Account{&listed}, g, nil)
			p.sequenceLoads(kongPlanTestLoads(&listed))
			require.NotNil(t, kongPlanNextActive(p, 2))
			require.False(t, p.confirm(ctx, g, &listed), "前置位置名额被抢")

			p.sequenceAccounts([]*Account{&listed}) // 负载读取失败的一轮：不再有前置段
			s := kongPlanNextActive(p, 2)
			require.NotNil(t, s)
			require.Equal(t, tc.at, s.Key.Seg)
			require.False(t, p.confirm(ctx, g, &fresh), "这时才复核到 credits 层")
			require.Equal(t, kongPlanSegCreditsOverflow, p.minSeg[2], "不重新获得已失去的余量机会")
			s = kongPlanNextActive(p, 2)
			require.NotNil(t, s, "credits 溢出段的位置仍会试到")
			require.Equal(t, kongPlanSegCreditsOverflow, s.Key.Seg)
			require.True(t, p.confirm(ctx, g, &fresh))
			require.Len(t, h.ledger.Entries(2), 1)
		})
	}
}

func TestKongPlanFirst_RoutingUsesSelectionLoadFactor(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.publish(t, 1, `[{"id": 1, "tier": "normal", "first_until": "`+h.firstUntil(time.Hour)+`"}]`)
	res := h.rt.tier(context.Background(), h.account(10, 0), h.now)
	two, ten := 2, 10
	for _, tc := range []struct {
		name   string
		factor *int
		conc   int
		live   int
		want   bool
	}{
		{"没有负载因子：按生效并发上限", nil, 10, 2, true},
		{"负载因子小于并发上限：按负载因子算已满", &two, 10, 2, false},
		{"负载因子大于并发上限：按负载因子算未满", &ten, 4, 4, true},
		{"并发不限：分母取 1，有在途就算满", nil, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.account(10, 0)
			a.LoadFactor = tc.factor
			v := &KongPlanAccountView{ID: 1, MaxConcurrency: tc.conc, Live: &KongPlanLiveView{Concurrency: tc.live}}
			r := kongPlanRouting(res, v, a, h.now)
			require.Equal(t, tc.want, r.FirstInEffect, r.FirstReason)
			if !tc.want {
				require.Equal(t, "busy", r.FirstReason)
			}
		})
	}
}
