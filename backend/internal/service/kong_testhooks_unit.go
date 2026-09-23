//go:build unit

package service

import (
	"context"
	"time"
)

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

// KongSetOpenAIBodyFastpathItemsOffForTest 打开请求体快速路径的全部项，只关掉 names 这几项（名字同
// KONG_OPENAI_BODY_FASTPATH_OFF），返回恢复函数；名字不认识时 panic，免得测试静默地什么都没关。
func KongSetOpenAIBodyFastpathItemsOffForTest(names ...string) func() {
	previous := kongFastpathSettingsCurrent
	var settings kongFastpathSettings
	for i := range settings.enabled {
		settings.enabled[i] = true
	}
	for _, name := range names {
		found := false
		for i, known := range kongFastpathItemNames {
			if name == known {
				settings.enabled[i] = false
				found = true
			}
		}
		if !found {
			panic("unknown fastpath item " + name)
		}
	}
	kongFastpathSettingsCurrent = func() kongFastpathSettings { return settings }
	return func() { kongFastpathSettingsCurrent = previous }
}

// KongInstallCodexCatalogForTest 给网关服务装上目录存档组件：候选账号取自 candidates，拉取交给 fetch，
// 不读写数据库，复核原样放行。返回停止函数。
func KongInstallCodexCatalogForTest(
	s *OpenAIGatewayService,
	candidates func() []Account,
	fetch func(account *Account, clientVersion string) (*OpenAIModelsResponse, error),
) func() {
	c := newKongCodexCatalog(true, nil, kongCodexCatalogTestGateway{candidates: candidates, fetch: fetch}, time.Now)
	s.SetKongCodexCatalog(c)
	return c.Stop
}

type kongCodexCatalogTestGateway struct {
	candidates func() []Account
	fetch      func(account *Account, clientVersion string) (*OpenAIModelsResponse, error)
}

func (g kongCodexCatalogTestGateway) kongCodexCatalogCandidates(context.Context, int64) ([]Account, error) {
	return g.candidates(), nil
}

func (g kongCodexCatalogTestGateway) kongCodexCatalogRecheck(_ context.Context, account *Account, _ int64) (*Account, string) {
	copied := *account
	return &copied, account.GetCredential("plan_type")
}

func (g kongCodexCatalogTestGateway) kongCodexCatalogFetch(_ context.Context, account *Account, clientVersion string) (*OpenAIModelsResponse, error) {
	return g.fetch(account, clientVersion)
}
