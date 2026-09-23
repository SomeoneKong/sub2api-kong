package service

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Codex 模型目录的上下文窗口折算。
//
// 上游目录给多数模型报 context_window=272000、max_context_window=872000。后者在 codex 里的语义
// 是「配置覆盖允许的上限」（codex-rs/protocol/src/openai_models.rs），实际生效窗口按
// context_window × effective_context_window_percent 算。网关在目录写给客户端之前把
// context_window 抬到 max_context_window × 比例，各个客户端就不必再各自配置
// model_context_window 或钉一份本地目录。

// KongCodexContextWindowRatioEnv 是折算比例，取值 [0,1]。不设置按 1（直接取 max），0 关闭改写。
const KongCodexContextWindowRatioEnv = "KONG_CODEX_CONTEXT_WINDOW_RATIO"

const kongCodexContextWindowDefaultRatio = 1.0

// 配错时关闭改写并记错误日志，而不是换成默认值：默认值是抬到 max，比上游行为更激进，
// 不该由一个写错的值触发。
var kongCodexContextWindowRatio = sync.OnceValue(func() float64 {
	value, present := os.LookupEnv(KongCodexContextWindowRatioEnv)
	ratio, err := kongParseCodexContextWindowRatio(value, present)
	if err != nil {
		slog.Error("codex 目录上下文窗口折算已关闭", "error", err)
		return 0
	}
	return ratio
})

func kongParseCodexContextWindowRatio(value string, present bool) (float64, error) {
	if !present {
		return kongCodexContextWindowDefaultRatio, nil
	}
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, fmt.Errorf("%s 被设成空值；要用默认值请不要设置该变量", KongCodexContextWindowRatioEnv)
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	// NaN 参与比较恒为假，会穿过区间检查，必须显式挡掉。
	if err != nil || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return 0, fmt.Errorf("%s 必须是 [0,1] 之间的小数，得到 %q", KongCodexContextWindowRatioEnv, raw)
	}
	return ratio, nil
}

// KongFinalizeCodexModelsManifest 在目录写给客户端之前折算上下文窗口，并按折算后的正文判定
// If-None-Match。
//
// 调用方传给上游构建函数的 If-None-Match 必须为空：那些函数按折算前的 ETag 判 304，持有
// 未折算正文的客户端会一直拿不到折算结果。
func KongFinalizeCodexModelsManifest(manifest *OpenAIModelsResponse, ifNoneMatch string) {
	kongFinalizeCodexModelsManifest(manifest, ifNoneMatch, kongCodexContextWindowRatio())
}

func kongFinalizeCodexModelsManifest(manifest *OpenAIModelsResponse, ifNoneMatch string, ratio float64) {
	if manifest == nil || manifest.NotModified {
		return
	}
	if ratio > 0 && len(manifest.Body) > 0 {
		body, changed, err := kongRaiseCodexContextWindows(manifest.Body, ratio)
		if err != nil {
			// 上游构建函数已经解析过这份正文，走到这里说明结构超出预期；原样下发比拒绝整个目录好。
			slog.Warn("codex 目录上下文窗口折算失败，原样下发", "error", err)
		} else if changed {
			manifest.Body = body
			manifest.ETag = codexModelsManifestBodyETag(body)
		}
	}
	if codexModelsManifestETagMatches(ifNoneMatch, manifest.ETag) {
		manifest.Body = nil
		manifest.NotModified = true
	}
}

// kongRaiseCodexContextWindows 把每个模型的 context_window 抬到 round(max_context_window × ratio)。
// 只往上调：折算值不超过现值、或缺 context_window / max_context_window 的模型原样保留
// （缺 context_window 时 codex 本来就以 max_context_window 为窗口）。
func kongRaiseCodexContextWindows(body []byte, ratio float64) ([]byte, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("decode manifest: %w", err)
	}
	rawModels, ok := envelope["models"]
	if !ok {
		return body, false, nil
	}
	var models []json.RawMessage
	if err := json.Unmarshal(rawModels, &models); err != nil {
		return nil, false, fmt.Errorf("decode models array: %w", err)
	}

	changed := false
	for i, rawModel := range models {
		var model map[string]json.RawMessage
		if err := json.Unmarshal(rawModel, &model); err != nil || model == nil {
			continue
		}
		window, ok := kongJSONInt64(model["context_window"])
		if !ok {
			continue
		}
		maxWindow, ok := kongJSONInt64(model["max_context_window"])
		if !ok || maxWindow <= 0 {
			continue
		}
		// ratio ≤ 1 且 max 是整数，四舍五入不会越过 max。
		target := int64(math.Round(float64(maxWindow) * ratio))
		if target <= window {
			continue
		}
		model["context_window"] = json.RawMessage(strconv.FormatInt(target, 10))
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, false, fmt.Errorf("encode model: %w", err)
		}
		models[i] = encoded
		changed = true
	}
	if !changed {
		return body, false, nil
	}

	encodedModels, err := json.Marshal(models)
	if err != nil {
		return nil, false, fmt.Errorf("encode models array: %w", err)
	}
	envelope["models"] = encodedModels
	updated, err := json.Marshal(envelope)
	if err != nil {
		return nil, false, fmt.Errorf("encode manifest: %w", err)
	}
	return updated, true, nil
}

// kongJSONInt64 读取整数字段；缺失、null 或非整数都返回 false。
func kongJSONInt64(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return 0, false
	}
	return *value, true
}
