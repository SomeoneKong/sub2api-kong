// Package kongcorpus 生成 codex CLI 形态的合成 Responses 请求与 SSE 响应，只供测试与基准使用。
//
// 请求体按 codex-rs ResponsesApiRequest 的字段顺序与 serde_json 的转义方式写出：顶层依次是
// model、instructions、input、tools、tool_choice、parallel_tool_calls、reasoning、store、stream、include、
// prompt_cache_key、text、client_metadata；字符串只转义引号、反斜杠与控制字符，非 ASCII 原样保留。
// 同一组 Options 总是生成同样的字节，便于录制对照基准。
package kongcorpus

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
)

// Shape 是请求形态：普通请求带顶层 instructions 与 tools；lite 请求把它们放进 input 开头。
type Shape int

const (
	ShapeSol Shape = iota
	ShapeLite
)

// Options 描述一份合成请求。零值是一份只有一轮对话的普通请求。
type Options struct {
	Shape Shape
	Seed  uint64
	// TargetBytes 是请求体的近似大小；对话按轮追加，直到不小于它。
	TargetBytes int
	// Model 为空时按形态取 gpt-6-sol 或 gpt-6-astra。
	Model string
	// NonStream 为真时 stream 写 false。
	NonStream bool
	// ExtraTools 是追加到工具列表末尾的工具定义（原始 JSON）。
	ExtraTools []string
	// PrefixItems 放在 input 最前（lite 形态在附加工具项与指令消息之后）。
	PrefixItems []string
	// SuffixItems 放在 input 最后。
	SuffixItems []string
	// TopLevelTail 是追加在顶层对象末尾的成员片段，形如 `"key":value`。
	TopLevelTail []string
	// OmitPromptCacheKey 为真时不写 prompt_cache_key。
	OmitPromptCacheKey bool
	// NoHistory 为真时不生成对话轮次，input 只含 lite 前缀与 PrefixItems、SuffixItems。
	NoHistory bool
	// ReasoningRaw 非空时替换 reasoning 的值（原始 JSON）。
	ReasoningRaw string
	// InputRaw 非空时替换整个 input 的值（原始 JSON），忽略其余与 input 有关的选项。
	InputRaw string
}

func (o Options) model() string {
	if o.Model != "" {
		return o.Model
	}
	if o.Shape == ShapeLite {
		return "gpt-6-astra"
	}
	return "gpt-6-sol"
}

type generator struct {
	rng *rand.Rand
}

