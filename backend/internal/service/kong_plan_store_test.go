//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// kongPlanFakeRepo 是内存里的仓储。saveErr 非空时写入返回错误；commitOnErr 为真时仍然落下这次写入，
// 模拟"事务已提交、确认丢失"。
type kongPlanFakeRepo struct {
	mu          sync.Mutex
	rows        map[string]KongPlanRow
	caller      *KongPoolCaller
	kinds       map[int64]KongPlanAccountKind
	loadErr     error
	saveErr     error
	commitOnErr bool
	loads       int
}

func newKongPlanFakeRepo() *kongPlanFakeRepo {
	gen, seq := int64(1), int64(0)
	return &kongPlanFakeRepo{
		rows: map[string]KongPlanRow{kongPlanKindPublish: {
			Kind: kongPlanKindPublish, Generation: &gen, Seq: &seq,
			Content: []byte(`{"generation": 1, "enabled": true, "last_seq": 0}`),
		}},
		kinds: map[int64]KongPlanAccountKind{
			1: {Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			2: {Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			3: {Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		},
	}
}

func (r *kongPlanFakeRepo) LoadKongPlan(context.Context) ([]KongPlanRow, *KongPoolCaller, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads++
	if r.loadErr != nil {
		return nil, nil, r.loadErr
	}
	out := make([]KongPlanRow, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, row)
	}
	var caller *KongPoolCaller
	if r.caller != nil {
		c := *r.caller
		caller = &c
	}
	return out, caller, nil
}

func (r *kongPlanFakeRepo) SaveKongPlanRows(_ context.Context, rows ...KongPlanRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil && !r.commitOnErr {
		return r.saveErr
	}
	for _, row := range rows {
		r.rows[row.Kind] = row
	}
	return r.saveErr
}

func (r *kongPlanFakeRepo) SaveKongPoolCaller(_ context.Context, c KongPoolCaller) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil && !r.commitOnErr {
		return r.saveErr
	}
	r.caller = &c
	return r.saveErr
}

func (r *kongPlanFakeRepo) KongPlanAccountKinds(_ context.Context, ids []int64) (map[int64]KongPlanAccountKind, error) {
	out := map[int64]KongPlanAccountKind{}
	for _, id := range ids {
		if k, ok := r.kinds[id]; ok {
			out[id] = k
		}
	}
	return out, nil
}

type kongPlanTestClock struct{ t time.Time }

func (c *kongPlanTestClock) now() time.Time { return c.t }

func newKongPlanTestStore(t *testing.T, repo *kongPlanFakeRepo) (*KongPlanStore, *kongPlanTestClock) {
	t.Helper()
	clock := &kongPlanTestClock{t: time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)}
	s := NewKongPlanStore(repo)
	s.now = clock.now
	s.Start(context.Background())
	return s, clock
}

func kongPlanTS(t time.Time) string { return t.Format(time.RFC3339) }

// kongPlanDispatchJSONBody 拼一份发布；accounts 与 fallback 直接给 JSON 数组。
func kongPlanDispatchJSONBody(gen, seq int64, expires time.Time, fallback, accounts string) string {
	return fmt.Sprintf(`{"generation": %d, "seq": %d, "plan_version": 5, "cycle_id": "c%d",
		"fallback": %s, "snapshot": {"expires_at": %q, "accounts": %s}}`,
		gen, seq, seq, fallback, kongPlanTS(expires), accounts)
}

func mustParseKongPlanDispatch(t *testing.T, body string) *KongPlanDispatchRequest {
	t.Helper()
	req, err := ParseKongPlanDispatch(strings.NewReader(body))
	require.NoError(t, err)
	return req
}

func requireKongPlanErr(t *testing.T, err error, code int, reason string) *infraerrors.ApplicationError {
	t.Helper()
	require.Error(t, err)
	appErr := infraerrors.FromError(err)
	require.Equal(t, int32(code), appErr.Code, err.Error())
	require.Equal(t, reason, appErr.Reason, err.Error())
	return appErr
}

const kongPlanFallback = `[{"id": 1, "tier": "normal", "credits": {"level": 1, "expires_at": "2026-12-31T23:59:00+08:00"}},
	{"id": 2, "tier": "standby"}]`

const kongPlanAccounts = `[{"id": 2, "tier": "asap", "admit_below": {"6h": 37.5, "24h": 60}, "max_sessions": 3},
	{"id": 1, "tier": "normal", "imminent": true, "credits": {"level": 1, "expires_at": "2026-12-31T23:59:00+08:00"}}]`

