//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
	"weak"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kongcorpus"
	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 请求体快速路径的差分测试：同一份请求体在快速路径开、关两种状态下，经过每个挂了门的原函数、以及 gjson /
// sjson 的查找与改写，结果必须完全相同；每道门判"跳过"时，原函数（快速路径关闭）也必须确实原样返回。

// kongBFEsc 把 %u 换成 JSON 的 \u 转义前缀，片段里的转义都这样写。
func kongBFEsc(s string) string { return strings.ReplaceAll(s, "%u", "\\u") }

// kongBFTarget 让合成请求体过登记门槛。
const kongBFTarget = 72 << 10

var kongBFItemFragments = []string{
	`{"type":"compaction_trigger"}`,
	`{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}`,
	`{"type":"function_call_output","call_id":"call_a","output":"ok"}`,
	`{"type":"function_call_output","call_id":"call_orphan","output":"lost"}`,
	`{"type":"custom_tool_call_output","call_id":"call_orphan2","output":"lost"}`,
	`{"type":"item_reference","id":"call_orphan"}`,
	`{"type":"function%u005fcall_output","call_id":"call_esc","output":"x"}`,
	`{"typ%u0065":"function_call_output","call_id":"call_esc2","output":"x"}`,
	`{"type":"message","type":"function_call_output","call_id":"call_dup","output":"x"}`,
	`{"type":"function_call_output","call_id":"call_dup2","call_id":"call_dup3","output":"x"}`,
	`{"type":"function_call_output","call_id":5,"output":"x"}`,
	`{"type":"message","role":"user","id":"msg_ok","content":[{"type":"input_text","text":"hi"}]}`,
	`{"type":"message","role":"user","id":"bad","content":[]}`,
	`{"type":"message","role":"user","id":"","content":[]}`,
	`{"type":"message","role":"user","id":5,"content":[]}`,
	`{"type":"message","role":"user","id":"msg_%u0078","content":[]}`,
	`{"type":"message","role":"user","call_id":"call_x","content":[]}`,
	`{"type":"reasoning","id":"rs_` + strings.Repeat("x", 70) + `","summary":[]}`,
	`{"type":5,"id":"bad"}`,
	`{"id":"bad"}`,
	`{"type":" message ","id":"bad","content":[]}`,
	`{"type":"message","role":"user","id":"msg_ok","id":"bad","content":[]}`,
	`{"type":"message","role":"user","id":"bad","id":"msg_ok","content":[]}`,
	`{"type":"message","type":"reasoning","role":"user","id":"msg_ok","content":[]}`,
	`{"type":"reasoning","type":"message","role":"user","id":"msg_ok","content":[]}`,
	`{"type":"message","role":"user","internal_chat_message_metadata_passthrough":{"a":1},"content":[]}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"x","internal_chat_message_metadata_passthrough":1}]}`,
	`{"type":"message","role":"user","internal_chat_message_metadata_passthroug%u0068":1,"content":[]}`,
	`{"type":"function_call_output","namespace":"codex_app","name":"automation_update","call_id":"c_auto","output":"Automation: x"}`,
	`{"type":"function_call_output","namespace":5,"name":"create_thread","output":"x"}`,
	`{"type":"function_call","namespace":"mcp","name":"tool","call_id":"c_ns","arguments":"{}"}`,
	`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]}`,
	`{"type":"message","role":"user","content":[{"type":"input%u005fimage","image_url":"data:image/png;base64,"}]}`,
	`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"python"}]}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":" PyThOn "}]}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"pyth%u006fn"}]}`,
	`{"type":"function_call","name":"python","call_id":"call_py","arguments":"{}"}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"(?=x) (?<y>z) (?:w) %u0028?=v)"}]}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"bad %ud800 surrogate"}]}`,
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"bad ` + string([]byte{0xff}) + ` utf8"}]}`,
	`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"t"}],"encrypted_content":"gAAAAB"}`,
	`{"type":"message","role":"user","content":"plain string content"}`,
	`5`,
	`"str"`,
	`null`,
	`[]`,
	// encoding/json 的嵌套上限是 10000 层：刚好在上限内的可解码，超过的不可解码。
	strings.Repeat("[", 9990) + strings.Repeat("]", 9990),
	strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
}

var kongBFToolFragments = []string{
	`{"type":"function","name":"python","description":"d","strict":false,"parameters":{"type":"object","properties":{}}}`,
	`{"type":"function","name":"Python ","parameters":{"type":"object"}}`,
	`{"type":"function","name":"lookahead","parameters":{"type":"object","properties":{"a":{"type":"string","pattern":"^(?=a)"}}}}`,
	`{"type":"function","name":"lookbehind_esc","parameters":{"type":"object","properties":{"a":{"type":"string","pattern":"%u0028?<=a)"}}}}`,
	`{"type":"function","name":"q_esc","parameters":{"type":"object","properties":{"a":{"type":"string","pattern":"(%u003f!a)"}}}}`,
	`{"type":"function","name":"eq_esc","parameters":{"type":"object","properties":{"a":{"type":"string","pattern":"(?%u003da)"}}}}`,
	`{"type":"function","name":"noncap","parameters":{"type":"object","properties":{"a":{"type":"string","pattern":"^(?:a|b)$"}}}}`,
	`{"type":"function","name":"nulltype","parameters":{"type":null}}`,
	`{"type":"function","name":"noparams"}`,
	`{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"inner","parameters":{"type":"object"}}]}`,
	`{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}`,
	`{"type":"web_search"}`,
}

var kongBFTopLevelFragments = []string{
	`"previous_response_id":"resp_1"`,
	`"previous_response_id":null`,
	`"previous_response_id":""`,
	`"input":[]`,
	`"stream":true`,
	`"inp%u0075t":[]`,
	`"parallel_tool_calls":true`,
	`"parallel_tool_calls":false`,
	`"max_output_tokens":100`,
	`"x":{"python":1}`,
	`"python":"python"`,
}

var kongBFReasoningFragments = []string{
	`{"effort":"none"}`,
	`{"effort":"high","summary":"auto"}`,
	`null`,
	`"x"`,
}

var kongBFInputFragments = []string{
	`"a string input"`,
	`{"type":"message"}`,
	`[]`,
	`null`,
	`[{"type":"compaction_trigger"}]`,
	`[{"type":"compaction_trigger"},{"type":"message","role":"user","content":[]}]`,
}

type kongBFBody struct {
	name string
	body []byte
}

func kongBFBodies() []kongBFBody {
	var out []kongBFBody
	seed := uint64(100)
	opts := func(shape kongcorpus.Shape, target int) kongcorpus.Options {
		seed++
		return kongcorpus.Options{Shape: shape, Seed: seed, TargetBytes: target}
	}
	add := func(name string, o kongcorpus.Options) {
		out = append(out, kongBFBody{name: name, body: kongcorpus.Request(o)})
	}
	shapes := []struct {
		name  string
		shape kongcorpus.Shape
	}{{"sol", kongcorpus.ShapeSol}, {"lite", kongcorpus.ShapeLite}}
	for _, s := range shapes {
		add(s.name+"/plain", opts(s.shape, kongBFTarget))
		add(s.name+"/small", opts(s.shape, 0))
		o := opts(s.shape, 0)
		o.NoHistory = true
		add(s.name+"/no_history", o)
		for i, fragment := range kongBFItemFragments {
			o := opts(s.shape, kongBFTarget)
			o.SuffixItems = []string{kongBFEsc(fragment)}
			add(fmt.Sprintf("%s/suffix_item_%d", s.name, i), o)
			o = opts(s.shape, kongBFTarget)
			o.PrefixItems = []string{kongBFEsc(fragment)}
			add(fmt.Sprintf("%s/prefix_item_%d", s.name, i), o)
		}
		for i, fragment := range kongBFToolFragments {
			o := opts(s.shape, kongBFTarget)
			o.ExtraTools = []string{kongBFEsc(fragment)}
			add(fmt.Sprintf("%s/tool_%d", s.name, i), o)
		}
		for i, fragment := range kongBFTopLevelFragments {
			o := opts(s.shape, kongBFTarget)
			o.TopLevelTail = []string{kongBFEsc(fragment)}
			add(fmt.Sprintf("%s/top_%d", s.name, i), o)
		}
		for i, fragment := range kongBFReasoningFragments {
			o := opts(s.shape, kongBFTarget)
			o.ReasoningRaw = fragment
			add(fmt.Sprintf("%s/reasoning_%d", s.name, i), o)
		}
	}
	for i, fragment := range kongBFItemFragments {
		o := opts(kongcorpus.ShapeSol, 0)
		o.SuffixItems = []string{kongBFEsc(fragment)}
		add(fmt.Sprintf("sol_small/suffix_item_%d", i), o)
	}
	for i, fragment := range kongBFInputFragments {
		o := opts(kongcorpus.ShapeSol, kongBFTarget)
		o.InputRaw = fragment
		add(fmt.Sprintf("sol/input_%d", i), o)
	}

	rng := rand.New(rand.NewPCG(7, 11))
	pick := func(list []string, n int) []string {
		picked := make([]string, n)
		for i := range picked {
			picked[i] = kongBFEsc(list[rng.IntN(len(list))])
		}
		return picked
	}
	for i := 0; i < 80; i++ {
		o := opts(shapes[rng.IntN(len(shapes))].shape, kongBFTarget)
		o.PrefixItems = pick(kongBFItemFragments, rng.IntN(2))
		o.SuffixItems = pick(kongBFItemFragments, 1+rng.IntN(3))
		o.ExtraTools = pick(kongBFToolFragments, rng.IntN(3))
		o.TopLevelTail = pick(kongBFTopLevelFragments, rng.IntN(2))
		add(fmt.Sprintf("combo_%d", i), o)
	}

	base := kongcorpus.Request(opts(kongcorpus.ShapeSol, kongBFTarget))
	withTrigger := func() []byte {
		o := opts(kongcorpus.ShapeSol, kongBFTarget)
		o.PrefixItems = []string{`{"type":"compaction_trigger"}`}
		return kongcorpus.Request(o)
	}()
	for name, body := range map[string][]byte{
		"truncated":          append([]byte(nil), base[:len(base)/2]...),
		"trailing_garbage":   append(append([]byte(nil), base...), 'x'),
		"trailing_object":    append(append([]byte(nil), base...), []byte(`{}`)...),
		"leading_space":      append([]byte(" \n\t"), base...),
		"trailing_space":     append(append([]byte(nil), base...), []byte(" \n")...),
		"bom":                append([]byte{0xEF, 0xBB, 0xBF}, base...),
		"array_top":          append(append([]byte("["), base...), ']'),
		"trigger_truncated":  append([]byte(nil), withTrigger[:len(withTrigger)-2]...),
		"trigger_whitespace": append([]byte("  "), withTrigger...),
	} {
		out = append(out, kongBFBody{name: "whole/" + name, body: body})
	}
	return out
}

// ── 原函数的结果 ─────────────────────────────────────────────────────────

type kongBFOutcome struct {
	body    string
	changed bool
	err     string
	extra   string
}

func kongBFDigest(s string) string { return fmt.Sprintf("%d:%016x", len(s), xxhash.Sum64String(s)) }

func kongBFErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func kongBFResult(r gjson.Result) string {
	return fmt.Sprintf("%d|%d|%v|%s|%s|%v", r.Type, r.Index, r.Indexes, kongBFDigest(r.Raw), kongBFDigest(r.Str), r.Num)
}

var kongBFGjsonPaths = []string{
	"model", "input", "stream", "tools", "reasoning", "reasoning.effort", "reasoning.summary",
	"input.0", "input.0.type", "input.1.content.0.text", "tools.0.name", "tools.0.parameters.type",
	"text.format.type", "missing", "missing.child", "model.child", "stream.x", "input.999",
	"prompt_cache_key", "previous_response_id", "include", "include.0", "store", "instructions",
	"client_metadata", "client_metadata.x", "parallel_tool_calls", "python", "x.python", "x-y",
	"input.#", "tools.#.name", "input.0|type", "@this", "mo*", "inp\\ut", "0", "", ".", "model.", ".model",
}

var kongBFSjsonEdits = []func([]byte) ([]byte, error){
	func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "model", "gpt-kong") },
	func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "stream", false) },
	func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "reasoning.effort", "low") },
	func(b []byte) ([]byte, error) { return sjson.SetRawBytes(b, "kong_new", []byte(`{"a":1}`)) },
	func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "input.0.type", "message") },
	func(b []byte) ([]byte, error) { return sjson.DeleteBytes(b, "input") },
	func(b []byte) ([]byte, error) { return sjson.DeleteBytes(b, "tools.0") },
	func(b []byte) ([]byte, error) { return sjson.DeleteBytes(b, "prompt_cache_key") },
	func(b []byte) ([]byte, error) { return sjson.DeleteBytes(b, "missing") },
	func(b []byte) ([]byte, error) {
		s, err := sjson.Set(kongBytesView(b), "model", "gpt-kong")
		return []byte(s), err
	},
}

func kongBFRunOriginals(body []byte) map[string]kongBFOutcome {
	out := make(map[string]kongBFOutcome, 128)
	put := func(name string, result []byte, changed bool, err error, extra string) {
		out[name] = kongBFOutcome{body: kongBFDigest(string(result)), changed: changed, err: kongBFErr(err), extra: extra}
	}
	next, changed, err := NormalizeCompactionTriggerInputOrder(body)
	put("trigger_order", next, changed, err, "")
	next, aliases, changed, err := aliasOpenAIOAuthReservedToolNamesBody(body)
	put("alias", next, changed, err, fmt.Sprint(aliases))
	next, changed, err = normalizeOpenAIResponsesLiteToolsPayload(body)
	put("lite_tools", next, changed, err, "")
	next, changed, err = sanitizeOpenAIResponsesToolSchemasForPlatform(body, PlatformOpenAI)
	put("tool_schemas", next, changed, err, "")
	next, changed, err = sanitizeOpenAIResponsesInputItemIDs(body)
	put("item_ids", next, changed, err, "")
	next, changed, err = normalizeOpenAIOAuthResponsesCompatibilityBody(body)
	put("oauth_compat", next, changed, err, "")
	for _, lite := range []bool{false, true} {
		next, changed, err = normalizeOpenAIResponsesWebSocketCompatibilityBody(body, kongTestOAuthAccount(), lite)
		put(fmt.Sprintf("ws_compat/lite=%v", lite), next, changed, err, "")
	}
	for _, compact := range []bool{false, true} {
		next, changed, err = normalizeOpenAIPassthroughOAuthBody(body, compact)
		put(fmt.Sprintf("passthrough_oauth/compact=%v", compact), next, changed, err, "")
	}
	for _, keep := range []bool{false, true} {
		next, err = stripOpenAIResponsesInputNamespaces(body, keep)
		put(fmt.Sprintf("strip_namespaces/keep=%v", keep), next, false, err, "")
	}
	put("empty_image", nil, openAIRequestBodyMayContainEmptyBase64InputImage(body), nil, "")
	put("has_trigger", nil, HasCompactionTriggerInInput(body), nil, "")

	view := kongBytesView(body)
	put("gjson_valid", nil, gjson.ValidBytes(body), nil, fmt.Sprint(gjson.Valid(view)))
	root := gjson.Parse(view)
	for _, path := range kongBFGjsonPaths {
		put("get/"+path, nil, false, nil, kongBFResult(gjson.GetBytes(body, path))+" "+kongBFResult(root.Get(path)))
	}
	var many []string
	for _, r := range gjson.GetManyBytes(body, kongBFGjsonPaths...) {
		many = append(many, kongBFResult(r))
	}
	put("get_many", nil, false, nil, strings.Join(many, " "))
	for i, edit := range kongBFSjsonEdits {
		next, err := edit(body)
		put(fmt.Sprintf("sjson/%d", i), next, false, err, "")
	}
	return out
}

// ── 门的判定与蕴含关系 ─────────────────────────────────────────────────

type kongBFGates struct {
	triggerSettled bool
	orphanMayApply bool
	noReservedName bool
	liteSettled    bool
	skipLookaround bool
	itemIDsClean   bool
	noMetadata     bool
	imageToken     bool
}

func kongBFGateDecisions(body []byte) kongBFGates {
	g := kongBFGates{
		triggerSettled: kongCompactionTriggerSettled(body),
		orphanMayApply: kongOrphanCleanupMayApply(body),
		noReservedName: kongNoReservedToolName(body),
		liteSettled:    kongLiteToolsSettled(body),
		itemIDsClean:   kongInputItemIDsClean(body),
		imageToken:     openAIRequestBodyMayContainInputImageToken(body),
	}
	if _, repaired, err := sanitizeOpenAIResponsesToolParameterTypes(body); err == nil {
		g.skipLookaround = kongSkipToolSchemaLookaroundPass(PlatformOpenAI, body, repaired)
	}
	if input := gjson.Get(kongBytesView(body), "input"); input.IsArray() {
		g.noMetadata = kongNoInputItemMetadataPassthrough(body, input)
	}
	return g
}

func (g kongBFGates) skips() map[string]bool {
	return map[string]bool{
		"trigger":    g.triggerSettled,
		"orphan":     !g.orphanMayApply,
		"alias":      g.noReservedName,
		"lite":       g.liteSettled,
		"lookaround": g.skipLookaround,
		"item_ids":   g.itemIDsClean,
		"metadata":   g.noMetadata,
		"image":      !g.imageToken,
	}
}

// kongBFCheckImplications 在快速路径关闭时运行原函数，核对每道判"跳过"的门：原函数确实原样返回。
func kongBFCheckImplications(t testing.TB, name string, body []byte, g kongBFGates) {
	unchanged := func(gate string, next []byte, changed bool, err error) {
		require.NoError(t, err, "%s：门 %s 判跳过，原函数却报错", name, gate)
		require.False(t, changed, "%s：门 %s 判跳过，原函数却有改写", name, gate)
		require.True(t, bytes.Equal(next, body), "%s：门 %s 判跳过，原函数的结果不同", name, gate)
	}
	if g.triggerSettled {
		next, changed, err := NormalizeCompactionTriggerInputOrder(body)
		unchanged("trigger", next, changed, err)
	}
	if kongComputeDecodable(body) {
		var reqBody map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber(body, &reqBody))
		if input, ok := reqBody["input"].([]any); ok {
			orphanChanged := sanitizeOpenAIResponsesOrphanToolOutputs(reqBody, input, strings.TrimSpace(firstNonEmptyString(reqBody["previous_response_id"])) != "")
			if !g.orphanMayApply {
				require.False(t, orphanChanged, "%s：孤儿清理门判跳过，原函数却有改写", name)
			}
			if facts, sure := kongComputeInputFacts(body); sure {
				require.Equal(t, !orphanChanged, facts.orphanFree, "%s：投影上的孤儿清理结论与完整解码不同", name)
			}
		}
	} else {
		require.True(t, g.orphanMayApply, "%s：不可解码的请求体不能判跳过", name)
	}
	if g.noReservedName {
		next, aliases, changed, err := aliasOpenAIOAuthReservedToolNamesBody(body)
		unchanged("alias", next, changed, err)
		require.Nil(t, aliases, "%s：别名门判跳过，原函数却有别名", name)
	}
	if g.liteSettled {
		next, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
		unchanged("lite", next, changed, err)
	}
	if g.skipLookaround {
		next, changed, err := sanitizeOpenAIResponsesToolSchemaPatterns(body)
		unchanged("lookaround", next, changed, err)
	}
	if g.itemIDsClean {
		next, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)
		unchanged("item_ids", next, changed, err)
	}
	if g.noMetadata {
		for _, item := range gjson.GetBytes(body, "input").Array() {
			require.False(t, item.IsObject() && item.Get(kongOpenAIInputItemMetadataName).Exists(), "%s：元数据门判跳过，却有项带这个字段", name)
		}
	}
	if !g.imageToken {
		require.False(t, openAIJSONValueMayContainEmptyBase64InputImage(gjson.GetBytes(body, "input")), "%s：图片预判为否，完整扫描却命中", name)
	}
}

// kongBFCheckBody 对一份请求体做完整的差分与蕴含检查，返回快速路径开启时各道门的判定。
func kongBFCheckBody(t testing.TB, name string, body []byte) kongBFGates {
	restore := KongSetOpenAIBodyFastpathForTest(true)
	release := KongRegisterRequestBody(body)
	gates := kongBFGateDecisions(body)
	on := kongBFRunOriginals(body)
	restore()

	restore = KongSetOpenAIBodyFastpathForTest(false)
	off := kongBFRunOriginals(body)
	kongBFCheckImplications(t, name, body, gates)
	restore()
	release()

	require.Len(t, on, len(off))
	for key, want := range off {
		require.Equal(t, want, on[key], "%s：%s 在快速路径开、关时结果不同", name, key)
	}
	return gates
}

func TestKongOpenAIBodyFastpathMatchesOriginal(t *testing.T) {
	decisions := make(map[string][2]int)
	for _, tc := range kongBFBodies() {
		for gate, skip := range kongBFCheckBody(t, tc.name, tc.body).skips() {
			counts := decisions[gate]
			if skip {
				counts[1]++
			} else {
				counts[0]++
			}
			decisions[gate] = counts
		}
	}
	// 语料要让每道门都既判过跳过、也判过回落，否则差分对这道门没有意义。
	for gate, counts := range decisions {
		require.Positive(t, counts[1], "门 %s 一次也没有判跳过", gate)
		require.Positive(t, counts[0], "门 %s 一次也没有回落", gate)
	}
}

// kongBFSlotBody 返回一份刚过登记门槛、结构简单的请求体，以 input 里片段的位置为界切成前后两段。靠一段长的
// instructions 过门槛，而不是真实的对话历史：fuzz 每次执行都要把全部检查跑两遍，请求体结构越简单越快。
func kongBFSlotBody() (prefix, suffix string) {
	prefix = `{"model":"gpt-6-sol","instructions":"` + strings.Repeat("x", kongBodyRegistryMinBytes) +
		`","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},`
	suffix = `],"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}],"stream":true,"prompt_cache_key":"k"}`
	return prefix, suffix
}

