//go:build unit

package service

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 线上实测的整帧形状：prolite 账号只有 primary 一个窗口，secondary 显式为 null，
// credits.balance 是字符串——同一帧里两种数字表示都出现过。
const realCodexRateLimitsFrame = `{"type":"codex.rate_limits","plan_type":"prolite",` +
	`"rate_limits":{"allowed":true,"limit_reached":false,` +
	`"primary":{"used_percent":61,"window_minutes":10080,"reset_after_seconds":564029,"reset_at":1790653541},` +
	`"secondary":null},` +
	`"code_review_rate_limits":null,"additional_rate_limits":null,` +
	`"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"promo":null}`

func TestParseOpenAIWSCodexRateLimitEvent(t *testing.T) {
	t.Run("线上实测帧：只有 primary，secondary 为 null", func(t *testing.T) {
		got := parseOpenAIWSCodexRateLimitEvent([]byte(realCodexRateLimitsFrame))
		require.NotNil(t, got)
		require.NotNil(t, got.PrimaryUsedPercent)
		require.InDelta(t, 61, *got.PrimaryUsedPercent, 0.001)
		require.Equal(t, 10080, *got.PrimaryWindowMinutes)
		require.Equal(t, 564029, *got.PrimaryResetAfterSeconds)
		// secondary 是 null，不能被当成 0——0% 与「没有这个窗口」是两件事。
		require.Nil(t, got.SecondaryUsedPercent)
		require.Nil(t, got.SecondaryWindowMinutes)
		require.Nil(t, got.SecondaryResetAfterSeconds)

		// 归一化沿用快照那一份：10080 分钟（>360）落到 7d。
		norm := got.Normalize()
		require.NotNil(t, norm)
		require.NotNil(t, norm.Used7dPercent)
		require.InDelta(t, 61, *norm.Used7dPercent, 0.001)
		require.Nil(t, norm.Used5hPercent)
	})

	t.Run("两个窗口都有时按 window_minutes 分 5h/7d", func(t *testing.T) {
		frame := []byte(`{"type":"codex.rate_limits","rate_limits":{` +
			`"primary":{"used_percent":12.5,"window_minutes":10080,"reset_after_seconds":600},` +
			`"secondary":{"used_percent":88,"window_minutes":300,"reset_after_seconds":60}}}`)
		norm := parseOpenAIWSCodexRateLimitEvent(frame).Normalize()
		require.NotNil(t, norm)
		require.InDelta(t, 88, *norm.Used5hPercent, 0.001)
		require.InDelta(t, 12.5, *norm.Used7dPercent, 0.001)
	})

	t.Run("数字字符串也认", func(t *testing.T) {
		frame := []byte(`{"type":"codex.rate_limits","rate_limits":{` +
			`"primary":{"used_percent":"61.5","window_minutes":"10080"}}}`)
		got := parseOpenAIWSCodexRateLimitEvent(frame)
		require.NotNil(t, got)
		require.InDelta(t, 61.5, *got.PrimaryUsedPercent, 0.001)
		require.Equal(t, 10080, *got.PrimaryWindowMinutes)
	})

	t.Run("取值域与 HTTP 侧逐字一致", func(t *testing.T) {
		// 两侧都用 Atoi 不等于喂给它的输入相同：事件侧若多做一步去空白，NBSP 这类字符就会让同一份
		// 输入在一条路上得出窗口、另一条上缺失，最终把同一个水位归到不同的 5h/7d 字段。
		// 所以被拒的必须正好是 Atoi 也拒的那些。
		for _, bad := range []string{"NaN", "Inf", "1e400", "10080.5", "  300  ", " 300 ", "", "12a", "+5x"} {
			frame := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{` +
				`"used_percent":50,"window_minutes":"` + bad + `"}}}`)
			got := parseOpenAIWSCodexRateLimitEvent(frame)
			require.NotNil(t, got, "used_percent 仍然有效，快照不该整份丢掉：window=%q", bad)
			require.Nil(t, got.PrimaryWindowMinutes, "window_minutes=%q 必须被拒（Atoi 也拒）", bad)
			require.Nil(t, ParseCodexRateLimitHeaders(http.Header{
				"X-Codex-Primary-Window-Minutes": []string{bad},
			}), "HTTP 侧对同一份输入也必须拒：%q", bad)
		}
		// Atoi 接受的，两侧都必须接受。
		for _, good := range []string{"300", "+5", "0300", "99999999999"} {
			frame := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"window_minutes":"` + good + `"}}}`)
			want, err := strconv.Atoi(good)
			require.NoError(t, err)
			require.Equal(t, want, *parseOpenAIWSCodexRateLimitEvent(frame).PrimaryWindowMinutes, "window=%q", good)
		}
	})

	t.Run("JSON Number 按原始文本解析，不经 float64 舍入", func(t *testing.T) {
		// v.Float() 会丢掉十进制精度：360.00000000000001 变成 360、9007199254740993 变成 ...992，
		// 事后再检查整值或范围都恢复不了原值，而 HTTP 侧的 Atoi 会直接拒掉这些写法。
		for _, bad := range []string{"360.00000000000001", "10080.0", "1e400", "-1e400"} {
			frame := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{` +
				`"used_percent":50,"window_minutes":` + bad + `}}}`)
			got := parseOpenAIWSCodexRateLimitEvent(frame)
			require.NotNil(t, got)
			require.Nil(t, got.PrimaryWindowMinutes, "window_minutes=%s 必须被拒", bad)
		}
		exact := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"window_minutes":9007199254740993}}}`)
		require.Equal(t, 9007199254740993, *parseOpenAIWSCodexRateLimitEvent(exact).PrimaryWindowMinutes,
			"整数字面量必须按原值取，不能经 float64")
		// 非有限的百分比同样挡在外面（ParseFloat 自己就报越界）。
		inf := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":1e400,"window_minutes":300}}}`)
		got := parseOpenAIWSCodexRateLimitEvent(inf)
		require.Nil(t, got.PrimaryUsedPercent, "非有限百分比必须被拒")
		require.Equal(t, 300, *got.PrimaryWindowMinutes, "同帧其它正常字段必须保留")
	})

	t.Run("荒谬的重置秒数两侧都挡，且只丢这一个字段", func(t *testing.T) {
		// time.Duration(sec)*time.Second 在 sec 超过约 9.22e9 时溢出，reset_at 会落到过去，
		// 被读成"该窗口已重置"，于是 100% 的窗口被调度直接跳过——额度暂停失效。
		for _, bad := range []int{9223372037, maxCodexResetAfterSeconds + 1, -1} {
			frame := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{` +
				`"used_percent":100,"window_minutes":300,"reset_after_seconds":` + strconv.Itoa(bad) + `}}}`)
			got := parseOpenAIWSCodexRateLimitEvent(frame)
			require.NotNil(t, got)
			require.Nil(t, got.PrimaryResetAfterSeconds, "reset_after_seconds=%d 必须被拒", bad)
			require.InDelta(t, 100, *got.PrimaryUsedPercent, 0.001, "同窗口的水位必须保留")
			require.Equal(t, 300, *got.PrimaryWindowMinutes)

			// HTTP 侧共用同一道校验，否则同一份输入两侧结论不同。
			h := ParseCodexRateLimitHeaders(http.Header{
				"X-Codex-Primary-Used-Percent":        []string{"100"},
				"X-Codex-Primary-Reset-After-Seconds": []string{strconv.Itoa(bad)},
			})
			require.NotNil(t, h)
			require.Nil(t, h.PrimaryResetAfterSeconds, "HTTP 侧 reset_after_seconds=%d 也必须被拒", bad)
		}
		okFrame := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"reset_after_seconds":564029}}}`)
		require.Equal(t, 564029, *parseOpenAIWSCodexRateLimitEvent(okFrame).PrimaryResetAfterSeconds)
	})

	t.Run("没有可用数据就返回 nil，不产出空快照", func(t *testing.T) {
		for _, payload := range []string{
			`{"type":"codex.rate_limits"}`,
			`{"type":"codex.rate_limits","rate_limits":{}}`,
			`{"type":"codex.rate_limits","rate_limits":{"primary":null,"secondary":null}}`,
			`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":"n/a"}}}`,
			`{"type":"response.completed"}`,
			``,
		} {
			require.Nil(t, parseOpenAIWSCodexRateLimitEvent([]byte(payload)), "payload=%s", payload)
		}
	})
}

