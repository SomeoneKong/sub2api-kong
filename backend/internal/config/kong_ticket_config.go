package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

// Codex 票据功能的配置。字段定义单独放在本文件里，`GatewayConfig` 那边只留一行挂载点——
// 本 fork 会长期 rebase 到新的上游基线，改动集中在自有文件里时冲突面最小。
//
// 注意这些键**没有热更新**：上游全程没有配置文件监视，config 只在启动时读一次（见 config.go
// 里的 viper 初始化）。改了要重启容器。想要热更新只能走 `settings` 表那条路（上游的账号调度
// 阈值就在那儿），代价是要另加读缓存与写入校验。

// DefaultKongTicketAcceptExtra 是接受白名单的内置默认：请求 `gpt-5.6-sol` 时，归因为
// `gpt-6-astra` 或 `gpt-5.5` 的票同样接受。
//
// **`gpt-6-astra`**：实测同一账号为 sol 取票时归因常返回 astra——拿到更高档位不该被判成降智。
// 方向是单向的，astra 不接受 sol，那正是这个功能要挡的东西。
//
// **`gpt-5.5`**：指纹法**分不开 sol 与 5.5**。实测一次为 sol 取的票，归因分布是
// sol 0.8214 / 5.5 0.1779 / astra 0.0000166——只看 sol 达不到 0.9 阈值，好票被拒并进冷却。
// 挑战是为 sol 发的、上游实际服务的就是 sol，所以那 17.8% 是**测量噪声，不是降级证据**；拿一个
// 分辨不出的差异去拒票就是无谓拒服。
//
// ⚠️ **代价**：sol 这道门因此**不再能发现 sol → 5.5 的下降**，它只保住"不掉到 luna / terra /
// 更低"。要恢复那一档的分辨力，需要一份能分开 5.6-sol 与 5.5 的校准资料，不是调阈值能解决的。
//
// **显式设成空串即没有任何额外接受关系**（每个模型只接受自己的归因）。
//
// ⚠️ **同时门控两个模型，在单条票据出口上是供不上票的**，这是算出来的、与参数取值无关：票按
// (账号, 模型) 分别存，而取票共用同一个出口槽（静默门槛跨模型），两个模型各自续期就要求出口上
// 相邻两次取票相隔 I/2 ≥ 静默门槛，即 I ≥ 2×门槛 > TTL；而 I = TTL − refresh_before < TTL。
// 无解。最优只能接受空窗：把静默设到门槛之上，代价是每个模型每周期都有一段无票期。要真正同时
// 覆盖两个模型，得让一张票跨模型复用——那要改票的身份定义，且"一张票能否跨模型用"尚未实测。
const DefaultKongTicketAcceptExtra = "gpt-5.6-sol:gpt-6-astra,gpt-5.5"

// KongTicketAcceptExtraEnv 是 AcceptExtra 对应的环境变量名（viper 的 `.` → `_` 映射结果）。
const KongTicketAcceptExtraEnv = "GATEWAY_KONG_CODEX_TICKET_ACCEPT_EXTRA"

// KongTicketStg0AcceptEnv 是 Stg0Accept 对应的环境变量名。
const KongTicketStg0AcceptEnv = "GATEWAY_KONG_CODEX_TICKET_STG0_ACCEPT"

// DefaultKongTicketStg0Accept 是 stg0 白名单的内置默认。除表中列出的，每个门控模型只接受上游回报
// 它自己（含快照后缀，如 `gpt-5.6-sol-2026-03-17`）。
//
// **表里只收"升级投放"**——上游把**更新的**模型投到旧模型名上。`gpt-5.6-sol` 收到 `gpt-6-sol` 就是
// 这种：拿到的是更好的东西，判死票等于为了一次升级白搭一整段出口静默。反方向（回报一个更旧或更小
// 的模型）绝不进这张表，那正是要拦的降智。
//
// 与 AcceptExtra 的默认值**不是同一类补偿**，两张表不要互抄（DESIGN §4.0）：AcceptExtra 补的是指纹
// 归因的测量噪声，那噪声客观存在、不配就会误拒好票；stg0 读的是上游自己的声明，没有噪声，所以这里
// 每加一项都必须能说清"为什么这个替换不算降智"。
//
// ⚠️ 默认拒绝极性有代价：上游投放一个**没配过**的新模型时，该模型的票会被 stg0 判 fail，于是不断
// 判死、不断重取（验证失败冷却限制了速率，但额度仍在烧）。
//
// **发现它要看逐模型的诊断行**（管理页面读最近一次最终验证事件，stg0 判死会显示成"上游回报 X"），
// 而**不是**近三天的业务统计——票被判死后业务在注入前就拒服、压根不产生上游响应 model，那个统计
// 因此反而会保持安静。两者是不同的数据通路，详见 DESIGN-codex-ticket.md §4.0。
const DefaultKongTicketStg0Accept = "gpt-5.6-sol:gpt-6-sol"