func FuzzKongOpenAIBodyFastpath(f *testing.F) {
	for _, fragment := range kongBFItemFragments {
		// 嵌套上万层的片段由确定性语料覆盖；放进种子会让变异一直围着这些大输入转，拖慢 fuzz。
		if len(fragment) < 4<<10 {
			f.Add(kongBFEsc(fragment))
		}
	}
	prefix, suffix := kongBFSlotBody()
	f.Fuzz(func(t *testing.T, fragment string) {
		kongBFCheckBody(t, "fuzz/registered", []byte(prefix+fragment+suffix))
		kongBFCheckBody(t, "fuzz/small", []byte(`{"model":"gpt-6-sol","input":[`+fragment+`]}`))
	})
}

// kongBFMutationTokens 是随机变异时插入的片段：JSON 的结构字符与转义、门关心的键名与取值。
var kongBFMutationTokens = []string{`{`, `}`, `[`, `]`, `"`, `,`, `:`, `\`, `\\`, `\"`,
	kongBFEsc(`%u00`), kongBFEsc(`%u005f`), kongBFEsc(`%u0028`), kongBFEsc(`%u003f`), kongBFEsc(`%ud800`),
	`"type"`, `"id"`, `"call_id"`, `"name"`, `"namespace"`, `"input"`, `"output"`, `"previous_response_id"`,
	`"function_call_output"`, `"function_call"`, `"compaction_trigger"`, `"item_reference"`, `"message"`, `"reasoning"`,
	`"input_image"`, `"image_url"`, `"data:image/png;base64,"`, `"python"`, `" PYTHON "`, `"codex_app"`, `"automation_update"`,
	`"internal_chat_message_metadata_passthrough"`, `(?=`, `(?<`, `null`, `5`, `true`, " ", "\t", string([]byte{0xff}), "x"}

