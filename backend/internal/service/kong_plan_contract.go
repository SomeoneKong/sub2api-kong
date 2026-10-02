package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 账号选择与 credits 的接口契约（fork 专有，设计见 DESIGN-openai-plan-dispatch.md）：
// 请求的解析、静态校验与内容指纹。状态检查与写入在 kong_plan_store.go。

const (
	KongPlanTierAsap    = "asap"
	KongPlanTierNormal  = "normal"
	KongPlanTierStandby = "standby"

	KongPlanCreditsModeShadow = "shadow"
	KongPlanCreditsModeOn     = "on"

	KongPlanInUseSnapshot = "snapshot"
	KongPlanInUseFallback = "fallback"
	KongPlanInUseLegacy   = "legacy"

	KongPlanHoldReasonDailyCap = "daily_cap"
	KongPlanHoldReasonManual   = "manual"

	KongPoolCallerNormal    = "normal"
	KongPoolCallerThrottled = "throttled"
	KongPoolCallerPaused    = "paused"

	kongPlanMaxLimit        = 64
	kongPlanMaxCreditsLevel = 9
	kongPlanMaxCycleID      = 128
	kongPlanMaxNote         = 200
	kongPlanMaxMargin       = 8
	kongPlanMaxReleaseItems = 100
	kongPlanSnapshotMaxTTL  = 6 * time.Hour
	kongPoolCallerMaxTTLH   = 24
	kongPoolCallerDefTTLH   = 6
)

// 错误码，即失败响应的 reason。
const (
	KongPlanReasonInvalid            = "KONG_PLAN_INVALID"
	KongPlanReasonPublishDisabled    = "KONG_PLAN_PUBLISH_DISABLED"
	KongPlanReasonGenerationMismatch = "KONG_PLAN_GENERATION_MISMATCH"
	KongPlanReasonSeqStale           = "KONG_PLAN_SEQ_STALE"
	KongPlanReasonSeqConflict        = "KONG_PLAN_SEQ_CONFLICT"
	KongPlanReasonSnapshotExpiry     = "KONG_PLAN_SNAPSHOT_EXPIRY_INVALID"
	KongPlanReasonCreditsNotOAuth    = "KONG_PLAN_CREDITS_NOT_OAUTH"
	KongPlanReasonRevisionStale      = "KONG_PLAN_REVISION_STALE"
	KongPlanReasonRevisionConflict   = "KONG_PLAN_REVISION_CONFLICT"
	KongPlanReasonForecastStale      = "KONG_PLAN_FORECAST_STALE"
	KongPlanReasonUnavailable        = "KONG_PLAN_UNAVAILABLE"
)

var kongPlanTriggerPattern = regexp.MustCompile(`^[A-Za-z0-9:._#+-]{1,64}$`)

// KongPlanAdmitBelow 是尽快档的准入线：本账号近 6h、近 24h 消耗（本账号百分点，含 credits 补记）的上限。
type KongPlanAdmitBelow struct {
	H6  float64 `json:"6h"`
	H24 float64 `json:"24h"`
}

// KongPlanCredits 是条目给的 credits 级别与作废时刻。
type KongPlanCredits struct {
	Level     int       `json:"level"`
	ExpiresAt time.Time `json:"expires_at"`
}

// KongPlanEntry 是兜底或快照里一个账号的条目。省略的 active / imminent 解析时补成 true / false，
// 所以同一份内容不论写法都得到同一个指纹。Paused 是计划暂停（只在快照里）：两轮排序把账号排进计划暂停段，
// 别的候选都接不住时才用它；为假时不写出，没有暂停的内容指纹与不认这个字段的版本相同。Grace 是第一层的会话宽限（只在
// 快照里）：给了就与人工约束里的宽限取小，只能更严；热账号限速时执行端写 0，没给时不写出，指纹同样不变。
type KongPlanEntry struct {
	ID             int64               `json:"id"`
	Tier           string              `json:"tier"`
	Active         bool                `json:"active"`
	Imminent       bool                `json:"imminent"`
	AdmitBelow     *KongPlanAdmitBelow `json:"admit_below"`
	MaxSessions    *int                `json:"max_sessions"`
	MaxConcurrency *int                `json:"max_concurrency"`
	Credits        *KongPlanCredits    `json:"credits"`
	Paused         bool                `json:"paused,omitempty"`
	Grace          *int                `json:"grace,omitempty"`
}

