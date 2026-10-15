# 读设置失败时沿用上一次的出站客户端身份

网关发往上游的客户端身份（Codex 版本号与 User-Agent、Claude Code 版本号、Antigravity 版本号）从设置表读，
进程内缓存 60 秒。读库失败时，现有代码不沿用上一次读到的值，而是换成编译期内置的常量，并把它缓存 5 秒。
内置常量随上游发版才更新，早已落后于自动同步的版本；上游又按客户端版本决定哪些模型可用，于是数据库一忙，
这 5 秒内出站的请求就整批被上游拒绝。本设计改为：已有缓存值时，读库出错本身不再把身份换成内置值；同时让回源结果
不会覆盖期间保存或失效写入的新状态。只改上游文件，做成可提给上游的修复。

## 1. 现象与根因

### 1.1 线上现象

10-15 下午宿主 CPU 吃满、网关连接池打满的约 40 分钟里（14:04–14:43），客户端收到 41 次上游 400，涉及几乎全部账号：

- `The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.`
- `The 'gpt-6-astra' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again.`

这两类错误此前 30 天里只零星出现过（最多一小时 3 次）；过载结束后同样的模型每 10 分钟仍有两三百次成功请求，再没出现。

网关日志里 `failed to get openai codex client version setting` 出现在 14:04:12、14:14:50–14:15:16、14:37:26–14:38:56、
14:42:31–14:42:53 四段，41 次 400 正好分成四簇，各自紧跟在一段读取失败之后几秒内。

### 1.2 根因

出站身份的收口点 `enforceCodexIdentityHeadersWithUA`（`openai_codex_identity.go`）把每个 OAuth 出站请求（HTTP 与新建的
WS 握手）的 `User-Agent` / `originator` / `version` 统一改写为规范身份。版本号只有一个来源
`SettingService.GetOpenAICodexClientVersion`（`setting_gateway_runtime.go`）：面板覆写 → 自动同步值 → 内置常量
`codexCLIVersion`（现为 `0.146.0`）；之后 `codexClientVersionFromUA` 再把低于上游门槛 `0.144.0` 的版本校正为内置常量。
线上自动同步值是 `0.162.1`。

`GetOpenAICodexClientVersion` 在缓存过期后经 singleflight 读库（超时 5 秒）。读库出错时：

```go
s.openAICodexVersionCache.Store(&cachedOpenAICodexClientVersion{
    version:   fallback,                       // codexCLIVersion = "0.146.0"
    expiresAt: time.Now().Add(openAICodexClientVersionErrorTTL).UnixNano(), // 5s
})
return fallback, nil
```

于是这 5 秒内解析出的身份都自称 Codex 0.146.0；上游对这个版本拒绝了 `gpt-6.1-sol` 与 `gpt-6-astra`。
（"0.146.0 被拒"是由时间对应推断的，没有单独发请求验证。）

同一文件里另外三个出站身份读取是同样的写法：

| 读取 | 内置值 | 用在 |
|---|---|---|
| `GetOpenAICodexUserAgent` | `DefaultOpenAICodexUserAgent`（`codex_cli_rs/0.146.0 (Ubuntu 22.4.0; …)`） | 规范 UA 的客户端名与 OS / 架构 / 终端指纹（版本段另由版本号重建） |
| `GetClaudeCodeClientVersion` | `claude.CLIVersion()`（环境变量或内置基线） | Claude Code 出站 UA 与计费归因里的版本号；版本过旧 Anthropic 回 400 `claude_code_version_too_old` |
| `GetAntigravityUserAgentVersion` | `antigravity.GetDefaultUserAgentVersion()` | Antigravity 出站 UA 版本 |

它们读失败时同样会把出站身份换成内置值 5 秒。

### 1.3 既有的发布竞态

四个读取在回源结束后都无条件 `Store`。失效函数（`InvalidateOpenAICodexClientVersionCache` /
`InvalidateClaudeCodeClientVersionCache`）与保存设置时的直写（`setting_update.go` 对 Codex UA、Antigravity 版本）
虽然调了 singleflight 的 `Forget`，但 `Forget` 不会取消已经在跑的回源：保存之前开始的一次回源若在保存之后才返回，
会把旧值再缓存 60 秒。本设计在 2.1 里沿用旧值，若不处理这个竞态，被写回的旧值还会在读库失败时继续被沿用，所以一并修。

