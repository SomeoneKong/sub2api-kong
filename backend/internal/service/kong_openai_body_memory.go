package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/tidwall/gjson"
)

// OpenAI 请求体的内存（设计见 DESIGN-openai-request-body-memory.md）：请求视图的字段不拖住请求体、item id
// 清洗一次拼出结果、身份改写的两次写回共用一份副本。各项只改"怎么写出结果"，写出的字节与原代码相同。

// kongDetachOpenAIRequestView 把视图取出的字符串换成独立副本。视图是零拷贝解析的，字段原本是请求体的子串；
// Forward 改写请求体之后会对新版本重建视图，字段一路传到请求结束，会把那个中间版本整块保活。
func kongDetachOpenAIRequestView(view *openAIRequestView) {
	if !kongFastpathOn(kongFastpathViewStrings) {
		return
	}
	view.Model = strings.Clone(view.Model)
	view.PromptCacheKey = strings.Clone(view.PromptCacheKey)
	view.PreviousResponseID = strings.Clone(view.PreviousResponseID)
	view.ServiceTier = strings.Clone(view.ServiceTier)
	view.ReasoningEffort = strings.Clone(view.ReasoningEffort)
}

// kongSpliceInputItemIDs 是 sanitizeOpenAIResponsesInputItemIDs 的快路径，只处理"只需要删 id"的请求：取值与判定
// 照抄原函数，删除区间照抄 sjson.DeleteBytes，最后按 replaceOpenAIRawInput 的格式一次拼出结果。handled 为假时由
// 原函数处理：开关关闭、有项需要删 call_id、或删除区间定位不到。
func kongSpliceInputItemIDs(body []byte) (out []byte, changed, handled bool) {
	if !kongFastpathOn(kongFastpathItemIDsSplice) {
		return nil, false, false
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, false, true
	}

	type inputItem struct {
		raw      string
		from, to int // 要删掉的区间，to 为 0 表示原样
	}
	var items []inputItem
	stripped, fallback := false, false
	input.ForEach(func(_, item gjson.Result) bool {
		parsed := inputItem{raw: item.Raw}
		if item.IsObject() {
			itemType := item.Get("type")
			id := item.Get("id")
			trimmedItemType := strings.TrimSpace(itemType.String())
			if item.Get("call_id").Exists() && shouldStripOpenAIResponsesNonPairCallID(trimmedItemType) {
				fallback = true
				return false
			}
			if id.Type == gjson.String && shouldStripOpenAIResponsesInputItemID(trimmedItemType, id.String()) {
				from, to, ok := kongSJSONDeleteRange(item.Raw, "id")
				if !ok {
					fallback = true
					return false
				}
				parsed.from, parsed.to = from, to
				stripped = true
			}
		}
		items = append(items, parsed)
		return true
	})
	if fallback {
		return nil, false, false
	}
	if !stripped {
		return body, false, true
	}

	size := len(body) - len(input.Raw) + 2
	for index, item := range items {
		size += len(item.raw) - (item.to - item.from)
		if index > 0 {
			size++
		}
	}
	result := make([]byte, 0, size)
	result = append(result, body[:input.Index]...)
	result = append(result, '[')
	for index, item := range items {
		if index > 0 {
			result = append(result, ',')
		}
		if item.to == 0 {
			result = append(result, item.raw...)
			continue
		}
		result = append(result, item.raw[:item.from]...)
		result = append(result, item.raw[item.to:]...)
	}
	result = append(result, ']')
	return append(result, body[input.Index+len(input.Raw):]...), true, true
}

// kongSJSONDeleteRange 给出 sjson.DeleteBytes(jstr, key) 对单层键会删掉的区间 [from, to)：DeleteBytes 的结果就是
// jstr[:from] + jstr[to:]。定位与 sjson 一样用 gjson.Get；往回找逗号或 { 的规则照抄 sjson v1.2.5 的
// deleteTailItem，前面是 { 时改为连同后面的逗号一起删。key 必须是不含 . 与通配符的简单键。
func kongSJSONDeleteRange(jstr, key string) (from, to int, ok bool) {
	res := gjson.Get(jstr, key)
	if res.Index <= 0 {
		return 0, 0, false
	}
	from, delNextComma := kongSJSONDeleteTailItem(jstr[:res.Index])
	to = res.Index + len(res.Raw)
	if delNextComma {
		for i := to; i < len(jstr); i++ {
			if jstr[i] <= ' ' {
				continue
			}
			if jstr[i] == ',' {
				to = i + 1
			}
			break
		}
	}
	return from, to, true
}

