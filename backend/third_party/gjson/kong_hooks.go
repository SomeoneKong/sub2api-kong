package gjson

import "sync/atomic"

// 本文件与 gjson.go 里标了 [kong] 的几处是 sub2api-kong 加的查询钩子，不属于 tidwall/gjson 上游；其余代码与
// v1.18.0 原样一致。升级 gjson 时换成新版本的原文，再把这几处重新加上。
//
// 钩子让嵌入方接管一部分 Get / Valid / ValidBytes 调用：返回 handled=false 时照常交给原生实现。钩子的结果
// 必须与原生实现逐字段相同。钩子内部要用 GetNative / ValidNative，不能再调用 Get / Valid，否则会重入自己。

// GetHook 接管 Get；handled 为 false 时由 GetNative 处理。
type GetHook func(json, path string) (result Result, handled bool)

// ValidHook 接管 Valid 与 ValidBytes；handled 为 false 时由原生校验处理。
type ValidHook func(json string) (valid, handled bool)

// 用 atomic.Value 而不是 atomic.Pointer：本模块的 go.mod 声明 go 1.12，不能用泛型。
var (
	kongGetHook   atomic.Value // GetHook
	kongValidHook atomic.Value // ValidHook
)

// SetQueryHooks 装上查询钩子；传 nil 则卸下对应的钩子。
func SetQueryHooks(get GetHook, valid ValidHook) {
	kongGetHook.Store(get)
	kongValidHook.Store(valid)
}