// 两侧口径必须同源：事件体与响应头描述同一份额度时，落到账号 extra 上的规范字段要一致。
// 各自归一化会对同一个账号算出不同水位，而那种不一致不报错，只会让调度照两个矛盾的数做决定。
func TestOpenAIWSCodexRateLimitEventMatchesHeaderPath(t *testing.T) {
	fromEvent := parseOpenAIWSCodexRateLimitEvent([]byte(`{"type":"codex.rate_limits","rate_limits":{` +
		`"primary":{"used_percent":61,"window_minutes":10080,"reset_after_seconds":564029},` +
		`"secondary":{"used_percent":7,"window_minutes":300,"reset_after_seconds":120}}}`))
	fromHeaders := ParseCodexRateLimitHeaders(http.Header{
		"X-Codex-Primary-Used-Percent":          []string{"61"},
		"X-Codex-Primary-Window-Minutes":        []string{"10080"},
		"X-Codex-Primary-Reset-After-Seconds":   []string{"564029"},
		"X-Codex-Secondary-Used-Percent":        []string{"7"},
		"X-Codex-Secondary-Window-Minutes":      []string{"300"},
		"X-Codex-Secondary-Reset-After-Seconds": []string{"120"},
	})
	require.NotNil(t, fromEvent)
	require.NotNil(t, fromHeaders)

	base := time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC)
	eventUpdates := buildCodexUsageExtraUpdates(fromEvent, base)
	headerUpdates := buildCodexUsageExtraUpdates(fromHeaders, base)
	// 只有时间戳类字段会因 UpdatedAt 不同而不同，比较其余全部键。
	for _, key := range []string{
		"codex_5h_used_percent", "codex_7d_used_percent",
		"codex_5h_window_minutes", "codex_7d_window_minutes",
		"codex_5h_reset_after_seconds", "codex_7d_reset_after_seconds",
		"codex_primary_used_percent", "codex_secondary_used_percent",
		"codex_primary_window_minutes", "codex_secondary_window_minutes",
	} {
		require.Equal(t, headerUpdates[key], eventUpdates[key], "键 %s 两侧口径不一致", key)
	}
}

