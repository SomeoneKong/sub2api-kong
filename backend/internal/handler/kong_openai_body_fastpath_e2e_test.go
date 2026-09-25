//go:build unit

package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kongcorpus"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 请求体快速路径的端到端对照：同一批合成请求经 handler → Forward → 透传发往假上游，比较每一次出站请求、
// 客户端响应、用量行、gin 上下文与业务日志。对照基准在改动前的原代码上录下，存在
// testdata/kong_openai_body_fastpath_golden.json；设置 KONG_BODY_FASTPATH_GOLDEN=update 重录。

const kongFPGoldenPath = "testdata/kong_openai_body_fastpath_golden.json"

type kongFPScript int

const (
	kongFPScriptOK kongFPScript = iota
	kongFPScriptRetry400
	kongFPScriptFailover
	kongFPScriptZstdReject
	kongFPScriptTurnState
)

type kongFPCase struct {
	name    string
	body    []byte
	headers http.Header
	script  kongFPScript
	// preread 为真时请求体以 PrereadBody 交给 handler：读取时不复制，流水线处理的就是 body 这份切片。
	preread bool
}

// kongFPEsc 把 %u 换成 JSON 的 \u 转义，免得源码里直接出现转义序列。
func kongFPEsc(s string) string { return strings.ReplaceAll(s, "%u", "\\u") }

func kongFPTool(name, parameters string) string {
	return `{"type":"function","name":` + name + `,"description":"extra tool","strict":false,"parameters":` + parameters + `}`
}