func TestParseKongPlanDispatch_Invalid(t *testing.T) {
	exp := "2026-10-02T08:00:00Z"
	cases := []struct {
		name, body, field string
	}{
		{"缺 generation", `{"seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "generation"},
		{"seq 为 0", `{"generation": 1, "seq": 0, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "seq"},
		{"cycle_id 过长", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "` + strings.Repeat("x", 129) + `", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "cycle_id"},
		{"缺 fallback", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "fallback"},
		{"时刻没有时区", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "2026-10-02T08:00:00", "accounts": []}}`, "snapshot.expires_at"},
		{"兜底里有 asap", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [{"id": 1, "tier": "asap", "admit_below": {"6h": 1, "24h": 2}}], "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "fallback[0].tier"},
		{"兜底带临期", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [{"id": 1, "tier": "normal", "imminent": true}], "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "fallback[0]"},
		{"asap 缺准入线", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": [{"id": 1, "tier": "asap"}]}}`, "snapshot.accounts[0].admit_below"},
		{"普通档带准入线", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": [{"id": 1, "tier": "normal", "admit_below": {"6h": 1, "24h": 2}}]}}`, "snapshot.accounts[0].admit_below"},
		{"未知档位", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": [{"id": 1, "tier": "urgent"}]}}`, "snapshot.accounts[0].tier"},
		{"账号重复", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": [{"id": 1, "tier": "normal"}, {"id": 1, "tier": "standby"}]}}`, "snapshot.accounts[1].id"},
		{"上限越界", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": [{"id": 1, "tier": "normal", "max_sessions": 65}]}}`, "snapshot.accounts[0].max_sessions"},
		{"级别越界", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": [{"id": 1, "tier": "normal", "credits": {"level": 10, "expires_at": "` + exp + `"}}]}}`, "snapshot.accounts[0].credits.level"},
		{"未知字段", `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": []}, "tight": true}`, "tight"},
		{"类型不对", `{"generation": "1", "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [], "snapshot": {"expires_at": "` + exp + `", "accounts": []}}`, "generation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKongPlanDispatch(strings.NewReader(tc.body))
			appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
			require.Equal(t, tc.field, appErr.Metadata["field"])
		})
	}
}

func TestParseKongPlanDispatch_FingerprintIgnoresFormatting(t *testing.T) {
	exp := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	a := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback, kongPlanAccounts))
	// 同样的内容：条目换序、省略的默认值写出来、时刻换时区写。
	b := mustParseKongPlanDispatch(t, `{"snapshot": {"accounts": [
			{"id": 1, "tier": "normal", "active": true, "imminent": true, "max_sessions": null,
			 "credits": {"expires_at": "2026-12-31T15:59:00Z", "level": 1}},
			{"id": 2, "tier": "asap", "admit_below": {"24h": 60, "6h": 37.5}, "max_sessions": 3, "credits": null}],
		"expires_at": "2026-10-02T16:00:00+08:00"},
		"fallback": [{"id": 2, "tier": "standby"}, {"id": 1, "tier": "normal", "credits": {"level": 1, "expires_at": "2026-12-31T23:59:00+08:00"}}],
		"cycle_id": "c7", "plan_version": 5, "seq": 7, "generation": 1}`)
	require.Equal(t, a.SHA256, b.SHA256)
	require.Equal(t, []int64{1, 2}, []int64{a.Snapshot.Accounts[0].ID, a.Snapshot.Accounts[1].ID})

	c := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback,
		strings.Replace(kongPlanAccounts, `"max_sessions": 3`, `"max_sessions": 4`, 1)))
	require.NotEqual(t, a.SHA256, c.SHA256)
}

func TestParseKongPlanDispatch_PausedFalseOmitted(t *testing.T) {
	exp := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	a := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback, kongPlanAccounts))
	b := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback,
		strings.Replace(kongPlanAccounts, `"max_sessions": 3`, `"max_sessions": 3, "paused": false`, 1)))
	require.Equal(t, a.SHA256, b.SHA256, "显式写 false 与省略的指纹相同：不暂停的条目指纹不变")
	out, err := json.Marshal(b.Snapshot.Accounts)
	require.NoError(t, err)
	require.NotContains(t, string(out), "paused", "为假时读回不写出")

	c := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback,
		strings.Replace(kongPlanAccounts, `"max_sessions": 3`, `"max_sessions": 3, "paused": true`, 1)))
	require.NotEqual(t, a.SHA256, c.SHA256)
	out, err = json.Marshal(c.Snapshot.Accounts)
	require.NoError(t, err)
	require.Contains(t, string(out), `"paused":true`)
}