## 2. 改法

只改 `setting_gateway_runtime.go` 里上述四个读取与两个失效函数。

### 2.1 读失败时沿用上一次的值

回源闭包在二次检查之后、查询之前，记下当时 `Load()` 的原始返回值 `prevRaw`（`any`，缓存从没写过时是无类型 nil），
再对它做类型断言得到旧条目 `prev`（可能为空）。查询出错（`err != nil` 且不是 `ErrSettingNotFound`）时：

- `prev` 非空（不论是否过期）：发布一个**新条目**，值取 `prev` 的值、过期时间为 5 秒后（错误 TTL），返回该值；
- `prev` 为空（进程启动后从没读成功过，或刚失效且失效前也没有值）：照旧发布并返回内置值。

其余不变：设置不存在（`ErrSettingNotFound`）不是读失败，照旧按优先级落到下一层 / 内置值、缓存 60 秒；读成功时的
取值、归一化、缓存 60 秒不变；错误 TTL 仍是 5 秒；singleflight 与独立的 5 秒读库超时不变。已发布的条目不改写，
总是换新条目。

同一文件里已有同样的做法：`refreshOpenAIQuotaAutoPauseSettings` 读失败时沿用 `prior`，
`GetCyberSessionBlockRuntime` 读白名单失败时沿用 `previous`。

### 2.2 只在缓存未被改写时发布

成功与失败两个分支都用 `atomic.Value.CompareAndSwap(prevRaw, next)` 发布，比较值必须是 `Load()` 的原始返回值：
零值 `atomic.Value` 的 `Load()` 返回无类型 nil，换成断言后的 typed nil 指针去比较会失败，首次发布就发不出去。

- 交换成功：返回 `next` 的值；
- 交换失败（回源期间缓存被保存直写或失效改写过）：不覆盖，返回缓存里当前条目的值。若当前条目是失效留下的已过期条目，
  这次调用返回它的值，下一次调用照常回源。

保存设置时的直写（`setting_update.go`）保持无条件 `Store`——它就是新状态，交换失败的那次回源不会再盖掉它。
每类缓存里存的都是同一具体类型的非 nil 指针（包括 2.3 的失效条目），满足 `CompareAndSwap` 的类型约束；每次写入都是
新分配的条目，指针各不相同，所以"缓存是否被改写过"只看指针是否还是 `prevRaw` 那一个。

### 2.3 失效时保留旧值

两个失效函数现在把缓存存成 typed nil，让下一次读取回源。这有两个问题：若那次回源恰好失败，2.1 找不到上一次的值，
又会落到内置常量；缓存本来就是空的时候，连续两次失效写入的值相同，失效之前开始的回源用 2.2 比较时分不出缓存被
失效过，仍会把旧值发布出去。

改为：每次失效都用 `CompareAndSwap` 循环发布一个**新分配的非 nil 已过期条目**——当前有条目时复制它的值，没有时
填这个读取原有的内置值。下一次读取照样回源；回源成功就是新值，失败则沿用失效前的值（失效前没有值时就是内置值，
与现状相同）。先发布过期条目、再 `Forget`：反过来的话，两步之间开始的回源会捕获失效前的条目，失效返回之后的读取
仍可能并进这次回源、拿到失效前的值。

## 3. 范围与已知限制

- **保证的范围**：已有缓存值时，读库出错本身不再触发默认身份替换。设置缺失、为空或非法时按原优先级落到下一层或内置值，
  低于上游门槛的版本由 `codexClientVersionFromUA` 校正为内置常量——这些既有行为不变。进程启动后从没读成功过时，
  读库失败仍用内置值。
- **WebSocket**：HTTP 请求与新建的 WS 握手每次都经过身份收口，受益于本修复；已建立的 WS 连接不重新握手，连接池也不按
  身份区分，复用中的连接沿用握手时的身份（包括设置更新之后）。这一点本修复不改。
