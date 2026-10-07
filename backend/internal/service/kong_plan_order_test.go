//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 两轮排序的共用夹具（testdata/kong_plan/order_cases.json）与选号各分支的调用链。

type kongPlanOrderFile struct {
	Defaults map[string]any `json:"defaults"`
	Cases    []struct {
		Name     string            `json:"name"`
		All      map[string]any    `json:"all"`
		Demoted  map[string]string `json:"demoted"`
		Accounts []map[string]any  `json:"accounts"`
		Expect   [][]any           `json:"expect"`
		Limits   map[string][]any  `json:"limits"`
	} `json:"cases"`
}

func kongPlanMerge(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if cur, ok := dst[k].(map[string]any); ok {
				merged := map[string]any{}
				kongPlanMerge(merged, cur)
				kongPlanMerge(merged, sub)
				dst[k] = merged
				continue
			}
		}
		dst[k] = v
	}
}

func kongPlanSegNamed(t *testing.T, name string) kongPlanSeg {
	for s := kongPlanSegRoom; s <= kongPlanSegRelay; s++ {
		if s.String() == name {
			return s
		}
	}
	t.Fatalf("未知的段 %q", name)
	return 0
}

func kongPlanCaseFacts(t *testing.T, raw map[string]any) kongPlanFacts {
	t.Helper()
	b, err := json.Marshal(raw)
	require.NoError(t, err)
	var a struct {
		ID    int64 `json:"id"`
		Type  string
		Entry struct {
			Tier        string              `json:"tier"`
			Active      bool                `json:"active"`
			Imminent    bool                `json:"imminent"`
			AdmitBelow  *KongPlanAdmitBelow `json:"admit_below"`
			MaxSessions *int                `json:"max_sessions"`
			Credits     *KongPlanCredits    `json:"credits"`
			Paused      bool                `json:"paused"`
		}
		Used7d          float64 `json:"used_7d"`
		Threshold7d     float64 `json:"threshold_7d"`
		CreditsOK       bool    `json:"credits_ok"`
		WindowUnknown   bool    `json:"window_unknown"`
		Sessions        int     `json:"sessions"`
		SessionsKnown   bool    `json:"sessions_known"`
		MaxSessions     int     `json:"max_sessions"`
		Grace           int     `json:"grace"`
		LoadKnown       bool    `json:"load_known"`
		ConcurrencyFull bool    `json:"concurrency_full"`
		Inc6            float64 `json:"inc6"`
		Inc24           float64 `json:"inc24"`
		PaceFull        bool    `json:"pace_full"`
		OpenEntry       bool    `json:"open_entry"`
		UnknownActive   bool    `json:"unknown_active"`
	}
	require.NoError(t, json.Unmarshal(b, &a))
	entry := KongPlanEntry{ID: a.ID, Tier: a.Entry.Tier, Active: a.Entry.Active, Imminent: a.Entry.Imminent,
		AdmitBelow: a.Entry.AdmitBelow, MaxSessions: a.Entry.MaxSessions, Credits: a.Entry.Credits, Paused: a.Entry.Paused}
	f := kongPlanFacts{ID: a.ID, Tier: kongPlanTierResult{Entry: entry, AtLine: a.OpenEntry || a.UnknownActive}}
	if a.Type != "relay" {
		switch {
		case a.Used7d < a.Threshold7d:
			f.Tier.Layer = kongPlanLayerSubscription
		case a.CreditsOK && entry.Credits != nil && !a.WindowUnknown:
			f.Tier.Layer = kongPlanLayerCredits
		default:
			f.Tier.Layer = kongPlanLayerPaused
		}
		f.SessionLimit = a.MaxSessions
		if entry.MaxSessions != nil {
			f.SessionLimit = *entry.MaxSessions
		}
		f.Grace = a.Grace
		f.Sessions, f.SessionsKnown = a.Sessions, a.SessionsKnown
	}
	f.LoadKnown = a.LoadKnown
	f.ConcurrencyFull = a.LoadKnown && a.ConcurrencyFull
	f.Inc6, f.Inc24, f.PaceFull = a.Inc6, a.Inc24, a.PaceFull
	return f
}