func TestParseKongPlanDispatch_Grace(t *testing.T) {
	exp := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	a := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback, kongPlanAccounts))
	out, err := json.Marshal(a.Snapshot.Accounts)
	require.NoError(t, err)
	require.NotContains(t, string(out), "grace", "没给时读回不写出，指纹与不认这个字段的版本相同")

	b := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 7, exp, kongPlanFallback,
		strings.Replace(kongPlanAccounts, `"max_sessions": 3`, `"max_sessions": 3, "grace": 0`, 1)))
	require.NotEqual(t, a.SHA256, b.SHA256)
	require.NotNil(t, b.Snapshot.Accounts[1].Grace)
	require.Equal(t, 0, *b.Snapshot.Accounts[1].Grace)
	out, err = json.Marshal(b.Snapshot.Accounts)
	require.NoError(t, err)
	require.Contains(t, string(out), `"grace":0`, "给了 0 也写出")

	cases := []struct{ name, fallback, accounts, field string }{
		{"兜底带 grace", strings.Replace(kongPlanFallback, `{"id": 2, "tier": "standby"}`, `{"id": 2, "tier": "standby", "grace": 0}`, 1), kongPlanAccounts, "fallback[1].grace"},
		{"grace 为负", kongPlanFallback, strings.Replace(kongPlanAccounts, `"max_sessions": 3`, `"max_sessions": 3, "grace": -1`, 1), "snapshot.accounts[0].grace"},
		{"grace 越界", kongPlanFallback, strings.Replace(kongPlanAccounts, `"max_sessions": 3`, `"max_sessions": 3, "grace": 9`, 1), "snapshot.accounts[0].grace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKongPlanDispatch(strings.NewReader(kongPlanDispatchJSONBody(1, 7, exp, tc.fallback, tc.accounts)))
			appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
			require.Equal(t, tc.field, appErr.Metadata["field"])
		})
	}
}

func TestKongPlanEntryGrace(t *testing.T) {
	g := func(v int) *int { return &v }
	require.Equal(t, 1, kongPlanEntryGrace(1, KongPlanEntry{}), "条目没给时用人工约束的值")
	require.Equal(t, 0, kongPlanEntryGrace(1, KongPlanEntry{Grace: g(0)}), "限速时归零")
	require.Equal(t, 1, kongPlanEntryGrace(2, KongPlanEntry{Grace: g(1)}))
	require.Equal(t, 1, kongPlanEntryGrace(1, KongPlanEntry{Grace: g(3)}), "快照只能收紧、不能放宽")
}

func TestKongPlanStore_DispatchStateChecks(t *testing.T) {
	ctx := context.Background()
	s, clock := newKongPlanTestStore(t, newKongPlanFakeRepo())
	exp := clock.t.Add(135 * time.Minute)
	put := func(gen, seq int64, accounts string) (*KongPlanDispatchResult, error) {
		return s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(gen, seq, exp, kongPlanFallback, accounts)))
	}

	res, err := put(1, 3, kongPlanAccounts)
	require.NoError(t, err)
	require.False(t, res.Idempotent)
	require.Equal(t, clock.t, res.AcceptedAt)

	res, err = put(1, 3, kongPlanAccounts)
	require.NoError(t, err)
	require.True(t, res.Idempotent, "同一序号、同样内容按成功处理")

	_, err = put(1, 3, `[]`)
	appErr := requireKongPlanErr(t, err, 409, KongPlanReasonSeqConflict)
	require.Equal(t, map[string]string{"generation": "1", "enabled": "true", "seq": "3"}, appErr.Metadata)

	_, err = put(1, 2, kongPlanAccounts)
	requireKongPlanErr(t, err, 409, KongPlanReasonSeqStale)

	_, err = put(2, 9, kongPlanAccounts)
	requireKongPlanErr(t, err, 409, KongPlanReasonGenerationMismatch)

	// 序号可以跳号：边车取 max(本地, 网关) + 1。
	_, err = put(1, 10, `[]`)
	require.NoError(t, err)
	pub, err := s.GetPublish(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10), pub.LastSeq)
}

func TestKongPlanStore_DispatchDynamicChecks(t *testing.T) {
	ctx := context.Background()
	s, clock := newKongPlanTestStore(t, newKongPlanFakeRepo())

	for _, exp := range []time.Time{clock.t, clock.t.Add(-time.Minute), clock.t.Add(6*time.Hour + time.Second)} {
		_, err := s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 1, exp, `[]`, `[]`)))
		requireKongPlanErr(t, err, 400, KongPlanReasonSnapshotExpiry)
	}

	exp := clock.t.Add(time.Hour)
	_, err := s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 1, exp,
		`[{"id": 3, "tier": "normal", "credits": {"level": 2, "expires_at": "2026-12-31T00:00:00Z"}}]`, `[]`)))
	appErr := requireKongPlanErr(t, err, 400, KongPlanReasonCreditsNotOAuth)
	require.Equal(t, "3", appErr.Metadata["account_id"])

	// 不存在的账号照常接受，列在 ignored_ids 里；它带 credits 也不算错。
	res, err := s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 1, exp,
		`[{"id": 3, "tier": "normal"}]`,
		`[{"id": 99, "tier": "normal", "credits": {"level": 2, "expires_at": "2026-12-31T00:00:00Z"}}, {"id": 1, "tier": "normal"}]`)))
	require.NoError(t, err)
	require.Equal(t, []int64{99}, res.IgnoredIDs)
}