- **读库期间请求要等**：缓存过期后，第一个请求（及 singleflight 上同时到达的请求）最多等 5 秒读库超时。规范 Codex UA
  依次读版本号与 UA，两者同时过期且都超时时，一次身份解析最多等约 10 秒。数据库持续出错时每个缓存约每"查询耗时 + 5 秒"
  回源一次（每次超时则约 10 秒一次）；读成功时约每 60 秒一次。改成后台刷新、先返回旧值（`GetOpenAIQuotaAutoPauseSettings`
  的做法）能去掉这个等待，但改动面更大，不在这个修复里。本修复解决的是身份降级，不是等待。
- **其他设置读取的读失败分支**：`IsBackendModeEnabled`、`getGatewayForwardingSettingsCached`、
  `GetClaudeCodeVersionBounds`、`GetCyberSessionBlockRuntime` 的开关、`GetCodexRestrictionPolicy`、
  `setting_features.go` 的停调阈值，读失败时也用默认值。它们不是出站客户端身份，默认值是各自的取舍
  （如版本上下限是有意的 fail-open），这次也没有观察到影响；不放进这个修复。
- **内置常量 `codexCLIVersion` 不改**：它由上游随发版维护。

## 4. 测试

单测（`//go:build unit`，沿用 `openai_codex_version_sync_service_test.go` 的 `codexVersionSettingRepoStub`、
`setting_claude_code_version_test.go` 的写法；桩的错误开关与取值要能在测试中途切换，并发用例里加锁）：

1. **沿用旧值**：四个读取各一条——先成功读到非内置值；把缓存条目换成已过期的副本；仓库改为返回错误；再读得到先前的值，
   缓存条目的过期时间约为 5 秒后；错误 TTL 内再读仍是这个值、不回源；把条目再标为过期、仓库恢复后读到仓库里的新值。
2. **从没读成功过**：读失败仍返回内置值（`TestGetOpenAICodexClientVersionFallsBackOnError` 保留）。
3. **失效后读失败**：成功读到值 → 调失效函数 → 仓库返回错误 → 读得到失效前的值。Codex 版本与 Claude Code 版本各一条。
4. **失效后读成功**：读到新值（现有的保存设置立即生效类测试保持通过）。
5. **发布竞态**（桩在取得旧值或错误之后发出"已阻塞"信号，等待放行时不持锁；成功返回与错误返回两条分支都覆盖）：
   - 保存直写：旧回源阻塞 → 直写新条目 → 放行旧回源；旧调用返回新值，缓存仍是新条目；
   - 失效并刷新：旧回源阻塞 → 失效 → 新一次回源发布新值 → 放行旧回源；旧调用返回新值，缓存仍是新条目；
   - 只失效：旧回源阻塞 → 失效 → 放行旧回源；缓存仍是已过期的失效条目（旧结果没被赋予 60 秒有效期），下一次读取确实回源；
   - 空缓存：两个版本缓存在从没读成功过时，旧回源阻塞期间连续失效两次，放行后旧结果同样发布不出去。
6. **首次发布**：四个零值缓存的首次成功与首次失败都能发布，随后命中缓存、不再回源。
7. **身份链路**：注入规范 UA 解析器，预热非内置版本后让版本读取、UA 读取分别失败与同时失败，经
   `enforceCodexIdentityHeadersWithUA` 收口，断言 UA 首段、UA 版本段与 `version` 头一致且是预热的版本；账号级陈旧 UA
   同样被重建为该版本。
8. **既有优先级**：成功读到缺失、空或非法值时仍按原优先级落到下一层 / 内置值（现有用例保持通过）。

## 5. 提交与发布

- 按 `FORK-NOTES.md`：一个"可提给上游的修复"提交（英文 conventional commit，只改上游文件、不引用 fork 符号），
  排在定制清单最前；与保留清理的修复一起发 `0.2.15-kong.9`；之后向上游提 PR，上游合并后 rebase 时丢掉。
- 线上验收：版本号读不到时网关日志照旧有 `failed to get openai codex client version setting`，但 HTTP 请求与新建 WS
  不再出现上述两类 400；部署后观察几天的错误日志。