func kongPlanKeyName(k kongPlanBucketKey) string {
	if !k.Seg.subscription() {
		return k.Seg.String()
	}
	name := k.Seg.String() + "/" + k.Group.String()
	if k.Healthy {
		name += "/healthy"
	} else {
		name += "/busy"
	}
	if k.Imminent {
		name += "/imminent"
	}
	return name
}

func kongPlanSlotName(s kongPlanSlot) string {
	name := strconv.FormatInt(s.ID, 10)
	if s.Ghost {
		name += "*"
	}
	return name
}

func kongPlanExpectName(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func TestKongPlanOrderCases(t *testing.T) {
	data, err := os.ReadFile("testdata/kong_plan/order_cases.json")
	require.NoError(t, err)
	var file kongPlanOrderFile
	require.NoError(t, json.Unmarshal(data, &file))
	require.NotEmpty(t, file.Cases)
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			facts := make([]kongPlanFacts, 0, len(c.Accounts))
			for _, raw := range c.Accounts {
				merged := map[string]any{}
				kongPlanMerge(merged, file.Defaults)
				kongPlanMerge(merged, c.All)
				kongPlanMerge(merged, raw)
				facts = append(facts, kongPlanCaseFacts(t, merged))
			}
			minSeg := map[int64]kongPlanSeg{}
			for id, seg := range c.Demoted {
				n, err := strconv.ParseInt(id, 10, 64)
				require.NoError(t, err)
				minSeg[n] = kongPlanSegNamed(t, seg)
			}
			slots := kongPlanOrder(facts, minSeg)

			type bucket struct {
				key string
				ids []string
			}
			var got []bucket
			for i, s := range slots {
				if i == 0 || s.Key != slots[i-1].Key {
					got = append(got, bucket{key: kongPlanKeyName(s.Key)})
				}
				got[len(got)-1].ids = append(got[len(got)-1].ids, kongPlanSlotName(s))
			}
			var want []bucket
			for _, e := range c.Expect {
				require.Len(t, e, 2)
				b := bucket{key: e[0].(string)}
				for _, id := range e[1].([]any) {
					b.ids = append(b.ids, kongPlanExpectName(id))
				}
				want = append(want, b)
			}
			norm := func(bs []bucket) []string {
				out := make([]string, len(bs))
				for i, b := range bs {
					ids := append([]string(nil), b.ids...)
					if !strings.HasPrefix(b.key, "credits") {
						sort.Strings(ids)
					}
					out[i] = b.key + " " + strings.Join(ids, ",")
				}
				return out
			}
			require.Equal(t, norm(want), norm(got))

			for id, limits := range c.Limits {
				var gotLimits []string
				for _, s := range slots {
					if strconv.FormatInt(s.ID, 10) == id {
						if s.Limit == kongSessionUnlimited {
							gotLimits = append(gotLimits, "unlimited")
						} else {
							gotLimits = append(gotLimits, strconv.Itoa(s.Limit))
						}
					}
				}
				var wantLimits []string
				for _, l := range limits {
					wantLimits = append(wantLimits, kongPlanExpectName(l))
				}
				require.Equal(t, wantLimits, gotLimits, "账号 %s 各位置的登记上限", id)
			}
		})
	}
}

// ---------- 选号各分支的调用链 ----------

type kongPlanFlakyLedgerStore struct {
	*kongPlanFakeLedgerStore
	enterErr error
}

func (s *kongPlanFlakyLedgerStore) MarkEntered(ctx context.Context, id, seq int64, at time.Time) (bool, time.Time, bool, error) {
	if s.enterErr != nil {
		return false, time.Time{}, false, s.enterErr
	}
	return s.kongPlanFakeLedgerStore.MarkEntered(ctx, id, seq, at)
}

type kongPlanSelHarness struct {
	*kongPlanTierHarness
	sess *kongSessionHarness
}

// kongPlanSelAccount 是一个 OpenAI OAuth 账号：used7d 到 99 即到停调阈值。
func (h *kongPlanTierHarness) selAccount(id int64, priority, maxSessions int, used7d float64) Account {
	a := kongSessionOAuth(id, priority, maxSessions)
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	a.Credentials = map[string]any{"account_scheduling_threshold": 99}
	a.Extra["codex_7d_used_percent"] = used7d
	a.Extra["codex_7d_window_minutes"] = 10080
	a.Extra["codex_7d_reset_at"] = h.reset.UTC().Format(time.RFC3339)
	a.Extra["codex_5h_used_percent"] = 1.0
	a.Extra["codex_5h_window_minutes"] = 300
	a.Extra["codex_5h_reset_at"] = h.now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	a.Extra["codex_usage_updated_at"] = h.now.Add(-time.Minute).UTC().Format(time.RFC3339)
	return a
}