func TestKongPlanStore_PublishGenerationAndClear(t *testing.T) {
	ctx := context.Background()
	s, clock := newKongPlanTestStore(t, newKongPlanFakeRepo())
	exp := clock.t.Add(135 * time.Minute)
	put := func(gen, seq int64) error {
		_, err := s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(gen, seq, exp, kongPlanFallback, kongPlanAccounts)))
		return err
	}
	publish := func(body string) *KongPlanPublishView {
		req, err := ParseKongPlanPublish(strings.NewReader(body))
		require.NoError(t, err)
		v, err := s.PutPublish(ctx, req)
		require.NoError(t, err)
		return v
	}

	require.NoError(t, put(1, 1))
	v := publish(`{"action": "disable", "note": "排查"}`)
	require.Equal(t, int64(2), v.Generation)
	require.False(t, v.Enabled)
	require.Equal(t, KongPlanInUseSnapshot, v.InUse, "停用不改所用的输入")
	requireKongPlanErr(t, put(2, 2), 409, KongPlanReasonPublishDisabled)

	// 先停用、后来决定清空：再次停用也让代次加一。
	v = publish(`{"action": "disable", "clear": "snapshot", "note": "回到兜底"}`)
	require.Equal(t, int64(3), v.Generation)
	require.Equal(t, KongPlanInUseFallback, v.InUse)
	require.False(t, v.Dispatch.HasSnapshot)
	require.True(t, v.Dispatch.HasFallback)

	v = publish(`{"action": "disable", "clear": "all", "note": "回到现状调度"}`)
	require.Equal(t, KongPlanInUseLegacy, v.InUse)
	require.Equal(t, int64(1), v.LastSeq, "清空不清最近一次接受的序号")

	v = publish(`{"action": "enable", "note": "恢复"}`)
	require.Equal(t, int64(5), v.Generation)
	// 清空前发出、序号更高的发布迟到：代次不符被拒。
	requireKongPlanErr(t, put(1, 50), 409, KongPlanReasonGenerationMismatch)
	require.NoError(t, put(5, 2))
	d, err := s.GetDispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, KongPlanInUseSnapshot, d.InUse)

	_, err = ParseKongPlanPublish(strings.NewReader(`{"action": "enable", "clear": "all", "note": "x"}`))
	requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	_, err = ParseKongPlanPublish(strings.NewReader(`{"action": "disable"}`))
	requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
}

func TestKongPlanStore_InUseFollowsSnapshotExpiry(t *testing.T) {
	ctx := context.Background()
	s, clock := newKongPlanTestStore(t, newKongPlanFakeRepo())
	d, err := s.GetDispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, KongPlanInUseLegacy, d.InUse)
	require.Nil(t, d.Fallback)

	exp := clock.t.Add(time.Hour)
	_, err = s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 1, exp, `[]`, kongPlanAccounts)))
	require.NoError(t, err)
	clock.t = exp.Add(-time.Second)
	d, err = s.GetDispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, KongPlanInUseSnapshot, d.InUse)
	clock.t = exp
	d, err = s.GetDispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, KongPlanInUseFallback, d.InUse, "空的兜底也是兜底")
	require.Equal(t, []KongPlanEntry{}, d.Fallback)
}

func TestKongPlanStore_Control(t *testing.T) {
	ctx := context.Background()
	s, _ := newKongPlanTestStore(t, newKongPlanFakeRepo())
	put := func(body string) (*KongPlanControlView, error) {
		req, err := ParseKongPlanControl(strings.NewReader(body))
		require.NoError(t, err)
		return s.PutControl(ctx, req)
	}

	v, err := s.GetControl(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), v.Revision)
	require.Equal(t, KongPlanCreditsModeShadow, v.CreditsMode)
	require.Equal(t, map[string]int{"default": 0}, v.OverflowMargin)

	body := `{"revision": 1, "credits_mode": "on", "allow": [4, 3], "floor": 0, "per_point": null,
		"overflow_margin": {"default": 1, "3": 2}, "file_hold": null}`
	v, err = put(body)
	require.NoError(t, err)
	require.Equal(t, []int64{3, 4}, v.Allow)

	v, err = put(`{"allow": [3, 4], "revision": 1, "credits_mode": "on", "floor": 0, "overflow_margin": {"3": 2, "default": 1}}`)
	require.NoError(t, err)
	require.True(t, v.Idempotent, "同号同内容：缺省的 null 与写法顺序不影响指纹")

	_, err = put(`{"revision": 1, "credits_mode": "shadow", "allow": [3, 4], "floor": 0, "overflow_margin": {"default": 1}}`)
	requireKongPlanErr(t, err, 409, KongPlanReasonRevisionConflict)

	hold, err := ParseKongPlanHold(strings.NewReader(`{"trigger": "daily_cap:c1", "reason": "daily_cap"}`))
	require.NoError(t, err)
	_, err = s.Hold(ctx, hold)
	require.NoError(t, err)

	v, err = put(`{"revision": 3, "credits_mode": "shadow", "allow": [], "floor": 5, "overflow_margin": {"default": 0},
		"file_hold": {"since": "2026-10-02T14:00:00+08:00"}}`)
	require.NoError(t, err)
	require.Equal(t, KongPlanCreditsModeShadow, v.CreditsMode)
	require.Len(t, v.LatchedHolds, 1, "同步不碰锁存暂停")
	require.Equal(t, time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC), v.FileHold.Since)

	_, err = put(body)
	appErr := requireKongPlanErr(t, err, 409, KongPlanReasonRevisionStale)
	require.Equal(t, "3", appErr.Metadata["revision"])

	for _, bad := range []string{
		`{"revision": 0, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": {"default": 0}}`,
		`{"revision": 1, "credits_mode": "off", "allow": [], "floor": 0, "overflow_margin": {"default": 0}}`,
		`{"revision": 1, "credits_mode": "on", "allow": [3, 3], "floor": 0, "overflow_margin": {"default": 0}}`,
		`{"revision": 1, "credits_mode": "on", "allow": [], "floor": -1, "overflow_margin": {"default": 0}}`,
		`{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0, "per_point": 0, "overflow_margin": {"default": 0}}`,
		`{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": {"3": 1}}`,
		`{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": {"default": 9}}`,
		`{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": {"default": 1, "x": 1}}`,
	} {
		_, err := ParseKongPlanControl(strings.NewReader(bad))
		requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	}
}

