//go:build unit

package service

import (
	"io"
	"strings"
	"testing"
	"time"
)

// SSE 的一个事件可以有多行 `data:`，规范要求把它们连起来再当成一份载荷。
//
// 逐行各自解码是错的：多行事件的每一行都不是完整 JSON，于是每一行都解码失败。而解码失败若只是
// continue，正文会静默少掉一段——归因只看「数字够不够」，217 个和 218 个都够，一份被损坏的回答
// 照样能授予资格。
func TestKongReadCodexSSETextMultiLineData(t *testing.T) {
	single := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"[1,2,3]"}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"output_tokens":7}}}`,
		``,
	}, "\n")
	// 与上面等价，但把同一个事件拆成多行 data。
	multi := strings.Join([]string{
		`data: {"type":"response.output_text.delta",`,
		`data:  "delta":"[1,2,3]"}`,
		``,
		`data: {"type":"response.completed",`,
		`data:  "response":{"usage":{"output_tokens":7}}}`,
		``,
	}, "\n")

	want, err := kongReadCodexSSEText(strings.NewReader(single))
	if err != nil {
		t.Fatalf("单行事件应当解析成功: %v", err)
	}
	got, err := kongReadCodexSSEText(strings.NewReader(multi))
	if err != nil {
		t.Fatalf("多行事件应当解析成功: %v", err)
	}
	if got.Text != want.Text {
		t.Errorf("多行事件的正文 = %q, want %q", got.Text, want.Text)
	}
	if want.OutputTokens == nil || got.OutputTokens == nil || *got.OutputTokens != *want.OutputTokens {
		t.Errorf("多行事件的 usage = %v, want %v", got.OutputTokens, want.OutputTokens)
	}
}

// 中途损坏的事件必须让这一份挑战失败，即使剩下的数字仍然够用。
func TestKongReadCodexSSETextRejectsCorruptEvent(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"[1,2,3,"}`,
		``,
		`data: {"type":"response.output_text.delta","delta":` + "\x01" + `}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"4,5]"}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n")
	if _, err := kongReadCodexSSEText(strings.NewReader(stream)); err == nil {
		t.Error("损坏的事件必须使本份挑战失败，不能只丢掉那一段继续")
	}

	// delta 不是字符串同样不可信——不能只当它是空串。
	notString := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":123}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n")
	if _, err := kongReadCodexSSEText(strings.NewReader(notString)); err == nil {
		t.Error("delta 不是字符串时必须失败")
	}
}

// 完成事件到达即返回，不再等 EOF——继续读只会把一份完整回答拖到读超时。
func TestKongReadCodexSSETextStopsAtCompleted(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"[1]"}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
		// 完成之后的垃圾不该影响结论。
		`data: {oops`,
		``,
	}, "\n")
	sse, err := kongReadCodexSSEText(strings.NewReader(stream))
	text := sse.Text
	if err != nil {
		t.Fatalf("完成事件之后的内容不该改判失败: %v", err)
	}
	if text != "[1]" {
		t.Errorf("正文 = %q, want %q", text, "[1]")
	}
}

// 流首 BOM、三种换行、未知空白行：都不能把一份合法的流判成损坏。
//
// BOM 不吃掉，第一行开头就多出那三个字节，`data:` 前缀匹配不上——那个事件被当作未知字段行
// 忽略，正文少掉第一段。而归因只看「数字够不够」，少一段仍然够，于是静默截断的回答照样授予资格。
func TestKongReadCodexSSETextFramingVariants(t *testing.T) {
	events := []string{
		`data: {"type":"response.output_text.delta","delta":"[1,2,3]"}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"output_tokens":7}}}`,
		``,
	}
	cases := []struct {
		name   string
		render func() string
	}{
		{"LF", func() string { return strings.Join(events, "\n") }},
		{"CRLF", func() string { return strings.Join(events, "\r\n") }},
		{"CR-only", func() string { return strings.Join(events, "\r") }},
		{"流首 BOM", func() string { return "\ufeff" + strings.Join(events, "\n") }},
		{"混合换行", func() string {
			return events[0] + "\r\n" + "\n" + events[2] + "\r" + "\n"
		}},
		{"夹注释与未知字段行", func() string {
			return ": keepalive\n" + "event: message\n" + events[0] + "\nid: 7\n\n" + events[2] + "\n\n"
		}},
		{"末尾无收尾空行", func() string { return events[0] + "\n\n" + events[2] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse, err := kongReadCodexSSEText(strings.NewReader(tc.render()))
			text, tokens := sse.Text, sse.OutputTokens
			if err != nil {
				t.Fatalf("合法的流不该失败: %v", err)
			}
			if text != "[1,2,3]" {
				t.Errorf("正文 = %q, want %q", text, "[1,2,3]")
			}
			if tokens == nil || *tokens != 7 {
				t.Errorf("usage = %v, want 7", tokens)
			}
		})
	}

	// 只含空格的行是未知字段行，不是事件边界：按边界处理会把多行 data 的事件提前解码。
	spaced := strings.Join([]string{
		`data: {"type":"response.output_text.delta",`,
		` `,
		`data:  "delta":"[9]"}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n")
	if sse, err := kongReadCodexSSEText(strings.NewReader(spaced)); err != nil || sse.Text != "[9]" {
		t.Errorf("夹空格行的多行事件应当正常拼接：text=%q err=%v", sse.Text, err)
	}
}