// newKongPlanSelHarness 建一个计划输入生效的选号环境。credits 是 credits 层的账号：已在白名单里、节奏状态里有当前
// 窗口与余额。entries 是快照条目（JSON 数组）。
func newKongPlanSelHarness(t *testing.T, accounts []Account, conc stubConcurrencyCache, entries string, credits ...int64) *kongPlanSelHarness {
	t.Helper()
	th := newKongPlanTierHarness(t)
	allow := make([]string, len(credits))
	for i, id := range credits {
		allow[i] = strconv.FormatInt(id, 10)
		th.setPace(id, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: th.reset, WindowSeq: 4,
			Balance: &kongPlanBalancePoint{At: th.now.Add(-time.Hour), Value: 500}})
	}
	th.control(t, 10, `"on"`, "["+strings.Join(allow, ",")+"]", 0)
	th.publish(t, 1, entries)
	return &kongPlanSelHarness{kongPlanTierHarness: th, sess: newKongSessionHarness(accounts, conc, nil)}
}

func (h *kongPlanTierHarness) creditsEntry(id int64, level int) string {
	return fmt.Sprintf(`{"id": %d, "tier": "normal", "credits": {"level": %d, "expires_at": %q}}`, id, level, h.now.Add(30*24*time.Hour).UTC().Format(time.RFC3339))
}

func TestKongPlanSelect_GraceBeforeCredits(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 1, 1, 99)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.creditsEntry(2, 1)+"]", 2)
	h.control(t, 11, `"on"`, "[2]", 0)
	h.sess.cache.put(1, "other")

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID, "第一层账号在宽限之内先于 credits")
	require.Equal(t, []int{2}, h.sess.cache.registerCaps, "宽限段按上限 + 宽限登记")
	require.Empty(t, h.ledger.Entries(2), "没用到 credits 层就不登记入层")

	h.sess.cache.put(1, "third")
	sel = h.sess.selectAccount(t, context.Background(), "s-next")
	require.Equal(t, int64(2), sel.Account.ID, "到了上限 + 宽限，credits 层先于第一层溢出")
	entries := h.ledger.Entries(2)
	require.Len(t, entries, 1)
	require.Equal(t, int64(4), entries[0].Seq, "按（账号，窗口序号）登记入层")
}

func TestKongPlanSelect_ConfirmRaceDemotesToGrace(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 99)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.creditsEntry(2, 1)+"]", 2)
	h.sess.cache.stealOnRegister[1] = "thief"

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID, "名额被抢后降到宽限段再试一次，仍先于 credits")
	require.Equal(t, []int{1, 2}, h.sess.cache.registerCaps)
	require.True(t, h.sess.cache.has(1, "s-new"))
}

func TestKongPlanSelect_EnterFailureSkipsCredits(t *testing.T) {
	th := newKongPlanTierHarness(t)
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 1, GroupIDs: []int64{1}}
	accounts := []Account{th.selAccount(2, 1, 1, 99), relay}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.creditsEntry(2, 1)+"]", 2)
	h.lstore.enterErr = errors.New("redis down")

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(3), sel.Account.ID, "入层登记不进去时不用 credits，落到中转")
	require.Empty(t, h.ledger.Entries(2))
}

func TestKongPlanSelect_RelayAfterAllOAuth(t *testing.T) {
	th := newKongPlanTierHarness(t)
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 0, GroupIDs: []int64{1}}
	accounts := []Account{relay, th.selAccount(1, 5, 1, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, `[{"id": 3, "tier": "asap", "admit_below": {"6h": 50, "24h": 50}}]`)
	h.sess.cache.put(1, "a")
	h.sess.cache.put(1, "b")

	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID, "中转不论档位、不论原优先级，排在溢出的 OAuth 之后")
}