// TestKongOpenAIBodyFastpathRandomMutations 对语料片段做固定种子的随机变异（插入、删除、复制一段），做与 fuzz 相同
// 的全部检查。原生 fuzz 带覆盖率插桩后很慢，这组变异让每次单测都能覆盖一批结构被打乱的输入。
func TestKongOpenAIBodyFastpathRandomMutations(t *testing.T) {
	rng := rand.New(rand.NewPCG(37, 41))
	var fragments []string
	for _, fragment := range kongBFItemFragments {
		if len(fragment) < 4<<10 {
			fragments = append(fragments, kongBFEsc(fragment))
		}
	}
	prefix, suffix := kongBFSlotBody()
	for i := 0; i < 300; i++ {
		fragment := fragments[rng.IntN(len(fragments))]
		if rng.IntN(3) == 0 {
			fragment += "," + fragments[rng.IntN(len(fragments))]
		}
		for edits := 1 + rng.IntN(4); edits > 0; edits-- {
			pos := rng.IntN(len(fragment) + 1)
			switch rng.IntN(4) {
			case 0, 1:
				fragment = fragment[:pos] + kongBFMutationTokens[rng.IntN(len(kongBFMutationTokens))] + fragment[pos:]
			case 2:
				if pos < len(fragment) {
					end := pos + 1 + rng.IntN(min(8, len(fragment)-pos))
					fragment = fragment[:pos] + fragment[end:]
				}
			default:
				if pos < len(fragment) {
					end := pos + 1 + rng.IntN(min(24, len(fragment)-pos))
					fragment = fragment[:end] + fragment[pos:end] + fragment[end:]
				}
			}
		}
		name := fmt.Sprintf("mutation_%d %q", i, fragment)
		kongBFCheckBody(t, name+" registered", []byte(prefix+fragment+suffix))
		kongBFCheckBody(t, name+" small", []byte(`{"model":"gpt-6-sol","input":[`+fragment+`]}`))
	}
}

