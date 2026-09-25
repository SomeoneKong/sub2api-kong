package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// 请求体快速路径的判否门（DESIGN §3.1–3.6、§3.9–3.11）：只在能证明原函数一定原样返回、没有错误时跳过，判断不了就交还
// 原函数。门里要比较或交给上游判定的字符串（含键名）照原函数的取值方式来：原函数读的是 encoding/json 解码出来的
// 值时，按 encoding/json 的语义取值，因为 gjson 的反转义在孤立代理项、非法 UTF-8 上与它不同；原函数本身用 gjson
// 取值时（§3.9、§3.10），原文带反斜杠就回落，不复刻 gjson 的反转义。

// ── 结构扫描 ─────────────────────────────────────────────────────────────
//
// 只在已确认合法的 JSON 上使用（门先检查可解码，元数据与 item id 的门先检查 gjson 有效性）；遇到意外的结构一律
// 返回 false，由调用方回落。跳过值时不解码，按字节找边界。

func kongSkipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// kongStringEnd 返回从 s[i]（一个引号）开始的字符串 token 结束后的位置。
func kongStringEnd(s string, i int) (int, bool) {
	start := i
	for j := i + 1; ; {
		k := strings.IndexByte(s[j:], '"')
		if k < 0 {
			return 0, false
		}
		quote := j + k
		// 引号前连续反斜杠的个数为偶数时，这个引号没有被转义。
		backslashes := 0
		for p := quote - 1; p > start && s[p] == '\\'; p-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return quote + 1, true
		}
		j = quote + 1
	}
}

// kongValueEnd 返回从 s[i] 开始的一个值结束后的位置。
func kongValueEnd(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	switch s[i] {
	case '"':
		return kongStringEnd(s, i)
	case '{', '[':
		depth := 0
		for i < len(s) {
			switch s[i] {
			case '"':
				end, ok := kongStringEnd(s, i)
				if !ok {
					return 0, false
				}
				i = end
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return 0, false
	default:
		for i < len(s) && s[i] != ',' && s[i] != '}' && s[i] != ']' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			i++
		}
		return i, true
	}
}

// kongScanObject 逐个交出对象成员的键与值（都是原文）。fn 返回 false 时提前停止，此时仍返回 true。
func kongScanObject(raw string, fn func(keyRaw, valueRaw string) bool) bool {
	i := kongSkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return false
	}
	i = kongSkipSpace(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return true
	}
	for i < len(raw) {
		if raw[i] != '"' {
			return false
		}
		keyEnd, ok := kongStringEnd(raw, i)
		if !ok {
			return false
		}
		keyRaw := raw[i:keyEnd]
		i = kongSkipSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return false
		}
		i = kongSkipSpace(raw, i+1)
		valueEnd, ok := kongValueEnd(raw, i)
		if !ok {
			return false
		}
		if !fn(keyRaw, raw[i:valueEnd]) {
			return true
		}
		i = kongSkipSpace(raw, valueEnd)
		if i < len(raw) && raw[i] == ',' {
			i = kongSkipSpace(raw, i+1)
			continue
		}
		return i < len(raw) && raw[i] == '}'
	}
	return false
}

// kongScanArray 逐个交出数组元素的原文。fn 返回 false 时提前停止，此时仍返回 true。
func kongScanArray(raw string, fn func(elementRaw string) bool) bool {
	i := kongSkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return false
	}
	i = kongSkipSpace(raw, i+1)
	if i < len(raw) && raw[i] == ']' {
		return true
	}
	for i < len(raw) {
		end, ok := kongValueEnd(raw, i)
		if !ok {
			return false
		}
		if !fn(raw[i:end]) {
			return true
		}
		i = kongSkipSpace(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i = kongSkipSpace(raw, i+1)
			continue
		}
		return i < len(raw) && raw[i] == ']'
	}
	return false
}

// KongDecodeJSONString 按 encoding/json 的语义解码一个字符串 token（含两侧引号）。原文没有反斜杠、又是合法
// UTF-8 时，解码结果就是原文本身，不分配。
func KongDecodeJSONString(raw string) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	inner := raw[1 : len(raw)-1]
	if strings.IndexByte(inner, '\\') < 0 && utf8.ValidString(inner) {
		return inner, true
	}
	var decoded string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return "", false
	}
	return decoded, true
}