func TestKongPlanSelect_HeldOnlyWhenOthersCannot(t *testing.T) {
	th := newKongPlanTierHarness(t)
	// 1 有周额度但计划暂停，2 在 credits 层：先用 credits 层的 2
	accounts := []Account{th.selAccount(1, 0, 3, 50), th.selAccount(2, 9, 3, 99)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{},
		`[{"id": 1, "tier": "normal", "paused": true}, `+th.creditsEntry(2, 1)+`]`, 2)
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(2), sel.Account.ID, "计划暂停的账号排在 credits 层之后")

	// 只剩计划暂停的账号（与中转）：用它，不因暂停让请求失败；排在中转之前
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 0, GroupIDs: []int64{1}}
	h2 := newKongPlanSelHarness(t, []Account{relay, th.selAccount(1, 5, 1, 50)}, stubConcurrencyCache{},
		`[{"id": 1, "tier": "normal", "paused": true}]`)
	h2.sess.cache.put(1, "a")
	sel = h2.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID, "别的 OAuth 都接不住时用计划暂停的账号，先于中转")
	require.Equal(t, []int{kongSessionUnlimited}, h2.sess.cache.registerCaps, "计划暂停段不受会话上限约束")
}

func TestKongPlanSelect_HeldKeepsExistingBinding(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 0, 3, 50), th.selAccount(2, 0, 3, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, `[]`)
	first := h.sess.selectAccount(t, context.Background(), "s-old")
	held := first.Account.ID
	other := int64(3) - held
	// 选中之后这个账号被计划暂停：已有会话照常复用，新会话落到另一个账号
	h.publish(t, 1, fmt.Sprintf(`[{"id": %d, "tier": "normal", "paused": true}]`, held))
	require.Equal(t, held, h.sess.selectAccount(t, context.Background(), "s-old").Account.ID, "已有会话不改绑")
	require.Equal(t, other, h.sess.selectAccount(t, context.Background(), "s-new").Account.ID, "新会话不给计划暂停的账号")
}

func TestKongPlanSelect_AsapAdmissionOrdersGroups(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 0, 30), th.selAccount(2, 9, 0, 30)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, `[{"id": 2, "tier": "asap", "admit_below": {"6h": 5, "24h": 20}}]`)
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(2), sel.Account.ID, "通过准入的尽快账号压过原优先级更高的普通账号")

	// 近 24h 消耗到了准入线：归入普通组，原优先级重新起作用
	h.setPace(2, kongPaceAccountState{Plan: "pro", Rises: []kongPaceRise{{At: h.now.Add(-time.Hour), Points: 20}}})
	sel = h.sess.selectAccount(t, context.Background(), "s-new2")
	require.Equal(t, int64(1), sel.Account.ID)

	// 有未结算的入层（视同到线）：同样不准入
	h.setPace(2, kongPaceAccountState{Plan: "pro"})
	require.NoError(t, h.ledger.MarkEntered(context.Background(), 2, 1, h.now))
	sel = h.sess.selectAccount(t, context.Background(), "s-new3")
	require.Equal(t, int64(1), sel.Account.ID)
}

func TestKongPlanSelect_FallbackWaitAndLoadErrorUseSequence(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 99)}
	entries := "[" + th.creditsEntry(2, 1) + "]"

	t.Run("第 3 层", func(t *testing.T) {
		h := newKongPlanSelHarness(t, accounts, kongSessionAllBusy(1, 2), entries, 2)
		h.sess.cache.put(1, "a")
		h.sess.cache.put(1, "b")
		sel := h.sess.selectAccount(t, context.Background(), "s-new")
		require.NotNil(t, sel.WaitPlan)
		require.Equal(t, int64(2), sel.Account.ID, "排队同样先 credits 余量段、后第一层溢出")
		require.Len(t, h.ledger.Entries(2), 1)
	})
	t.Run("负载读取失败", func(t *testing.T) {
		h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{loadBatchErr: errors.New("redis down")}, entries, 2)
		h.sess.cache.put(1, "a")
		sel := h.sess.selectAccount(t, context.Background(), "s-new")
		require.Equal(t, int64(1), sel.Account.ID, "宽限段先于 credits")
		require.Equal(t, []int{2}, h.sess.cache.registerCaps)
	})
}