// ── 字符串层面的判定 ───────────────────────────────────────────────────

// kongBFEncodeRune 把一个字符随机写成字面、短转义或 \u 转义（大小写十六进制都有），返回字符串内容里的原文。
func kongBFEncodeRune(rng *rand.Rand, r rune) string {
	mustEscape := r == '"' || r == '\\' || r < 0x20
	switch rng.IntN(4) {
	case 0:
		if !mustEscape {
			return string(r)
		}
	case 1:
		switch r {
		case '"':
			return "\\\""
		case '\\':
			return "\\\\"
		case '/':
			return "\\/"
		case '\n':
			return "\\n"
		case '\t':
			return "\\t"
		}
	case 2:
		if r <= 0xFFFF {
			return fmt.Sprintf(kongBFEsc("%u%04X"), r)
		}
	}
	if r > 0xFFFF {
		high, low := utf16.EncodeRune(r)
		return fmt.Sprintf(kongBFEsc("%u%04x%u%04x"), high, low)
	}
	return fmt.Sprintf(kongBFEsc("%u%04x"), r)
}

func kongBFEncode(rng *rand.Rand, s string) string {
	var b strings.Builder
	for _, r := range s {
		_, _ = b.WriteString(kongBFEncodeRune(rng, r))
	}
	return b.String()
}