// response.incomplete 是合法的失败终端：必须立刻带原因失败，不能当未知事件忽略后继续等 EOF。
func TestKongReadCodexSSETextIncomplete(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"[1]"}`,
		``,
		`data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`,
		``,
	}, "\n")
	_, err := kongReadCodexSSEText(strings.NewReader(stream))
	if err == nil {
		t.Fatal("response.incomplete 必须判失败")
	}
	if !strings.Contains(err.Error(), "max_output_tokens") {
		t.Errorf("失败原因要带上 incomplete_details.reason，实际: %v", err)
	}
}

// 回报的 model：**终止事件的声明覆盖中途事件**，而"终止"包含失败类终止。
//
// 上游的口径是只有终止事件报告实际处理的档位（见 upstreamResponseModelObserver），中途事件里那个
// 是预告。只认 completed/done 的话，`response.failed` 里的新声明会被忽略——上游在 created 说 astra、
// 在 failed 说 luna 时，我们只看得见 astra，明确的降档证据就丢了。
func TestKongReadCodexSSEReportedModelPrecedence(t *testing.T) {
	const early = "gpt-6-astra"
	const late = "gpt-5.6-luna"
	mk := func(terminal string, terminalModel string) string {
		lines := []string{
			`data: {"type":"response.created","response":{"model":"` + early + `"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"[1]"}`,
			``,
		}
		// response 对象只能出现一次：拼两个 `"response"` 键会让后者覆盖前者（JSON 重复键），
		// model 就丢了——那是测试构造的错，不是被测代码的行为。
		var fields []string
		if terminalModel != "" {
			fields = append(fields, `"model":"`+terminalModel+`"`)
		}
		if terminal == "response.incomplete" {
			fields = append(fields, `"incomplete_details":{"reason":"max_output_tokens"}`)
		}
		payload := `data: {"type":"` + terminal + `"`
		if len(fields) > 0 {
			payload += `,"response":{` + strings.Join(fields, ",") + `}`
		}
		payload += `}`
		return strings.Join(append(lines, payload, ``), "\n")
	}

	cases := []struct {
		name     string
		terminal string
		model    string
		want     string
	}{
		{"completed 覆盖", "response.completed", late, late},
		{"done 覆盖", "response.done", late, late},
		// 失败类终止同样是终止声明。这三条是修复前会漏掉的。
		{"failed 覆盖", "response.failed", late, late},
		{"incomplete 覆盖", "response.incomplete", late, late},
		{"cancelled 覆盖", "response.cancelled", late, late},
		// 反方向：早期不一致、最终一致时也要取最终的，否则会误拒一张好票。
		{"最终声明一致时取最终", "response.completed", early, early},
		// 终止事件没带 model 时保留已有证据，不清空。
		{"终止缺 model 保留早期", "response.completed", "", early},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse, _ := kongReadCodexSSEText(strings.NewReader(mk(tc.terminal, tc.model)))
			if sse.ReportedModel != tc.want {
				t.Errorf("ReportedModel = %q, want %q", sse.ReportedModel, tc.want)
			}
		})
	}
}

// 完成事件完整到达后必须立即返回，哪怕连接还开着。
//
// 读到行尾的 `\r` 时若去等下一个字节确认是不是 CRLF，CR-only 的流会在 `response.completed\r\r`
// 之后一直卡住，拖到挑战的读超时——已经到手的一份完整回答白白作废。流分块到达时，跨块的 CRLF
// 仍要认成一个行终止符，不能多读出一个空行（那会被当成事件边界，把多行 data 的事件提前解码）。
func TestKongReadCodexSSETextReturnsWithoutWaitingForNextByte(t *testing.T) {
	delta := `data: {"type":"response.output_text.delta","delta":"[1,2,3]"}`
	completed := `data: {"type":"response.completed","response":{"usage":{"output_tokens":7}}}`
	cases := map[string][]string{
		"LF":      {delta + "\n\n" + completed + "\n\n"},
		"CRLF":    {delta + "\r\n\r\n" + completed + "\r\n\r\n"},
		"CR-only": {delta + "\r\r" + completed + "\r\r"},
		// 多行 data 的事件在 CR 与 LF 之间被切开：LF 属于上一行的终止符，不是空行。
		"跨块 CRLF": {`data: {"type":"response.output_text.delta",` + "\r", "\n" + `data: "delta":"[1,2,3]"}` + "\r\n\r\n" + completed + "\r", "\n\r\n"},
	}
	for name, chunks := range cases {
		t.Run(name, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
			go func() {
				for _, chunk := range chunks {
					if _, err := pw.Write([]byte(chunk)); err != nil {
						return
					}
				}
				// 不关闭 writer：模拟上游在完成事件之后仍保持连接。
			}()
			type res struct {
				sse kongSSEAnswer
				err error
			}
			done := make(chan res, 1)
			go func() {
				sse, err := kongReadCodexSSEText(pr)
				done <- res{sse, err}
			}()
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("合法的流不该失败: %v", r.err)
				}
				if r.sse.Text != "[1,2,3]" {
					t.Errorf("正文 = %q, want %q", r.sse.Text, "[1,2,3]")
				}
			case <-time.After(2 * time.Second):
				_ = pr.CloseWithError(io.ErrClosedPipe)
				t.Fatal("完成事件已到达，解析却还在等后续字节")
			}
		})
	}
}