func TestKongPlanStore_HoldRelease(t *testing.T) {
	ctx := context.Background()
	s, _ := newKongPlanTestStore(t, newKongPlanFakeRepo())
	hold := func(trigger, reason string) *KongPlanHoldResult {
		req, err := ParseKongPlanHold(strings.NewReader(fmt.Sprintf(`{"trigger": %q, "reason": %q}`, trigger, reason)))
		require.NoError(t, err)
		res, err := s.Hold(ctx, req)
		require.NoError(t, err)
		return res
	}
	release := func(body string) *KongPlanReleaseResult {
		req, err := ParseKongPlanRelease(strings.NewReader(body))
		require.NoError(t, err)
		res, err := s.Release(ctx, req)
		require.NoError(t, err)
		return res
	}

	require.Equal(t, &KongPlanHoldResult{State: "active"}, hold("daily_cap:c1", "daily_cap"))
	require.Equal(t, &KongPlanHoldResult{State: "active", Idempotent: true}, hold("daily_cap:c1", "daily_cap"))
	require.Equal(t, &KongPlanReleaseResult{Released: []string{"daily_cap:c1"}, Active: []string{}},
		release(`{"triggers": ["daily_cap:c1"], "note": "人工确认"}`))
	require.Equal(t, &KongPlanHoldResult{State: "released"}, hold("daily_cap:c1", "daily_cap"), "迟到的同编号设置不再生效")

	// 编号原本不存在也记下。
	release(`{"triggers": ["manual:late"], "note": "预先作废"}`)
	require.Equal(t, "released", hold("manual:late", "manual").State)

	hold("daily_cap:c2", "daily_cap")
	hold("manual:m1", "manual")
	require.Equal(t, &KongPlanReleaseResult{Released: []string{"daily_cap:c2"}, Active: []string{"manual:m1"}},
		release(`{"reason": "daily_cap", "note": "按原因解除"}`))

	v, err := s.GetControl(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"daily_cap:c1", "manual:late", "daily_cap:c2"}, v.ReleasedTriggers)
	require.Len(t, v.LatchedHolds, 1)

	for _, bad := range []string{
		`{"trigger": "has space", "reason": "manual"}`,
		`{"trigger": "` + strings.Repeat("a", 65) + `", "reason": "manual"}`,
		`{"trigger": "t", "reason": "other"}`,
	} {
		_, err := ParseKongPlanHold(strings.NewReader(bad))
		requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	}
	for _, bad := range []string{
		`{"triggers": ["t"], "reason": "manual", "note": "x"}`,
		`{"triggers": ["t"]}`,
		`{"triggers": [], "note": "x"}`,
	} {
		_, err := ParseKongPlanRelease(strings.NewReader(bad))
		requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	}
}

const kongPlanForecastBody = `{"cycle_id": "c1", "plan_version": 5, "computed_at": %q, "interval_min": 60,
	"per_session_pp_per_hour": 1.5, "avg_session_pp": 0.8, "demand_estimate_pp_per_hour": 10.1,
	"accounts": [{"id": 11, "k": 1.0, "depth_pp_per_hour": 2.5}],
	"forecast": [{"at": "2026-10-02T15:00:00+08:00", "pp_per_hour": 9}],
	"runway_h": {"immediate": 67, "all_cards": 167, "concentrate": 121},
	"credits_runway_h": [{"level": 1, "hours": null}], "next_reset_at": "2026-10-03T01:00:00+08:00"}`

func TestKongPlanStore_Forecast(t *testing.T) {
	ctx := context.Background()
	s, _ := newKongPlanTestStore(t, newKongPlanFakeRepo())
	put := func(at string) error {
		f, err := ParseKongPlanForecast(strings.NewReader(fmt.Sprintf(kongPlanForecastBody, at)))
		require.NoError(t, err)
		_, err = s.PutForecast(ctx, f)
		return err
	}
	require.NoError(t, put("2026-10-02T14:00:00+08:00"))
	requireKongPlanErr(t, put("2026-10-02T13:59:00+08:00"), 409, KongPlanReasonForecastStale)
	require.NoError(t, put("2026-10-02T06:00:00Z"), "同一时刻照常接受")

	_, err := ParseKongPlanForecast(strings.NewReader(`{"cycle_id": "c1", "plan_version": 5, "computed_at": "2026-10-02T06:00:00Z",
		"interval_min": 60, "per_session_pp_per_hour": 1, "avg_session_pp": 1, "demand_estimate_pp_per_hour": 1,
		"accounts": [], "tight": true}`))
	appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	require.Equal(t, "tight", appErr.Metadata["field"], "池子紧张不进慢速部分")
}