func TestNoteOpenAIWSCodexRateLimits(t *testing.T) {
	newSvc := func(acct Account) (*OpenAIGatewayService, chan map[string]any) {
		calls := make(chan map[string]any, 4)
		repo := &snapshotUpdateAccountRepo{
			stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{acct}},
			updateExtraCalls:      calls,
		}
		// 每个子测试给一份**独立**的节流器：默认回落到包级单例，按 accountID 记账，
		// 于是同一账号的第二个子测试会被上一个吞掉——那样这些用例对「门是否生效」就没有区分力了。
		return &OpenAIGatewayService{
			accountRepo:           repo,
			codexSnapshotThrottle: newAccountWriteThrottle(0),
		}, calls
	}
	account := Account{ID: 7, Platform: PlatformOpenAI}

	t.Run("额度事件落到账号 extra", func(t *testing.T) {
		svc, calls := newSvc(account)
		svc.noteOpenAIWSCodexRateLimits(context.Background(), &account, openAIWSCodexRateLimitsEvent,
			[]byte(realCodexRateLimitsFrame))
		select {
		case updates := <-calls:
			require.InDelta(t, 61, updates["codex_7d_used_percent"], 0.001)
		case <-time.After(3 * time.Second):
			t.Fatal("额度快照没有落库")
		}
	})

	t.Run("影子账号不写 codex_*", func(t *testing.T) {
		// 影子账号的 codex_* 只由 QueryUsage(/wham/usage) 更新，口径与 HTTP 各落点一致；
		// 从业务流量回填会把父账号的水位覆盖成影子自己的。
		parent := int64(7)
		shadow := Account{ID: 8, Platform: PlatformOpenAI, ParentAccountID: &parent}
		svc, calls := newSvc(shadow)
		svc.noteOpenAIWSCodexRateLimits(context.Background(), &shadow, openAIWSCodexRateLimitsEvent,
			[]byte(realCodexRateLimitsFrame))
		select {
		case updates := <-calls:
			t.Fatalf("影子账号不该写 codex_*：%v", updates)
		case <-time.After(300 * time.Millisecond):
		}
	})

	t.Run("非额度事件不碰账号", func(t *testing.T) {
		svc, calls := newSvc(account)
		// 同一条连接上别的带外事件与业务事件都不该触发写入。
		for _, et := range []string{"codex.response.metadata", "responsesapi.websocket_timing", "response.completed", ""} {
			svc.noteOpenAIWSCodexRateLimits(context.Background(), &account, et,
				[]byte(realCodexRateLimitsFrame))
		}
		select {
		case updates := <-calls:
			t.Fatalf("非额度事件不该写账号：%v", updates)
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// 连接级的握手头不得回填逐轮额度：那份是拨号时刻的，会把带内事件采到的实时水位覆盖成旧值
// （落库节流 30s，一轮超过它就会发生），还会用当前时间重算旧的相对重置秒数。
func TestCodexQuotaHeadersRejectsWSHandshakeHeaders(t *testing.T) {
	headers := http.Header{"X-Codex-Primary-Used-Percent": []string{"7"}}

	handshake := &OpenAIForwardResult{ResponseHeaders: headers, OpenAIWSMode: true,
		ResponseHeadersFromWSHandshake: true}
	require.Nil(t, handshake.CodexQuotaHeaders(), "握手头不得用于逐轮额度")

	// WS-HTTP bridge 同样是 WS 模式，但它的响应头是本轮真实的 HTTP 响应头，必须照常刷新——
	// 所以判据不能是 !OpenAIWSMode。
	bridge := &OpenAIForwardResult{ResponseHeaders: headers, OpenAIWSMode: true}
	require.Equal(t, headers, bridge.CodexQuotaHeaders(), "bridge 的本轮响应头必须照常用于额度")

	plainHTTP := &OpenAIForwardResult{ResponseHeaders: headers}
	require.Equal(t, headers, plainHTTP.CodexQuotaHeaders())

	require.Nil(t, (*OpenAIForwardResult)(nil).CodexQuotaHeaders())
}

// 三条 WS 通路里的每一个 OpenAIForwardResult 都必须带上握手头标记。
//
// **不按"取头表达式里出现了握手函数名"来挑构造点**：把头先赋给局部变量、或包一层 maps.Clone，
// 都会让那处构造点从检查里消失，而全局的"至少找到一处"又被其余构造点满足，于是漏标静默通过。
// 这三个文件里的结果全都是 WS 轮次的结果，所以规则直接取"每一个都必须打标"——bridge 那条路的
// 结果构造在另一个文件里，它的响应头是本轮真实的 HTTP 响应头，不在此列。
func TestOpenAIWSResultsMarkHandshakeHeaders(t *testing.T) {
	const marker = "ResponseHeadersFromWSHandshake"
	total := 0
	for _, file := range []string{
		"openai_ws_forwarder_ingress.go",
		"openai_ws_forwarder_v2.go",
		"openai_ws_v2_passthrough_adapter.go",
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)

		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if name, ok := lit.Type.(*ast.Ident); !ok || name.Name != "OpenAIForwardResult" {
				return true
			}
			total++
			marked := false
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != marker {
					continue
				}
				// 必须是字面量 true：只看"出现过 true"会让 `!true` 也通过。
				value, ok := kv.Value.(*ast.Ident)
				marked = ok && value.Name == "true"
			}
			require.True(t, marked,
				"%s:%d 的 OpenAIForwardResult 没有 %s: true",
				file, fset.Position(lit.Pos()).Line, marker)
			return true
		})
	}
	require.NotZero(t, total, "没找到任何 OpenAIForwardResult 构造点——测试的定位方式已失效")
}