// kongSJSONDeleteTailItem 逐行照抄 sjson v1.2.5 的 deleteTailItem，返回截断后的长度而不是切片。
func kongSJSONDeleteTailItem(buf string) (int, bool) {
loop:
	for i := len(buf) - 1; i >= 0; i-- {
		// look for either a ',',':','['
		switch buf[i] {
		case '[':
			return len(buf), true
		case ',':
			return i, false
		case ':':
			// delete tail string
			i--
			for ; i >= 0; i-- {
				if buf[i] == '"' {
					i--
					for ; i >= 0; i-- {
						if buf[i] == '"' {
							i--
							if i >= 0 && buf[i] == '\\' {
								i--
								continue
							}
							for ; i >= 0; i-- {
								// look for either a ',','{'
								switch buf[i] {
								case '{':
									return i + 1, true
								case ',':
									return i, false
								}
							}
						}
					}
					break
				}
			}
			break loop
		}
	}
	return len(buf), false
}

// kongIdentitySpliceReserve 是改写 client_metadata 时多留的容量，够随后在同一份副本上原地写回 prompt_cache_key。
const kongIdentitySpliceReserve = 256

// kongSetIdentityRaw 等价于 setRaw(body, key, raw)，setRaw 是调用处原来的 sjson.SetRawBytes：sjson 替换已有的顶层
// 键，就是把 gjson.Get 定位到的那段值换掉。这里按同样的定位拼出新切片并多留一点容量；键不存在时交给 setRaw。
func kongSetIdentityRaw(setRaw func([]byte, string, []byte) ([]byte, error), body []byte, key string, raw []byte) ([]byte, error) {
	if !kongFastpathOn(kongFastpathIdentitySplice) {
		return setRaw(body, key, raw)
	}
	res := gjson.Get(kongBytesView(body), key)
	if res.Index <= 0 {
		return setRaw(body, key, raw)
	}
	end := res.Index + len(res.Raw)
	out := make([]byte, 0, len(body)-len(res.Raw)+len(raw)+kongIdentitySpliceReserve)
	out = append(out, body[:res.Index]...)
	out = append(out, raw...)
	return append(out, body[end:]...), nil
}

// kongSetIdentityString 等价于 set(body, key, value)，set 是调用处原来的 sjson.SetBytes。private 为真表示 body 是
// 调用方刚分配、还没交给任何人的副本（上一处改写确实发生过），容量够时就在它上面原地写回，不再复制一份；否则
// 照常走 set。值要按 sjson 的规则转义、或键不存在时也交给 set。
func kongSetIdentityString(set func([]byte, string, any) ([]byte, error), body []byte, private bool, key, value string) ([]byte, error) {
	if !private || !kongFastpathOn(kongFastpathIdentitySplice) || kongSJSONMustMarshalString(value) {
		return set(body, key, value)
	}
	res := gjson.Get(kongBytesView(body), key)
	if res.Index <= 0 {
		return set(body, key, value)
	}
	start, end := res.Index, res.Index+len(res.Raw)
	valueLen := len(value) + 2
	size := len(body) - len(res.Raw) + valueLen
	if size > cap(body) {
		return set(body, key, value)
	}
	oldLen := len(body)
	body = body[:max(size, oldLen)]
	copy(body[start+valueLen:], body[end:oldLen])
	body[start] = '"'
	copy(body[start+1:], value)
	body[start+1+len(value)] = '"'
	return body[:size], nil
}

// kongSJSONMustMarshalString 照抄 sjson v1.2.5 的 mustMarshalString：为真时 sjson 用 encoding/json 编码字符串，
// 否则直接加引号。
func kongSJSONMustMarshalString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > 0x7f || s[i] == '"' || s[i] == '\\' {
			return true
		}
	}
	return false
}

func init() {
	// 入站 zstd 的快路径在 httputil 里，httputil 不能引用 service，开关由这里注入。
	httputil.KongRequestZstdFastpathEnabled = func() bool { return kongFastpathOn(kongFastpathZstdIn) }
}
