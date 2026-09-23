package service

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// openAIWSCodexRateLimitsEvent 是上游在原生 WS 上逐轮下发额度快照的带外事件名。
const openAIWSCodexRateLimitsEvent = "codex.rate_limits"

// parseOpenAIWSCodexRateLimitEvent 从 `codex.rate_limits` 事件体里取额度快照。
//
// 这是 ParseCodexRateLimitHeaders 的对偶：HTTP 侧额度在响应头（`x-codex-primary-*` /
// `x-codex-secondary-*`），而原生 WS 逐轮没有响应头，上游把同一份数据放进这个带外事件的
// `rate_limits.primary` / `.secondary`。
//
// **只做提取，不做归一化。** 5h/7d 的判定（Normalize 按 window_minutes 比长短）与落库映射
// （buildCodexUsageExtraUpdates）都沿用快照那一份：两侧各自归一化会对同一个账号算出不同的水位，
// 而那种不一致不会报错，只会让调度照着两个互相矛盾的数做决定。
//
// `x-codex-primary-over-secondary-limit-percent` 在事件里没有对应字段，留空——快照允许缺项。
func parseOpenAIWSCodexRateLimitEvent(payload []byte) *OpenAICodexUsageSnapshot {
	if len(payload) == 0 {
		return nil
	}
	limits := gjson.GetBytes(payload, "rate_limits")
	if !limits.IsObject() {
		return nil
	}

	snapshot := &OpenAICodexUsageSnapshot{}
	hasData := false

	// **取值域与 HTTP 侧逐字对齐：按原始文本走同一组 strconv 调用，不做额外的空白处理。**
	// 两侧都用 Atoi 不等于喂给 Atoi 的输入相同——`strings.TrimSpace` 会吃掉 NBSP 这类空白，而 HTTP
	// 响应头是原样交给 Atoi 的，于是同一份 `" 300 "` 在一条路上得出 300、另一条上缺失，
	// 最终把同一个水位归到不同的 5h/7d 字段。
	//
	// JSON Number 也按 `v.Raw` 的原始文本解析，不用 `v.Float()`：float64 已经丢掉十进制精度，
	// `360.00000000000001` 会变成 360、`9007199254740993` 会变成 ...992，事后再怎么检查整值或范围
	// 都恢复不了原值。走原始文本则顺带解决非有限值——`ParseFloat("1e400")` 自己就报越界，而
	// `json.Marshal` 不接受非有限浮点：一个坏字段会让 UpdateExtra 整份写入失败（连同同帧其它正常
	// 窗口），失败还被调用方静默吞掉、节流额度却已占用。
	rawText := func(v gjson.Result) (string, bool) {
		switch v.Type {
		case gjson.Number:
			return v.Raw, true
		case gjson.String:
			return v.String(), true
		default:
			return "", false
		}
	}
	percentOf := func(window gjson.Result, key string) *float64 {
		text, ok := rawText(window.Get(key))
		if !ok {
			return nil
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return &f
	}
	intOf := func(window gjson.Result, key string) *int {
		text, ok := rawText(window.Get(key))
		if !ok {
			return nil
		}
		i, err := strconv.Atoi(text)
		if err != nil {
			return nil
		}
		return &i
	}
	secondsOf := func(window gjson.Result, key string) *int {
		return saneCodexResetAfterSeconds(intOf(window, key))
	}

	if primary := limits.Get("primary"); primary.IsObject() {
		if v := percentOf(primary, "used_percent"); v != nil {
			snapshot.PrimaryUsedPercent = v
			hasData = true
		}
		if v := secondsOf(primary, "reset_after_seconds"); v != nil {
			snapshot.PrimaryResetAfterSeconds = v
			hasData = true
		}
		if v := intOf(primary, "window_minutes"); v != nil {
			snapshot.PrimaryWindowMinutes = v
			hasData = true
		}
	}
	if secondary := limits.Get("secondary"); secondary.IsObject() {
		if v := percentOf(secondary, "used_percent"); v != nil {
			snapshot.SecondaryUsedPercent = v
			hasData = true
		}
		if v := secondsOf(secondary, "reset_after_seconds"); v != nil {
			snapshot.SecondaryResetAfterSeconds = v
			hasData = true
		}
		if v := intOf(secondary, "window_minutes"); v != nil {
			snapshot.SecondaryWindowMinutes = v
			hasData = true
		}
	}

	if !hasData {
		return nil
	}
	snapshot.UpdatedAt = time.Now().Format(time.RFC3339)
	return snapshot
}

// noteOpenAIWSCodexRateLimits 把原生 WS 上逐轮到达的额度快照喂给账号。
//
// 不接它的后果不止于观测缺失：`codex_5h_used_percent` / `codex_7d_used_percent` 是
// account_scheduling_threshold_eval 的输入，而这两项在原生 WS 上原先唯一的来源是**拨号时刻**的
// 握手响应头——三条通路把它放进 `OpenAIForwardResult.ResponseHeaders`，成功回调再据此回填额度。
// 连接池里的连接活得很久，所以纯 WS 流量的账号一直拿着旧水位做调度决定，且没有任何信号。
// 那条旧来源现在由 CodexQuotaHeaders 挡住（否则它会反过来覆盖本文件采到的实时水位）。
//
// **与交付无关，所以不看客户端还在不在**：额度是账号的事实，客户端走了它照样在变。落库那一步
// （updateCodexUsageSnapshot）自己带节流与脱钩 goroutine，逐轮调用不会变成写风暴。
func (s *OpenAIGatewayService) noteOpenAIWSCodexRateLimits(ctx context.Context, account *Account, eventType string, payload []byte) {
	if s == nil || account == nil || len(payload) == 0 {
		return
	}
	if strings.TrimSpace(eventType) != openAIWSCodexRateLimitsEvent {
		return
	}
	// 影子账号的 codex_* 只由 QueryUsage(/wham/usage) 更新，口径与 HTTP 各落点一致。
	if account.IsShadow() {
		return
	}
	if snapshot := parseOpenAIWSCodexRateLimitEvent(payload); snapshot != nil {
		s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
	}
}

// CodexQuotaHeaders 返回可用于**逐轮**额度刷新的响应头，连接级的握手头返回 nil。
//
// 逐轮额度只认真正属于本轮的响应头。WS 通路把连接级握手响应头放进 ResponseHeaders（供限流信号与
// retry-after 用），拿它回填会造成两种损害：把带内事件刚写进去的实时水位**覆盖成拨号时刻的旧值**
// （落库节流窗口 30s，一轮耗时超过它就会发生，codex 的编码轮次经常超过），以及用当前时间重算旧的
// 相对重置秒数，得出一个偏后的 reset_at。
//
// 判据只能是这个显式标记，**不能用 `!OpenAIWSMode`**：WS-HTTP bridge 那条路同样是 WS 模式，但它的
// ResponseHeaders 是本轮真实的 HTTP 响应头，那份额度必须照常刷新。
func (r *OpenAIForwardResult) CodexQuotaHeaders() http.Header {
	if r == nil || r.ResponseHeadersFromWSHandshake {
		return nil
	}
	return r.ResponseHeaders
}

// maxCodexResetAfterSeconds 是 `reset_after_seconds` 的合理上限（366 天）。
//
// 额度窗口最长是 7 天，这个上限只用来挡住荒谬值。**它挡的不是显示错误而是调度失效**：
// `time.Duration(sec) * time.Second` 在 sec 超过约 9.22e9 时溢出，算出来的 reset_at 会落到过去
// （实测 9223372037 秒 → 1734 年），而 openAIQuotaWindowReset 会把"已过去"读成"该窗口已重置"，
// 于是一个 100% 的窗口被 account_scheduling_threshold_eval 直接跳过——额度暂停失效。
const maxCodexResetAfterSeconds = 366 * 24 * 60 * 60

// saneCodexResetAfterSeconds 挡掉落在合理区间之外的重置秒数（两条采集路共用）。
//
// 只丢这一个字段，同一窗口的其它字段照常保留：水位本身仍然可信，不该因为倒计时荒谬就整份丢掉。
func saneCodexResetAfterSeconds(sec *int) *int {
	if sec == nil || *sec < 0 || *sec > maxCodexResetAfterSeconds {
		return nil
	}
	return sec
}