func kongFPCases() []kongFPCase {
	const mid = 300 << 10
	sol := func(seed uint64, target int) kongcorpus.Options {
		return kongcorpus.Options{Shape: kongcorpus.ShapeSol, Seed: seed, TargetBytes: target}
	}
	lite := func(seed uint64, target int) kongcorpus.Options {
		return kongcorpus.Options{Shape: kongcorpus.ShapeLite, Seed: seed, TargetBytes: target}
	}
	build := func(name string, o kongcorpus.Options, script kongFPScript) kongFPCase {
		return kongFPCase{name: name, body: kongcorpus.Request(o), headers: kongcorpus.Headers(o), script: script}
	}
	with := func(o kongcorpus.Options, edit func(*kongcorpus.Options)) kongcorpus.Options {
		edit(&o)
		return o
	}
	automationOutput := "Automation: Daily sync\nAutomation ID: daily-sync\nAutomation memory: $CODEX_HOME/automations/daily-sync/memory.md\nLast run: never\n\nRun the sync."
	delegationOutput := "<codex_delegation><source_thread_id>thread-1</source_thread_id><input>Please review the patch</input></codex_delegation>"
	bigString := kongcorpus.Quote(strings.Repeat("line with \"quotes\" and \\ backslash\n", 8000))

	cases := []kongFPCase{
		build("sol_big", sol(1, 1100<<10), kongFPScriptOK),
		build("lite_big", lite(2, 1100<<10), kongFPScriptOK),
		build("sol_mid", sol(3, mid), kongFPScriptOK),
		build("lite_mid", lite(4, mid), kongFPScriptOK),
		build("sol_small", sol(5, 20<<10), kongFPScriptOK),
		build("sol_nonstream", with(sol(6, mid), func(o *kongcorpus.Options) { o.NonStream = true }), kongFPScriptOK),
		build("sol_python_tool", with(sol(7, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(`"python"`, `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}`)}
		}), kongFPScriptOK),
		build("sol_python_tool_spaced", with(sol(8, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(`" PyThon "`, `{"type":"object","properties":{}}`)}
		}), kongFPScriptOK),
		build("sol_python_tool_escaped", with(sol(9, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(kongFPEsc(`"%u0070ython"`), `{"type":"object","properties":{}}`)}
		}), kongFPScriptOK),
		build("sol_orphan_output", with(sol(10, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"function_call_output","call_id":"call_orphan_1","output":"stale"}`}
		}), kongFPScriptOK),
		build("sol_orphan_surrogate", with(sol(11, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{
				kongFPEsc(`{"type":"function_call","name":"shell","arguments":"{}","call_id":"%ud800%u0061"}`),
				kongFPEsc(`{"type":"function_call_output","call_id":"%ufffd","output":"ok"}`),
			}
		}), kongFPScriptOK),
		build("sol_orphan_escaped_key", with(sol(12, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{kongFPEsc(`{"typ%u0065":"function_call_output","call_id":"call_orphan_2","output":"x"}`)}
		}), kongFPScriptOK),
		build("sol_trigger_misplaced", with(sol(13, mid), func(o *kongcorpus.Options) {
			o.PrefixItems = []string{`{"type":"compaction_trigger"}`}
		}), kongFPScriptOK),
		build("sol_trigger_last", with(sol(14, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"compaction_trigger"}`}
		}), kongFPScriptOK),
		build("sol_automation", with(sol(15, 0), func(o *kongcorpus.Options) {
			o.NoHistory = true
			o.SuffixItems = []string{`{"type":"function_call_output","namespace":"codex_app","name":"automation_update","output":` + kongcorpus.Quote(automationOutput) + `}`}
		}), kongFPScriptOK),
		build("sol_delegation", with(sol(16, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":` + kongcorpus.Quote(delegationOutput) + `}`}
		}), kongFPScriptOK),
		build("sol_lookaround", with(sol(17, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(`"lookup_id"`, `{"type":"object","properties":{"id":{"type":"string","pattern":"^(?=abc)[a-z]+$"}}}`)}
		}), kongFPScriptOK),
		build("sol_lookaround_escaped", with(sol(18, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(`"lookup_id"`, kongFPEsc(`{"type":"object","properties":{"id":{"type":"string","pattern":"^%u0028%u003f%u003dabc)[a-z]+$"}}}`))}
		}), kongFPScriptOK),
		build("sol_null_param_type", with(sol(19, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(`"null_root"`, `{"type":null,"properties":{"a":{"type":"string"}}}`)}
		}), kongFPScriptOK),
		build("sol_required_null", with(sol(20, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{kongFPTool(`"required_null"`, `{"type":"object","properties":{"a":{"type":"object","properties":{},"required":null}}}`)}
		}), kongFPScriptOK),
		build("sol_stream_options", with(sol(21, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"stream_options":{"include_usage":true}`}
		}), kongFPScriptOK),
		build("sol_sampling", with(sol(22, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"temperature":0.2`}
		}), kongFPScriptOK),
		build("sol_dup_reasoning", with(sol(23, mid), func(o *kongcorpus.Options) {
			o.ReasoningRaw = `{"summary":"auto"}`
			o.TopLevelTail = []string{`"reasoning":{"effort":"none"}`, `"temperature":0.2`}
		}), kongFPScriptOK),
		build("sol_previous_response_id", with(sol(24, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"previous_response_id":"resp_prev123"`}
		}), kongFPScriptOK),
		build("sol_image_generation_tool", with(sol(25, mid), func(o *kongcorpus.Options) {
			o.ExtraTools = []string{`{"type":"image_generation","size":"1024x1024"}`}
		}), kongFPScriptOK),
		build("sol_many_top_keys", with(sol(26, mid), func(o *kongcorpus.Options) {
			for i := range 300 {
				o.TopLevelTail = append(o.TopLevelTail, fmt.Sprintf(`"x_extra_%d":%d`, i, i))
			}
		}), kongFPScriptOK),
		build("sol_string_input", with(sol(27, 0), func(o *kongcorpus.Options) { o.InputRaw = bigString }), kongFPScriptOK),
		build("lite_namespace_tool", with(lite(28, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"tools":[{"type":"namespace","name":"ns_docs","description":"docs","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}]`}
		}), kongFPScriptOK),
		build("lite_namespace_spaced", with(lite(29, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"tools":[{"type":" namespace ","name":"ns_docs","description":"docs","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}]`}
		}), kongFPScriptOK),
		build("sol_retry_max_output_tokens", with(sol(30, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"max_output_tokens":1000`}
		}), kongFPScriptRetry400),
		build("sol_failover", sol(31, mid), kongFPScriptFailover),
		build("lite_failover", lite(32, mid), kongFPScriptFailover),
		build("sol_zstd_reject", sol(33, mid), kongFPScriptZstdReject),
		build("sol_turn_state", sol(34, mid), kongFPScriptTurnState),
	}

	userText := func(extra string) string {
		return `{"type":"message","role":"user",` + extra + `"content":[{"type":"input_text","text":"hi"}]}`
	}
	cases = append(cases,
		build("sol_input_metadata", with(sol(40, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{userText(`"internal_chat_message_metadata_passthrough":{"k":"v"},`)}
		}), kongFPScriptOK),
		build("sol_input_metadata_escaped_key", with(sol(41, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{userText(kongFPEsc(`"internal_chat_message_metadata_passthroug%u0068":{"k":"v"},`))}
		}), kongFPScriptOK),
		build("sol_bad_item_id", with(sol(42, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{userText(`"id":"bad-id",`)}
		}), kongFPScriptOK),
		build("sol_long_item_id", with(sol(43, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"reasoning","id":"rs_` + strings.Repeat("x", 70) + `","summary":[],"encrypted_content":"gAAAAA"}`}
		}), kongFPScriptOK),
		build("sol_message_call_id", with(sol(44, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{userText(`"call_id":"call_stray",`)}
		}), kongFPScriptOK),
		build("sol_escaped_item_type_bad_id", with(sol(45, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{kongFPEsc(`{"type":"m%u0065ssage","id":"bad","role":"user","content":[{"type":"input_text","text":"hi"}]}`)}
		}), kongFPScriptOK),
		build("sol_empty_base64_image", with(sol(46, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"data:image/png;base64,"}]}`}
		}), kongFPScriptOK),
		build("sol_escaped_empty_base64_image", with(sol(47, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{kongFPEsc(`{"type":"message","role":"user","content":[{"type":"input%u005fimage","image_url":"data:image/png;base64,"}]}`)}
		}), kongFPScriptOK),
		build("sol_reasoning_content", with(sol(48, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"thinking"}],"encrypted_content":"gAAAAB"}`}
		}), kongFPScriptOK),
		build("sol_lookaround_in_text", with(sol(49, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"match (?<year>[0-9]{4}) then (?=x) and (?:y)"}]}`}
		}), kongFPScriptOK),
		build("sol_escaped_top_key", with(sol(50, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{kongFPEsc(`"x%u0079z":1`)}
		}), kongFPScriptOK),
		build("sol_dup_stream", with(sol(51, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"stream":true`}
		}), kongFPScriptOK),
	)
	nonCodex := kongcorpus.Headers(sol(52, mid))
	nonCodex.Set("User-Agent", "curl/8.5.0")
	nonCodex.Del("originator")
	cases = append(cases, kongFPCase{name: "sol_non_codex_client", body: kongcorpus.Request(sol(52, mid)), headers: nonCodex})

	// 嵌套上万层的未知字段：普通请求照原样校验、解码；带 compaction trigger 的会转成 compact，原代码只提取白名单
	// 字段、不校验这个字段。
	deepField := `"ignored":` + strings.Repeat("[", 40000) + "0" + strings.Repeat("]", 40000)
	cases = append(cases,
		build("sol_deep_ignored_field", with(sol(53, mid), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{deepField}
		}), kongFPScriptOK),
		build("compact_deep_ignored_field", with(sol(54, mid), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{`{"type":"compaction_trigger"}`}
			o.TopLevelTail = []string{deepField}
		}), kongFPScriptOK),
	)

	controlBody := kongcorpus.Request(with(sol(35, mid), func(o *kongcorpus.Options) {
		o.SuffixItems = []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"a` + "\x01" + `b"}]}`}
	}))
	cases = append(cases, kongFPCase{name: "sol_control_char", body: controlBody, headers: kongcorpus.Headers(sol(35, mid))})

	bomBody := append([]byte("\xef\xbb\xbf"), kongcorpus.Request(sol(36, mid))...)
	cases = append(cases, kongFPCase{name: "sol_bom", body: bomBody, headers: kongcorpus.Headers(sol(36, mid))})

	garbage := []byte(`garbage{"model":"gpt-6-sol","stream":true,"input":[{"type":"compaction_trigger"}]}`)
	cases = append(cases, kongFPCase{name: "garbage_prefix_trigger", body: garbage, headers: kongcorpus.Headers(sol(37, 0))})

	truncated := kongcorpus.Request(sol(38, mid))
	cases = append(cases, kongFPCase{name: "sol_truncated", body: truncated[:len(truncated)/2], headers: kongcorpus.Headers(sol(38, mid))})

	// 线上形态：历史项带 id（custom_tool_call_output 的 ctco_ id 会被清洗）、夹 base64 图片、请求体经本地代理 zstd
	// 压缩上来。
	ids := func(o kongcorpus.Options) kongcorpus.Options {
		o.ItemIDs = true
		return o
	}
	images := func(o kongcorpus.Options) kongcorpus.Options {
		o.Images, o.ImageBytes = 6, 200<<10
		return o
	}
	cases = append(cases,
		build("sol_ids_big", ids(sol(60, 1100<<10)), kongFPScriptOK),
		build("lite_ids_big", ids(lite(61, 1100<<10)), kongFPScriptOK),
		build("sol_ids_images", images(ids(sol(62, 2500<<10))), kongFPScriptOK),
		kongFPZstdIn(build("sol_ids_images_zstd_in", images(ids(sol(63, 2500<<10))), kongFPScriptOK), kongFPZstdFrame),
		kongFPZstdIn(build("lite_ids_zstd_in", ids(lite(64, mid)), kongFPScriptOK), kongFPZstdFrame),
		kongFPZstdIn(build("sol_zstd_in_small", sol(65, 20<<10), kongFPScriptOK), kongFPZstdFrame),
		kongFPZstdIn(build("sol_zstd_in_stream", images(ids(sol(66, 2500<<10))), kongFPScriptOK), kongFPZstdStream),
		kongFPZstdIn(build("sol_zstd_in_multiframe", ids(sol(67, mid)), kongFPScriptOK), kongFPZstdMultiFrame),
		kongFPZstdIn(build("sol_zstd_in_truncated", ids(sol(68, mid)), kongFPScriptOK), kongFPZstdTruncated),
		kongFPZstdIn(build("sol_zstd_in_garbage", ids(sol(69, mid)), kongFPScriptOK), kongFPZstdGarbage),
		kongFPZstdIn(build("sol_zstd_in_empty", sol(70, 0), kongFPScriptOK), kongFPZstdEmpty),
		build("sol_ids_failover", images(ids(sol(71, mid))), kongFPScriptFailover),
		build("sol_ids_zstd_reject", images(ids(sol(72, mid))), kongFPScriptZstdReject),
		build("sol_ids_retry_max_output_tokens", with(ids(sol(73, mid)), func(o *kongcorpus.Options) {
			o.TopLevelTail = []string{`"max_output_tokens":1000`}
		}), kongFPScriptRetry400),
		kongFPZstdIn(build("sol_ids_images_zstd_in_failover", images(ids(sol(74, 2500<<10))), kongFPScriptFailover), kongFPZstdFrame),
		build("sol_ids_edges", with(ids(sol(75, mid)), func(o *kongcorpus.Options) {
			o.SuffixItems = []string{
				`{"id":"ctco_first","type":"custom_tool_call_output","call_id":"call_edge_1","output":"first"}`,
				`{"type":"custom_tool_call_output","call_id":"call_edge_2","output":"last","id":"ctco_last"}`,
				`{"type" : "custom_tool_call_output" , "id" : "ctco_spaced" ,` + "\n" + ` "call_id":"call_edge_3","output":"spaced"}`,
				kongFPEsc(`{"type":"custom_tool_call_output","i%u0064":"ctco_escaped_key","call_id":"call_edge_4","output":"escaped"}`),
				`{"type":"custom_tool_call_output","id":"ctco_dup","id":"fc_dup","call_id":"call_edge_5","output":"dup"}`,
				`{"id":"bad-first","call_id":"call_stray_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
				`{"type":"reasoning","call_id":"call_stray_2","id":"bad-mid","summary":[],"encrypted_content":"gAAAAA"}`,
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"no id"}],"call_id":"call_stray_3"}`,
			}
		}), kongFPScriptOK),
	)
	return cases
}

type kongFPZstdMode int

const (
	kongFPZstdFrame      kongFPZstdMode = iota // 本地代理的写法：EncodeAll，帧头带原始大小
	kongFPZstdStream                           // 流式编码器，帧头不带原始大小
	kongFPZstdMultiFrame                       // 前后两半各成一帧
	kongFPZstdTruncated                        // 只发前三分之二
	kongFPZstdGarbage                          // 帧后跟着不是 zstd 的字节
	kongFPZstdEmpty                            // 带 Content-Encoding 的空请求体
)

// kongFPZstdIn 把用例的请求体换成 zstd 压缩后的字节，并带上 Content-Encoding。
func kongFPZstdIn(tc kongFPCase, mode kongFPZstdMode) kongFPCase {
	newEncoder := func() *zstd.Encoder {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
		if err != nil {
			panic(err)
		}
		return encoder
	}
	plain := tc.body
	var wire []byte
	switch mode {
	case kongFPZstdFrame, kongFPZstdTruncated, kongFPZstdGarbage:
		encoder := newEncoder()
		wire = encoder.EncodeAll(plain, nil)
		_ = encoder.Close()
		if mode == kongFPZstdTruncated {
			wire = wire[:len(wire)*2/3]
		}
		if mode == kongFPZstdGarbage {
			wire = append(wire, []byte("not a zstd frame")...)
		}
	case kongFPZstdStream:
		var buf bytes.Buffer
		encoder := newEncoder()
		encoder.Reset(&buf)
		_, _ = encoder.Write(plain)
		_ = encoder.Close()
		wire = buf.Bytes()
	case kongFPZstdMultiFrame:
		encoder := newEncoder()
		half := len(plain) / 2
		wire = encoder.EncodeAll(plain[:half], nil)
		wire = encoder.EncodeAll(plain[half:], wire)
		_ = encoder.Close()
	case kongFPZstdEmpty:
		wire = []byte{}
	}
	tc.body = wire
	tc.headers = tc.headers.Clone()
	tc.headers.Set("Content-Encoding", "zstd")
	return tc
}

// ── 假上游 ───────────────────────────────────────────────────────────────

type kongFPCall struct {
	AccountID       int64               `json:"account_id"`
	Method          string              `json:"method"`
	URL             string              `json:"url"`
	Headers         map[string][]string `json:"headers"`
	ContentEncoding string              `json:"content_encoding,omitempty"`
	Wire            kongFPDigest        `json:"wire"`
	Plain           kongFPDigest        `json:"plain"`
}

type kongFPUpstream struct {
	service.HTTPUpstream
	script kongFPScript
	mu     sync.Mutex
	calls  []kongFPCall
}

func (u *kongFPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	wire, _ := io.ReadAll(req.Body)
	plain := wire
	encoding := req.Header.Get("Content-Encoding")
	if encoding == "zstd" {
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		plain, err = decoder.DecodeAll(wire, nil)
		decoder.Close()
		if err != nil {
			return nil, err
		}
	}
	u.mu.Lock()
	index := len(u.calls)
	u.calls = append(u.calls, kongFPCall{
		AccountID:       accountID,
		Method:          req.Method,
		URL:             req.URL.String(),
		Headers:         kongFPHeaders(req.Header),
		ContentEncoding: encoding,
		Wire:            kongFPDigestOf(wire),
		Plain:           kongFPDigestOf(plain),
	})
	u.mu.Unlock()
	return u.respond(index, plain), nil
}

func (u *kongFPUpstream) respond(index int, plain []byte) *http.Response {
	jsonResponse := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{fmt.Sprintf("req_err_%d", index)}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	if index == 0 {
		switch u.script {
		case kongFPScriptRetry400:
			return jsonResponse(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: 'max_output_tokens'"}}`)
		case kongFPScriptFailover:
			return jsonResponse(529, `{"error":{"type":"server_error","message":"upstream overloaded"}}`)
		case kongFPScriptZstdReject:
			return jsonResponse(http.StatusUnsupportedMediaType, `{"error":{"message":"unsupported content encoding"}}`)
		}
	}
	header := http.Header{
		"Content-Type": []string{"text/event-stream"},
		"X-Request-Id": []string{fmt.Sprintf("req_ok_%d", index)},
	}
	if u.script == kongFPScriptTurnState {
		header.Set("X-Codex-Turn-State", "ts_kong_fixture")
	}
	model := gjson.GetBytes(plain, "model").String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(kongcorpus.SSEResponse(uint64(index)+1, model, 40))),
	}
}

