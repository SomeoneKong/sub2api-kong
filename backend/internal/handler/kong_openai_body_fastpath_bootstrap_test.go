//go:build unit

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kongcorpus"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// bootstrap 门的差分测试：page context、automation 与 delegation 三个规范化在快速路径开、关时结果相同；候选判定的
// 前提（type 为 function_call_output、namespace 为字符串，page context 还要求 namespace 为 agent）由守护测试钉住。

const (
	kongBFAutomationOutput = "Automation: Daily sync\nAutomation ID: daily-sync\nAutomation memory: $CODEX_HOME/automations/daily-sync/memory.md\nLast run: never\n\nRun the sync."
	kongBFDelegationOutput = "<codex_delegation><source_thread_id>thread-1</source_thread_id><input>Please review the patch</input></codex_delegation>"
)

func kongBFBootstrapItem(namespace, name, output string) string {
	return `{"type":"function_call_output","namespace":` + namespace + `,"name":"` + name + `","call_id":"call_boot","output":` + output + `}`
}

func kongBFPageItem(namespace, name, output string) string {
	return `{"type":"function_call_output","namespace":` + namespace + `,"name":"` + name + `","output":` + output + `}`
}

func kongBFBootstrapBodies() map[string][]byte {
	automation := kongcorpus.Quote(kongBFAutomationOutput)
	delegation := kongcorpus.Quote(kongBFDelegationOutput)
	pageOpen := kongcorpus.Quote(`{"page_id":"page_1"}`)
	pagePart := kongcorpus.Quote("Companion Page snapshot part 1 of 1:\n" + `{"page_id":null,"document_view":null}`)
	escape := func(s string) string { return strings.ReplaceAll(s, "%u", "\\u") }
	items := map[string]string{
		"automation":            kongBFBootstrapItem(`"codex_app"`, "automation_update", automation),
		"delegation_app":        kongBFBootstrapItem(`"codex_app"`, "create_thread", delegation),
		"delegation_tui":        kongBFBootstrapItem(`"codex_tui"`, "send_message_to_thread", delegation),
		"no_namespace":          `{"type":"function_call_output","name":"automation_update","call_id":"c","output":` + automation + `}`,
		"namespace_number":      kongBFBootstrapItem(`5`, "automation_update", automation),
		"namespace_null":        kongBFBootstrapItem(`null`, "create_thread", delegation),
		"namespace_escaped":     kongBFBootstrapItem(escape(`"codex%u005fapp"`), "automation_update", automation),
		"type_escaped":          escape(`{"type":"function%u005fcall_output","namespace":"codex_app","name":"automation_update","output":`) + automation + `}`,
		"key_escaped":           escape(`{"type":"function_call_output","namespac%u0065":"codex_app","name":"automation_update","output":`) + automation + `}`,
		"namespace_duplicate":   `{"type":"function_call_output","namespace":"other","namespace":"codex_app","name":"automation_update","output":` + automation + `}`,
		"output_not_string":     kongBFBootstrapItem(`"codex_app"`, "automation_update", `{"text":"x"}`),
		"output_lone_surrogate": kongBFBootstrapItem(`"codex_app"`, "automation_update", escape(`"Automation: %ud800"`)),
		"wrong_name":            kongBFBootstrapItem(`"codex_app"`, "other", automation),
		"function_call":         `{"type":"function_call","namespace":"codex_app","name":"automation_update","call_id":"c","arguments":"{}"}`,
		"plain_output":          `{"type":"function_call_output","call_id":"c","output":"ok"}`,
		"history_pair":          `{"type":"function_call","call_id":"call_boot","name":"exec","arguments":"{}"}`,
		"message":               `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		"page_open":             kongBFPageItem(`"agent"`, "codex_apps_open_page", pageOpen),
		"page_part":             kongBFPageItem(`"agent"`, "codex_apps_selected_page_part_1_of_1", pagePart),
		"page_blank_call_id":    `{"type":"function_call_output","namespace":"agent","name":"codex_apps_open_page","call_id":" ","output":` + pageOpen + `}`,
		"page_real_call_id":     `{"type":"function_call_output","namespace":"agent","name":"codex_apps_open_page","call_id":"call_real","output":` + pageOpen + `}`,
		"page_other_namespace":  kongBFPageItem(`"codex_app"`, "codex_apps_open_page", pageOpen),
		"page_no_namespace":     `{"type":"function_call_output","name":"codex_apps_open_page","output":` + pageOpen + `}`,
		"page_namespace_number": kongBFPageItem(`5`, "codex_apps_open_page", pageOpen),
		"page_namespace_escape": kongBFPageItem(escape(`"ag%u0065nt"`), "codex_apps_open_page", pageOpen),
		"page_type_escaped":     escape(`{"type":"function%u005fcall_output","namespace":"agent","name":"codex_apps_open_page","output":`) + pageOpen + `}`,
		"page_key_escaped":      escape(`{"type":"function_call_output","namespac%u0065":"agent","name":"codex_apps_open_page","output":`) + pageOpen + `}`,
		"page_namespace_dup":    `{"type":"function_call_output","namespace":"agent","namespace":"other","name":"codex_apps_open_page","output":` + pageOpen + `}`,
		"page_bad_snapshot":     kongBFPageItem(`"agent"`, "codex_apps_open_page", kongcorpus.Quote(`{"other":null}`)),
	}
	bodies := make(map[string][]byte)
	seed := uint64(500)
	for name, item := range items {
		for _, shape := range []kongcorpus.Shape{kongcorpus.ShapeSol, kongcorpus.ShapeLite} {
			for _, target := range []int{0, 72 << 10} {
				seed++
				o := kongcorpus.Options{Shape: shape, Seed: seed, TargetBytes: target, SuffixItems: []string{item}}
				bodies[fmt.Sprintf("%s/shape=%d/target=%d", name, shape, target)] = kongcorpus.Request(o)
				seed++
				o = kongcorpus.Options{Shape: shape, Seed: seed, NoHistory: true, SuffixItems: []string{item}}
				bodies[fmt.Sprintf("%s/shape=%d/no_history", name, shape)] = kongcorpus.Request(o)
			}
		}
		seed++
		o := kongcorpus.Options{Seed: seed, NoHistory: true, SuffixItems: []string{item}, TopLevelTail: []string{`"previous_response_id":"resp_1"`}}
		bodies[name+"/previous_response_id"] = kongcorpus.Request(o)
		seed++
		o = kongcorpus.Options{Seed: seed, NoHistory: true, SuffixItems: []string{item}, TopLevelTail: []string{`"input":[]`}}
		bodies[name+"/duplicate_input"] = kongcorpus.Request(o)
	}
	seed++
	valid := kongcorpus.Request(kongcorpus.Options{Seed: seed, NoHistory: true, SuffixItems: []string{items["automation"]}})
	bodies["truncated"] = valid[:len(valid)-1]
	bodies["trailing_garbage"] = append(append([]byte(nil), valid...), 'x')
	bodies["leading_space"] = append([]byte("  "), valid...)
	return bodies
}

func TestKongCodexCallOutputBootstrapGateMatchesOriginal(t *testing.T) {
	type outcome struct {
		body    string
		changed bool
	}
	normalizers := []struct {
		name      string
		normalize func([]byte) ([]byte, bool)
		mayApply  func([]byte) bool
	}{
		{"page_context", normalizeCodexPageContext, kongCodexPageContextMayApply},
		{"automation", normalizeCodexAutomationBootstrap, func(body []byte) bool {
			return service.KongCodexCallOutputBootstrapMayApply(body, isCodexAutomationCandidate)
		}},
		{"delegation", normalizeCodexDelegationBootstrap, func(body []byte) bool {
			return service.KongCodexCallOutputBootstrapMayApply(body, isCodexDelegationCandidate)
		}},
	}
	skipped := make(map[string]int)
	rewritten := make(map[string]int)
	for name, body := range kongBFBootstrapBodies() {
		for _, n := range normalizers {
			restore := service.KongSetOpenAIBodyFastpathForTest(true)
			release := service.KongRegisterRequestBody(body)
			mayApply := n.mayApply(body)
			got, changed := n.normalize(body)
			on := outcome{string(got), changed}
			restore()

			restore = service.KongSetOpenAIBodyFastpathForTest(false)
			got, changed = n.normalize(body)
			off := outcome{string(got), changed}
			restore()
			release()

			require.Equal(t, off, on, "%s/%s", n.name, name)
			if !mayApply {
				skipped[n.name]++
				require.False(t, off.changed, "%s/%s：门判跳过，原函数却有改写", n.name, name)
				require.True(t, bytes.Equal([]byte(off.body), body), "%s/%s", n.name, name)
			}
			if off.changed {
				rewritten[n.name]++
			}
		}
	}
	for _, n := range normalizers {
		require.Positive(t, skipped[n.name], "%s：语料里没有门判跳过的请求体", n.name)
	}
	require.Positive(t, rewritten["page_context"], "page_context：语料里没有会被改写的请求体，差分没有意义")
	require.Positive(t, rewritten["automation"]+rewritten["delegation"], "bootstrap：语料里没有会被改写的请求体，差分没有意义")
}

// page context 的门只认 namespace 为 agent 的 function_call_output 项；原函数若开始改写别的项，门就会漏判。
func TestKongCodexPageContextNeedsAgentNamespace(t *testing.T) {
	base := map[string]any{"type": "function_call_output", "namespace": "agent", "name": "codex_apps_open_page", "output": `{"page_id":null}`}
	request := func(item map[string]any) []byte {
		body, err := json.Marshal(map[string]any{"model": "gpt-5", "input": []any{item}})
		require.NoError(t, err)
		return body
	}
	// 原函数的行为要在门关着时看，否则反例先被门拦下，验证不到原函数本身。
	original := func(body []byte) ([]byte, bool) {
		defer service.KongSetOpenAIBodyFastpathForTest(false)()
		return normalizeCodexPageContext(body)
	}
	mayApply := func(body []byte) bool {
		defer service.KongSetOpenAIBodyFastpathForTest(true)()
		return kongCodexPageContextMayApply(body)
	}
	_, changed := original(request(base))
	require.True(t, changed)
	require.True(t, mayApply(request(base)))
	for _, edit := range []func(map[string]any){
		func(item map[string]any) { delete(item, "namespace") },
		func(item map[string]any) { item["namespace"] = "codex_app" },
		func(item map[string]any) { item["namespace"] = "Agent" },
		func(item map[string]any) { item["namespace"] = 5 },
		func(item map[string]any) { item["namespace"] = nil },
		func(item map[string]any) { item["namespace"] = []any{"agent"} },
		func(item map[string]any) { delete(item, "type") },
		func(item map[string]any) { item["type"] = "function_call" },
		func(item map[string]any) { item["type"] = 5 },
	} {
		item := make(map[string]any, len(base))
		for key, value := range base {
			item[key] = value
		}
		edit(item)
		body := request(item)
		got, changed := original(body)
		require.False(t, changed, "%v", item)
		require.Equal(t, body, got)
		require.False(t, mayApply(body), "%v", item)
	}
}

// 门只把 type 为 function_call_output、namespace 为字符串的项交给候选判定；判定若不再要求这两点，门就会漏判。
func TestKongCodexBootstrapCandidatesNeedNamespace(t *testing.T) {
	candidates := []struct {
		name        string
		isCandidate func(map[string]any) bool
		item        map[string]any
	}{
		{"automation", isCodexAutomationCandidate, map[string]any{"type": "function_call_output", "namespace": "codex_app", "name": "automation_update", "output": kongBFAutomationOutput}},
		{"delegation", isCodexDelegationCandidate, map[string]any{"type": "function_call_output", "namespace": "codex_tui", "name": "create_thread", "output": kongBFDelegationOutput}},
	}
	for _, tc := range candidates {
		require.True(t, tc.isCandidate(tc.item), tc.name)
		for _, edit := range []func(map[string]any){
			func(item map[string]any) { delete(item, "namespace") },
			func(item map[string]any) { item["namespace"] = 5 },
			func(item map[string]any) { item["namespace"] = nil },
			func(item map[string]any) { item["namespace"] = map[string]any{"name": "codex_app"} },
			func(item map[string]any) { delete(item, "type") },
			func(item map[string]any) { item["type"] = "function_call" },
			func(item map[string]any) { item["type"] = 5 },
		} {
			item := make(map[string]any, len(tc.item))
			for key, value := range tc.item {
				item[key] = value
			}
			edit(item)
			require.False(t, tc.isCandidate(item), "%s：%v", tc.name, item)
		}
	}
}
