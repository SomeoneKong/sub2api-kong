//go:build unit

package service

import (
	"strings"
	"testing"
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

	wantText, wantTokens, err := kongReadCodexSSEText(strings.NewReader(single))
	if err != nil {
		t.Fatalf("单行事件应当解析成功: %v", err)
	}
	gotText, gotTokens, err := kongReadCodexSSEText(strings.NewReader(multi))
	if err != nil {
		t.Fatalf("多行事件应当解析成功: %v", err)
	}
	if gotText != wantText {
		t.Errorf("多行事件的正文 = %q, want %q", gotText, wantText)
	}
	if wantTokens == nil || gotTokens == nil || *gotTokens != *wantTokens {
		t.Errorf("多行事件的 usage = %v, want %v", gotTokens, wantTokens)
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
	if _, _, err := kongReadCodexSSEText(strings.NewReader(stream)); err == nil {
		t.Error("损坏的事件必须使本份挑战失败，不能只丢掉那一段继续")
	}

	// delta 不是字符串同样不可信——不能只当它是空串。
	notString := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":123}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n")
	if _, _, err := kongReadCodexSSEText(strings.NewReader(notString)); err == nil {
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
	text, _, err := kongReadCodexSSEText(strings.NewReader(stream))
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
			text, tokens, err := kongReadCodexSSEText(strings.NewReader(tc.render()))
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
	if text, _, err := kongReadCodexSSEText(strings.NewReader(spaced)); err != nil || text != "[9]" {
		t.Errorf("夹空格行的多行事件应当正常拼接：text=%q err=%v", text, err)
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
	_, _, err := kongReadCodexSSEText(strings.NewReader(stream))
	if err == nil {
		t.Fatal("response.incomplete 必须判失败")
	}
	if !strings.Contains(err.Error(), "max_output_tokens") {
		t.Errorf("失败原因要带上 incomplete_details.reason，实际: %v", err)
	}
}
