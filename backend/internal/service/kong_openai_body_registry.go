package service

import (
	"encoding/json"
	"strings"
	"sync"
	"unsafe"

	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

// 请求体登记表（DESIGN §3.7）与 gjson 顶层查找加速（§3.8）。
//
// 只登记流水线自己持有、生命期明确的请求体版本：登记期间表里持有切片，这份内存不会被回收、也就不会被别的
// 对象复用；请求体又不会被原地修改，所以"起始地址 + 长度"唯一确定一份字节。表里只存由这份字节算出的事实
// （数值与布尔值），在首次用到时计算。

const (
	// kongBodyRegistryMinBytes 以下的请求体不登记：顶层扫描本来就便宜，不值得多一次表查找。
	kongBodyRegistryMinBytes = 64 << 10
	// kongBodyIndexMaxMembers 以上的顶层成员不建索引，这份请求体只是不加速。
	kongBodyIndexMaxMembers = 256
)

type kongBodySpan struct {
	start, end int
}

// kongBodyTopLevelIndex 是顶层成员的索引：每个键第一次出现时值的位置。
type kongBodyTopLevelIndex struct {
	values map[string]kongBodySpan
}

type kongBodyEntry struct {
	body []byte
	refs int // 受 kongBodyRegistry.mu 保护
	sum  uint64

	// jsonValid 是 encoding/json 的 Valid：嵌套超过 10000 层就返回 false，递归深度有界。gjson 的 Valid 递归没有
	// 深度上限，只在 jsonValid 为真时才在这里调用；嵌套更深的请求体交还原生实现，由原代码在原来的位置、以原来的
	// 方式处理。
	jsonValidOnce sync.Once
	jsonValid     bool

	gjsonValidOnce sync.Once
	gjsonValid     bool

	indexOnce sync.Once
	index     *kongBodyTopLevelIndex // nil 表示不加速

	inputOnce      sync.Once
	inputFacts     kongBodyInputFacts
	inputFactsSure bool

	bootstrapOnce  sync.Once
	bootstrapItems []map[string]any
	bootstrapSure  bool
}

var kongBodyRegistry = struct {
	mu      sync.RWMutex
	entries map[uintptr]*kongBodyEntry
}{entries: make(map[uintptr]*kongBodyEntry)}

// kongBodyRegistryCheckMutation 为真时，登记时记下校验和、最后一次注销时核对，发现请求体被原地修改就 panic。
// 只在测试构建里打开（kong_testhooks_unit.go）。
var kongBodyRegistryCheckMutation = false

// KongRegisterRequestBody 登记一份请求体，返回可重复调用的注销函数。过短的请求体、快速路径关闭时不登记。
func KongRegisterRequestBody(body []byte) (release func()) {
	if len(body) < kongBodyRegistryMinBytes || !kongFastpathAnyOn() {
		return func() {}
	}
	key := uintptr(unsafe.Pointer(unsafe.SliceData(body)))
	kongBodyRegistry.mu.Lock()
	entry, ok := kongBodyRegistry.entries[key]
	switch {
	case !ok:
		entry = &kongBodyEntry{body: body}
		if kongBodyRegistryCheckMutation {
			entry.sum = xxhash.Sum64(body)
		}
		kongBodyRegistry.entries[key] = entry
	case len(entry.body) != len(body):
		// 同一起始地址、不同长度的切片：表里只能放一份，这一份不登记。
		kongBodyRegistry.mu.Unlock()
		return func() {}
	}
	entry.refs++
	kongBodyRegistry.mu.Unlock()

	// 注销后闭包不再引用条目：调用方常把注销函数挂在 defer 上，提前注销之后，闭包不能继续保活这份请求体。
	var once sync.Once
	held := entry
	return func() {
		once.Do(func() {
			released := held
			held = nil
			kongBodyRegistry.mu.Lock()
			defer kongBodyRegistry.mu.Unlock()
			released.refs--
			if released.refs > 0 {
				return
			}
			delete(kongBodyRegistry.entries, key)
			if kongBodyRegistryCheckMutation && xxhash.Sum64(released.body) != released.sum {
				panic("kong: 登记的请求体被原地修改")
			}
		})
	}
}

// kongLookupRegisteredBody 按起始地址与长度查登记的请求体，不存在时返回 nil。
func kongLookupRegisteredBody(data unsafe.Pointer, length int) *kongBodyEntry {
	if length < kongBodyRegistryMinBytes {
		return nil
	}
	kongBodyRegistry.mu.RLock()
	entry := kongBodyRegistry.entries[uintptr(data)]
	kongBodyRegistry.mu.RUnlock()
	if entry == nil || len(entry.body) != length {
		return nil
	}
	return entry
}

func kongBodyEntryFor(body []byte) *kongBodyEntry {
	return kongLookupRegisteredBody(unsafe.Pointer(unsafe.SliceData(body)), len(body))
}

func (e *kongBodyEntry) jsonValidity() bool {
	e.jsonValidOnce.Do(func() { e.jsonValid = json.Valid(e.body) })
	return e.jsonValid
}

// gjsonState 返回请求体在 gjson 意义上是否合法；known 为 false 时这里不判断，调用方交还原生实现。
func (e *kongBodyEntry) gjsonState() (valid, known bool) {
	if !e.jsonValidity() {
		return false, false
	}
	e.gjsonValidOnce.Do(func() { e.gjsonValid = gjson.ValidNative(kongBytesView(e.body)) })
	return e.gjsonValid, true
}

// gjsonValidity 报告请求体确定在 gjson 意义上合法。
func (e *kongBodyEntry) gjsonValidity() bool {
	valid, known := e.gjsonState()
	return valid && known
}

func (e *kongBodyEntry) topLevelIndex() *kongBodyTopLevelIndex {
	e.indexOnce.Do(func() {
		if e.gjsonValidity() {
			e.index = kongBuildTopLevelIndex(kongBytesView(e.body))
		}
	})
	return e.index
}

// kongBuildTopLevelIndex 用原生 gjson 遍历一次顶层。不是对象、键里有转义、有重复键或成员过多时返回 nil：
// 这些情况下原生查找的语义不能用"按键取第一次出现的值"来概括，一律交还原生实现。
func kongBuildTopLevelIndex(json string) *kongBodyTopLevelIndex {
	root := gjson.Parse(json)
	if !root.IsObject() {
		return nil
	}
	base := uintptr(unsafe.Pointer(unsafe.StringData(json)))
	index := &kongBodyTopLevelIndex{values: make(map[string]kongBodySpan, 16)}
	usable := true
	root.ForEach(func(key, value gjson.Result) bool {
		if len(index.values) >= kongBodyIndexMaxMembers || strings.IndexByte(key.Raw, '\\') >= 0 || len(key.Raw) < 2 {
			usable = false
			return false
		}
		name := key.Raw[1 : len(key.Raw)-1]
		if _, duplicate := index.values[name]; duplicate {
			usable = false
			return false
		}
		start, ok := kongOffsetWithin(base, len(json), value.Raw)
		if !ok {
			usable = false
			return false
		}
		index.values[name] = kongBodySpan{start: start, end: start + len(value.Raw)}
		return true
	})
	if !usable {
		return nil
	}
	return index
}

// kongOffsetWithin 返回 s 在以 base 开头、长 length 的字符串里的偏移。
func kongOffsetWithin(base uintptr, length int, s string) (int, bool) {
	if len(s) == 0 {
		return 0, false
	}
	offset := int(uintptr(unsafe.Pointer(unsafe.StringData(s))) - base)
	if offset < 0 || offset+len(s) > length {
		return 0, false
	}
	return offset, true
}

// ── gjson 钩子 ───────────────────────────────────────────────────────────

func init() {
	gjson.SetQueryHooks(kongGjsonGetHook, kongGjsonValidHook)
}

func kongRegisteredForString(json string) *kongBodyEntry {
	if len(json) < kongBodyRegistryMinBytes || !kongFastpathOn(kongFastpathIndex) {
		return nil
	}
	return kongLookupRegisteredBody(unsafe.Pointer(unsafe.StringData(json)), len(json))
}

func kongGjsonValidHook(json string) (valid, handled bool) {
	entry := kongRegisteredForString(json)
	if entry == nil {
		return false, false
	}
	return entry.gjsonState()
}

func kongGjsonGetHook(json, path string) (gjson.Result, bool) {
	entry := kongRegisteredForString(json)
	if entry == nil {
		return gjson.Result{}, false
	}
	first, rest, ok := kongSplitSimplePath(path)
	if !ok {
		return gjson.Result{}, false
	}
	index := entry.topLevelIndex()
	if index == nil {
		return gjson.Result{}, false
	}
	span, found := index.values[first]
	if !found {
		return gjson.Result{}, true
	}
	value := json[span.start:span.end]
	if rest == "" {
		result := gjson.Parse(value)
		result.Index = span.start
		return result, true
	}
	if value[0] != '{' && value[0] != '[' {
		// 原生 gjson 不会沿标量值继续取子路径。
		return gjson.Result{}, true
	}
	result := gjson.GetNative(value, rest)
	result.Index = 0
	if offset, within := kongOffsetWithin(uintptr(unsafe.Pointer(unsafe.StringData(json))), len(json), result.Raw); within {
		result.Index = offset
	}
	return result, true
}

// kongSplitSimplePath 把只由字母、数字、_、- 组成、用 . 连接的路径拆成第一段与其余部分。其他写法（通配、
// 转义、查询、修饰符、管道、空段）返回 false。
func kongSplitSimplePath(path string) (first, rest string, ok bool) {
	if path == "" {
		return "", "", false
	}
	segmentStart := 0
	firstEnd := -1
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '.' {
			if i == segmentStart {
				return "", "", false
			}
			if firstEnd < 0 {
				firstEnd = i
			}
			segmentStart = i + 1
			continue
		}
		c := path[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return "", "", false
		}
	}
	if firstEnd == len(path) {
		return path, "", true
	}
	return path[:firstEnd], path[firstEnd+1:], true
}