func kongBFDecode(content string) (string, bool) {
	var decoded string
	if err := json.Unmarshal([]byte(`"`+content+`"`), &decoded); err != nil {
		return "", false
	}
	return decoded, true
}

func TestKongJSONStringIsReservedToolNameMatchesDecode(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	spaces := []rune{' ', '\t', '\n', '\r', 0x0B, 0x85, 0xA0, 0x2003, 0x3000, 0x200B, 0xFEFF}
	letters := []rune("pythonPYTHONx")
	extras := []rune{0x17F, 0x212A, 0x1F600, 'K', 'k', 's'}
	positives := 0
	for i := 0; i < 20000; i++ {
		var content strings.Builder
		if rng.IntN(2) == 0 {
			// 接近保留名的写法：前后随机空白，保留名大小写随机，偶尔插删改一个字符。
			for n := rng.IntN(3); n > 0; n-- {
				_, _ = content.WriteString(kongBFEncodeRune(rng, spaces[rng.IntN(len(spaces))]))
			}
			name := []rune(codexReservedPythonToolName)
			for j := range name {
				if rng.IntN(2) == 0 {
					name[j] -= 'a' - 'A'
				}
			}
			if rng.IntN(4) == 0 {
				j := rng.IntN(len(name))
				switch rng.IntN(3) {
				case 0:
					name = append(name[:j], name[j+1:]...)
				case 1:
					name[j] = extras[rng.IntN(len(extras))]
				default:
					name = append(name[:j], append([]rune{letters[rng.IntN(len(letters))]}, name[j:]...)...)
				}
			}
			_, _ = content.WriteString(kongBFEncode(rng, string(name)))
			for n := rng.IntN(3); n > 0; n-- {
				_, _ = content.WriteString(kongBFEncodeRune(rng, spaces[rng.IntN(len(spaces))]))
			}
		} else {
			for n := rng.IntN(10); n > 0; n-- {
				switch rng.IntN(6) {
				case 0:
					_, _ = content.WriteString(kongBFEncodeRune(rng, spaces[rng.IntN(len(spaces))]))
				case 1:
					_, _ = content.WriteString(kongBFEncodeRune(rng, extras[rng.IntN(len(extras))]))
				case 2:
					_, _ = content.WriteString(kongBFEsc([]string{"%ud800", "%udc00", "%uDBFF%uDFFF"}[rng.IntN(3)]))
				case 3:
					_ = content.WriteByte(0xff)
				default:
					_, _ = content.WriteString(kongBFEncodeRune(rng, letters[rng.IntN(len(letters))]))
				}
			}
		}
		raw := content.String()
		decoded, ok := kongBFDecode(raw)
		if !ok {
			continue
		}
		want := aliasOpenAIOAuthReservedToolName(decoded) != decoded
		if want {
			positives++
		}
		require.Equal(t, want, kongJSONStringIsReservedToolName(raw), "内容 %q 解码为 %q", raw, decoded)

		got, gotOK := KongDecodeJSONString(`"` + raw + `"`)
		require.True(t, gotOK)
		require.Equal(t, decoded, got, "内容 %q", raw)
	}
	require.Greater(t, positives, 1000)
}

