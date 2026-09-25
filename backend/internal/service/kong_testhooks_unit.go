//go:build unit

package service

import "time"

// 只在 -tags=unit 下编译：给其他包的测试用来隔离进程级状态，生产二进制里没有这些函数。

func init() {
	// 测试构建里核对"登记的请求体不被原地修改"：登记时记下校验和，最后一次注销时比对。
	kongBodyRegistryCheckMutation = true
}

// KongSwapOpenAIRequestZstdForTest 换上一份全新的出站压缩状态（开启、没有被标记为改发明文的端点），
// 返回恢复函数。压缩被拒后的"改发明文 24 小时"是进程级状态，不隔离会串到后面的用例。
func KongSwapOpenAIRequestZstdForTest() func() {
	previous := kongOpenAIRequestZstdInstance
	fresh := newKongOpenAIRequestZstd(true, time.Now)
	kongOpenAIRequestZstdInstance = func() *kongOpenAIRequestZstd { return fresh }
	return func() { kongOpenAIRequestZstdInstance = previous }
}

// KongSetOpenAIBodyFastpathForTest 把请求体快速路径整体打开或关闭，返回恢复函数。
func KongSetOpenAIBodyFastpathForTest(on bool) func() {
	previous := kongFastpathSettingsCurrent
	var settings kongFastpathSettings
	for i := range settings.enabled {
		settings.enabled[i] = on
	}
	kongFastpathSettingsCurrent = func() kongFastpathSettings { return settings }
	return func() { kongFastpathSettingsCurrent = previous }
}
