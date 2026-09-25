//go:build unit

package service

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kongcorpus"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

// ── §2.1 请求视图 ──────────────────────────────────────────────────────────

func kongPointsInto(s string, body []byte) bool {
	if len(s) == 0 || len(body) == 0 {
		return false
	}
	p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	start := uintptr(unsafe.Pointer(unsafe.SliceData(body)))
	return p >= start && p < start+uintptr(len(body))
}

func TestKongRequestViewStringsDetached(t *testing.T) {
	body := []byte(`{"model":" gpt-6-sol ","stream":true,"prompt_cache_key":"pck-1","previous_response_id":"resp_1","service_tier":"priority","reasoning":{"effort":"high"}}`)
	defer KongSetOpenAIBodyFastpathForTest(false)()
	want := newOpenAIRequestView(body)
	restore := KongSetOpenAIBodyFastpathForTest(true)
	got := newOpenAIRequestView(body)
	restore()
	require.Equal(t, want.Model, got.Model)
	require.Equal(t, want.PromptCacheKey, got.PromptCacheKey)
	require.Equal(t, want.PreviousResponseID, got.PreviousResponseID)
	require.Equal(t, want.ServiceTier, got.ServiceTier)
	require.Equal(t, want.ReasoningEffort, got.ReasoningEffort)
	require.Equal(t, want.Stream, got.Stream)
	for name, value := range map[string]string{
		"model": got.Model, "prompt_cache_key": got.PromptCacheKey, "previous_response_id": got.PreviousResponseID,
		"service_tier": got.ServiceTier, "reasoning_effort": got.ReasoningEffort,
	} {
		require.False(t, kongPointsInto(value, body), "%s 仍是请求体的子串", name)
	}
	require.True(t, kongPointsInto(want.PromptCacheKey, body), "前提：关闭时字段是请求体的子串")
}

// ── §2.4 item id 清洗 ──────────────────────────────────────────────────────

// kongSanitizeModes 是比较 item id 清洗的几种开关组合：全关是原函数；全开先过判否门再走快路径；关掉判否门让快路径
// 也处理本来会被门放行的请求体。
var kongSanitizeModes = []struct {
	name string
	set  func() func()
}{
	{"fastpath_on", func() func() { return KongSetOpenAIBodyFastpathForTest(true) }},
	{"splice_without_gate", func() func() { return KongSetOpenAIBodyFastpathItemsOffForTest("item_ids") }},
}

func kongRequireSanitizeSame(t *testing.T, body []byte, label string) {
	t.Helper()
	// 原函数与每种模式各用一份独立的输入：快路径若原地改了输入，期望值不会跟着变，输入快照也会对不上。
	input := bytes.Clone(body)
	restore := KongSetOpenAIBodyFastpathForTest(false)
	wantOut, wantChanged, wantErr := sanitizeOpenAIResponsesInputItemIDs(input)
	restore()
	want := bytes.Clone(wantOut)
	require.Equal(t, body, input, "%s：原函数修改了输入", label)
	for _, mode := range kongSanitizeModes {
		input := bytes.Clone(body)
		restore := mode.set()
		got, gotChanged, gotErr := sanitizeOpenAIResponsesInputItemIDs(input)
		restore()
		require.Equal(t, fmt.Sprint(wantErr), fmt.Sprint(gotErr), "%s/%s：错误不同", label, mode.name)
		require.Equal(t, wantChanged, gotChanged, "%s/%s：changed 不同", label, mode.name)
		require.True(t, bytes.Equal(want, got), "%s/%s：结果不同\nwant %s\ngot  %s", label, mode.name, want, got)
		require.Equal(t, body, input, "%s/%s：输入被原地修改", label, mode.name)
		if !wantChanged && len(input) > 0 {
			require.True(t, unsafe.SliceData(input) == unsafe.SliceData(got), "%s/%s：无改动时应原样返回请求体", label, mode.name)
		}
	}
}

func kongWrapInput(input string) []byte {
	return []byte(`{"model":"gpt-6-sol","input":` + input + `,"stream":true}`)
}

func TestKongSpliceInputItemIDsMatchesOriginalOnCorpus(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		for _, shape := range []kongcorpus.Shape{kongcorpus.ShapeSol, kongcorpus.ShapeLite} {
			for _, ids := range []bool{false, true} {
				o := kongcorpus.Options{Shape: shape, Seed: seed, TargetBytes: 200 << 10, ItemIDs: ids, Images: int(seed % 3), ImageBytes: 8 << 10}
				kongRequireSanitizeSame(t, kongcorpus.Request(o), fmt.Sprintf("seed=%d shape=%d ids=%v", seed, shape, ids))
			}
		}
	}
}