func TestKongMayContainLookaroundNeverMisses(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 17))
	alphabet := []rune("(?=!<:a\\u0283f")
	positives, skipped := 0, 0
	for i := 0; i < 50000; i++ {
		var plain strings.Builder
		for n := 1 + rng.IntN(10); n > 0; n-- {
			_, _ = plain.WriteRune(alphabet[rng.IntN(len(alphabet))])
			if rng.IntN(20) == 0 {
				_, _ = plain.WriteString([]string{"(?=", "(?!", "(?<=", "(?<!"}[rng.IntN(4)])
			}
		}
		content := kongBFEncode(rng, plain.String())
		decoded, ok := kongBFDecode(content)
		require.True(t, ok)
		body := []byte(`{"pattern":"` + content + `"}`)
		if hasRegexLookaround(decoded) {
			positives++
			require.True(t, kongMayContainLookaround(body), "原文 %s 解码后含 lookaround", body)
		} else if !kongMayContainLookaround(body) {
			skipped++
		}
	}
	require.Greater(t, positives, 1000)
	require.Greater(t, skipped, 100)
}

// 四种 lookaround 的每个字符分别取字面量、小写或大写十六进制的 \u 转义，全部组合都要命中。
func TestKongMayContainLookaroundAllSpellings(t *testing.T) {
	for _, lookaround := range []string{"(?=", "(?!", "(?<=", "(?<!"} {
		runes := []rune(lookaround)
		combos := 1
		for range runes {
			combos *= 3
		}
		for combo := 0; combo < combos; combo++ {
			var content strings.Builder
			for i, n := 0, combo; i < len(runes); i, n = i+1, n/3 {
				switch n % 3 {
				case 0:
					_, _ = content.WriteRune(runes[i])
				case 1:
					_, _ = content.WriteString(fmt.Sprintf(kongBFEsc("%u%04x"), runes[i]))
				default:
					_, _ = content.WriteString(fmt.Sprintf(kongBFEsc("%u%04X"), runes[i]))
				}
			}
			body := []byte(`{"pattern":"a` + content.String() + `b)"}`)
			decoded, ok := kongBFDecode(`a` + content.String() + `b)`)
			require.True(t, ok)
			require.True(t, hasRegexLookaround(decoded), "%s", body)
			require.True(t, kongMayContainLookaround(body), "%s", body)
		}
	}
	for _, plain := range []string{`{"pattern":"^(?:a|b)$"}`, `{"pattern":"(a)(b)"}`, `{"pattern":"a?b"}`, kongBFEsc(`{"text":"%u003c(x)"}`)} {
		require.False(t, kongMayContainLookaround([]byte(plain)), plain)
	}
}

func TestKongMayContainEscapedWordCharNeverMisses(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 23))
	alphabet := []rune("input_imagex\\\"")
	positives := 0
	for i := 0; i < 50000; i++ {
		var plain strings.Builder
		if rng.IntN(2) == 0 {
			_, _ = plain.WriteString("input_image")
		}
		for n := rng.IntN(6); n > 0; n-- {
			_, _ = plain.WriteRune(alphabet[rng.IntN(len(alphabet))])
		}
		content := kongBFEncode(rng, plain.String())
		decoded, ok := kongBFDecode(content)
		require.True(t, ok)
		if strings.Contains(decoded, "input_image") {
			positives++
			raw := []byte(`{"type":"` + content + `"}`)
			require.True(t, bytes.Contains(raw, []byte("input_image")) || kongMayContainEscapedWordChar(raw), "原文 %s 解码后含 input_image", raw)
		}
	}
	require.Greater(t, positives, 1000)
}

func TestKongSkipPlainJSONStringBytesMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewPCG(29, 31))
	special := []byte{'"', '\\', 0x00, 0x1f, 0x20, 0x21, 0x5b, 0x5c, 0x5d, 0x7f, 0x80, 0xff, 'a'}
	naive := func(b []byte, i int) int {
		for i < len(b) && b[i] != '"' && b[i] != '\\' && b[i] >= 0x20 {
			i++
		}
		return i
	}
	for n := 0; n < 48; n++ {
		for round := 0; round < 200; round++ {
			b := make([]byte, n)
			for i := range b {
				if rng.IntN(8) == 0 {
					b[i] = special[rng.IntN(len(special))]
				} else {
					b[i] = byte(0x20 + rng.IntN(0xE0))
				}
			}
			for i := 0; i <= n; i++ {
				require.Equal(t, naive(b, i), kongSkipPlainJSONStringBytes(b, i), "字节 %x 从 %d 起", b, i)
			}
		}
	}
}

// ── gjson 钩子 ───────────────────────────────────────────────────────────

func TestKongSplitSimplePath(t *testing.T) {
	for _, tc := range []struct {
		path, first, rest string
		ok                bool
	}{
		{"model", "model", "", true},
		{"reasoning.effort", "reasoning", "effort", true},
		{"input.0.type", "input", "0.type", true},
		{"x-y_z.A9", "x-y_z", "A9", true},
		{"", "", "", false},
		{".", "", "", false},
		{"model.", "", "", false},
		{".model", "", "", false},
		{"a..b", "", "", false},
		{"input.#", "", "", false},
		{"tools.#.name", "", "", false},
		{"a|b", "", "", false},
		{"@this", "", "", false},
		{"mo*", "", "", false},
		{"m?", "", "", false},
		{"inp\\ut", "", "", false},
		{"a b", "", "", false},
	} {
		first, rest, ok := kongSplitSimplePath(tc.path)
		require.Equal(t, tc.ok, ok, tc.path)
		if ok {
			require.Equal(t, tc.first, first, tc.path)
			require.Equal(t, tc.rest, rest, tc.path)
		}
	}
}

func TestKongGjsonHookMatchesNative(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	pad := `"pad":"` + strings.Repeat("x", kongBodyRegistryMinBytes) + `"`
	plain := `{"a":1,"b":{"c":[1,2,{"d":"x"}],"e":"f"},"s":"str","n":null,"t":true,"arr":[{"k":1},{"k":2}],"sp" : [ 1 , 2 ] ,` + pad + `}`
	bodies := []struct {
		name    string
		body    string
		handled bool // 顶层单段路径是否由钩子接管
	}{
		{"plain", plain, true},
		{"leading_space", " \n" + plain, true},
		{"trailing_space", plain + " \n", true},
		{"duplicate", `{"a":1,"a":2,` + pad + `}`, false},
		{"escaped_key", kongBFEsc(`{"%u0061":1,"a":2,`) + pad + `}`, false},
		{"array_top", `[` + plain + `]`, false},
		{"duplicate_nested", `{"a":{},"a":{"b":1},` + pad + `}`, false},
		{"string_looks_like_array", `{"a":"[123]","b":"{\"c\":1}",` + pad + `}`, true},
		{"too_many_members", kongBFManyMembers(kongBodyIndexMaxMembers+1) + pad + `}`, false},
		{"members_at_limit", kongBFManyMembers(kongBodyIndexMaxMembers-1) + pad + `}`, true},
		{"truncated", plain[:len(plain)-1], false},
		{"trailing_garbage", plain + "x", false},
	}
	paths := []string{"a", "a.b", "a.0", "b.c.0", "m0", "m254", "m300", "b", "b.c", "b.c.2", "b.c.2.d", "b.e", "b.missing", "s", "s.x", "n", "n.x", "t", "arr", "arr.1.k", "arr.5",
		"sp", "sp.0", "missing", "missing.x", "pad", "a.b.c", "0", "b.#", "arr.#.k", "b|c", "@this", "a*", ""}
	for _, tc := range bodies {
		body := []byte(tc.body)
		release := KongRegisterRequestBody(body)
		view := kongBytesView(body)
		require.Equal(t, gjson.ValidNative(view), gjson.Valid(view), tc.name)
		require.Equal(t, gjson.ValidNative(view), gjson.ValidBytes(body), tc.name)
		_, handled := kongGjsonGetHook(view, "a")
		require.Equal(t, tc.handled, handled, tc.name)
		for _, path := range paths {
			require.Equal(t, gjson.GetNative(view, path), gjson.Get(view, path), "%s: %s", tc.name, path)
			require.Equal(t, gjson.GetNative(view, path), gjson.GetBytes(body, path), "%s: %s", tc.name, path)
		}
		// 钩子内部只走原生入口：在遍历回调里再查同一份请求体，不会重入、结果不变。
		gjson.Parse(view).ForEach(func(_, _ gjson.Result) bool {
			require.Equal(t, gjson.GetNative(view, "b.e"), gjson.Get(view, "b.e"), tc.name)
			require.Equal(t, gjson.ValidNative(view), gjson.Valid(view), tc.name)
			return true
		})
		for _, path := range []string{"a", "a.b", "b.e", "missing", "arr.0.k"} {
			restore := KongSetOpenAIBodyFastpathForTest(false)
			want, wantErr := sjson.SetBytes(body, path, "v")
			restore()
			got, gotErr := sjson.SetBytes(body, path, "v")
			require.Equal(t, kongBFErr(wantErr), kongBFErr(gotErr), "%s: %s", tc.name, path)
			require.Equal(t, string(want), string(got), "%s: %s", tc.name, path)
		}
		release()
	}
}

// kongBFManyMembers 返回一个带 n 个顶层成员、末尾留着逗号的对象开头。
func kongBFManyMembers(n int) string {
	var b strings.Builder
	_ = b.WriteByte('{')
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `"m%d":%d,`, i, i)
	}
	return b.String()
}