func TestKongPlanStore_Caller(t *testing.T) {
	ctx := context.Background()
	s, clock := newKongPlanTestStore(t, newKongPlanFakeRepo())
	v, err := s.GetCaller(ctx)
	require.NoError(t, err)
	require.Equal(t, &KongPoolCallerView{State: KongPoolCallerNormal}, v)

	req, err := ParseKongPoolCaller(strings.NewReader(`{"state": "throttled", "desired_pp_per_hour": 5, "ttl_h": 2}`))
	require.NoError(t, err)
	keyID := int64(42)
	_, err = s.PutCaller(ctx, req, &keyID)
	require.NoError(t, err)
	v, err = s.GetCaller(ctx)
	require.NoError(t, err)
	require.Equal(t, KongPoolCallerThrottled, v.State)
	require.Equal(t, clock.t.Add(2*time.Hour), *v.Until)
	require.Equal(t, int64(42), *v.APIKeyID)

	clock.t = clock.t.Add(2 * time.Hour)
	v, err = s.GetCaller(ctx)
	require.NoError(t, err)
	require.Equal(t, KongPoolCallerNormal, v.State)
	require.True(t, v.Expired)

	_, err = ParseKongPoolCaller(strings.NewReader(`{"state": "throttled", "ttl_h": 25}`))
	requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
}

// 写入结果不确定（已提交、确认丢失）：返回错误，下一次读写先从库里重载，同一份发布的重试按成功处理。
func TestKongPlanStore_UncertainWriteReloads(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	s, clock := newKongPlanTestStore(t, repo)
	req := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 1, clock.t.Add(time.Hour), kongPlanFallback, kongPlanAccounts))

	repo.saveErr, repo.commitOnErr = errors.New("连接在提交后断开"), true
	_, err := s.PutDispatch(ctx, req)
	requireKongPlanErr(t, err, 500, "KONG_PLAN_WRITE_FAILED")
	repo.saveErr, repo.commitOnErr = nil, false

	pub, err := s.GetPublish(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), pub.LastSeq, "重载后看到已提交的发布")
	res, err := s.PutDispatch(ctx, req)
	require.NoError(t, err)
	require.True(t, res.Idempotent)

	// 没提交的失败：重载后仍是旧状态，重试被正常接受。
	req2 := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 2, clock.t.Add(time.Hour), kongPlanFallback, `[]`))
	repo.saveErr = errors.New("写入前断开")
	_, err = s.PutDispatch(ctx, req2)
	require.Error(t, err)
	repo.saveErr = nil
	res, err = s.PutDispatch(ctx, req2)
	require.NoError(t, err)
	require.False(t, res.Idempotent)
}

func TestKongPlanStore_LoadFailure(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	repo.loadErr = errors.New("库不可用")
	s, _ := newKongPlanTestStore(t, repo)
	_, err := s.GetPublish(ctx)
	requireKongPlanErr(t, err, 503, KongPlanReasonUnavailable)
	require.Nil(t, s.state.Load(), "加载成功之前选号按现状调度")

	repo.loadErr = nil
	_, err = s.GetPublish(ctx)
	require.NoError(t, err, "之后的读写会再试")

	// 内容坏了不能退回缺省值：缺省的代次会让旧的写入重新被接受。
	bad := newKongPlanFakeRepo()
	bad.rows[kongPlanKindPublish] = KongPlanRow{Kind: kongPlanKindPublish, Content: []byte(`{"generation": "x"}`)}
	s2, _ := newKongPlanTestStore(t, bad)
	_, err = s2.GetPublish(ctx)
	requireKongPlanErr(t, err, 503, KongPlanReasonUnavailable)
}