func TestKongPlanSelect_NonBatchBranchRunsFullLoop(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 99)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "["+th.creditsEntry(2, 1)+"]", 2)
	h.sess.svc.concurrencyService = nil
	h.sess.cache.put(1, "a")
	h.sess.cache.put(1, "b")
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(2), sel.Account.ID, "计划生效时非批量分支也按完整序列")

	// 计划输入未生效：非批量分支照旧只看原优先级
	kongPlanRT.Store(nil)
	sel = h.sess.selectAccount(t, context.Background(), "s-new2")
	require.Equal(t, int64(1), sel.Account.ID)
}

func TestKongPlanSelect_LegacyWhenNoPlanInput(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, `[]`)
	h.sess.cache.put(1, "a")
	h.sess.cache.stealOnRegister[2] = "thief"

	// 兜底生效（快照过期）：2 的名额被抢后降到宽限段，按上限 + 宽限登记
	h.now = h.now.Add(2 * time.Hour)
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID)
	require.Equal(t, []int{1, 2}, h.sess.cache.registerCaps)

	// 清空发布后回到现状调度：确认登记都按账号自身的上限，名额被抢的账号在末尾按溢出放行
	req, err := ParseKongPlanPublish(strings.NewReader(`{"action": "disable", "clear": "all", "note": "回到现状调度"}`))
	require.NoError(t, err)
	_, err = h.store.PutPublish(context.Background(), req)
	require.NoError(t, err)
	require.False(t, kongPlanInEffect())
	h.sess.cache.sessions = map[int64]map[string]time.Time{}
	h.sess.cache.put(1, "a")
	h.sess.cache.stealOnRegister[2] = "thief"
	h.sess.cache.registerCaps = nil
	sel = h.sess.selectAccount(t, context.Background(), "s-new2")
	require.Equal(t, int64(1), sel.Account.ID)
	require.Equal(t, []int{1, kongSessionUnlimited}, h.sess.cache.registerCaps)
}

func TestKongPlanSelect_MarksLeftWhenBackInSubscription(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(2, 1, 0, 20)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, `[]`, 2)
	require.NoError(t, h.ledger.MarkEntered(context.Background(), 2, 4, h.now.Add(-3*time.Hour)))

	// 旧窗口回报的读数（重置时刻比当前窗口早出容差）不登记离层
	stale := accounts[0]
	stale.Extra = map[string]any{}
	for k, v := range accounts[0].Extra {
		stale.Extra[k] = v
	}
	stale.Extra["codex_7d_reset_at"] = h.reset.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	h.sess.svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{stale}}
	h.sess.selectAccount(t, context.Background(), "s-1")
	require.Nil(t, h.ledger.Entries(2)[0].LeftAt)

	h.sess.svc.accountRepo = stubOpenAIAccountRepo{accounts: accounts}
	h.sess.selectAccount(t, context.Background(), "s-2")
	e := h.ledger.Entries(2)[0]
	require.NotNil(t, e.LeftAt)
	require.True(t, e.LeftAt.Equal(parseExtraTime(accounts[0].Extra["codex_usage_updated_at"])), "离层时刻取读数的源查询时刻")
}

// 计划生效时节奏重排按（桶，原优先级）分段：同优先级的中转不再让整段放弃重排，到线的账号也不会被排到别的桶前面。
func TestKongPlanPaceReorderStaysInBucket(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 40, reset, kongPaceT0), kongPaceRow(2, "pro", 40, reset, kongPaceT0), kongPaceRow(3, "pro", 40, reset, kongPaceT0)}
	p, _, _ := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	p.rand = func() float64 { return 0 }
	svc := &OpenAIGatewayService{kongPace: p}
	d := svc.kongPaceBegin(context.Background(), nil, "h", "gpt", false)

	relay := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: 10, Concurrency: 5}
	plan := &kongPlanSelection{facts: map[int64]kongPlanFacts{
		1: {ID: 1, Tier: kongPlanTierResult{Layer: kongPlanLayerSubscription, Entry: KongPlanEntry{Tier: KongPlanTierNormal, Active: true}}},
		2: {ID: 2, Tier: kongPlanTierResult{Layer: kongPlanLayerSubscription, Entry: KongPlanEntry{Tier: KongPlanTierNormal, Active: true}}},
		3: {ID: 3, Tier: kongPlanTierResult{Layer: kongPlanLayerSubscription, Entry: KongPlanEntry{Tier: KongPlanTierStandby, Active: true}}},
		9: {ID: 9},
	}, minSeg: map[int64]kongPlanSeg{}}
	d.attachPlan(plan)
	in := kongPaceAvailable(kongPaceOAuth(3, 10), relay, kongPaceOAuth(1, 10), kongPaceOAuth(2, 10))
	in[1] = accountWithLoad{account: relay, loadInfo: &AccountLoadInfo{AccountID: 9}}
	in = plan.preferRoomLoads(nil, in)
	out := plan.sequenceLoads(d.reorder(in, openAILegacyUpstreamRateOrder{}))
	var ids []int64
	for _, item := range out {
		ids = append(ids, item.account.ID)
	}
	require.Len(t, ids, 4)
	require.ElementsMatch(t, []int64{1, 2}, ids[:2], "普通组在前")
	require.Equal(t, []int64{3, 9}, ids[2:], "备用组、中转依次在后")
	for _, v := range d.rounds[0].Candidates {
		if v.AccountID == 1 || v.AccountID == 2 {
			require.True(t, v.Paced, "同优先级混有中转时，OAuth 的桶照样按节奏排")
		}
	}
}