// ── 登记表 ───────────────────────────────────────────────────────────────

func TestKongBodyRegistryLifetime(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	body := bytes.Repeat([]byte("a"), 2*kongBodyRegistryMinBytes)
	require.Nil(t, kongBodyEntryFor(body))

	first := KongRegisterRequestBody(body)
	second := KongRegisterRequestBody(body)
	require.NotNil(t, kongBodyEntryFor(body))
	require.Nil(t, kongBodyEntryFor(body[:len(body)-1]), "同一起始地址、不同长度的切片不是同一份请求体")
	first()
	first()
	require.NotNil(t, kongBodyEntryFor(body), "重复注销只算一次")
	second()
	require.Nil(t, kongBodyEntryFor(body))

	// 同一起始地址已经登记了另一个长度时，这一份不登记，注销也不影响已有的那份。
	whole := KongRegisterRequestBody(body)
	shorter := KongRegisterRequestBody(body[:len(body)-1])
	require.Nil(t, kongBodyEntryFor(body[:len(body)-1]))
	shorter()
	require.NotNil(t, kongBodyEntryFor(body))
	whole()
	require.Nil(t, kongBodyEntryFor(body))

	small := body[:kongBodyRegistryMinBytes-1]
	releaseSmall := KongRegisterRequestBody(small)
	require.Nil(t, kongBodyEntryFor(small))
	releaseSmall()

	restore := KongSetOpenAIBodyFastpathForTest(false)
	releaseOff := KongRegisterRequestBody(body)
	require.Nil(t, kongBodyEntryFor(body), "快速路径全关时不登记")
	releaseOff()
	restore()
}

func TestKongBodyRegistryDetectsMutation(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	body := bytes.Repeat([]byte("a"), kongBodyRegistryMinBytes)
	release := KongRegisterRequestBody(body)
	body[0] = 'b'
	require.PanicsWithValue(t, "kong: 登记的请求体被原地修改", release)
	require.Nil(t, kongBodyEntryFor(body))
}

// 注销之后，注销函数本身不再引用条目：调用方把它挂在 defer 上时，提前注销就结束了登记带来的保活。
func TestKongBodyRegistryReleaseDropsEntry(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	body := bytes.Repeat([]byte("a"), 1<<20)

	first := KongRegisterRequestBody(body)
	second := KongRegisterRequestBody(body)
	entry := weak.Make(kongBodyEntryFor(body))
	require.NotNil(t, entry.Value())
	first()
	first()
	runtime.GC()
	require.NotNil(t, entry.Value(), "还有一份登记，条目不能被回收")
	second()
	runtime.GC()
	runtime.GC()
	require.Nil(t, entry.Value(), "全部注销后，已调用过的注销函数不应再保活条目")
	runtime.KeepAlive(first)
	runtime.KeepAlive(second)
}

// 嵌套极深的请求体交还原生实现：gjson 的 Valid 递归没有深度上限，快速路径不能先于原代码去调用它（compact 请求的
// 原文，原代码只做字段提取，不做递归校验）。子进程里把栈上限压到 16 MiB 再走一遍查询与各道门：encoding/json 在
// 10000 层处停下，用不了这么多栈；调用了 gjson 的 Valid 就会以栈溢出退出。
func TestKongDeepBodyLeftToNative(t *testing.T) {
	if os.Getenv("KONG_BF_DEEP_CHILD") == "1" {
		kongBFDeepBodyChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKongDeepBodyLeftToNative$", "-test.count=1")
	cmd.Env = append(os.Environ(), "KONG_BF_DEEP_CHILD=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func kongBFDeepBodyChild(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	const depth = 200000
	body := []byte(`{"model":"gpt-6-sol","input":[],"ignored":` + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + `}`)
	release := KongRegisterRequestBody(body)
	defer release()
	view := kongBytesView(body)

	previous := debug.SetMaxStack(16 << 20)
	defer debug.SetMaxStack(previous)
	_, handled := kongGjsonValidHook(view)
	require.False(t, handled)
	_, handled = kongGjsonGetHook(view, "model")
	require.False(t, handled)
	require.Equal(t, "gpt-6-sol", gjson.Get(view, "model").String())
	require.False(t, gjson.Get(view, "stream").Exists())
	require.False(t, HasCompactionTriggerInInput(body))
	require.False(t, kongInputItemIDsClean(body))
	require.False(t, kongNoInputItemMetadataPassthrough(body, gjson.Get(view, "input")))
	require.True(t, KongCodexCallOutputBootstrapMayApply(body, func(map[string]any) bool { return false }))
	require.False(t, kongBodyDecodable(body))
}

func TestKongBodyRegistryConcurrent(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	pad := strings.Repeat("x", kongBodyRegistryMinBytes)
	bodies := make([][]byte, 6)
	for i := range bodies {
		bodies[i] = []byte(fmt.Sprintf(`{"id":%d,"nested":{"v":%d},"pad":"%s"}`, i, i*10, pad))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(worker), 1))
			for round := 0; round < 300; round++ {
				i := rng.IntN(len(bodies))
				body := bodies[i]
				release := KongRegisterRequestBody(body)
				view := kongBytesView(body)
				if kongBodyEntryFor(body) == nil {
					errs <- fmt.Errorf("第 %d 份请求体登记后查不到", i)
					release()
					return
				}
				if got, want := gjson.Get(view, "nested.v"), gjson.GetNative(view, "nested.v"); got.Raw != want.Raw || got.Index != want.Index {
					errs <- fmt.Errorf("第 %d 份请求体的钩子结果与原生不同", i)
					release()
					return
				}
				release()
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	for _, body := range bodies {
		require.Nil(t, kongBodyEntryFor(body), "全部注销后不应还在登记表里")
	}
}