// 重启：新的实例从同一份存储读回，所有读接口的结果与重启前相同。
func TestKongPlanStore_ReloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	s, clock := newKongPlanTestStore(t, repo)
	_, err := s.PutDispatch(ctx, mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 4, clock.t.Add(time.Hour), kongPlanFallback, kongPlanAccounts)))
	require.NoError(t, err)
	ctl, err := ParseKongPlanControl(strings.NewReader(`{"revision": 2, "credits_mode": "on", "allow": [1], "floor": 10,
		"per_point": 2.5, "overflow_margin": {"default": 1}, "file_hold": {"since": "2026-10-02T05:00:00Z"}}`))
	require.NoError(t, err)
	_, err = s.PutControl(ctx, ctl)
	require.NoError(t, err)
	hold, err := ParseKongPlanHold(strings.NewReader(`{"trigger": "manual:a", "reason": "manual", "note": "止损"}`))
	require.NoError(t, err)
	_, err = s.Hold(ctx, hold)
	require.NoError(t, err)
	f, err := ParseKongPlanForecast(strings.NewReader(fmt.Sprintf(kongPlanForecastBody, "2026-10-02T14:00:00+08:00")))
	require.NoError(t, err)
	_, err = s.PutForecast(ctx, f)
	require.NoError(t, err)
	caller, err := ParseKongPoolCaller(strings.NewReader(`{"state": "paused"}`))
	require.NoError(t, err)
	_, err = s.PutCaller(ctx, caller, nil)
	require.NoError(t, err)

	views := func(s *KongPlanStore) string {
		d, err := s.GetDispatch(ctx)
		require.NoError(t, err)
		p, err := s.GetPublish(ctx)
		require.NoError(t, err)
		c, err := s.GetControl(ctx)
		require.NoError(t, err)
		cl, err := s.GetCaller(ctx)
		require.NoError(t, err)
		b, err := json.Marshal([]any{d, p, c, cl, s.state.Load().forecast})
		require.NoError(t, err)
		return string(b)
	}
	before := views(s)
	s2, clock2 := newKongPlanTestStore(t, repo)
	clock2.t = clock.t
	require.Equal(t, before, views(s2))
}

// 并发写入：内存状态不会被较早的写入覆盖回去，且与库里最后一次写入一致。
func TestKongPlanStore_ConcurrentWritesMonotonic(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	s, _ := newKongPlanTestStore(t, repo)
	const n = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 1; i <= n; i++ {
		req, err := ParseKongPlanControl(strings.NewReader(fmt.Sprintf(
			`{"revision": %d, "credits_mode": "on", "allow": [], "floor": %d, "overflow_margin": {"default": 0}}`, i, i)))
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = s.PutControl(ctx, req)
		}()
	}
	close(start)
	wg.Wait()
	v, err := s.GetControl(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(n), v.Revision)
	var stored KongPlanControl
	require.NoError(t, json.Unmarshal(repo.rows[kongPlanKindControl].Content, &stored))
	require.Equal(t, int64(n), stored.Revision)
	require.Equal(t, float64(n), stored.Floor)
}