func TestKongSpliceInputItemIDsEdges(t *testing.T) {
	cases := map[string]string{
		"id_first":           `[{"id":"ctco_1","type":"custom_tool_call_output","call_id":"c1","output":"x"}]`,
		"id_last":            `[{"type":"custom_tool_call_output","call_id":"c1","output":"x","id":"ctco_1"}]`,
		"id_only_member":     `[{"id":"bad"},{"type":"message","id":"bad"}]`,
		"spaced":             "[{ \"type\" : \"custom_tool_call_output\" , \"id\" : \"ctco_1\" ,\n \"call_id\":\"c1\"}]",
		"spaced_first":       "[{  \"id\"\t:\t\"bad\"  ,  \"type\":\"message\",\"role\":\"user\",\"content\":[]}]",
		"spaced_last":        "[{\"type\":\"message\",\"content\":[] ,  \"id\" : \"bad\"  }]",
		"escaped_key":        `[{"type":"custom_tool_call_output","i\u0064":"ctco_1","call_id":"c1"}]`,
		"escaped_quote_key":  `[{"type":"message","a\"b":1,"id":"bad"}]`,
		"duplicate_id_bad":   `[{"type":"custom_tool_call_output","id":"ctco_1","id":"fc_2","call_id":"c1"}]`,
		"duplicate_id_good":  `[{"type":"custom_tool_call_output","id":"fc_1","id":"ctco_2","call_id":"c1"}]`,
		"empty_id":           `[{"type":"message","id":"","role":"user","content":[]}]`,
		"long_id":            `[{"type":"reasoning","id":"rs_` + strings.Repeat("x", 70) + `","summary":[]}]`,
		"number_id":          `[{"type":"message","id":7,"role":"user"}]`,
		"null_id":            `[{"type":"message","id":null,"role":"user"}]`,
		"object_id":          `[{"type":"message","id":{"x":"y"},"role":"user"}]`,
		"nested_id_only":     `[{"type":"message","content":[{"type":"input_text","id":"bad","text":"x"}]}]`,
		"call_id_only":       `[{"type":"reasoning","id":"rs_keep","call_id":"remove","summary":[]}]`,
		"call_id_null":       `[{"type":"message","call_id":null,"role":"user","content":[]}]`,
		"call_id_and_id":     `[{"id":"bad","call_id":"x","type":"message","role":"user","content":[]}]`,
		"call_id_elsewhere":  `[{"type":"custom_tool_call_output","id":"ctco_1","call_id":"c"},{"type":"message","call_id":"x","content":[]}]`,
		"unknown_type":       `[{"type":"mystery","id":"anything"}]`,
		"no_type":            `[{"id":"anything"}]`,
		"spaced_type":        `[{"type":" message ","id":"bad"}]`,
		"non_objects":        `[1,"str",null,[{"id":"bad"}],{"type":"message","id":"bad"}]`,
		"array_spacing":      "[ {\"type\":\"message\",\"id\":\"bad\"} ,\n {\"type\":\"message\",\"id\":\"msg_ok\"} ]",
		"clean_array_spaced": "[ {\"type\":\"message\",\"id\":\"msg_ok\"} , {\"type\":\"reasoning\",\"id\":\"rs_ok\"} ]",
		"empty_array":        `[]`,
		"string_input":       `"hello"`,
		"unicode":            `[{"type":"message","id":"bad","content":[{"type":"input_text","text":"用户 \u00e9 \"q\""}]}]`,
		"many":               "[" + strings.Repeat(`{"type":"custom_tool_call_output","id":"ctco_x","call_id":"c","output":"o"},`, 300) + `{"type":"message","id":"msg_1"}]`,
	}
	for name, input := range cases {
		kongRequireSanitizeSame(t, kongWrapInput(input), name)
	}
	kongRequireSanitizeSame(t, []byte(`{"model":"m"}`), "no_input")
	kongRequireSanitizeSame(t, []byte(` { "input" : [{"type":"message","id":"bad"}] , "model":"m" } `), "top_level_spacing")
}

