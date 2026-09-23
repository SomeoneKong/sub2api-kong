# sub2api-kong

[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 的 fork，用于自用部署的定制。
上游以 LGPL-3.0 授权，本 fork 同样以 LGPL-3.0 发布，自 2026-09 起在其基础上修改。

**改了什么看 git**：`git log --oneline <当前 base>..main` 是定制清单，diff 是细节（改动不在文件头
重复标注）。本文只写不随版本变化的东西——约定、规则、流程。

## 定制的边界

只碰四类东西：**codex 票的被动观测**（收票与请求特征，设计见 `DESIGN-codex-ticket.md`）、
**OpenAI 账号指纹测试**（设计见 `DESIGN-openai-fingerprint-test.md`）、**OpenAI 账号消耗节奏**（旧版选号的重排，设计见
`DESIGN-openai-account-pace.md`），以及**fork 自身必须适配的部分**（版本检查、发布标识）。
上游其余部分一律不动——定制面越窄，越能持续跟上上游的 bug 修复。

有三处是**结构性的、不随上游版本变化的约束**，值得写在这里而不是只留在 commit 里：

- **版本检查必须指向本 fork。** 否则上游每发一个新版就提示一次更新，点下去会把定制覆盖成
  官方版。这是 `githubRepo` 那处改动存在的全部理由。
- **新增数据结构不走 ent。** 上游把 ent 生成代码全部入库（三百多个文件），加一个 schema 就会重新
  生成 `ent/migrate/schema.go`、`ent/mutation.go` 等上游每周改动数次的文件——生成代码的 rebase
  冲突是最难手工解决的那一类。所以：账号级配置存进已有的 `accounts.extra`（jsonb），新表用
  `migrations/9xx_kong_*.sql` 加手写 `*sql.DB` repository。上游自己就有这个形态可照抄
  （`audit_log_repo.go`、`auth_cache_invalidation_outbox_repo.go`）。
- **容器部署下不要用面板上的就地升级/回滚。** 它替换的是容器可写层里的二进制，重启即回到
  镜像版本，"升级成功"是假的。代码层面**故意不加门控**——上游对这几个函数有测试，禁用等于
  连带改上游测试文件，白白扩大 rebase 冲突面；而改了 `githubRepo` 之后最坏结果只是"重启后
  白升一次"，不再有覆盖定制的风险。换镜像才是正道。顺带一个反直觉点：上游的 `BuildType`
  （`source`/`release`）看着像天然开关，但它只被透传给前端展示、**没有任何门控**。

## 维护约定

| 约定 | 原因 |
|---|---|
| **定制清单就是 `base..main`，按主题保持少数几个提交** | 次序固定：**可提给上游的修复**（英文 conventional commit，只改上游文件、不引用任何 fork 符号）→ fork 适配（CI、发布、版本检查）→ 功能后端 → 功能前端 → 文档。上游修复紧贴基线，才能原样 cherry-pick 到上游提 PR，上游合并后下次 rebase 直接丢掉；功能的后续修改用 `--fixup` 并回对应提交，不在 main 上堆增量。每个提交都必须能独立编译、通过测试 |
| 定制测试用 `*_kong_test.go` | 独立文件名，不与上游测试文件冲突 |
| **包级标识符也加 `kong` 前缀**，不只是文件名 | 通用辅助函数名在上游的大包里很可能已经存在——实测 `asString`、`nullString`、`ptrInt64` 三次都撞了。更麻烦的是反向：上游将来新增一个同名函数，会让我们的文件直接编译失败 |
| **本地验证必须带 `-tags=unit`**（CI 的口径是 `make test-unit`） | 上游有大量测试文件带 `//go:build unit`。不带标签跑，那些文件根本不参与编译——`ptrInt64` 重名就是这样躲过本地验证、直到 CI 才暴露的 |
| 我们自己的新文件用 `kong_*.go`，其测试用 `kong_*_test.go` | `*_kong_test.go` 那条约定针对的是**给上游文件加测试**（避免与上游的同名测试文件冲突）。自建文件不存在这个风险，测试名跟着源文件更好找 |
| **不改 Go module path** | 仍是 `github.com/Wei-Shaw/sub2api`。改它要动几百处 import、制造巨大 rebase 冲突面；本 fork 只构建镜像、不作为库被引用 |
| **不改 `backend/cmd/server/VERSION`** | 版本优先级是 `--build-arg VERSION` > git tag > 该文件，走 build-arg 即可 |
| CI / Security Scan 只在 `main` 与 PR 上触发 | 上游的 `on: push` 无分支过滤。本 fork 会持续 push `base/*` tag 归档上游基线，不限制就每个 tag 都跑一遍全量 CI——跑的还是纯上游代码 |
| **不要跑 `wire generate`** | codex 票的观测、指纹测试与消耗节奏的装配是手工写在 `cmd/server/wire_gen.go` 里的（直接给 `AdminHandlers` 赋字段、调 `SetKongTicketObserver` / `SetKongOpenAIAccountPace`，外加收票写入与节奏组件的 `Start`、cleanup 里的 `StopKongTicketObserver` / `StopKongOpenAIAccountPace`），wire 生成不出这几行。重新生成会把它们抹掉，而编译不会报错——只会让功能静默失效 |
| **HTTP 的票观测挂在 `doOpenAIUpstream`，不挂在各业务分支** | 那是全部 OpenAI **HTTP** 上游发送的汇聚点（二十来个调用点；原生 WS 的帧不经过它，见下一行）。逐个分支去插必漏——Responses 透传、chat-completions 转 Responses、messages 转 Responses、WS-HTTP bridge、alpha-search、images 桥接各自构造请求与处理响应，而漏没漏不会报错，只会安静地少收票。发送前记下出站头上的票、把逐轮记录器挂到出站请求的 context 上，拿到响应头后记下下发的票并收票。用量行在更外层由各协议组装，用 `KongFeaturesFromRequest(upstreamReq)` 从出站请求取回特征：现在取的是 Responses 转发、透传与 WS-HTTP bridge 三处，新接一条通路就在它组装结果处加一行 |
| **原生 WS 三条通路各自接入** | 帧不流经 `doOpenAIUpstream`。ctx_pool（`openai_ws_forwarder_ingress.go`）、HTTP→WS（`openai_ws_forwarder_v2.go`）、passthrough（`openai_ws_v2_passthrough_adapter.go`）各有四类单行挂钩：上送帧处按**剥离之后**的帧建逐轮记录器（`KongNewWSFrameFeatures` / `KongNewWSMapFeatures`），拿到连接后收握手票（`CollectHandshake`；池化通路传一次性的 `ConsumeHandshakeTurnState`），读到上游事件处 `ObserveWSEvent`，结算处先 `RecordOutboundIfAbsent`（连接级的值：池化通路是 `SentHandshakeTurnState`，passthrough 是自己拨号时发出的头）再取 `Snapshot`。客户端的票可以在帧的 `client_metadata` 里，也可以只在 upgrade 头上、由本服务放进上游握手头，所以两处都要看。**易漏点**：passthrough 的首帧不走逐帧过滤器（它在 relay 启动前单独写上游），要单独建记录器；下行挂钩要放在帧类型判断之前（`response.metadata` 用 Binary 帧一样发得出来），也不能只挂在写客户端的路径上——客户端断连后的排空期间上游照样在发事件 |
| **逐轮额度只认本轮的响应头** | 三条 WS 通路把**连接级的握手响应头**放进 `OpenAIForwardResult.ResponseHeaders`（供限流信号与 retry-after 用），而成功回调会据此回填额度。那份额度是拨号时刻的，连接池的连接活得很久——直接回填会把带内 `codex.rate_limits` 采到的实时水位**覆盖成旧值**（落库节流窗口 30s，一轮超过它就会发生），还会用当前时间重算旧的相对重置秒数。所以取额度的响应头一律经 `CodexQuotaHeaders()`，不要直接读 `ResponseHeaders`；**判据不能用 `!OpenAIWSMode`**——WS-HTTP bridge 同样是 WS 模式，但它的 `ResponseHeaders` 是本轮真实的 HTTP 响应头，那份额度必须照常刷新 |
| **额度的归一化只能有一份** | 原生 WS 逐轮没有响应头，额度快照来自带外事件 `codex.rate_limits`（`parseOpenAIWSCodexRateLimitEvent`），HTTP 侧来自 `x-codex-primary-*` / `x-codex-secondary-*` 响应头（`ParseCodexRateLimitHeaders`）。**两者只在提取层分叉**，5h/7d 的判定（`Normalize` 按 window_minutes 比长短）与落库映射（`buildCodexUsageExtraUpdates`）必须共用同一份：各自归一化会对同一个账号算出不同水位，而 `codex_5h_used_percent` / `codex_7d_used_percent` 是调度阈值的输入，不一致不报错、只会让调度照两个矛盾的数做决定 |
| **带内载体事件有两种拼写，WS 那种带 `codex.` 前缀** | 原生 WS 上游发 `codex.response.metadata`，SSE 上是 `response.metadata`。只认后者，WS 上的收票、请求特征与溯源登记会**同时静默失效**——票就在帧的顶层 `headers` 里，只是类型名对不上，调用点全部提前 return，没有任何信号。不要按前缀泛化——同一条连接还有 `codex.rate_limits` 等带外事件 |
| **票是不透明 token，长度与指纹只作观测，不参与任何判定** | 票是上游的加密认证串，长度只是明文长度的旁路泄露，上游一改明文结构就变（2026-09-23 就变过一次）。所以调度、拒服、交付都不看它们；用量行里的长度只作记录（发现结构变化靠的正是它，不要删）。**票的身份一律用内容哈希 `kongStateFingerprint`**：票的前若干字符在数周内几乎不变，"长度 + 前缀"式的键会让同一长度的所有票同键 |
| **turn-state 溯源表不设容量上限** | 表里记的是「这个 blob 是我们替哪个账号发出去的」，判定口径是查无来源即不剥——无从判定来源就剥，等于无谓丢掉客户端的对话上下文。所以淘汰未过期条目会把「已知别家铸造」降成「查无来源」，failover 换号后那个 blob 又被原样送到新账号，正是这道守卫要挡的事。要设硬上限，就得在交付前先预留登记容量、满额即拒服；在那之前容量只由 TTL 与后台清扫约束 |
| **消耗节奏只挂在旧版选号的第 2 层，只改顺序不改候选** | 钩子全在 `openai_gateway_scheduling.go` 的 `selectAccountWithLoadAwareness` 里，都是单行、带 `[kong]` 注释：进入第 2 层处 `kongPaceBegin` + `defer finish`、`shuffleWithinSortGroups` 之后 `reorder`（传入随后倍率排序用的 `rateOrder`）、抢槽循环前 `finalOrder`、两处抢到后 `acquired`、负载读取失败分支开头 `loadFailed`。rebase 时要复核的就是这几处；上游改动这个函数时，要确认 `reorder` 仍在同优先级内的随机打散之后、倍率排序与 compact 分层之前——放到后面会把上游的硬约束冲掉。第 1 层（粘性）与第 3 层（兜底等待）刻意不接：粘性优先于短期均衡，兜底等待本就不挑号。过滤条件一概不碰，所以最坏情况也只是退回上游的顺序 |
| 消耗节奏的参数放数据目录下的 `openai-account-pace.yaml`，不进 `config.yaml`、也不走环境变量 | 要在线调：环境变量每改一次都要重建容器、掐断进行中的流式响应，而组件每分钟检查一次文件，改完即生效；参数又是按套餐分组的嵌套结构，环境变量表达不了。不进 `config.yaml` 是因为上游的 `config.go` 是高频改动面，加字段就多一处 rebase 冲突。文件不存在即功能关闭，改坏时沿用上一份有效配置并在日志与 Redis 的 meta 里报错 |
| 接转发链路用「可选依赖 + setter」 | `OpenAIGatewayService` 的构造函数参数表很长且是上游高频改动面。加字段 + `SetKongTicketObserver` 能把改动收在一处，未注入时所有接入点退化为空操作 |
| 前端定制放 `frontend/src/features/<主题>/`，上游文件只做追加 | 用量明细的请求特征列（`features/request-features/`）碰上游的点全是**追加**：`UsageTable.vue` 一行 import 与一个 `#cell-request_features` 插槽、`types/index.ts` 里 `AdminUsageLog` 的 `kong_request_features` 字段、管理员 `views/admin/UsageView.vue` 的列定义一项（标签用中文字面量，不加 i18n 键）。账号指纹测试（`features/openai-fingerprint-test/`，文案在 feature 内）碰上游的只有两处各两行：`AccountActionMenu.vue`「测试连接」下方的菜单项组件与它的 import，`AccountsView.vue` 的弹窗宿主与它的 import——弹窗不能挂在菜单里，菜单关闭时菜单组件随之卸载。rebase 时要复核的就是这几处 |
| 后端响应结构要显式写 `json` tag | 本功能的 handler 直接序列化 service 层结构体。上游那些结构多数也没 tag，但我们的响应里混着 `gin.H` 的 snake_case 字段——不写 tag 会让同一个响应里两种命名风格并存，前端类型也跟着别扭 |
| `upstream` remote 禁止 push | `git remote set-url --push upstream DISABLED` |
| `base/*` tag 必须 push 到 origin | push 后该 commit object 即归本仓库。上游会 force push、删 tag、撤 release，不这么做就得靠运气 |

## 本地验证的已知差异

⚠️ **判断测试是否通过只看 `go test` 自己的退出码或完整的 FAIL 计数。** 上游测试会往 stderr 打
大量日志（`[Billing] Using fallback pricing...` 之类），所以
`go test ./... | grep -v '^ok' | head -20` 这种写法会被日志噪音占满前几十行、把 FAIL 行整段切掉，
而管道的退出码来自 `head` 不是 `go test`——两头都在骗人，结果是"全绿"的假象。

`go test -tags=unit ./internal/service/` 在 Windows 上会有一个上游测试失败：
`TestOllamaProbeCallback_StaleLongDoesNotOverrideNewShort`（`stale long callback must not pass
the CAS`）。**它在纯上游基线 tag 上同样失败**，与本 fork 的定制无关，CI（Linux）也是绿的——
属平台或时序相关。遇到时不要顺着它排查，先在 `git worktree add <tmp> <base tag>` 的纯上游树上
复现一次，确认是上游自带的再放过。

**`-race` 下上游测试套整体不干净**，这不是本 fork 的问题。`kong-race.yml`（只手动触发，见下节）
在 `./internal/service/` 上跑 `-race` 时**必然红**：2026-09-19 实测本分支 81 个用例失败、
纯上游基线 `base/v0.2.7-aea725f2` 100 个失败，竞态点完全相同——`ratelimit_service.go:540`、
`openai_ws_forwarder_logutil.go:515`、`openai_gateway_forward.go` 里调 `forwardOpenAIWSV2` 那一处
（行号差的正是本 fork 插入的注释），另有 `logger.go` / `slog_handler.go` / `scheduler_snapshot_service.go`。

所以**判读方式不是看红绿，而是看竞态报告里有没有 `kong_*.go` 帧、以及有没有 Kong 用例失败**。
那次实测两者都是 0。要在纯上游基线上复现作对照：从 `base/*` tag 建临时分支、只把
`.github/workflows/kong-race.yml` 加进去、push 后 `gh workflow run kong-race.yml --ref <该分支>`，
比完删分支（`workflow_dispatch` 要求 workflow 文件存在于被指定的 ref 上，所以必须建分支）。

## 版本号与发布

发布 tag 形如 `v<上游基线版本>-kong.<n>`。序号在同一上游基线内递增，跟进新基线时归 1
（`v0.2.5-kong.7` → `v0.2.6-kong.1`）。

`-kong.<n>` 后缀参与版本比较（见 `update_service.go` 的 `parseVersion`），所以序号必须是
数字；精确的上游基线 SHA 记在 tag message、release body 与镜像 OCI label 里。

两份 GoReleaser 配置的 `prerelease` 固定为 `false`：上游的 `auto` 按 SemVer 把带后缀的 tag 标成预发布，
而版本检查查的是 `/releases/latest`、回滚候选也排除预发布——那样本 fork 的每个正式发布都对它们不可见。

镜像发布到 GHCR：**`ghcr.io/someonekong/sub2api`**。注意**不是** `sub2api-kong`——镜像名写死在
`.goreleaser.yaml` 的 `image_templates` 里（`<owner>/sub2api`），只有 owner 取自环境变量，与仓库名
无关。改名要动那个上游文件，多一处 rebase 冲突面，而 namespace 已经足够区分，所以保持现名。
推 `v*` tag 即触发
`.github/workflows/release.yml` 构建发布，镜像名由 `github.repository` 推导，无需配置。

发布行为由**仓库变量 `SIMPLE_RELEASE=true`** 控制：只出 x86_64 的 GHCR 镜像，跳过多架构
qemu 与其它产物。部署目标只有 amd64，这样省掉大半构建时间。用仓库变量而不是改 workflow，
是为了不增加 `release.yml` 的 rebase 冲突面——上游本就留了 `vars.SIMPLE_RELEASE` 这个入口。
未配 Docker Hub / Telegram secrets 时，相应步骤自带守卫会跳过。

版本号无需在开发时维护：打 tag 即决定。`update-version` job 会把 tag 版本写进工作区的
`VERSION` 文件（`go:embed` 进二进制），手工构建则走 `--build-arg VERSION`（ldflags 优先级
最高）。**裸 `go build` 不传参时自报仓库里 `VERSION` 的值，即上游版本号，属预期**——该文件
刻意不跟随本 fork 的版本。

## 与上游同步

当前基线：`git tag -l 'base/*' --sort=-creatordate | head -1`（按时间排，不能按字典序——
`base/v0.2.10` 会排在 `base/v0.2.7` 前面）。跟进新基线：

```bash
git fetch upstream --tags
git tag -a base/<版本>-<sha8> <sha> -m "上游基线：<sha>"
git push origin base/<版本>-<sha8>
git rebase <新 base>          # 重放 base..main；上游已合并的修复提交在这一步丢掉
```

冲突只会出现在定制触及的文件上。要知道是哪些：

```bash
git diff --name-only $(git tag -l 'base/*' --sort=-creatordate | head -1)..HEAD
```