// KongTicketBatchFetchEnv 是 BatchFetchAllModels 对应的环境变量名。
const KongTicketBatchFetchEnv = "GATEWAY_KONG_CODEX_TICKET_BATCH_FETCH_ALL_MODELS"

// DefaultKongTicketBatchFetchAllModels 是批量取票的内置默认：**开启**。
//
// 依据是一条实测事实：**正常档窗口一旦打开约 4 分钟内有效，且窗口内的活动不会把它提前关闭**
// （三次独立观测一致；42 秒一次轮询仍撑满 4 分钟，见研究资料 DETECTION.md §4）。那几次观测靠票长度
// 读出档位，而上游 state 是不透明 token、长度已不再对应档位，所以这条事实今后只能用指纹验证的结论
// 复核——在拿到新证据之前，默认值保持不变。所以一次静默换
// 来的不是"一张票"，而是一个**可连续取票的窗口**——在窗口里把所有门控模型的票一次取齐，成本与
// 取一张相同。
//
// ⚠️ 这条事实不成立的话，本开关只会让第二张起必然拿到降智档票（照样入库，由指纹验证判死、白花
// 验证额度）。它是整个机制的地基，换上游、换出口之后要重新确认。
const DefaultKongTicketBatchFetchAllModels = true

// KongTicketFusedFingerprintEnv 是 FetchFusedFingerprint 对应的环境变量名。
const KongTicketFusedFingerprintEnv = "GATEWAY_KONG_CODEX_TICKET_FETCH_FUSED_FINGERPRINT"

// DefaultKongTicketFetchFusedFingerprint 是取票融合指纹的内置默认：**开启**。
//
// 依据是协议事实：turn-state 是 **sticky-routing token，钉住的正是那次请求被路由到的目标**，且在
// turn start 下发（读 codex 源码所得，见研究资料 FACTS.md 的协议事实一节）。所以取票那次响应的
// 正文，就是那张票所钉住的那个目标生成的——票是该次请求路由结果的载体，不是独立于它的东西。于是
// 取票请求的正文可以直接当第一份指纹样本，省掉一次往返（取票 ~1s、单份挑战 27–31s）。
//
// ⚠️ 两处差异是**推理而非实测**，这也是本开关必须存在的理由：
//
//  1. **出口不同**：融合样本在票据出口上产生，而业务注入用票时走流量出口。"票是否编码了打票时的
//     出口 IP"仍是未决问题（FACTS.md §5 Q1）——若编码，融合样本测到的是"打票出口上的档位"。
//  2. **无票态 vs 注入态**：融合样本是无票状态下的路由结果，注入后是按票路由。
//
// 叠加早停（生产实测多数验证一份样本就判定接受），融合样本往往是那张票的**唯一**证据。所以未达标
// 时刻意**不据它判不合格**，只记 inconclusive 并丢弃重跑（见 verifyTicket 的 fused 分支）。
const DefaultKongTicketFetchFusedFingerprint = true

// applyKongTicketEnvOverrides 在 Unmarshal 之后按环境变量覆盖票据配置。
//
// **为什么必须单独覆盖**：viper 的 `AutomaticEnv` 默认忽略**空**环境变量，所以把
// `GATEWAY_KONG_CODEX_TICKET_ACCEPT_EXTRA` 显式设成空串时，解码结果仍是非空默认值——运维要求
// "sol 只接受自己"却拿到了含 5.5 的默认白名单，而这个方向是**放宽**判据，不是收紧。
//
// 不开全局 `AllowEmptyEnv`：那会改变上游其它所有配置项对空值的语义。这里只覆盖这一个字段，与
// 上游对 `SERVER_TRUSTED_PROXIES` 的处理同一范式。
func applyKongTicketEnvOverrides(cfg *KongCodexTicketConfig) {
	if raw, present := os.LookupEnv(KongTicketAcceptExtraEnv); present {
		cfg.AcceptExtra = raw
	}
	// 显式覆盖而不是"非空才覆盖"：内置默认非空，所以把这个变量设成空串是一个**有意义的指令**
	// （"回退到只接受模型自己"），静默忽略它等于让人改不动这张表。
	if raw, present := os.LookupEnv(KongTicketStg0AcceptEnv); present {
		cfg.Stg0Accept = raw
	}
}