func TestParseKongPlan_StrictBodiesAndCanonicalValues(t *testing.T) {
	control := `{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": {"default": 1}}`
	_, err := ParseKongPlanControl(strings.NewReader(control + " \n\t"))
	require.NoError(t, err, "JSON 之后只有空白可以")
	for _, tail := range []string{"]", "}", `{"a": 1}`, "x", "1"} {
		_, err := ParseKongPlanControl(strings.NewReader(control + tail))
		appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
		require.Equal(t, "body", appErr.Metadata["field"], tail)
	}

	// 换成 UTC 后超出可表示的年份：拒收，不在算指纹时出错。
	for _, at := range []string{"9999-12-31T23:59:59-01:00", "0001-01-01T00:30:00+01:00"} {
		_, err := ParseKongPlanControl(strings.NewReader(`{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0,
			"overflow_margin": {"default": 1}, "file_hold": {"since": "` + at + `"}}`))
		appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
		require.Equal(t, "file_hold.since", appErr.Metadata["field"])
	}

	// cycle_id 按字符数限制。
	exp := `"snapshot": {"expires_at": "2026-10-02T08:00:00Z", "accounts": []}`
	body := func(cycle string) string {
		return `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "` + cycle + `", "fallback": [], ` + exp + `}`
	}
	_, err = ParseKongPlanDispatch(strings.NewReader(body(strings.Repeat("周", 128))))
	require.NoError(t, err)
	_, err = ParseKongPlanDispatch(strings.NewReader(body(strings.Repeat("周", 129))))
	requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)

	// -0 与 0 是同一个值，同一个指纹。
	a, err := ParseKongPlanControl(strings.NewReader(control))
	require.NoError(t, err)
	b, err := ParseKongPlanControl(strings.NewReader(strings.Replace(control, `"floor": 0`, `"floor": -0.0`, 1)))
	require.NoError(t, err)
	require.Equal(t, a.SHA256, b.SHA256)
	asap := func(v string) string {
		return `{"generation": 1, "seq": 1, "plan_version": 1, "cycle_id": "c", "fallback": [],
			"snapshot": {"expires_at": "2026-10-02T08:00:00Z", "accounts": [{"id": 1, "tier": "asap", "admit_below": {"6h": ` + v + `, "24h": 1}}]}}`
	}
	d1, err := ParseKongPlanDispatch(strings.NewReader(asap("0")))
	require.NoError(t, err)
	d2, err := ParseKongPlanDispatch(strings.NewReader(asap("-0")))
	require.NoError(t, err)
	require.Equal(t, d1.SHA256, d2.SHA256)

	// 宽限里的 null 不能当作 0。
	for _, m := range []string{`{"default": null}`, `{"default": 1, "3": null}`} {
		_, err := ParseKongPlanControl(strings.NewReader(`{"revision": 1, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": ` + m + `}`))
		requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	}

	// 空数组保持为空数组。
	require.NotNil(t, a.Allow)
	f, err := ParseKongPlanForecast(strings.NewReader(`{"cycle_id": "c", "plan_version": 1, "computed_at": "2026-10-02T06:00:00Z",
		"interval_min": 60, "per_session_pp_per_hour": 1, "avg_session_pp": 1, "demand_estimate_pp_per_hour": 1, "accounts": []}`))
	require.NoError(t, err)
	require.NotNil(t, f.Accounts)
	s, _ := newKongPlanTestStore(t, newKongPlanFakeRepo())
	v, err := s.PutControl(context.Background(), a)
	require.NoError(t, err)
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"allow":[]`)
}

// 已提交、确认丢失之后不经任何读取直接重试：先重载，按幂等成功，接受时刻仍是第一次的。
func TestKongPlanStore_UncertainCommitDirectRetry(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	s, clock := newKongPlanTestStore(t, repo)
	req := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, 1, clock.t.Add(time.Hour), kongPlanFallback, kongPlanAccounts))
	first := clock.t
	repo.saveErr, repo.commitOnErr = errors.New("确认丢失"), true
	_, err := s.PutDispatch(ctx, req)
	require.Error(t, err)
	repo.saveErr, repo.commitOnErr = nil, false

	clock.t = clock.t.Add(time.Minute)
	res, err := s.PutDispatch(ctx, req)
	require.NoError(t, err)
	require.True(t, res.Idempotent)
	require.Equal(t, first, res.AcceptedAt)

	// 新修订号已提交而确认丢失：迟到的旧修订号直接到达，照样被拒。
	ctl := func(rev int64) *KongPlanControlRequest {
		r, err := ParseKongPlanControl(strings.NewReader(fmt.Sprintf(
			`{"revision": %d, "credits_mode": "on", "allow": [], "floor": 0, "overflow_margin": {"default": 0}}`, rev)))
		require.NoError(t, err)
		return r
	}
	repo.saveErr, repo.commitOnErr = errors.New("确认丢失"), true
	_, err = s.PutControl(ctx, ctl(5))
	require.Error(t, err)
	repo.saveErr, repo.commitOnErr = nil, false
	_, err = s.PutControl(ctx, ctl(4))
	requireKongPlanErr(t, err, 409, KongPlanReasonRevisionStale)
}

// 发布与人工约束交错并发写入：两份状态都保留最新的一次，内存、库与重建的实例一致。
func TestKongPlanStore_ConcurrentMixedWrites(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	s, clock := newKongPlanTestStore(t, repo)
	const n = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 1; i <= n; i++ {
		d := mustParseKongPlanDispatch(t, kongPlanDispatchJSONBody(1, int64(i), clock.t.Add(time.Hour), kongPlanFallback, kongPlanAccounts))
		c, err := ParseKongPlanControl(strings.NewReader(fmt.Sprintf(
			`{"revision": %d, "credits_mode": "on", "allow": [%d], "floor": 0, "overflow_margin": {"default": 0}}`, i, i)))
		require.NoError(t, err)
		h, err := ParseKongPlanHold(strings.NewReader(fmt.Sprintf(`{"trigger": "manual:%d", "reason": "manual"}`, i)))
		require.NoError(t, err)
		wg.Add(3)
		go func() { defer wg.Done(); <-start; _, _ = s.PutDispatch(ctx, d) }()
		go func() { defer wg.Done(); <-start; _, _ = s.PutControl(ctx, c) }()
		go func() { defer wg.Done(); <-start; _, _ = s.Hold(ctx, h) }()
	}
	close(start)
	wg.Wait()

	pub, err := s.GetPublish(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(n), pub.LastSeq)
	ctlView, err := s.GetControl(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(n), ctlView.Revision)
	require.Equal(t, []int64{n}, ctlView.Allow)
	require.Len(t, ctlView.LatchedHolds, n, "人工约束的同步不会冲掉并发设置的锁存暂停")

	rebuilt, _ := newKongPlanTestStore(t, repo)
	pub2, err := rebuilt.GetPublish(ctx)
	require.NoError(t, err)
	ctl2, err := rebuilt.GetControl(ctx)
	require.NoError(t, err)
	require.Equal(t, pub.LastSeq, pub2.LastSeq)
	require.Equal(t, ctlView.Revision, ctl2.Revision)
	require.Len(t, ctl2.LatchedHolds, n)
}

// 启动时的加载有时限：仓储一直不返回时，Start 按时返回，之后的读写再试。
func TestKongPlanStore_StartIsBounded(t *testing.T) {
	repo := &kongPlanBlockingRepo{kongPlanFakeRepo: newKongPlanFakeRepo()}
	s := NewKongPlanStore(repo)
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		s.Start(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start 没有按时返回")
	}
	require.Nil(t, s.state.Load())
}

type kongPlanBlockingRepo struct{ *kongPlanFakeRepo }

func (r *kongPlanBlockingRepo) LoadKongPlan(ctx context.Context) ([]KongPlanRow, *KongPoolCaller, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}