// KongPlanSnapshot 是快照：在兜底之上加入边车的实时决定，带有效期。
type KongPlanSnapshot struct {
	ExpiresAt time.Time       `json:"expires_at"`
	Accounts  []KongPlanEntry `json:"accounts"`
}

// KongPlanDispatchRequest 是解析、校验过的一次发布。条目按账号编号升序，时刻都是 UTC。
type KongPlanDispatchRequest struct {
	Generation  int64
	Seq         int64
	PlanVersion int64
	CycleID     string
	Fallback    []KongPlanEntry
	Snapshot    KongPlanSnapshot
	SHA256      string
}

// KongPlanControlRequest 是边车同步的人工约束（安全线派生的部分与文件暂停）。
type KongPlanControlRequest struct {
	Revision       int64
	CreditsMode    string
	Allow          []int64
	Floor          float64
	PerPoint       *float64
	OverflowMargin map[string]int
	FileHold       *KongPlanFileHold
	SHA256         string
}

// KongPlanFileHold 是暂停文件对应的暂停。
type KongPlanFileHold struct {
	Since time.Time `json:"since"`
}

// KongPlanPublishRequest 是人停用、恢复或清空发布的请求。
type KongPlanPublishRequest struct {
	Action string
	Clear  string
	Note   string
}

// KongPlanHoldRequest 设置一个锁存暂停。
type KongPlanHoldRequest struct {
	Trigger string
	Reason  string
	Note    string
}

// KongPlanReleaseRequest 按编号或按原因解除锁存暂停，二者取一。
type KongPlanReleaseRequest struct {
	Triggers []string
	Reason   string
	Note     string
}

// kongPlanDefaultUnitMultiple 是没给单位倍数时的取值：点是 x20 账号周额度的 1%。
const kongPlanDefaultUnitMultiple = 20.0

// KongPlanForecast 是容量视图的慢速部分。forecast、runway_h、credits_runway_h 原样转给调用方。
type KongPlanForecast struct {
	CycleID                 string    `json:"cycle_id"`
	PlanVersion             int64     `json:"plan_version"`
	ComputedAt              time.Time `json:"computed_at"`
	IntervalMin             int       `json:"interval_min"`
	PerSessionPPPerHour     float64   `json:"per_session_pp_per_hour"`
	AvgSessionPP            float64   `json:"avg_session_pp"`
	DemandEstimatePPPerHour float64   `json:"demand_estimate_pp_per_hour"`
	// UnitMultiple 是内部单位的倍数：点是 x(UnitMultiple) 账号周额度的 1%，K = 账号倍数 ÷ UnitMultiple。
	UnitMultiple   float64                   `json:"unit_multiple"`
	Accounts       []KongPlanForecastAccount `json:"accounts"`
	Forecast       json.RawMessage           `json:"forecast"`
	RunwayH        json.RawMessage           `json:"runway_h"`
	CreditsRunwayH json.RawMessage           `json:"credits_runway_h"`
	NextResetAt    *time.Time                `json:"next_reset_at"`
	ReceivedAt     time.Time                 `json:"received_at"`
}

// unitMultiple 返回单位倍数；更早保存、没有这一项的慢速部分按 20。
func (f *KongPlanForecast) unitMultiple() float64 {
	if f.UnitMultiple > 0 {
		return f.UnitMultiple
	}
	return kongPlanDefaultUnitMultiple
}

// KongPlanForecastAccount 是账号参数：套餐系数 K（一份周额度合多少个 100 点）、它的时间线与深度线速度。
type KongPlanForecastAccount struct {
	ID             int64              `json:"id"`
	K              float64            `json:"k"`
	KTimeline      []KongPlanKSegment `json:"k_timeline"`
	DepthPPPerHour float64            `json:"depth_pp_per_hour"`
}

// KongPlanKSegment 是套餐系数时间线的一段：从 From 起系数为 K，直到下一段的起点。
type KongPlanKSegment struct {
	From time.Time `json:"from"`
	K    float64   `json:"k"`
}

// kAt 返回账号在 at 时刻的套餐系数：时间线里最后一个起点不晚于 at 的分段；没有这样的分段（时间线为空或全在
// 未来）时用 K。时间线已按起点严格升序校验过。
func (a KongPlanForecastAccount) kAt(at time.Time) float64 {
	k := a.K
	for _, s := range a.KTimeline {
		if s.From.After(at) {
			break
		}
		k = s.K
	}
	return k
}