// kongNonString 在投影里代替非字符串的值：上游判定只经 .(string) 或 firstNonEmptyString 读这些字段，
// 任何非字符串都得到同样的结论。
type kongNonString struct{}

func kongProjectValue(valueRaw string) (any, bool) {
	if strings.HasPrefix(valueRaw, `"`) {
		decoded, ok := KongDecodeJSONString(valueRaw)
		return decoded, ok
	}
	return kongNonString{}, true
}

// ── 可解码与 input 判定 ─────────────────────────────────────────────────

// kongBodyDecodable 判断 decodeOpenAIJSONUseNumber(body, &map) 会不会成功：json.Valid 与 Decoder 同按 v1 的
// 宽松语义，嵌套上限也相同，所以"合法且顶层是对象"恰好等于解码成功。
func kongBodyDecodable(body []byte) bool {
	if entry := kongBodyEntryFor(body); entry != nil {
		return kongStartsWithObject(entry.body) && entry.jsonValidity()
	}
	return kongComputeDecodable(body)
}

func kongComputeDecodable(body []byte) bool {
	return kongStartsWithObject(body) && json.Valid(body)
}

func kongStartsWithObject(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// kongBodyInputFacts 是 §3.3 与 §3.4 两道门的结论，由一趟 input 扫描同时算出。
type kongBodyInputFacts struct {
	triggerSettled bool // NormalizeCompactionTriggerInputOrder 必然原样返回
	orphanFree     bool // sanitizeOpenAIResponsesOrphanToolOutputs 必然返回 false
}

// kongInputFacts 返回 input 判定；sure 为 false 表示判断不了（重复键、结构异常），调用方回落。调用前先确认可解码。
func kongInputFacts(body []byte) (kongBodyInputFacts, bool) {
	if entry := kongBodyEntryFor(body); entry != nil {
		entry.inputOnce.Do(func() { entry.inputFacts, entry.inputFactsSure = kongComputeInputFacts(entry.body) })
		return entry.inputFacts, entry.inputFactsSure
	}
	return kongComputeInputFacts(body)
}

func kongComputeInputFacts(body []byte) (kongBodyInputFacts, bool) {
	view := kongBytesView(body)
	inputRaw, previousRaw, inputCount, previousCount, sure := kongTopLevelInputMembers(body, view)
	if !sure || inputCount > 1 || previousCount > 1 {
		return kongBodyInputFacts{}, false
	}
	if inputCount == 0 || !strings.HasPrefix(inputRaw, "[") {
		// 两个原函数都只处理数组形式的 input。
		return kongBodyInputFacts{triggerSettled: true, orphanFree: true}, true
	}

	// 孤儿清理只看调用项、输出项与 item_reference；其余对象项在它的两轮循环里都原样保留，用非对象的占位值
	// 代替，结论不变，也省得为每一项分配投影。
	var items []any
	itemCount := 0
	triggerCount := 0
	lastIsTrigger := false
	if !kongScanArray(inputRaw, func(elementRaw string) bool {
		itemCount++
		if !strings.HasPrefix(elementRaw, "{") {
			items = append(items, kongNonString{})
			lastIsTrigger = false
			return true
		}
		var raws [4]string // type、id、call_id、name 的原文
		var counts [4]int
		if !kongScanObject(elementRaw, func(keyRaw, valueRaw string) bool {
			name, ok := KongDecodeJSONString(keyRaw)
			if !ok {
				sure = false
				return false
			}
			field := -1
			switch name {
			case "type":
				field = 0
			case "id":
				field = 1
			case "call_id":
				field = 2
			case "name":
				field = 3
			}
			if field < 0 {
				return true
			}
			counts[field]++
			if counts[field] > 1 {
				sure = false
				return false
			}
			raws[field] = valueRaw
			return true
		}) || !sure {
			sure = false
			return false
		}
		var projected [4]any
		for i, raw := range raws {
			if counts[i] == 0 {
				continue
			}
			value, ok := kongProjectValue(raw)
			if !ok {
				sure = false
				return false
			}
			projected[i] = value
		}
		typ, _ := projected[0].(string)
		isTrigger := counts[0] == 1 && typ == "compaction_trigger"
		if isTrigger {
			triggerCount++
		}
		lastIsTrigger = isTrigger
		trimmed := strings.TrimSpace(typ)
		if trimmed != "item_reference" && !isCodexToolCallContextItemType(trimmed) && !isCodexToolCallOutputItemType(trimmed) {
			items = append(items, kongNonString{})
			return true
		}
		item := make(map[string]any, 4)
		for i, field := range [...]string{"type", "id", "call_id", "name"} {
			if counts[i] == 1 {
				item[field] = projected[i]
			}
		}
		items = append(items, item)
		return true
	}) || !sure {
		return kongBodyInputFacts{}, false
	}

	facts := kongBodyInputFacts{
		triggerSettled: itemCount == 0 || triggerCount == 0 || (triggerCount == 1 && lastIsTrigger),
	}
	var previous any
	if previousCount == 1 {
		value, ok := kongProjectValue(previousRaw)
		if !ok {
			return kongBodyInputFacts{}, false
		}
		previous = value
	}
	hasPreviousResponseID := strings.TrimSpace(firstNonEmptyString(previous)) != ""
	facts.orphanFree = !sanitizeOpenAIResponsesOrphanToolOutputs(map[string]any{"input": items}, items, hasPreviousResponseID)
	return facts, true
}

// kongTopLevelInputMembers 取顶层 input 与 previous_response_id 的原文和出现次数（按 encoding/json 的键名
// 语义）。登记过的请求体有顶层索引时直接用它：建索引时已排除键名带转义与重复键，这时两种键名语义一致。
func kongTopLevelInputMembers(body []byte, view string) (inputRaw, previousRaw string, inputCount, previousCount int, sure bool) {
	if entry := kongBodyEntryFor(body); entry != nil {
		if index := entry.topLevelIndex(); index != nil {
			if span, ok := index.values["input"]; ok {
				inputRaw, inputCount = view[span.start:span.end], 1
			}
			if span, ok := index.values["previous_response_id"]; ok {
				previousRaw, previousCount = view[span.start:span.end], 1
			}
			return inputRaw, previousRaw, inputCount, previousCount, true
		}
	}
	sure = true
	if !kongScanObject(view, func(keyRaw, valueRaw string) bool {
		name, ok := KongDecodeJSONString(keyRaw)
		if !ok {
			sure = false
			return false
		}
		switch name {
		case "input":
			inputCount++
			inputRaw = valueRaw
		case "previous_response_id":
			previousCount++
			previousRaw = valueRaw
		}
		return true
	}) {
		sure = false
	}
	return inputRaw, previousRaw, inputCount, previousCount, sure
}

// ── §3.4 compaction trigger 排序 ────────────────────────────────────────

// kongCompactionTriggerSettled 为真时 NormalizeCompactionTriggerInputOrder 必然返回 (body, false, nil)。
func kongCompactionTriggerSettled(body []byte) bool {
	if !kongFastpathOn(kongFastpathTrigger) || !kongBodyDecodable(body) {
		return false
	}
	facts, sure := kongInputFacts(body)
	return sure && facts.triggerSettled
}

// ── §3.3 孤儿 output 清理 ──────────────────────────────────────────────

// kongOrphanCleanupMayApply 为假时孤儿清理必然不改写，不必为它完整解码。
func kongOrphanCleanupMayApply(body []byte) bool {
	if !kongFastpathOn(kongFastpathOrphan) || !kongBodyDecodable(body) {
		return true
	}
	facts, sure := kongInputFacts(body)
	return !sure || !facts.orphanFree
}

// ── §3.5 python 工具别名 ───────────────────────────────────────────────

// kongNoReservedToolName 为真时 aliasOpenAIOAuthReservedToolNamesBody 必然返回 (body, nil, false, nil)：没有任何
// 字符串需要改成别名，也就不会改写或报名字冲突。检查全部字符串（键和值），与工具名出现在哪里无关。
func kongNoReservedToolName(body []byte) bool {
	if !kongFastpathOn(kongFastpathAlias) || !kongBodyDecodable(body) {
		return false
	}
	view := kongBytesView(body)
	for i := 0; i < len(view); {
		j := strings.IndexByte(view[i:], '"')
		if j < 0 {
			return true
		}
		start := i + j
		end, ok := kongStringEnd(view, start)
		if !ok {
			return false
		}
		if kongJSONStringIsReservedToolName(view[start+1 : end-1]) {
			return false
		}
		i = end
	}
	return true
}

// kongJSONStringIsReservedToolName 判断一个字符串 token 的内容（不含引号）按 encoding/json 解码后，是否满足
// aliasOpenAIOAuthReservedToolName(s) != s，即去掉首尾空白、忽略大小写后等于 codexReservedPythonToolName。
// 边读边解码，遇到第一个不符的字符就返回，不分配。
func kongJSONStringIsReservedToolName(content string) bool {
	target := codexReservedPythonToolName
	matched := 0
	phase := 0 // 0：开头的空白；1：比对保留名；2：结尾的空白
	for i := 0; i < len(content); {
		r, size := kongDecodeJSONRune(content, i)
		i += size
		switch phase {
		case 0:
			if unicode.IsSpace(r) {
				continue
			}
			phase = 1
			fallthrough
		case 1:
			want, wantSize := utf8.DecodeRuneInString(target[matched:])
			if !kongRuneEqualFold(r, want) {
				return false
			}
			matched += wantSize
			if matched == len(target) {
				phase = 2
			}
		case 2:
			if !unicode.IsSpace(r) {
				return false
			}
		}
	}
	return phase == 2
}

// kongDecodeJSONRune 按 encoding/json 的规则从字符串内容的 s[i] 解出一个字符：处理转义与代理对，孤立的代理项
// 与非法 UTF-8 都记为 U+FFFD。
func kongDecodeJSONRune(s string, i int) (rune, int) {
	if s[i] != '\\' {
		r, size := utf8.DecodeRuneInString(s[i:])
		return r, size
	}
	if i+1 >= len(s) {
		return utf8.RuneError, 1
	}
	switch s[i+1] {
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'u':
		first, ok := kongHex4(s, i+2)
		if !ok {
			return utf8.RuneError, 2
		}
		if first >= 0xD800 && first < 0xDC00 {
			if i+12 <= len(s) && s[i+6] == '\\' && s[i+7] == 'u' {
				if second, ok := kongHex4(s, i+8); ok && second >= 0xDC00 && second < 0xE000 {
					return (first-0xD800)<<10 | (second - 0xDC00) + 0x10000, 12
				}
			}
			return utf8.RuneError, 6
		}
		if first >= 0xDC00 && first < 0xE000 {
			return utf8.RuneError, 6
		}
		return first, 6
	default:
		return rune(s[i+1]), 2
	}
}

func kongHex4(s string, i int) (rune, bool) {
	if i+4 > len(s) {
		return 0, false
	}
	var r rune
	for _, c := range []byte(s[i : i+4]) {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

// kongRuneEqualFold 按 strings.EqualFold 的逐字符规则比较。
func kongRuneEqualFold(a, b rune) bool {
	if a == b {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

// ── §3.2 lite 工具规范化 ───────────────────────────────────────────────

// kongLiteToolsSettled 为真时 normalizeOpenAIResponsesLiteToolsPayload 必然返回 (body, false, nil)。
//
// normalizeOpenAIResponsesLiteTools 只有在 tools 里有 namespace 工具时才读 input，其余分支只读
// parallel_tool_calls、reasoning 与 tools。所以没有 namespace 工具时，在"除 input 外的顶层成员"组成的骨架上
// 调用它，结论与完整 map 相同。
func kongLiteToolsSettled(body []byte) bool {
	if !kongFastpathOn(kongFastpathLite) || !kongBodyDecodable(body) {
		return false
	}
	skeleton := make(map[string]any, 16)
	seen := make(map[string]struct{}, 16)
	sure := true
	if !kongScanObject(kongBytesView(body), func(keyRaw, valueRaw string) bool {
		name, ok := KongDecodeJSONString(keyRaw)
		if !ok {
			sure = false
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			sure = false
			return false
		}
		seen[name] = struct{}{}
		if name == "input" {
			return true
		}
		var value any
		if err := decodeOpenAIJSONUseNumber([]byte(valueRaw), &value); err != nil {
			sure = false
			return false
		}
		skeleton[name] = value
		return true
	}) || !sure {
		return false
	}
	if tools, ok := skeleton["tools"].([]any); ok {
		for _, raw := range tools {
			if tool, ok := raw.(map[string]any); ok && strings.TrimSpace(firstNonEmptyString(tool["type"])) == "namespace" {
				return false
			}
		}
	}
	changed, err := normalizeOpenAIResponsesLiteTools(skeleton)
	return err == nil && !changed
}

// ── §3.6 schema 清洗的第二遍 ───────────────────────────────────────────

// kongSkipToolSchemaPatternPass 为真时，sanitizeOpenAIResponsesToolSchemasForPlatform 的第二遍（删除 Python re 不接受
// 的 pattern 与 patternProperties 键）必然原样返回：第一遍的预判放行、对同一份字节做了解析且没有编辑（正常走完全文并
// 做完校验，或撞到两遍共有的深度上限而原样返回），第二遍只在某个 pattern 值或 patternProperties 键解码后被
// hasUnsupportedOpenAIRegexPattern 判为不接受时才编辑，而原文里没有任何可能拼出这类写法的字节。8 MiB 以下的请求体
// 够不着第一遍的编辑数上限，第一遍不会提前停下而跳过校验。
func kongSkipToolSchemaPatternPass(platform string, body []byte, firstPassChanged bool) bool {
	return kongFastpathOn(kongFastpathLookaround) &&
		!firstPassChanged &&
		shouldRepairOpenAIResponsesNullToolSchemaType(platform) &&
		shouldSanitizeOpenAIResponsesToolSchemaPatterns(platform) &&
		len(body) < 8<<20 &&
		openAIResponsesBodyMayNeedToolParameterRepair(body) &&
		!kongMayContainUnsupportedRegex(body)
}

var kongLiteralParenQuery = []byte("(?")

// kongMayContainUnsupportedRegex 判断原文里有没有哪个字符串可能在解码后被 hasUnsupportedOpenAIRegexPattern 判为 Python
// re 不接受。那个判定只认两类写法：反斜杠后面是 Python 不认的 ASCII 字母，或反斜杠在末尾；(? 后面是 =、!、<、'，
// 或 (? 在末尾。这里命中的是：
//   - (、? 或反斜杠写成 \u 转义；
//   - 转义的反斜杠 \\ 后面紧跟字符串结束、Python 不认的字母，或解码为这类字母的 \u 转义；
//   - 字面的 (? 后面紧跟 =、!、<、'、字符串结束，或解码为前四者之一的 \u 转义。
//
// 不区分是不是 pattern、不看字符类、不把相邻的反斜杠配对，只会多判，不会漏判。常见的 (?:、\\n、\\d 不命中。
func kongMayContainUnsupportedRegex(body []byte) bool {
	view := kongBytesView(body)
	for i := 0; ; {
		j := bytes.IndexByte(body[i:], '\\')
		if j < 0 {
			break
		}
		k := i + j
		if k+1 >= len(body) {
			return true
		}
		switch body[k+1] {
		case 'u':
			r, ok := kongHex4(view, k+2)
			if !ok || r == '(' || r == '?' || r == '\\' {
				return true
			}
			i = k + 6
		case '\\':
			if kongRegexEscapeUnsupported(body, view, k+2) {
				return true
			}
			i = k + 2
		default:
			i = k + 2
		}
	}
	for i := 0; ; {
		j := bytes.Index(body[i:], kongLiteralParenQuery)
		if j < 0 {
			return false
		}
		next := i + j + len(kongLiteralParenQuery)
		if next >= len(body) {
			return true
		}
		switch body[next] {
		case '=', '!', '<', '\'', '"':
			return true
		case '\\':
			if next+1 < len(body) && body[next+1] == 'u' {
				r, ok := kongHex4(view, next+2)
				if !ok || r == '=' || r == '!' || r == '<' || r == '\'' {
					return true
				}
			}
		}
		i = next
	}
}

// kongRegexEscapeUnsupported 看转义的反斜杠之后（原文第 pos 字节起）解码出的下一个字符：字符串在这里结束，或者它是
// Python re 不认的 ASCII 字母，都算命中。
func kongRegexEscapeUnsupported(body []byte, view string, pos int) bool {
	if pos >= len(body) || body[pos] == '"' {
		return true
	}
	c := body[pos]
	if c == '\\' {
		if pos+1 >= len(body) {
			return true
		}
		if body[pos+1] != 'u' {
			return false // 其余短转义解码为引号、反斜杠、斜杠或控制字符，都不是字母
		}
		r, ok := kongHex4(view, pos+2)
		if !ok {
			return true
		}
		if r >= 0x80 {
			return false
		}
		c = byte(r)
	}
	return isASCIILetter(c) && !pythonRegexEscapeAllowed(c)
}

// ── 空 base64 图片检查的预判 ───────────────────────────────────────────

// kongMayContainEscapedWordChar 判断原文里有没有把小写字母或下划线写成 \u 转义（005f、005F、006X、007X）。
// openAIJSONValueMayContainEmptyBase64InputImage 找的是 type 为 input_image 的项；原文里没有字面的 input_image
// 时，只有这类转义才可能拼出它。codex 请求里常见的 \u001b 之类不命中。逐个看反斜杠，只会多判。
func kongMayContainEscapedWordChar(body []byte) bool {
	for i := 0; ; {
		j := bytes.IndexByte(body[i:], '\\')
		if j < 0 {
			return false
		}
		k := i + j
		if k+4 < len(body) && body[k+1] == 'u' && body[k+2] == '0' && body[k+3] == '0' {
			switch body[k+4] {
			case '6', '7':
				return true
			case '5':
				if k+5 < len(body) && (body[k+5] == 'f' || body[k+5] == 'F') {
					return true
				}
			}
		}
		i = k + 1
	}
}

// ── item id 清洗 ──────────────────────────────────────────────────────

// kongInputItemIDsClean 为真时 sanitizeOpenAIResponsesInputItemIDs 必然原样返回：没有任何一项需要删 id 或 call_id。
// 原函数用 gjson 取每项第一次出现的 type、id、call_id，这里按同样的语义取值，判定交给原来的
// shouldStripOpenAIResponsesInputItemID 与 shouldStripOpenAIResponsesNonPairCallID。键名或相关的值带反斜杠、
// type 不是字符串时一律回落：那时 gjson 的取值与原文不同。只对登记过、在 gjson 意义上合法的请求体生效。
func kongInputItemIDsClean(body []byte) bool {
	if !kongFastpathOn(kongFastpathItemIDs) {
		return false
	}
	entry := kongBodyEntryFor(body)
	if entry == nil || !entry.gjsonValidity() {
		return false
	}
	input := gjson.Get(kongBytesView(body), "input")
	if !input.IsArray() {
		return true
	}
	clean := true
	sure := kongScanArray(input.Raw, func(elementRaw string) bool {
		if !strings.HasPrefix(elementRaw, "{") {
			return true
		}
		var typeRaw, idRaw string
		var hasType, hasID, hasCallID bool
		ok := kongScanObject(elementRaw, func(keyRaw, valueRaw string) bool {
			if strings.IndexByte(keyRaw, '\\') >= 0 {
				clean = false
				return false
			}
			switch keyRaw[1 : len(keyRaw)-1] {
			case "type":
				if !hasType {
					hasType, typeRaw = true, valueRaw
				}
			case "id":
				if !hasID {
					hasID, idRaw = true, valueRaw
				}
			case "call_id":
				hasCallID = true
			}
			return true
		})
		if !ok || !clean {
			clean = false
			return false
		}
		itemType := ""
		if hasType {
			if !strings.HasPrefix(typeRaw, `"`) || strings.IndexByte(typeRaw, '\\') >= 0 {
				clean = false
				return false
			}
			itemType = typeRaw[1 : len(typeRaw)-1]
		}
		trimmed := strings.TrimSpace(itemType)
		if hasCallID && shouldStripOpenAIResponsesNonPairCallID(trimmed) {
			clean = false
			return false
		}
		if hasID && strings.HasPrefix(idRaw, `"`) {
			if strings.IndexByte(idRaw, '\\') >= 0 || shouldStripOpenAIResponsesInputItemID(trimmed, idRaw[1:len(idRaw)-1]) {
				clean = false
				return false
			}
		}
		return true
	})
	return sure && clean
}

// ── §3.1 handler 的 bootstrap ──────────────────────────────────────────

// KongCodexCallOutputBootstrapMayApply 为假时 handler 的 normalizeCodexCallOutputBootstrap 必然不改写：input 里
// 没有任何一项让 isCandidate 为真。两个候选判定都要求 type 为 function_call_output、namespace 是特定的非空
// 字符串，所以只把这样的项（连同 name 与 output）交给 isCandidate。原函数遇到重复键、解码失败都静默返回原体，
// 所以可解码这个前提只是为了让结构扫描可靠。它用 encoding/json 的 Valid 判断：嵌套超过 10000 层就停下，不会像
// gjson 的 Valid 那样对嵌套极深的请求体无限递归。
func KongCodexCallOutputBootstrapMayApply(body []byte, isCandidate func(map[string]any) bool) bool {
	if !kongFastpathOn(kongFastpathBootstrap) {
		return true
	}
	candidates, sure := kongBootstrapCandidates(body)
	if !sure {
		return true
	}
	for _, item := range candidates {
		if isCandidate(item) {
			return true
		}
	}
	return false
}

// kongBootstrapCandidates 取出 input 里 type 为 function_call_output、namespace 为字符串的项的投影。automation
// 与 delegation 两次调用对同一份请求体共用这份结果；通常一项也没有。
func kongBootstrapCandidates(body []byte) ([]map[string]any, bool) {
	if entry := kongBodyEntryFor(body); entry != nil {
		entry.bootstrapOnce.Do(func() { entry.bootstrapItems, entry.bootstrapSure = kongComputeBootstrapCandidates(entry.body) })
		return entry.bootstrapItems, entry.bootstrapSure
	}
	return kongComputeBootstrapCandidates(body)
}

func kongComputeBootstrapCandidates(body []byte) ([]map[string]any, bool) {
	if !kongBodyDecodable(body) {
		return nil, false
	}
	input := gjson.Get(kongBytesView(body), "input")
	if !input.IsArray() {
		return nil, true
	}
	var candidates []map[string]any
	sure := true
	if !kongScanArray(input.Raw, func(elementRaw string) bool {
		if !strings.HasPrefix(elementRaw, "{") {
			return true
		}
		var typeRaw, namespaceRaw, nameRaw, outputRaw string
		if !kongScanObject(elementRaw, func(keyRaw, valueRaw string) bool {
			name, ok := KongDecodeJSONString(keyRaw)
			if !ok {
				sure = false
				return false
			}
			switch name {
			case "type":
				typeRaw = valueRaw
			case "namespace":
				namespaceRaw = valueRaw
			case "name":
				nameRaw = valueRaw
			case "output":
				outputRaw = valueRaw
			}
			return true
		}) || !sure {
			sure = false
			return false
		}
		if !strings.HasPrefix(typeRaw, `"`) || !strings.HasPrefix(namespaceRaw, `"`) {
			return true
		}
		typ, ok := KongDecodeJSONString(typeRaw)
		if !ok {
			sure = false
			return false
		}
		if typ != "function_call_output" {
			return true
		}
		item := map[string]any{"type": typ}
		for field, raw := range map[string]string{"namespace": namespaceRaw, "name": nameRaw, "output": outputRaw} {
			if raw == "" {
				continue
			}
			value, ok := kongProjectValue(raw)
			if !ok {
				sure = false
				return false
			}
			item[field] = value
		}
		candidates = append(candidates, item)
		return true
	}) || !sure {
		return nil, false
	}
	return candidates, true
}

// ── OAuth 兼容处理的逐项元数据检查 ─────────────────────────────────────

// kongOpenAIInputItemMetadataName 是 normalizeOpenAIOAuthResponsesCompatibilityBody 逐项删除的字段。
const kongOpenAIInputItemMetadataName = "internal_chat_message_metadata_passthrough"

// kongNoInputItemMetadataPassthrough 为真时，normalizeOpenAIOAuthResponsesCompatibilityBody 最后那轮逐项删除
// 必然什么都不删：input 里没有任何对象项带这个键。原逻辑用 gjson 按键名查找（先反转义再比较），所以键名带
// 反斜杠的一律回落。只对登记过、在 gjson 意义上合法的请求体生效，结构扫描才可靠。
func kongNoInputItemMetadataPassthrough(body []byte, input gjson.Result) bool {
	if !kongFastpathOn(kongFastpathMetadata) {
		return false
	}
	entry := kongBodyEntryFor(body)
	if entry == nil || !entry.gjsonValidity() || !input.IsArray() {
		return false
	}
	found := false
	sure := kongScanArray(input.Raw, func(elementRaw string) bool {
		if !strings.HasPrefix(elementRaw, "{") {
			return true
		}
		ok := kongScanObject(elementRaw, func(keyRaw, _ string) bool {
			if strings.IndexByte(keyRaw, '\\') >= 0 || keyRaw[1:len(keyRaw)-1] == kongOpenAIInputItemMetadataName {
				found = true
				return false
			}
			return true
		})
		if !ok {
			found = true
		}
		return !found
	})
	return sure && !found
}

// kongFastpathAnyOn 报告是否有任何一项开着；全关时不登记请求体。
func kongFastpathAnyOn() bool {
	settings := kongFastpathSettingsCurrent()
	for _, on := range settings.enabled {
		if on {
			return true
		}
	}
	return false
}