// 读上游帧的地方每多一个，就可能再漏一次额度采集——预热那条独立读循环就是这么漏掉的：
// 它自己消费事件，正式循环再也看不到，之后断连或报错就直接返回，账号一直留着旧水位且无任何信号。
// 所以把「读上游 WS 帧的函数必须调额度采集」钉成结构不变量；passthrough 不直接读（走 relay 回调），
// 单独限定到它的 BeforeWriteClient 函数体内检查。
//
// 判定按 AST：注释掉的调用不算调用；只认服务接收者 `s.` 上的调用，同名的无关方法不算；读循环这一侧
// 不下钻到嵌套的函数字面量里，免得把一个根本没被调用的 closure 当成已接入。
//
// **它保证的是"语法上接上了"，不是"运行时一定执行到"**——后者要靠各通路的行为测试，
// 而那需要上游连接夹具，目前是已知缺口。
func TestEveryUpstreamWSReaderIngestsRateLimits(t *testing.T) {
	const ingest = "noteOpenAIWSCodexRateLimits"

	for _, file := range []string{
		"openai_ws_forwarder_ingress.go",
		"openai_ws_forwarder_v2.go",
		"openai_ws_forwarder_support.go",
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		readers := 0
		for _, body := range astFuncBodies(parsed) {
			if !astCallsDirect(body, "ReadMessageWithContextTimeout", "") {
				continue
			}
			readers++
			require.True(t, astCallsDirect(body, ingest, "s"),
				"%s:%d 的函数体读了上游帧但没在同一体内接额度采集",
				file, fset.Position(body.Pos()).Line)
		}
		require.NotZero(t, readers, "%s 里没找到上游读点——测试的定位方式已失效", file)
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "openai_ws_v2_passthrough_adapter.go", nil, 0)
	require.NoError(t, err)
	hooks := 0
	ast.Inspect(parsed, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "BeforeWriteClient" {
			return true
		}
		fn, ok := kv.Value.(*ast.FuncLit)
		require.True(t, ok, "BeforeWriteClient 不再是函数字面量——测试的定位方式已失效")
		hooks++
		require.True(t, astCallsDirect(fn.Body, ingest, "s"),
			"passthrough 的 BeforeWriteClient 回调体内没接额度采集")
		return true
	})
	require.NotZero(t, hooks, "passthrough 里没找到 BeforeWriteClient——测试的定位方式已失效")
}

// astFuncBodies 列出文件里所有函数体（含闭包）。
func astFuncBodies(file *ast.File) []*ast.BlockStmt {
	var bodies []*ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body != nil {
				bodies = append(bodies, fn.Body)
			}
		case *ast.FuncLit:
			bodies = append(bodies, fn.Body)
		}
		return true
	})
	return bodies
}

// astCallsDirect 判断某个函数体里**直接**有没有对应调用——不下钻到嵌套的函数字面量。
//
// "同一个函数体"这个范围是关键：读点在哪个体里，采集就得在哪个体里。只要范围放宽到整个顶层函数，
// 把采集写进一个根本没被调用的 closure 也会算作已接入。
//
// recv 非空时只认该接收者上的调用（采集用 "s"，同名但挂在别的接收者上的方法不算接入）；
// recv 为空时不问接收者（上游读点挂在 lease 上）。
func astCallsDirect(body *ast.BlockStmt, name, recv string) bool {
	found := false
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if found || n == nil {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if recv == "" {
			found = true
			return false
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == recv {
			found = true
		}
		return !found
	}
	for _, stmt := range body.List {
		ast.Inspect(stmt, visit)
		if found {
			break
		}
	}
	return found
}