// KongPoolCallerRequest 是调用方声明。
type KongPoolCallerRequest struct {
	State            string
	DesiredPPPerHour *float64
	TTLHours         int
	Note             string
}

func kongPlanInvalid(field, message string) error {
	return infraerrors.BadRequest(KongPlanReasonInvalid, message).WithMetadata(map[string]string{"field": field})
}

// kongPlanDecode 严格解析请求体：未知字段、尾随内容都算不合法，契约字段拼错能立即暴露。
func kongPlanDecode(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return kongPlanInvalid(kongPlanDecodeField(err), "请求体不合法："+err.Error())
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return kongPlanInvalid("body", "请求体在 JSON 之后还有内容")
	}
	return nil
}

func kongPlanDecodeField(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return typeErr.Field
	}
	const unknown = `json: unknown field "`
	if msg := err.Error(); strings.HasPrefix(msg, unknown) {
		return strings.TrimSuffix(strings.TrimPrefix(msg, unknown), `"`)
	}
	return "body"
}

func kongPlanParseTime(raw *string, field string) (time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return time.Time{}, kongPlanInvalid(field, field+" 必填")
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*raw))
	if err != nil {
		return time.Time{}, kongPlanInvalid(field, field+" 不是带时区的 RFC 3339 时刻")
	}
	if t = t.UTC(); t.Year() < 1 || t.Year() > 9999 {
		return time.Time{}, kongPlanInvalid(field, field+" 超出可表示的年份")
	}
	return t, nil
}

// kongPlanCycleIDOK 按字符数限制 cycle_id 的长度。
func kongPlanCycleIDOK(v *string) bool {
	return v != nil && strings.TrimSpace(*v) != "" && len([]rune(*v)) <= kongPlanMaxCycleID
}

// kongPlanZero 把 -0 统一成 0：等值的内容要得到同一个指纹。
func kongPlanZero(v float64) float64 {
	if v == 0 {
		return 0
	}
	return v
}

func kongPlanCheckNote(note *string, field string, required bool) (string, error) {
	n := ""
	if note != nil {
		n = strings.TrimSpace(*note)
	}
	if required && n == "" {
		return "", kongPlanInvalid(field, field+" 必填")
	}
	if len([]rune(n)) > kongPlanMaxNote {
		return "", kongPlanInvalid(field, fmt.Sprintf("%s 不超过 %d 个字符", field, kongPlanMaxNote))
	}
	return n, nil
}

