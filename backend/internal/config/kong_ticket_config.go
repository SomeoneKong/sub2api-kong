package config

// Codex 票据功能的配置。字段定义单独放在本文件里，`GatewayConfig` 那边只留一行挂载点——
// 本 fork 会长期 rebase 到新的上游基线，改动集中在自有文件里时冲突面最小。
//
// 注意这些键**没有热更新**：上游全程没有配置文件监视，config 只在启动时读一次（见 config.go
// 里的 viper 初始化）。改了要重启容器。想要热更新只能走 `settings` 表那条路（上游的账号调度
// 阈值就在那儿），代价是要另加读缓存与写入校验。

// KongCodexTicketConfig 是 Codex 票据功能里需要运维调整的部分。
type KongCodexTicketConfig struct {
	// AcceptExtra 是「某个门控模型额外接受哪些归因结果」的白名单，格式
	// `model:accepted[,accepted…][;model:…]`，例如 `gpt-5.6-sol:gpt-6-astra`——请求 sol 时，
	// 归因为 astra 的票同样接受。
	//
	// **每个模型永远接受自己的归因**，不必也不该在这里重复写；漏写自己的后果是该模型永久拒服，
	// 所以那一条由代码补齐、不交给配置。
	//
	// 为什么是白名单而不是一条档位链：校准资料里是 13 个候选，跨 gpt 与 claude 两个家族，
	// gpt-5.6 内部还有 sol / terra / luna 三个同辈。它们之间并不存在一条全序，硬排成链等于凭空
	// 给互不可比的模型定先后，而那个先后直接决定放行与拒服。
	//
	// 写成一个字符串而不是 yaml 里的结构化 map，是为了让它同时能用 viper 的 env 绑定设定
	// （`GATEWAY_KONG_CODEX_TICKET_ACCEPT_EXTRA`）——现网两个实例都没有挂 config 文件，配置全
	// 从环境变量来，而 map 没法从单个环境变量表达。
	AcceptExtra string `mapstructure:"accept_extra"`
}