// kongRandomItem 随机拼一个 input 项：类型、id、call_id 的有无与取值、成员顺序、空白都随机。
func kongRandomItem(rng *rand.Rand) string {
	if rng.IntN(20) == 0 {
		return []string{`1`, `"text"`, `null`, `[]`, `{}`}[rng.IntN(5)]
	}
	types := []string{"message", "reasoning", "custom_tool_call_output", "custom_tool_call", "function_call", "function_call_output",
		"web_search_call", "tool_search_call", "image_generation_call", "local_shell_call", "agent_message", "additional_tools", "mystery", " message "}
	ids := []string{`"ctco_abc"`, `"fc_1"`, `"fco_1"`, `"ctc_1"`, `"msg_1"`, `"rs_1"`, `"ws_1"`, `"tsc_1"`, `"bad"`, `""`,
		`"rs_` + strings.Repeat("y", 70) + `"`, `7`, `null`, `{"a":"b"}`, `"m\u0073g_1"`}
	var members []string
	if rng.IntN(10) > 0 {
		members = append(members, `"type":"`+types[rng.IntN(len(types))]+`"`)
	}
	if rng.IntN(5) > 0 {
		key := `"id"`
		if rng.IntN(15) == 0 {
			key = `"i\u0064"`
		}
		members = append(members, key+":"+ids[rng.IntN(len(ids))])
		if rng.IntN(12) == 0 {
			members = append(members, `"id":`+ids[rng.IntN(len(ids))])
		}
	}
	if rng.IntN(6) == 0 {
		members = append(members, `"call_id":`+[]string{`"call_1"`, `null`, `""`}[rng.IntN(3)])
	}
	if rng.IntN(2) == 0 {
		members = append(members, `"output":"`+strings.Repeat("o", rng.IntN(40))+`"`)
	}
	if rng.IntN(3) == 0 {
		members = append(members, `"content":[{"type":"input_text","id":"inner","text":"t"}]`)
	}
	rng.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	space := func() string { return []string{"", "", "", " ", "\n", "\t ", "  "}[rng.IntN(7)] }
	var b strings.Builder
	b.WriteString("{" + space())
	for i, member := range members {
		if i > 0 {
			b.WriteString(space() + "," + space())
		}
		colon := strings.Index(member, ":")
		if member[colon-1] == '"' {
			b.WriteString(member[:colon] + space() + ":" + space() + member[colon+1:])
		} else {
			b.WriteString(member)
		}
	}
	b.WriteString(space() + "}")
	return b.String()
}

func TestKongSpliceInputItemIDsRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261015, 7))
	for i := range 3000 {
		n := rng.IntN(8)
		items := make([]string, n)
		for j := range items {
			items[j] = kongRandomItem(rng)
		}
		sep := []string{",", " , ", ",\n"}[rng.IntN(3)]
		kongRequireSanitizeSame(t, kongWrapInput("["+strings.Join(items, sep)+"]"), fmt.Sprintf("random#%d", i))
	}
}

// TestKongSJSONDeleteRangeMatchesSJSON 直接比较删除区间与 sjson.DeleteBytes 的结果。
func TestKongSJSONDeleteRangeMatchesSJSON(t *testing.T) {
	rng := rand.New(rand.NewPCG(99, 1))
	objects := []string{
		`{"id":"x"}`, `{ "id" : "x" }`, `{"a":1,"id":"x"}`, `{"id":"x","a":1}`, `{"id":"x" , "a":1}`, "{\"a\":1 ,\n\"id\":\"x\"\n}",
		`{"a":{"id":"inner"},"id":"x"}`, `{"a\"b":1,"id":"x"}`, `{"a\\":1,"id":"x"}`, `{"i\u0064":"x","b":2}`, `{"id":"x","id":"y"}`,
		`{"b":[1,2,{"c":"d"}],"id":"x","e":null}`, `{"id":{"nested":"y"},"a":1}`, `{"id":7}`,
	}
	for range 2000 {
		objects = append(objects, kongRandomItem(rng))
	}
	for _, obj := range objects {
		want, err := sjson.DeleteBytes([]byte(obj), "id")
		from, to, ok := kongSJSONDeleteRange(obj, "id")
		if !ok {
			require.NoError(t, err, obj)
			require.Equal(t, obj, string(want), "定位不到时 sjson 原样返回：%s", obj)
			continue
		}
		require.NoError(t, err, obj)
		require.Equal(t, string(want), obj[:from]+obj[to:], obj)
	}
}

// ── §2.5 身份改写 ──────────────────────────────────────────────────────────

func kongWithSpareCap(b []byte, spare int) []byte {
	out := make([]byte, len(b), len(b)+spare)
	copy(out, b)
	return out
}

func TestKongSetIdentityRawMatchesSJSON(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	bodies := []string{
		`{"model":"m","client_metadata":{"a":"b"}}`,
		`{"client_metadata":{"a":"b"},"model":"m"}`,
		`{"model":"m","client_metadata" : {"a":"b"} , "x":1}`,
		`{"model":"m"}`,
		`{"model":"m","client_metadata":{"a":"b"},"client_metadata":{"c":"d"}}`,
		`{"model":"m","client\u005fmetadata":{"a":"b"}}`,
		`{"model":"m","client_metadata":null}`,
	}
	raw := []byte(`{"x-codex-installation-id":"scoped-1","session_id":"s"}`)
	for _, body := range bodies {
		want, wantErr := sjson.SetRawBytes([]byte(body), "client_metadata", raw)
		input := []byte(body)
		got, gotErr := kongSetIdentityRaw(sjson.SetRawBytes, input, "client_metadata", raw)
		require.Equal(t, fmt.Sprint(wantErr), fmt.Sprint(gotErr), body)
		require.Equal(t, string(want), string(got), body)
		require.Equal(t, body, string(input), "输入不应被修改：%s", body)
	}
}