func TestKongPlanEffectiveLimits(t *testing.T) {
	th := newKongPlanTierHarness(t)
	acc := th.selAccount(1, 1, 0, 50)
	acc.Concurrency = 10
	two := 2
	acc.LoadFactor = nil
	th.publish(t, 1, `[{"id": 1, "tier": "normal", "max_sessions": 1, "max_concurrency": 3}]`)
	require.Equal(t, 1, kongEffectiveMaxSessions(&acc))
	require.True(t, kongSessionLimited(&acc), "账号自身不限、覆盖值限制时也算受限")
	require.Equal(t, 3, KongEffectiveConcurrency(&acc))
	require.Equal(t, 3, kongEffectiveLoadFactor(&acc), "负载因子留空时取生效并发上限")
	acc.LoadFactor = &two
	require.Equal(t, 2, kongEffectiveLoadFactor(&acc), "设了负载因子就用它")
	require.Equal(t, 3, KongWSTurnConcurrency(&acc, 7), "WS 后续轮次按此刻的生效值，不沿用建连时的值")

	other := th.selAccount(2, 1, 4, 50)
	other.Concurrency = 6
	require.Equal(t, 4, kongEffectiveMaxSessions(&other), "条目里没有的账号用自身的设置")
	require.Equal(t, 6, KongEffectiveConcurrency(&other))
	require.Equal(t, 6, KongWSTurnConcurrency(&other, 7), "撤销覆盖后回到账号自身的设置")

	kongPlanRT.Store(nil)
	require.Equal(t, 0, kongEffectiveMaxSessions(&acc), "计划输入未生效时用账号自身的设置")
	require.Equal(t, 10, KongEffectiveConcurrency(&acc))
	require.Equal(t, 7, KongWSTurnConcurrency(&acc, 7), "计划输入未生效时用建连时定下的值")
}

func TestKongPlanSelect_EffectiveLimitsApply(t *testing.T) {
	th := newKongPlanTierHarness(t)
	a1, a2 := th.selAccount(1, 1, 0, 50), th.selAccount(2, 2, 0, 50)
	entries := `[{"id": 1, "tier": "normal", "max_sessions": 1, "max_concurrency": 2}]`

	t.Run("会话上限覆盖", func(t *testing.T) {
		h := newKongPlanSelHarness(t, []Account{a1, a2}, stubConcurrencyCache{}, entries)
		h.control(t, 11, `"on"`, "[]", 0)
		h.sess.cache.put(1, "a")
		h.sess.cache.put(1, "b")
		sel := h.sess.selectAccount(t, context.Background(), "s-new")
		require.Equal(t, int64(2), sel.Account.ID, "1 的生效上限是 1、已到上限 + 宽限，新会话落到 2")
	})
	t.Run("并发上限覆盖", func(t *testing.T) {
		h := newKongPlanSelHarness(t, []Account{a1}, kongSessionAllBusy(1), entries)
		sel := h.sess.selectAccount(t, context.Background(), "s-new")
		require.NotNil(t, sel.WaitPlan)
		require.Equal(t, 2, sel.WaitPlan.MaxConcurrency, "排队计划按生效并发上限")
	})
}