func kongPlanSHA256(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// 只序列化本文件里的普通结构，失败说明代码有误。
		panic(fmt.Sprintf("kong plan: 序列化内容指纹失败: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type kongPlanAdmitBelowJSON struct {
	H6  *float64 `json:"6h"`
	H24 *float64 `json:"24h"`
}

type kongPlanCreditsJSON struct {
	Level     *int    `json:"level"`
	ExpiresAt *string `json:"expires_at"`
}

type kongPlanEntryJSON struct {
	ID             *int64                  `json:"id"`
	Tier           *string                 `json:"tier"`
	Active         *bool                   `json:"active"`
	Imminent       *bool                   `json:"imminent"`
	AdmitBelow     *kongPlanAdmitBelowJSON `json:"admit_below"`
	MaxSessions    *int                    `json:"max_sessions"`
	MaxConcurrency *int                    `json:"max_concurrency"`
	Credits        *kongPlanCreditsJSON    `json:"credits"`
	Paused         *bool                   `json:"paused"`
	Grace          *int                    `json:"grace"`
}

type kongPlanDispatchJSON struct {
	Generation  *int64               `json:"generation"`
	Seq         *int64               `json:"seq"`
	PlanVersion *int64               `json:"plan_version"`
	CycleID     *string              `json:"cycle_id"`
	Fallback    *[]kongPlanEntryJSON `json:"fallback"`
	Snapshot    *struct {
		ExpiresAt *string              `json:"expires_at"`
		Accounts  *[]kongPlanEntryJSON `json:"accounts"`
	} `json:"snapshot"`
}

// ParseKongPlanDispatch 解析并静态校验一次发布（不看任何状态）。
func ParseKongPlanDispatch(body io.Reader) (*KongPlanDispatchRequest, error) {
	var raw kongPlanDispatchJSON
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	if raw.Generation == nil || *raw.Generation < 1 {
		return nil, kongPlanInvalid("generation", "generation 必须是正整数")
	}
	if raw.Seq == nil || *raw.Seq < 1 {
		return nil, kongPlanInvalid("seq", "seq 必须是正整数")
	}
	if raw.PlanVersion == nil || *raw.PlanVersion < 0 {
		return nil, kongPlanInvalid("plan_version", "plan_version 必须是非负整数")
	}
	if !kongPlanCycleIDOK(raw.CycleID) {
		return nil, kongPlanInvalid("cycle_id", fmt.Sprintf("cycle_id 必填，不超过 %d 个字符", kongPlanMaxCycleID))
	}
	if raw.Fallback == nil {
		return nil, kongPlanInvalid("fallback", "fallback 必填（可以是空数组）")
	}
	if raw.Snapshot == nil || raw.Snapshot.Accounts == nil {
		return nil, kongPlanInvalid("snapshot", "snapshot 与 snapshot.accounts 必填")
	}
	expiresAt, err := kongPlanParseTime(raw.Snapshot.ExpiresAt, "snapshot.expires_at")
	if err != nil {
		return nil, err
	}
	fallback, err := kongPlanParseEntries(*raw.Fallback, "fallback", true)
	if err != nil {
		return nil, err
	}
	accounts, err := kongPlanParseEntries(*raw.Snapshot.Accounts, "snapshot.accounts", false)
	if err != nil {
		return nil, err
	}
	req := &KongPlanDispatchRequest{
		Generation:  *raw.Generation,
		Seq:         *raw.Seq,
		PlanVersion: *raw.PlanVersion,
		CycleID:     *raw.CycleID,
		Fallback:    fallback,
		Snapshot:    KongPlanSnapshot{ExpiresAt: expiresAt, Accounts: accounts},
	}
	req.SHA256 = kongPlanSHA256(struct {
		Generation  int64            `json:"generation"`
		Seq         int64            `json:"seq"`
		PlanVersion int64            `json:"plan_version"`
		CycleID     string           `json:"cycle_id"`
		Fallback    []KongPlanEntry  `json:"fallback"`
		Snapshot    KongPlanSnapshot `json:"snapshot"`
	}{req.Generation, req.Seq, req.PlanVersion, req.CycleID, req.Fallback, req.Snapshot})
	return req, nil
}

func kongPlanParseLimit(v *int, field string) (*int, error) {
	if v == nil {
		return nil, nil
	}
	if *v < 1 || *v > kongPlanMaxLimit {
		return nil, kongPlanInvalid(field, fmt.Sprintf("%s 必须为 null 或 1..%d", field, kongPlanMaxLimit))
	}
	n := *v
	return &n, nil
}

// kongPlanParseEntries 校验条目并按账号编号升序排列。兜底条目只能是普通或备用档，
// 不带准入线与临期，active 一律为 true。
func kongPlanParseEntries(raw []kongPlanEntryJSON, field string, fallback bool) ([]KongPlanEntry, error) {
	out := make([]KongPlanEntry, 0, len(raw))
	seen := make(map[int64]bool, len(raw))
	for i, r := range raw {
		f := fmt.Sprintf("%s[%d]", field, i)
		if r.ID == nil || *r.ID < 1 {
			return nil, kongPlanInvalid(f+".id", "id 必须是正整数")
		}
		if seen[*r.ID] {
			return nil, kongPlanInvalid(f+".id", fmt.Sprintf("账号 %d 在同一列表里重复", *r.ID))
		}
		seen[*r.ID] = true
		if r.Tier == nil {
			return nil, kongPlanInvalid(f+".tier", "tier 必填")
		}
		e := KongPlanEntry{ID: *r.ID, Tier: *r.Tier, Active: true}
		switch e.Tier {
		case KongPlanTierAsap:
			if fallback {
				return nil, kongPlanInvalid(f+".tier", "兜底里不能有 asap")
			}
		case KongPlanTierNormal, KongPlanTierStandby:
		default:
			return nil, kongPlanInvalid(f+".tier", "tier 只能是 asap / normal / standby")
		}
		if r.Active != nil {
			e.Active = *r.Active
		}
		if r.Imminent != nil {
			e.Imminent = *r.Imminent
		}
		if r.Paused != nil {
			e.Paused = *r.Paused
		}
		if fallback && (!e.Active || e.Imminent || e.Paused) {
			return nil, kongPlanInvalid(f, "兜底条目的 active 必须为 true、imminent 与 paused 必须为 false")
		}
		if r.Grace != nil {
			if fallback {
				return nil, kongPlanInvalid(f+".grace", "兜底条目不能带 grace")
			}
			if *r.Grace < 0 || *r.Grace > kongPlanMaxMargin {
				return nil, kongPlanInvalid(f+".grace", fmt.Sprintf("grace 的取值为 0..%d", kongPlanMaxMargin))
			}
			g := *r.Grace
			e.Grace = &g
		}
		if e.Tier == KongPlanTierAsap {
			if r.AdmitBelow == nil || r.AdmitBelow.H6 == nil || r.AdmitBelow.H24 == nil {
				return nil, kongPlanInvalid(f+".admit_below", "asap 必须带 admit_below 的 6h 与 24h")
			}
			if *r.AdmitBelow.H6 < 0 || *r.AdmitBelow.H24 < 0 {
				return nil, kongPlanInvalid(f+".admit_below", "admit_below 不能为负")
			}
			e.AdmitBelow = &KongPlanAdmitBelow{H6: kongPlanZero(*r.AdmitBelow.H6), H24: kongPlanZero(*r.AdmitBelow.H24)}
		} else if r.AdmitBelow != nil {
			return nil, kongPlanInvalid(f+".admit_below", "只有 asap 带 admit_below")
		}
		var err error
		if e.MaxSessions, err = kongPlanParseLimit(r.MaxSessions, f+".max_sessions"); err != nil {
			return nil, err
		}
		if e.MaxConcurrency, err = kongPlanParseLimit(r.MaxConcurrency, f+".max_concurrency"); err != nil {
			return nil, err
		}
		if r.Credits != nil {
			if r.Credits.Level == nil || *r.Credits.Level < 1 || *r.Credits.Level > kongPlanMaxCreditsLevel {
				return nil, kongPlanInvalid(f+".credits.level", fmt.Sprintf("credits.level 必须是 1..%d", kongPlanMaxCreditsLevel))
			}
			at, err := kongPlanParseTime(r.Credits.ExpiresAt, f+".credits.expires_at")
			if err != nil {
				return nil, err
			}
			e.Credits = &KongPlanCredits{Level: *r.Credits.Level, ExpiresAt: at}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type kongPlanControlJSON struct {
	Revision       *int64           `json:"revision"`
	CreditsMode    *string          `json:"credits_mode"`
	Allow          *[]int64         `json:"allow"`
	Floor          *float64         `json:"floor"`
	PerPoint       *float64         `json:"per_point"`
	OverflowMargin *map[string]*int `json:"overflow_margin"`
	FileHold       *struct {
		Since *string `json:"since"`
	} `json:"file_hold"`
}

// ParseKongPlanControl 解析并校验人工约束的同步请求。
func ParseKongPlanControl(body io.Reader) (*KongPlanControlRequest, error) {
	var raw kongPlanControlJSON
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	if raw.Revision == nil || *raw.Revision < 1 {
		return nil, kongPlanInvalid("revision", "revision 必须是正整数")
	}
	if raw.CreditsMode == nil || (*raw.CreditsMode != KongPlanCreditsModeShadow && *raw.CreditsMode != KongPlanCreditsModeOn) {
		return nil, kongPlanInvalid("credits_mode", "credits_mode 只能是 shadow / on")
	}
	if raw.Allow == nil {
		return nil, kongPlanInvalid("allow", "allow 必填（可以是空数组）")
	}
	allow := append(make([]int64, 0, len(*raw.Allow)), (*raw.Allow)...)
	sort.Slice(allow, func(i, j int) bool { return allow[i] < allow[j] })
	for i, id := range allow {
		if id < 1 {
			return nil, kongPlanInvalid("allow", "allow 里的账号编号必须是正整数")
		}
		if i > 0 && allow[i-1] == id {
			return nil, kongPlanInvalid("allow", fmt.Sprintf("allow 里账号 %d 重复", id))
		}
	}
	if raw.Floor == nil || *raw.Floor < 0 {
		return nil, kongPlanInvalid("floor", "floor 必填且不能为负")
	}
	if raw.PerPoint != nil && *raw.PerPoint <= 0 {
		return nil, kongPlanInvalid("per_point", "per_point 必须为 null 或正数")
	}
	if raw.OverflowMargin == nil {
		return nil, kongPlanInvalid("overflow_margin", "overflow_margin 必填")
	}
	margin := make(map[string]int, len(*raw.OverflowMargin))
	for k, v := range *raw.OverflowMargin {
		if k != "default" {
			if id, err := strconv.ParseInt(k, 10, 64); err != nil || id < 1 || strconv.FormatInt(id, 10) != k {
				return nil, kongPlanInvalid("overflow_margin", "overflow_margin 的键只能是 default 或账号编号")
			}
		}
		if v == nil || *v < 0 || *v > kongPlanMaxMargin {
			return nil, kongPlanInvalid("overflow_margin", fmt.Sprintf("overflow_margin 的取值为 0..%d", kongPlanMaxMargin))
		}
		margin[k] = *v
	}
	if _, ok := margin["default"]; !ok {
		return nil, kongPlanInvalid("overflow_margin", "overflow_margin 必须带 default")
	}
	req := &KongPlanControlRequest{
		Revision:       *raw.Revision,
		CreditsMode:    *raw.CreditsMode,
		Allow:          allow,
		Floor:          kongPlanZero(*raw.Floor),
		OverflowMargin: margin,
	}
	if raw.PerPoint != nil {
		v := *raw.PerPoint
		req.PerPoint = &v
	}
	if raw.FileHold != nil {
		since, err := kongPlanParseTime(raw.FileHold.Since, "file_hold.since")
		if err != nil {
			return nil, err
		}
		req.FileHold = &KongPlanFileHold{Since: since}
	}
	req.SHA256 = kongPlanSHA256(struct {
		Revision       int64             `json:"revision"`
		CreditsMode    string            `json:"credits_mode"`
		Allow          []int64           `json:"allow"`
		Floor          float64           `json:"floor"`
		PerPoint       *float64          `json:"per_point"`
		OverflowMargin map[string]int    `json:"overflow_margin"`
		FileHold       *KongPlanFileHold `json:"file_hold"`
	}{req.Revision, req.CreditsMode, req.Allow, req.Floor, req.PerPoint, req.OverflowMargin, req.FileHold})
	return req, nil
}

// ParseKongPlanPublish 解析人停用、恢复或清空发布的请求。
func ParseKongPlanPublish(body io.Reader) (*KongPlanPublishRequest, error) {
	var raw struct {
		Action *string `json:"action"`
		Clear  *string `json:"clear"`
		Note   *string `json:"note"`
	}
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	req := &KongPlanPublishRequest{Clear: "none"}
	if raw.Action == nil || (*raw.Action != "disable" && *raw.Action != "enable") {
		return nil, kongPlanInvalid("action", "action 只能是 disable / enable")
	}
	req.Action = *raw.Action
	if raw.Clear != nil {
		switch *raw.Clear {
		case "none", "snapshot", "all":
			req.Clear = *raw.Clear
		default:
			return nil, kongPlanInvalid("clear", "clear 只能是 none / snapshot / all")
		}
		if req.Action == "enable" {
			return nil, kongPlanInvalid("clear", "clear 只能与 disable 一起用")
		}
	}
	note, err := kongPlanCheckNote(raw.Note, "note", true)
	if err != nil {
		return nil, err
	}
	req.Note = note
	return req, nil
}

// ParseKongPlanHold 解析设置锁存暂停的请求。
func ParseKongPlanHold(body io.Reader) (*KongPlanHoldRequest, error) {
	var raw struct {
		Trigger *string `json:"trigger"`
		Reason  *string `json:"reason"`
		Note    *string `json:"note"`
	}
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	if raw.Trigger == nil || !kongPlanTriggerPattern.MatchString(*raw.Trigger) {
		return nil, kongPlanInvalid("trigger", "trigger 为 1–64 个字符，限 [A-Za-z0-9:._#+-]")
	}
	if raw.Reason == nil || (*raw.Reason != KongPlanHoldReasonDailyCap && *raw.Reason != KongPlanHoldReasonManual) {
		return nil, kongPlanInvalid("reason", "reason 只能是 daily_cap / manual")
	}
	note, err := kongPlanCheckNote(raw.Note, "note", false)
	if err != nil {
		return nil, err
	}
	return &KongPlanHoldRequest{Trigger: *raw.Trigger, Reason: *raw.Reason, Note: note}, nil
}

// ParseKongPlanRelease 解析解除锁存暂停的请求：按编号或按原因，二者取一。
func ParseKongPlanRelease(body io.Reader) (*KongPlanReleaseRequest, error) {
	var raw struct {
		Triggers *[]string `json:"triggers"`
		Reason   *string   `json:"reason"`
		Note     *string   `json:"note"`
	}
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	if (raw.Triggers == nil) == (raw.Reason == nil) {
		return nil, kongPlanInvalid("triggers", "triggers 与 reason 二者取一")
	}
	req := &KongPlanReleaseRequest{}
	if raw.Triggers != nil {
		if len(*raw.Triggers) == 0 || len(*raw.Triggers) > kongPlanMaxReleaseItems {
			return nil, kongPlanInvalid("triggers", fmt.Sprintf("triggers 为 1..%d 个编号", kongPlanMaxReleaseItems))
		}
		seen := make(map[string]bool, len(*raw.Triggers))
		for _, t := range *raw.Triggers {
			if !kongPlanTriggerPattern.MatchString(t) {
				return nil, kongPlanInvalid("triggers", "trigger 为 1–64 个字符，限 [A-Za-z0-9:._#+-]")
			}
			if !seen[t] {
				seen[t] = true
				req.Triggers = append(req.Triggers, t)
			}
		}
	} else {
		if *raw.Reason != KongPlanHoldReasonDailyCap && *raw.Reason != KongPlanHoldReasonManual {
			return nil, kongPlanInvalid("reason", "reason 只能是 daily_cap / manual")
		}
		req.Reason = *raw.Reason
	}
	note, err := kongPlanCheckNote(raw.Note, "note", true)
	if err != nil {
		return nil, err
	}
	req.Note = note
	return req, nil
}

// ParseKongPlanForecast 解析容量视图的慢速部分。
func ParseKongPlanForecast(body io.Reader) (*KongPlanForecast, error) {
	var raw struct {
		CycleID                 *string                    `json:"cycle_id"`
		PlanVersion             *int64                     `json:"plan_version"`
		ComputedAt              *string                    `json:"computed_at"`
		IntervalMin             *int                       `json:"interval_min"`
		PerSessionPPPerHour     *float64                   `json:"per_session_pp_per_hour"`
		AvgSessionPP            *float64                   `json:"avg_session_pp"`
		DemandEstimatePPPerHour *float64                   `json:"demand_estimate_pp_per_hour"`
		UnitMultiple            *float64                   `json:"unit_multiple"`
		Accounts                *[]KongPlanForecastAccount `json:"accounts"`
		Forecast                json.RawMessage            `json:"forecast"`
		RunwayH                 json.RawMessage            `json:"runway_h"`
		CreditsRunwayH          json.RawMessage            `json:"credits_runway_h"`
		NextResetAt             *string                    `json:"next_reset_at"`
	}
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	if !kongPlanCycleIDOK(raw.CycleID) {
		return nil, kongPlanInvalid("cycle_id", fmt.Sprintf("cycle_id 必填，不超过 %d 个字符", kongPlanMaxCycleID))
	}
	if raw.PlanVersion == nil || *raw.PlanVersion < 0 {
		return nil, kongPlanInvalid("plan_version", "plan_version 必须是非负整数")
	}
	computedAt, err := kongPlanParseTime(raw.ComputedAt, "computed_at")
	if err != nil {
		return nil, err
	}
	if raw.IntervalMin == nil || *raw.IntervalMin < 1 {
		return nil, kongPlanInvalid("interval_min", "interval_min 必须是正整数")
	}
	for field, v := range map[string]*float64{
		"per_session_pp_per_hour":     raw.PerSessionPPPerHour,
		"avg_session_pp":              raw.AvgSessionPP,
		"demand_estimate_pp_per_hour": raw.DemandEstimatePPPerHour,
	} {
		if v == nil || *v < 0 {
			return nil, kongPlanInvalid(field, field+" 必填且不能为负")
		}
	}
	unit := kongPlanDefaultUnitMultiple
	if raw.UnitMultiple != nil {
		if !(*raw.UnitMultiple > 0) {
			return nil, kongPlanInvalid("unit_multiple", "unit_multiple 必须为正数（不给时按 20）")
		}
		unit = *raw.UnitMultiple
	}
	if raw.Accounts == nil {
		return nil, kongPlanInvalid("accounts", "accounts 必填（可以是空数组）")
	}
	accounts := append(make([]KongPlanForecastAccount, 0, len(*raw.Accounts)), (*raw.Accounts)...)
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	for i, a := range accounts {
		if a.ID < 1 || a.K < 0 || a.DepthPPPerHour < 0 {
			return nil, kongPlanInvalid("accounts", "accounts 的 id 必须为正、k 与 depth_pp_per_hour 不能为负")
		}
		if i > 0 && accounts[i-1].ID == a.ID {
			return nil, kongPlanInvalid("accounts", fmt.Sprintf("accounts 里账号 %d 重复", a.ID))
		}
		timeline, err := kongPlanCheckKTimeline(a.KTimeline, a.ID)
		if err != nil {
			return nil, err
		}
		accounts[i].KTimeline = timeline
	}
	f := &KongPlanForecast{
		CycleID:                 *raw.CycleID,
		PlanVersion:             *raw.PlanVersion,
		ComputedAt:              computedAt,
		IntervalMin:             *raw.IntervalMin,
		PerSessionPPPerHour:     *raw.PerSessionPPPerHour,
		AvgSessionPP:            *raw.AvgSessionPP,
		DemandEstimatePPPerHour: *raw.DemandEstimatePPPerHour,
		UnitMultiple:            unit,
		Accounts:                accounts,
		Forecast:                kongPlanRawOrNull(raw.Forecast),
		RunwayH:                 kongPlanRawOrNull(raw.RunwayH),
		CreditsRunwayH:          kongPlanRawOrNull(raw.CreditsRunwayH),
	}
	if raw.NextResetAt != nil {
		at, err := kongPlanParseTime(raw.NextResetAt, "next_reset_at")
		if err != nil {
			return nil, err
		}
		f.NextResetAt = &at
	}
	return f, nil
}

// kongPlanCheckKTimeline 校验账号的套餐系数时间线并把起点统一成 UTC：起点必填、严格升序，系数为正。
func kongPlanCheckKTimeline(raw []KongPlanKSegment, id int64) ([]KongPlanKSegment, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]KongPlanKSegment, len(raw))
	for i, s := range raw {
		from := s.From.UTC()
		if s.From.IsZero() || from.Year() < 1 || from.Year() > 9999 {
			return nil, kongPlanInvalid("accounts", fmt.Sprintf("账号 %d 的 k_timeline[%d].from 必填且在可表示的年份内", id, i))
		}
		if !(s.K > 0) {
			return nil, kongPlanInvalid("accounts", fmt.Sprintf("账号 %d 的 k_timeline[%d].k 必须为正", id, i))
		}
		if i > 0 && !from.After(out[i-1].From) {
			return nil, kongPlanInvalid("accounts", fmt.Sprintf("账号 %d 的 k_timeline 起点必须严格升序", id))
		}
		out[i] = KongPlanKSegment{From: from, K: s.K}
	}
	return out, nil
}

func kongPlanRawOrNull(m json.RawMessage) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage("null")
	}
	return m
}

// ParseKongPoolCaller 解析调用方声明。
func ParseKongPoolCaller(body io.Reader) (*KongPoolCallerRequest, error) {
	var raw struct {
		State            *string  `json:"state"`
		DesiredPPPerHour *float64 `json:"desired_pp_per_hour"`
		TTLHours         *int     `json:"ttl_h"`
		Note             *string  `json:"note"`
	}
	if err := kongPlanDecode(body, &raw); err != nil {
		return nil, err
	}
	if raw.State == nil {
		return nil, kongPlanInvalid("state", "state 必填")
	}
	switch *raw.State {
	case KongPoolCallerNormal, KongPoolCallerThrottled, KongPoolCallerPaused:
	default:
		return nil, kongPlanInvalid("state", "state 只能是 normal / throttled / paused")
	}
	req := &KongPoolCallerRequest{State: *raw.State, TTLHours: kongPoolCallerDefTTLH}
	if raw.DesiredPPPerHour != nil {
		if *raw.DesiredPPPerHour < 0 {
			return nil, kongPlanInvalid("desired_pp_per_hour", "desired_pp_per_hour 不能为负")
		}
		v := *raw.DesiredPPPerHour
		req.DesiredPPPerHour = &v
	}
	if raw.TTLHours != nil {
		if *raw.TTLHours < 1 || *raw.TTLHours > kongPoolCallerMaxTTLH {
			return nil, kongPlanInvalid("ttl_h", fmt.Sprintf("ttl_h 为 1..%d", kongPoolCallerMaxTTLH))
		}
		req.TTLHours = *raw.TTLHours
	}
	note, err := kongPlanCheckNote(raw.Note, "note", false)
	if err != nil {
		return nil, err
	}
	req.Note = note
	return req, nil
}