func newGenerator(seed uint64) *generator {
	return &generator{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Request 生成请求体。
func Request(o Options) []byte {
	g := newGenerator(o.Seed)
	var b strings.Builder
	b.Grow(o.TargetBytes + 64<<10)

	instructions := g.instructions()
	tools := append(g.tools(), o.ExtraTools...)

	_, _ = b.WriteString(`{"model":`)
	_, _ = b.WriteString(Quote(o.model()))
	if o.Shape == ShapeSol {
		_, _ = b.WriteString(`,"instructions":`)
		_, _ = b.WriteString(Quote(instructions))
	}
	_, _ = b.WriteString(`,"input":`)
	if o.InputRaw != "" {
		_, _ = b.WriteString(o.InputRaw)
	} else {
		_ = b.WriteByte('[')
		first := true
		item := func(raw string) {
			if !first {
				_ = b.WriteByte(',')
			}
			first = false
			_, _ = b.WriteString(raw)
		}
		if o.Shape == ShapeLite {
			item(`{"type":"additional_tools","id":` + Quote("at_"+g.hex(32)) + `,"role":"developer","tools":[` + strings.Join(tools, ",") + `]}`)
			item(`{"type":"message","id":` + Quote("msg_"+g.hex(32)) + `,"role":"developer","content":[{"type":"input_text","text":` + Quote(instructions) + `}]}`)
		}
		for _, raw := range o.PrefixItems {
			item(raw)
		}
		for turn := 0; !o.NoHistory && (turn == 0 || b.Len() < o.TargetBytes); turn++ {
			for _, raw := range g.turn(turn) {
				item(raw)
			}
		}
		for _, raw := range o.SuffixItems {
			item(raw)
		}
		_ = b.WriteByte(']')
	}
	if o.Shape == ShapeSol {
		_, _ = b.WriteString(`,"tools":[`)
		_, _ = b.WriteString(strings.Join(tools, ","))
		_ = b.WriteByte(']')
	}
	_, _ = b.WriteString(`,"tool_choice":"auto"`)
	reasoning := `{"effort":"high","summary":"auto"}`
	if o.Shape == ShapeLite {
		_, _ = b.WriteString(`,"parallel_tool_calls":false`)
		reasoning = `{"effort":"high","summary":"auto","context":"all_turns"}`
	} else {
		_, _ = b.WriteString(`,"parallel_tool_calls":true`)
	}
	if o.ReasoningRaw != "" {
		reasoning = o.ReasoningRaw
	}
	_, _ = b.WriteString(`,"reasoning":`)
	_, _ = b.WriteString(reasoning)
	_, _ = b.WriteString(`,"store":false`)
	if o.NonStream {
		_, _ = b.WriteString(`,"stream":false`)
	} else {
		_, _ = b.WriteString(`,"stream":true`)
	}
	_, _ = b.WriteString(`,"include":["reasoning.encrypted_content"]`)
	if !o.OmitPromptCacheKey {
		_, _ = b.WriteString(`,"prompt_cache_key":`)
		_, _ = b.WriteString(Quote(g.uuid()))
	}
	_, _ = b.WriteString(`,"text":{"verbosity":"low"}`)
	_, _ = b.WriteString(`,"client_metadata":{"x-codex-installation-id":`)
	_, _ = b.WriteString(Quote(g.uuid()))
	_, _ = b.WriteString(`,"x-codex-window-id":`)
	_, _ = b.WriteString(Quote(g.uuid()))
	_ = b.WriteByte('}')
	for _, raw := range o.TopLevelTail {
		_ = b.WriteByte(',')
		_, _ = b.WriteString(raw)
	}
	_ = b.WriteByte('}')
	return []byte(b.String())
}

// Headers 返回 codex_exec 发出的请求头。session_id 由 Seed 决定。
func Headers(o Options) http.Header {
	g := newGenerator(o.Seed ^ 0x5bd1e995)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", "codex_exec/0.156.0 (Ubuntu 24.4.0; x86_64) xterm-256color (codex_exec; 0.156.0)")
	h.Set("originator", "codex_exec")
	h.Set("session_id", g.uuid())
	h.Set("x-codex-window-id", g.uuid())
	if o.Shape == ShapeLite {
		h.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	}
	return h
}

// SSEResponse 生成一次流式响应：reasoning 摘要增量、一个 function_call 的参数增量，最后是带用量的
// response.completed。deltas 控制增量事件的个数。
func SSEResponse(seed uint64, model string, deltas int) []byte {
	g := newGenerator(seed ^ 0x2545f4914f6cdd1d)
	var b strings.Builder
	event := func(name, data string) {
		_, _ = b.WriteString("event: ")
		_, _ = b.WriteString(name)
		_, _ = b.WriteString("\ndata: ")
		_, _ = b.WriteString(data)
		_, _ = b.WriteString("\n\n")
	}
	respID := "resp_" + g.hex(24)
	callID := "call_" + g.alnum(24)
	event("response.created", `{"type":"response.created","response":{"id":`+Quote(respID)+`,"object":"response","model":`+Quote(model)+`,"status":"in_progress"}}`)
	event("response.in_progress", `{"type":"response.in_progress","response":{"id":`+Quote(respID)+`,"object":"response","model":`+Quote(model)+`,"status":"in_progress"}}`)
	event("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}`)
	for i := 0; i < deltas; i++ {
		event("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":`+Quote(g.words(1+g.rng.IntN(4))+" ")+`}`)
	}
	encrypted := g.encrypted(2048)
	event("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"done"}],"encrypted_content":`+Quote(encrypted)+`}}`)
	event("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"shell","arguments":"","call_id":`+Quote(callID)+`}}`)
	args := `{"command":["bash","-lc","ls -la"],"workdir":"/work"}`
	for i := 0; i < len(args); i += 4 {
		end := min(i+4, len(args))
		event("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":`+Quote(args[i:end])+`}`)
	}
	event("response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","name":"shell","arguments":`+Quote(args)+`,"call_id":`+Quote(callID)+`}}`)
	event("response.completed", `{"type":"response.completed","response":{"id":`+Quote(respID)+`,"object":"response","model":`+Quote(model)+`,"status":"completed","usage":{"input_tokens":218000,"input_tokens_details":{"cached_tokens":201000},"output_tokens":1500,"output_tokens_details":{"reasoning_tokens":900},"total_tokens":219500}}}`)
	return []byte(b.String())
}

// Quote 按 serde_json 的方式把字符串写成 JSON 字面量。
func Quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	_ = b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			_, _ = b.WriteString(`\"`)
		case '\\':
			_, _ = b.WriteString(`\\`)
		case '\n':
			_, _ = b.WriteString(`\n`)
		case '\r':
			_, _ = b.WriteString(`\r`)
		case '\t':
			_, _ = b.WriteString(`\t`)
		case '\b':
			_, _ = b.WriteString(`\b`)
		case '\f':
			_, _ = b.WriteString(`\f`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				_ = b.WriteByte(c)
			}
		}
	}
	_ = b.WriteByte('"')
	return b.String()
}

func (g *generator) turn(n int) []string {
	var items []string
	if n%4 == 0 {
		items = append(items, `{"type":"message","role":"user","content":[{"type":"input_text","text":`+Quote(g.prose(40+g.rng.IntN(120)))+`}]}`)
	}
	items = append(items, `{"type":"reasoning","summary":[{"type":"summary_text","text":`+Quote("**"+g.words(4)+"**\n\n"+g.prose(20+g.rng.IntN(40)))+`}],"encrypted_content":`+Quote(g.encrypted(1500+g.rng.IntN(4500)))+`}`)
	callID := "call_" + g.alnum(24)
	if n%5 == 3 {
		patch := "*** Begin Patch\n*** Update File: src/" + g.word() + ".py\n@@\n-" + g.codeLine() + "\n+" + g.codeLine() + "\n*** End Patch"
		items = append(items,
			`{"type":"custom_tool_call","status":"completed","call_id":`+Quote(callID)+`,"name":"apply_patch","input":`+Quote(patch)+`}`,
			`{"type":"custom_tool_call_output","call_id":`+Quote(callID)+`,"output":`+Quote("Success. Updated the following files:\nM src/"+g.word()+".py\n")+`}`,
		)
	} else {
		args := `{"command":["bash","-lc",` + Quote(g.command()) + `],"workdir":"/home/dev/project","timeout_ms":120000}`
		items = append(items,
			`{"type":"function_call","name":"shell","arguments":`+Quote(args)+`,"call_id":`+Quote(callID)+`}`,
			`{"type":"function_call_output","call_id":`+Quote(callID)+`,"output":`+Quote(g.toolOutput(500+g.rng.IntN(14000)))+`}`,
		)
	}
	if n%6 == 5 {
		items = append(items, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+Quote(g.prose(30+g.rng.IntN(80)))+`}]}`)
	}
	return items
}

func (g *generator) instructions() string {
	var b strings.Builder
	_, _ = b.WriteString("You are Codex, a coding agent running in a terminal-based environment.\n\n")
	for i := 0; i < 60; i++ {
		_, _ = b.WriteString("## ")
		_, _ = b.WriteString(g.words(3))
		_, _ = b.WriteString("\n\n")
		_, _ = b.WriteString(g.prose(30))
		_, _ = b.WriteString(" (see `")
		_, _ = b.WriteString(g.word())
		_, _ = b.WriteString(".md`).\n\n")
	}
	return b.String()
}

func (g *generator) tools() []string {
	fn := func(name, description, parameters string) string {
		return `{"type":"function","name":` + Quote(name) + `,"description":` + Quote(description) + `,"strict":false,"parameters":` + parameters + `}`
	}
	tools := []string{
		fn("shell", "Runs a shell command and returns its output. Use it for builds, tests (e.g. `python -m pytest`) and file inspection.",
			`{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"},"description":"The command to execute"},"workdir":{"type":"string","description":"The working directory"},"timeout_ms":{"type":"number","description":"The timeout for the command in milliseconds"}},"required":["command"],"additionalProperties":false}`),
		`{"type":"custom","name":"apply_patch","description":"Use the apply_patch tool to edit files.","format":{"type":"grammar","syntax":"lark","definition":` + Quote("start: begin_patch hunk+ end_patch\nbegin_patch: \"*** Begin Patch\" LF\nend_patch: \"*** End Patch\" LF?\n") + `}}`,
		fn("update_plan", "Updates the task plan.",
			`{"type":"object","properties":{"explanation":{"type":"string"},"plan":{"type":"array","items":{"type":"object","properties":{"step":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["step","status"],"additionalProperties":false}}},"required":["plan"],"additionalProperties":false}`),
		fn("view_image", "Attach a local image (by filesystem path) to the conversation context.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Local filesystem path to an image file"}},"required":["path"],"additionalProperties":false}`),
	}
	for i := 0; i < 12; i++ {
		name := "mcp__" + g.word() + "__" + g.word()
		tools = append(tools, fn(name, g.prose(20), `{"type":"object","properties":{"query":{"type":"string","description":`+Quote(g.prose(8))+`},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["query"],"additionalProperties":false}`))
	}
	return tools
}

var vocabulary = []string{
	"the", "function", "returns", "error", "python", "config", "handler", "request", "value", "cache", "index",
	"module", "test", "build", "update", "file", "path", "result", "service", "stream", "token", "buffer",
	"parse", "schema", "render", "commit", "branch", "merge", "deploy", "worker", "queue", "session",
	"用户", "请求", "配置", "错误", "“引号”", "naïve", "café",
}

func (g *generator) word() string { return vocabulary[g.rng.IntN(len(vocabulary))] }

func (g *generator) words(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = g.word()
	}
	return strings.Join(parts, " ")
}

func (g *generator) prose(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			_ = b.WriteByte(' ')
		}
		_, _ = b.WriteString(g.word())
		switch g.rng.IntN(14) {
		case 0:
			_, _ = b.WriteString(",")
		case 1:
			_, _ = b.WriteString(".")
		case 2:
			_, _ = b.WriteString(" (" + g.word() + ")")
		}
	}
	return b.String()
}

func (g *generator) codeLine() string {
	switch g.rng.IntN(5) {
	case 0:
		return fmt.Sprintf("def %s_%d(self, %s):", g.word(), g.rng.IntN(100), g.word())
	case 1:
		return fmt.Sprintf("    return %s[%q] if %s else None", g.word(), g.word(), g.word())
	case 2:
		return fmt.Sprintf("\tif err := %s(ctx); err != nil { return fmt.Errorf(\"%s: %%w\", err) }", g.word(), g.word())
	case 3:
		return fmt.Sprintf("const %s = /^[a-z]+\\d{%d}$/;", g.word(), g.rng.IntN(9)+1)
	default:
		return fmt.Sprintf("// %s", g.prose(8))
	}
}

func (g *generator) command() string {
	switch g.rng.IntN(4) {
	case 0:
		return "rg -n \"" + g.word() + "\" src | head -50"
	case 1:
		return "python -m pytest tests/test_" + g.word() + ".py -q"
	case 2:
		return "sed -n '1,200p' src/" + g.word() + ".py"
	default:
		return "git diff --stat && go test ./..."
	}
}

func (g *generator) toolOutput(size int) string {
	var b strings.Builder
	for b.Len() < size {
		switch g.rng.IntN(7) {
		case 0:
			_, _ = b.WriteString("\x1b[32mPASS\x1b[0m " + g.word() + "_test (0." + fmt.Sprint(g.rng.IntN(99)) + "s)\n")
		case 1:
			fmt.Fprintf(&b, "src/%s.py:%d:%d: %s\n", g.word(), g.rng.IntN(900)+1, g.rng.IntN(80)+1, g.prose(6))
		case 2:
			_, _ = b.WriteString(`{"level":"info","msg":"` + g.word() + `","path":"C:\\Users\\dev\\` + g.word() + `"}` + "\n")
		default:
			_, _ = b.WriteString(g.codeLine())
			_ = b.WriteByte('\n')
		}
	}
	return b.String()
}

func (g *generator) encrypted(size int) string {
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = byte(g.rng.UintN(256))
	}
	return "gAAAAA" + base64.StdEncoding.EncodeToString(raw)
}

const alnumChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (g *generator) alnum(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alnumChars[g.rng.IntN(len(alnumChars))]
	}
	return string(b)
}

func (g *generator) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[g.rng.IntN(16)]
	}
	return string(b)
}

func (g *generator) uuid() string {
	h := g.hex(32)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