type kongFPUsageRepo struct {
	service.UsageLogRepository
	mu   sync.Mutex
	logs []*service.UsageLog
}

func (r *kongFPUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return true, nil
}

// ── 装配 ─────────────────────────────────────────────────────────────────

func newKongFPHandler(t testing.TB, upstream service.HTTPUpstream, usageRepo service.UsageLogRepository) *OpenAIGatewayHandler {
	t.Helper()
	account := func(id int64, priority int) service.Account {
		return service.Account{
			ID:          id,
			Name:        fmt.Sprintf("kong-fastpath-%d", id),
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Priority:    priority,
			Credentials: map[string]any{
				"access_token":       fmt.Sprintf("token-%d", id),
				"chatgpt_account_id": fmt.Sprintf("chatgpt-account-%d", id),
				"chatgpt_user_id":    fmt.Sprintf("chatgpt-user-%d", id),
			},
			Extra: map[string]any{"openai_passthrough": true},
		}
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{account(1, 0), account(2, 1)}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil,
		upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

func newKongFPContext(tc kongFPCase) (*gin.Context, *httptest.ResponseRecorder) {
	groupID := int64(4242)
	var reader io.Reader = bytes.NewReader(tc.body)
	if tc.preread {
		reader = httputil.NewPrereadBody(tc.body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", reader)
	for key, values := range tc.headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      77,
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
		User:    &service.User{ID: 700},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 700, Concurrency: 0})
	return c, rec
}

// ── 结果 ─────────────────────────────────────────────────────────────────

type kongFPDigest struct {
	Len    int    `json:"len"`
	SHA256 string `json:"sha256"`
	Text   string `json:"text,omitempty"`
}

func kongFPDigestOf(data []byte) kongFPDigest {
	sum := sha256.Sum256(data)
	d := kongFPDigest{Len: len(data), SHA256: hex.EncodeToString(sum[:])}
	if len(data) <= 2048 {
		d.Text = string(data)
	}
	return d
}

type kongFPLog struct {
	Level     string         `json:"level"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields,omitempty"`
}

type kongFPOutcome struct {
	Upstream        []kongFPCall        `json:"upstream"`
	Status          int                 `json:"status"`
	ResponseHeaders map[string][]string `json:"response_headers"`
	ResponseBody    kongFPDigest        `json:"response_body"`
	Usage           []any               `json:"usage"`
	ContextKeys     map[string]any      `json:"context_keys"`
	Logs            []kongFPLog         `json:"logs"`
}

// kongFPVolatileKey 是随时间或随机源变化的字段名，比较前统一屏蔽。
var kongFPVolatileKey = regexp.MustCompile(`(?i)(latency|duration|elapsed|_ms$|firsttoken|time|_at$|created|timestamp|started)`)

func kongFPMask(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			if kongFPVolatileKey.MatchString(key) {
				out[key] = "<masked>"
				continue
			}
			out[key] = kongFPMask(inner)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = kongFPMask(inner)
		}
		return out
	case string:
		if len(typed) > 2048 {
			sum := sha256.Sum256([]byte(typed))
			return fmt.Sprintf("<len %d sha256 %s>", len(typed), hex.EncodeToString(sum[:]))
		}
		return typed
	default:
		return value
	}
}