// normalizeKongTicketEnv 在 **Unmarshal 之前**把票据的布尔项规范化，必须在解码前调用。
//
// 为什么不能放到解码之后：这个键注册了默认值，于是 viper 会把环境变量当 `bool` 解码，而
// `" false "`（带空格）或 `yes` 这类值**直接让整个 `viper.Unmarshal` 失败**——`main.go` 据此终止
// 启动，哪怕一个账号都没开票据功能。配置笔误不该升级成整个网关不可用（与死配置只告警不拒启动
// 是同一条判断）。
//
// `viper.Set` 的优先级高于环境变量，所以写回一个已规范化的布尔值也就同时挡掉了原始字符串。
func normalizeKongTicketEnv() {
	normalizeKongTicketBool(KongTicketBatchFetchEnv,
		"gateway.kong_codex_ticket.batch_fetch_all_models",
		DefaultKongTicketBatchFetchAllModels)
	normalizeKongTicketBool(KongTicketFusedFingerprintEnv,
		"gateway.kong_codex_ticket.fetch_fused_fingerprint",
		DefaultKongTicketFetchFusedFingerprint)
}

// normalizeKongTicketBool 规范化一个布尔开关。非法值回退到**内置默认**并告警。
//
// 不回退成 false：两个开关的 false 都是"悄悄退回旧行为"——批量取票关掉会让同时门控的两个模型重新
// 出现空窗，融合关掉会让每次取票多付一次往返。把笔误解读成关闭，问题会以性能退化的形式藏很久。
func normalizeKongTicketBool(env, key string, def bool) {
	raw, present := os.LookupEnv(env)
	if !present {
		return
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		slog.Warn("codex 票据：布尔开关取值非法，按默认值处理",
			"env", env, "value", raw, "default", def)
		viper.Set(key, def)
		return
	}
	viper.Set(key, v)
}

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

	// Stg0Accept 是「某个门控模型额外接受哪些**上游回报值**」的白名单，格式与 AcceptExtra 相同
	// （见 DefaultKongTicketStg0Accept 的语义与代价）。
	//
	// ⚠️ **与 AcceptExtra 是两张表，不要合并、也不要互抄值。** AcceptExtra 里的项是指纹区分度
	// 不足的补偿（sol 接受 gpt-5.5 的归因，因为指纹分不开两者）；stg0 读的是上游自己的声明，把
	// 那条补偿抄过来会让一次上游明说的降智被放过。
	Stg0Accept string `mapstructure:"stg0_accept"`

	// FetchFusedFingerprint 决定取票请求是否顺带充当第一份指纹样本（见
	// DefaultKongTicketFetchFusedFingerprint 的依据与差异说明）。
	FetchFusedFingerprint bool `mapstructure:"fetch_fused_fingerprint"`
	// BatchFetchAllModels 决定一次取票是否**并发**把所有门控模型的票一起取回来。
	//
	// 开启的收益不是"省一次请求"，而是**让多个门控模型能共用同一段静默**：取票间隔由
	// `TTL − refresh_before` 决定（默认 3300s），而拿到正常档票要求出口静默约 2040s——两个模型各自
	// 续期就要求相邻两次取票相隔 I/2 ≥ 2040，即 I ≥ 4080 > TTL，无解。一次取齐把这个约束整个
	// 消掉了：一个周期只有一次出口活动，覆盖全部模型。
	//
	// 并发而不是串行：全部请求在同一瞬间发出，于是不存在"窗口在批次中途关闭"这回事，也不需要
	// 批次预算与顺序安排。
	//
	// 关掉它就退回"一次只取触发模型那一张"，此时同时门控两个模型必然有空窗（约 11.8% 的时间
	// 拒服，推导见上述不等式）。
	BatchFetchAllModels bool `mapstructure:"batch_fetch_all_models"`
}