func TestKongSetIdentityStringMatchesSJSON(t *testing.T) {
	defer KongSetOpenAIBodyFastpathForTest(true)()
	bodies := []string{
		`{"model":"m","prompt_cache_key":"abc","client_metadata":{}}`,
		`{"prompt_cache_key":"abc","model":"m"}`,
		`{"model":"m","prompt_cache_key" : "abc" ,"x":1}`,
		`{"model":"m"}`,
		`{"model":"m","prompt_cache_key":"abc","prompt_cache_key":"def"}`,
		`{"model":"m","prompt\u005fcache_key":"abc"}`,
		`{"model":"m","prompt_cache_key":"` + strings.Repeat("k", 300) + `"}`,
	}
	values := []string{"scoped:abc", "s", strings.Repeat("v", 200), "needs \"escape\"", "用户", "tab\t"}
	for _, body := range bodies {
		for _, value := range values {
			want, wantErr := sjson.SetBytes([]byte(body), "prompt_cache_key", value)
			for _, spare := range []int{0, 8, 512} {
				for _, private := range []bool{false, true} {
					input := kongWithSpareCap([]byte(body), spare)
					got, gotErr := kongSetIdentityString(sjson.SetBytes, input, private, "prompt_cache_key", value)
					label := fmt.Sprintf("body=%s value=%q spare=%d private=%v", body, value, spare, private)
					require.Equal(t, fmt.Sprint(wantErr), fmt.Sprint(gotErr), label)
					require.Equal(t, string(want), string(got), label)
					if !private {
						require.Equal(t, body, string(input), "不是私有副本时不应修改输入：%s", label)
					}
				}
			}
		}
	}
}

func TestKongIdentitySpliceMatchesOriginal(t *testing.T) {
	accounts := []*Account{
		{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}},
		{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "acct-12", "chatgpt_user_id": "user-12"}},
	}
	var bodies [][]byte
	for seed := uint64(1); seed <= 4; seed++ {
		bodies = append(bodies, kongcorpus.Request(kongcorpus.Options{Seed: seed, TargetBytes: 60 << 10, ItemIDs: seed%2 == 0}))
		bodies = append(bodies, kongcorpus.Request(kongcorpus.Options{Seed: seed, TargetBytes: 10 << 10, OmitPromptCacheKey: true}))
	}
	bodies = append(bodies,
		[]byte(`{"model":"m","prompt_cache_key":"pck","input":[]}`),
		[]byte(`{"model":"m","client_metadata":{"session_id":"sess-1","x-codex-window-id":"w"},"prompt_cache_key":"sess-1"}`),
		[]byte(`{"model":"m","client_metadata":"not-object","prompt_cache_key":"p"}`),
		[]byte(`{"model":"m","client_metadata":{},"prompt_cache_key":"pck"}`),
		[]byte(`{"model":"m","client_metadata":{"other":"x"},"prompt_cache_key":"pck","stream":true}`),
	)
	// 每份再配一份容量富余的副本：client_metadata 没改写时 next 就是传进来的请求体，调用点若误把它当私有副本，
	// 原地写回会改坏输入，下面的输入不变断言就会失败。
	for _, body := range append([][]byte(nil), bodies...) {
		bodies = append(bodies, kongWithSpareCap(body, 512))
	}
	for _, account := range accounts {
		for i, body := range bodies {
			original := append([]byte(nil), body...)
			restore := KongSetOpenAIBodyFastpathForTest(false)
			want, wantChanged, wantErr := applyCodexAccountIdentityClientMetadataRaw(body, account, 77)
			restore()
			restore = KongSetOpenAIBodyFastpathForTest(true)
			got, gotChanged, gotErr := applyCodexAccountIdentityClientMetadataRaw(body, account, 77)
			restore()
			label := fmt.Sprintf("account=%d body#%d", account.ID, i)
			require.Equal(t, fmt.Sprint(wantErr), fmt.Sprint(gotErr), label)
			require.Equal(t, wantChanged, gotChanged, label)
			require.Equal(t, string(want), string(got), label)
			require.Equal(t, original, body, "%s：输入被修改", label)
		}
	}
}