func kongFPJSONValue(value any) any {
	if raw, ok := value.([]byte); ok {
		digest := kongFPDigestOf(raw)
		digest.Text = ""
		return digest
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("<%T>", value)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Sprintf("<%T>", value)
	}
	return kongFPMask(decoded)
}

func kongFPHeaders(header http.Header) map[string][]string {
	out := make(map[string][]string, len(header))
	for key, values := range header {
		out[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	return out
}

func runKongFPCase(t *testing.T, tc kongFPCase) kongFPOutcome {
	t.Helper()
	restoreZstd := service.KongSwapOpenAIRequestZstdForTest()
	defer restoreZstd()

	upstream := &kongFPUpstream{script: tc.script}
	usageRepo := &kongFPUsageRepo{}
	handler := newKongFPHandler(t, upstream, usageRepo)
	c, rec := newKongFPContext(tc)

	sink, restoreLog := captureHandlerStructuredLog(t)
	handler.Responses(c)
	restoreLog()

	outcome := kongFPOutcome{
		Upstream:        upstream.calls,
		Status:          c.Writer.Status(),
		ResponseHeaders: kongFPHeaders(rec.Result().Header),
		ResponseBody:    kongFPDigestOf(rec.Body.Bytes()),
		ContextKeys:     map[string]any{},
	}
	for _, log := range usageRepo.logs {
		outcome.Usage = append(outcome.Usage, kongFPJSONValue(log))
	}
	for key, value := range c.Keys {
		if kongFPVolatileKey.MatchString(key) {
			outcome.ContextKeys[key] = "<masked>"
			continue
		}
		outcome.ContextKeys[key] = kongFPJSONValue(value)
	}
	sink.mu.Lock()
	for _, event := range sink.events {
		if event == nil {
			continue
		}
		entry := kongFPLog{Level: event.Level, Component: event.Component, Message: event.Message}
		if len(event.Fields) > 0 {
			if masked, ok := kongFPJSONValue(event.Fields).(map[string]any); ok {
				entry.Fields = masked
			}
		}
		outcome.Logs = append(outcome.Logs, entry)
	}
	sink.mu.Unlock()
	return outcome
}

func kongFPRunAll(t *testing.T) map[string]kongFPOutcome {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cases := kongFPCases()
	// 预热一次：各开关的 sync.OnceValue 在进程内首次使用时会打一行启动日志，不能算进第一个用例。
	runKongFPCase(t, cases[0])
	outcomes := make(map[string]kongFPOutcome)
	for _, tc := range cases {
		outcomes[tc.name] = runKongFPCase(t, tc)
	}
	return outcomes
}

func kongFPMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	return raw
}

// TestKongOpenAIBodyFastpathGolden 对照原代码录下的基准：快速路径全关、全开、请求体内存各项逐项关闭各跑一遍，都必须与基准一致。
// 基准只能在改动前的原代码上重录（KONG_BODY_FASTPATH_GOLDEN=update），否则就失去了"原版本"的意义。
func TestKongOpenAIBodyFastpathGolden(t *testing.T) {
	path := filepath.FromSlash(kongFPGoldenPath)
	if os.Getenv("KONG_BODY_FASTPATH_GOLDEN") == "update" {
		outcomes := kongFPRunAll(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, append(kongFPMarshal(t, outcomes), '\n'), 0o644))
		t.Logf("对照基准已重录：%s（%d 个用例）", path, len(outcomes))
		return
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "缺少对照基准，先在原代码上用 KONG_BODY_FASTPATH_GOLDEN=update 录制")
	var golden map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &golden))

	type goldenMode struct {
		name string
		set  func() func()
	}
	modes := []goldenMode{
		{"fastpath_off", func() func() { return service.KongSetOpenAIBodyFastpathForTest(false) }},
		{"fastpath_on", func() func() { return service.KongSetOpenAIBodyFastpathForTest(true) }},
	}
	// 请求体内存的各项（DESIGN-openai-request-body-memory.md）逐项关掉各跑一遍：单独关掉任何一项都必须与基准一致。
	for _, item := range []string{"view_strings", "zstd_out", "zstd_in", "item_ids_splice", "identity_splice"} {
		modes = append(modes, goldenMode{"without_" + item, func() func() { return service.KongSetOpenAIBodyFastpathItemsOffForTest(item) }})
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			restore := mode.set()
			defer restore()
			outcomes := kongFPRunAll(t)
			if actualPath := os.Getenv("KONG_BODY_FASTPATH_ACTUAL"); actualPath != "" {
				require.NoError(t, os.WriteFile(actualPath+"."+mode.name+".json", append(kongFPMarshal(t, outcomes), '\n'), 0o644))
			}
			names := make([]string, 0, len(outcomes))
			for name := range outcomes {
				names = append(names, name)
			}
			sort.Strings(names)
			require.Len(t, golden, len(names), "用例集合与对照基准不一致")
			for _, name := range names {
				want, ok := golden[name]
				require.True(t, ok, "对照基准里没有用例 %s", name)
				require.JSONEq(t, string(want), string(kongFPMarshal(t, outcomes[name])), "用例 %s 与原代码不一致", name)
			}
		})
	}
}

// TestKongOpenAIBodyFastpathInboundNotMutated 核对流水线不原地修改入站请求体：以 PrereadBody 交给 handler，流水线
// 处理的就是测试手里的这份切片，跑完逐字节比较。快速路径关、开各跑一遍全部用例，登记门槛上下的请求体都在其中；
// 对照基准照旧经普通 reader 读入，覆盖真实的读取链路。
func TestKongOpenAIBodyFastpathInboundNotMutated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, on := range []bool{false, true} {
		restore := service.KongSetOpenAIBodyFastpathForTest(on)
		for _, tc := range kongFPCases() {
			tc.body = append([]byte(nil), tc.body...)
			tc.preread = true
			want := append([]byte(nil), tc.body...)
			runKongFPCase(t, tc)
			require.True(t, bytes.Equal(want, tc.body), "快速路径开=%v，用例 %s：入站请求体被原地修改", on, tc.name)
		}
		restore()
	}
}
