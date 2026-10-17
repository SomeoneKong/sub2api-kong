# OpenAI 请求的逐请求字节数

**状态**：已实现，无配置项。仓库组织与发布流程见 `FORK-NOTES.md`。

用途是核算带宽：各出口（账号的代理；本部署里账号代理都指向宿主上的 mihomo，一个入站端口对应一台出口机）用了多少、
各账号用了多少，并且能落到具体请求上分析——哪类请求、哪个 key / 用户占了多少流量，以后据此做请求量的控制。账号会在
出口之间调换，所以每个数都要记下**发送当时**的账号与出口，不能靠事后查账号的当前配置。

## 1. 现状

**主路径**：最近一天的用量行里，`/v1/responses` 的 HTTP 流式请求 77,858 行，全部带 `kong_request_features`；`/v1/alpha/search`
（非流式）4,053 行，不带。线上 12 个 OpenAI OAuth 账号都开了自动透传（`extra.openai_passthrough`），Responses 走
`forwardOpenAIPassthrough`。WS 不是主要场景。

**上行**（客户端 → 网关 → 上游）：

- 客户端可以压缩请求体（本地代理发 zstd；`httputil.ReadRequestBodyWithPrealloc` 还认 gzip、deflate），读出后去掉
  `Content-Encoding`、`ContentLength` 改成解压后的长度。
- 分组模型白名单（`group_model_allowlist.go`）与组合路由（`routes/gateway.go`）两个中间件会在 handler 之前读一遍请求体，
  以 `PrereadBody` 回填；组合路由还可能改写模型字段。Responses 的 handler 用宽松 JSON 读体函数，返回前已做
  `NormalizeLenientJSONRequestBody`（去 BOM、转义字符串内的控制字节）。所以 handler 拿到的长度既不是客户端发来的字节，也不
  一定是纯解压后的长度。
- 网关改写请求体后，`doOpenAIUpstream` 经 `kongSendOpenAIUpstreamCompressed`（fork 文件）发出：发往 `chatgpt.com` 的压缩后
  直接交 `httpUpstream.Do`，上游像是不接受时在同一次调用里用明文重发；不压缩的交 `sendOpenAIUpstream`（插件判定在其中）。
  `httpUpstream.Do` 可能在主机校验、取客户端、建连时就失败，此时正文一个字节都没发；`http.Client` 跟随 307 / 308 重定向时会
  经 `GetBody` 重放正文。

**下行**（上游 → 网关 → 客户端）：

- **chatgpt.com 的 SSE 从不压缩**，与 `Accept-Encoding` 无关；普通 JSON 支持 gzip 与 br（工作区 `FACTS.md`「上游的响应压缩」，
  用本机 ChatGPT 登录的 codex 经本地中转实测）。官方 Codex 不发 `Accept-Encoding`。
- 网关的 OpenAI 转发现在也不显式带 `Accept-Encoding`，但 Go 的 transport 会自动声明 gzip——与官方客户端不一致，对 SSE 又没有
  作用。§6 把它去掉，之后网关读到的上游字节就是线上的消息体字节。
- zstd 被拒的判定（`kongZstdRejected`）：415 不读正文；400 / 422 最多探读 64 KiB，回填已读前缀后交还调用方。
- 透传分支逐行读上游 SSE、原样写给客户端；只在模型映射、函数参数或图片状态不规范、请求的工具名被改写、出错合成
  `response.failed` 时改正文。流式在终止事件后 `break`，不读到 EOF。网关对客户端不压缩，也没有包 ResponseWriter 的压缩或
  计数中间件。

**用量行**：

- Responses 在 handler 的 `submitResponsesUsage` 里提交（成功、非 failover 错误、客户端断开、failover 时客户端已走四处共用，
  异步写库）；`AccountID` 是最后一次尝试的账号，行里没有代理快照。
- alpha search 在 `recordAlphaSearchUsage` 里提交，result 不带请求特征。
- cyber 拒绝另由 `recordCyberPolicyIfMarked` 在 `context.Background()` 的 goroutine 里写一行 `request_type = 4` 的用量行，最近
  7 天 4 行（共 47 万行）。
- `kong_request_features`（JSONB）目前只放票据特征，`IsEmpty` 只看四个票据字段；透传分支在读响应体之前就取了特征快照。

## 2. 口径与格式

每个用量行在 `kong_request_features` 里加一个 `bytes` 对象：

```json
"bytes": {
  "in": 51234, "body": 182345, "out": 20410,
  "port": 23625, "up": 183001, "up_wire": 61234, "sends": 1, "down": 20345,
  "others": [ {"account": 7, "port": 23618, "up": 183001, "up_wire": 61280, "sends": 1, "down": 210} ],
  "plugin": 0
}
```

**客户端一侧**（整个请求）：

| 键 | 含义 |
|---|---|
| `in` | 从客户端连接上读进来的请求体字节（解压前） |
| `body` | handler 读体函数返回的请求体长度（已解压，可能已含组合路由的模型改写与宽松 JSON 归一化） |
| `out` | 写给客户端的响应体字节（含 SSE 心跳注释） |

**上游一侧**——顶层是**这个用量行的账号**经 `port` 那个出口的全部尝试；同一请求里经其他账号或其他出口的尝试，按
（账号，端口）合并成 `others` 的一项，只有出现时才写：

| 键 | 含义 |
|---|---|
| `port` | 发送时账号代理 URL 的端口（没有代理为 0） |
| `up` | 正文开始被 transport 读取的那些发送，各自的明文长度之和（网关改写后、zstd 压缩前） |
| `up_wire` | transport 实际读走的正文字节之和（压缩后；含重定向重放；不等于 TCP 已确认送达的量） |
| `sends` | 网关发出的 HTTP 请求数（换号、同账号重试、透传内部重试、zstd 被拒后的明文重发各算一次；重定向跳转不另算） |
| `down` | 从上游响应体读到的原始字节之和（在网关的解压层之前计，所以上游即使主动压缩也是线上的消息体字节；被拒响应的探读前缀只计一次；没读的尾部不计） |
| `plugin` | 交给插件发送的次数，只在大于 0 时写；这些发送的字节不计入上面各项（插件自带传输层，网关量不到） |

- 按账号或出口汇总时：顶层记给（行的 `account_id`，`port`），`others` 的每一项记给它自己的（`account`，`port`）。整个请求的
  上游量 = 顶层 + `others`。
- 都是**消息体字节**，不含头、HTTP/2 分帧与 TLS；这部分按 §5 的经验系数换算。
- 缺值：`bytes` 整个不出现 = **没采到**（未接入的端点与路径，见 §7），不是"没有流量"；`bytes` 出现时顶层各键都在，0 就是真的 0。

查询示例（各出口近一天的上游流量）：

```sql
WITH parts AS (
  SELECT account_id, (b->>'port')::int AS port, (b->>'up_wire')::bigint AS up_wire, (b->>'down')::bigint AS down
  FROM usage_logs, LATERAL (SELECT kong_request_features->'bytes' AS b) x
  WHERE created_at > now() - interval '1 day' AND b IS NOT NULL
  UNION ALL
  SELECT (o->>'account')::bigint, (o->>'port')::int, (o->>'up_wire')::bigint, (o->>'down')::bigint
  FROM usage_logs, jsonb_array_elements(kong_request_features->'bytes'->'others') o
  WHERE created_at > now() - interval '1 day'
)
SELECT port, sum(up_wire) AS up_wire, sum(down) AS down FROM parts GROUP BY port;
```

## 3. 采集

### 3.1 请求级计数容器

- 一个 fork 中间件包在 `bodyLimit` 与 `textBodyLimit` 外面（`routes/gateway.go` 里定义这两个限制的两行）。三组 Responses
  路由（`/v1`、根路径、`/backend-api/codex`）经 `bodyLimit`；三个 alpha search 入口里，`/v1` 与 `/backend-api/codex` 的先经组上的
  `bodyLimit`、再经路由上的 `textBodyLimit`，根路径的只经 `textBodyLimit`。所以中间件必须**幂等**：context 里已有容器就什么都
  不做，保证容器只建一次、请求体只包一层。
- 它新建一个计数容器挂到 `c.Request` 的 context 上，并把 `c.Request.Body` 包一层计数 reader，**包在限制中间件的
  `MaxBytesReader` 里面**：中间件预读、handler 读、zstd 分块读，数到的都是网络上读进来的原始字节，`MaxBytesError` 原样上抛。
- 之后各层用 `WithContext` 在它上面派生，值一直保留：handler 传给 `Forward` 的 `c.Request.Context()`、透传分支的
  `context.WithoutCancel`、`NewRequestWithContext`、`BeforeHTTP` 的派生，最终在发送处用 `request.Context().Value` 取得到。
- 上游各项按（账号 ID，端口）分组累加；全部用原子计数或加锁——上游响应体由服务层 goroutine 读，客户端心跳由另一个
  goroutine 写，合并发生在 handler。计数 reader 在底层 `Read` 返回之后、自己返回之前就把字节记上，所以合并之前已经完成的
  读取一定在账上；合并那一刻仍在进行的读取不保证计入（见 §3.4 的边界）。

### 3.2 handler：请求体长度

`Responses` 与 `AlphaSearch` 读完请求体后各一行，把读体函数返回的长度记进容器的 `body`。

### 3.3 上游发送与响应

**按实际走的分支计数**。插件判定在 `kongSendOpenAIUpstreamCompressed` 里预判一次，`sendOpenAIUpstream` 里
（`RoundTripOpenAIOAuth` 的 `handled`）又按插件的当前绑定判一次，绑定可以在两次之间在线切换。所以计数不放在预判之后，
而是放在一个 fork 辅助函数里，**每一次真正交给 `httpUpstream.Do` 的发送都经过它**：

- 压缩分支：`kongSendOpenAIUpstreamCompressed` 里的 `send()`（含明文重发）改调这个辅助函数。
- 不压缩分支：`sendOpenAIUpstream` 最后那行 `httpUpstream.Do` 换成这个辅助函数（单处替换）；`handled` 为真的分支加一行，把
  `plugin` 加 1。插件发送不包请求体也不包响应体。

辅助函数的入参是请求、代理、账号与**这次发送的明文长度**：`compress` 会把 `Body`、`GetBody`、`ContentLength` 换成压缩后的
版本，所以 `kongSendOpenAIUpstreamCompressed` 在调用 `compress` 之前先记下 `request.ContentLength` 作为明文长度，压缩发送与被拒
后的明文重发都传它；不压缩的分支传当前的 `ContentLength`。不为取长度额外读正文。辅助函数对每次发送做三件事：

- **请求体**：交给 `httpUpstream.Do` 之前，给 `request.Body` 包一层计数 reader，并把 `request.GetBody` 换成"返回包了计数的新
  reader"的版本，`http.Client` 跟随重定向时的重放因此也计入。正文第一次被读时把传入的明文长度计入 `up`（建连就失败、正文没
  被读的发送，`up` 与 `up_wire` 都不加）。`BeforeHTTP` 事先保存的 `GetBody`（票据观测读模型用）与 `compress` 读正文都发生在
  包装之前，不计。
- **发送次数**：`sends` 加 1。
- **响应体的计数目标**：把这次发送的（账号，端口）计数目标放进请求 context。`repository/http_upstream.go` 的 `doUpstreamRequest`
  在调用 `decompressResponseBody` **之前**加一行：context 里有计数目标时给 `resp.Body` 包一层计数 reader。这样数到的是解压之前
  的原始字节，上游主动发来压缩响应时也不会记成解压后的大小；没有计数目标的请求（模型目录、探测、其他平台）不受影响。服务
  层不再重复包装。

响应体计数的效果：

- 被拒的第一份响应：415 不读正文，通常计 0（上游主动压缩时，解压器初始化读到的头部字节会计入）；400 / 422 探读的前缀经计数
  reader 读一次，回填的前缀从内存给出、不再经过计数，所以只计一次；关闭后不为计量额外排空。
- 最终返回的响应由透传分支读多少计多少（含错误体与客户端断开后的排空）。

边界：

- `down` 只统计 `doUpstreamRequest` 拿到的最终响应。`http.Client` 跟随重定向时在内部读过并关掉的中间响应看不到，
  不计（chatgpt.com 的 POST 实际不重定向），算在 §5 的差额里。
- 归属：账号取 `account.ID`，端口取这次发送用的 `proxyURL`。
- 只在账号平台为 OpenAI 时计；容器里没有记录的请求不写 `bytes`。

### 3.4 提交用量时合并

在 `submitResponsesUsage` 与 `recordAlphaSearchUsage` 里，**提交异步任务之前同步**取 `c.Writer.Size()`（handler 返回后 gin
context 会被回收复用，不能留到异步任务里；`-1` 表示没写过，按 0 记）作为 `out`，连同容器内容写进 `res.KongRequestFeatures.Bytes`：
行的账号与端口那一组放顶层，其余组放 `others`。只在行的账号平台为 OpenAI 时写。特征为 nil 时新建一份；`Snapshot` 给出的是
副本，可以直接改。

合并时机的边界：合并发生在 `Forward` 返回之后。透传分支在 `Forward` 所在的 goroutine 里同步读完响应体，上游在正常响应之前
也已收完请求体，所以线上路径合并时没有在途读取。只有上游提前报错、transport 还在后台读请求体准备上传的情况，那几次读取
可能落在合并之后、不进账；为此加"等待在途读取结束"的收尾协议不划算（关闭与发送前失败的路径都要处理，还要避免持锁等待），
不做，计入 §5 的差额。

## 4. 存储

- 键放在已有的 `kong_request_features`（JSONB）里，**不加列、不加迁移、不碰上游的插入代码**。`KongRequestFeatures.IsEmpty`
  要改成 `bytes` 非空也算非空，否则只有字节数的特征在写库（`nullKongRequestFeaturesJSON`）与回读时会被当成空丢掉。前端的
  请求特征列只认四个票据键，其余键忽略，不受影响。
- 体积（在线上库用 `pg_column_size` 实测）：`bytes` 子对象约 200 B（多一项 `others` 再加约 70 B，只在换号时出现）；
  `usage_logs` 现在整行平均 510 B（不含索引），每行约增加 40%。按每天约 8 万行约 16 MB / 天，库里 31 天约 500 MB。
- 长期分析：库里的明细保留 31 天（现行规则不变）；周归档导出整行，更久的分析用归档（`sub2api-archive/`，DuckDB 直接读）。

## 5. 标定与核对

取 `B = Σ(up_wire + down)`，对窗口内所有用量行的顶层与 `others` 求和（不加 `up`、`in`、`out`）；mihomo 同一窗口的总量为 `M`。
两者是近似关系：

`M ≈ B + H + X + ε`

- `H`：这些请求的协议开销（请求头、HTTP/2 分帧、TLS）。
- `X`：不在用量行里的流量——刷新令牌、查额度、模型目录、指纹测试、以错误结束而没有用量行的请求（最近一天约 170 个，按字节的
  占比未知）、WS、cyber 拒绝行、插件发送、重定向的中间响应、读到终止事件后没读的响应尾部、合并后才完成的在途读取、用量
  记录被丢弃或写库失败的请求。
- `ε`：有正有负的测量差。`up_wire` 是 transport 读走的字节，HTTP/2 先读正文再等流控写出，发送中止时读走的不一定发出；
  两边的时间窗口不同（见下）；mihomo 采样会丢重启前最后一个间隔。

用法：

- **经验系数** `M / B` 同时含 `H`、`X` 与 `ε`，不是纯协议开销。按已拍板的做法用它换算，前提是这几项的占比都稳定；它漂移时
  说明其中某项在变，不能直接判断是哪一项。
- **未解释差额** `M - B` 是 `H + X + ε`，可能为负的部分来自 `ε`，不能解释成"负的后台流量"。
- **时间口径**：用量行的 `created_at` 是异步任务执行的时刻，长流的字节全记在结束的那个小时；mihomo 账本把两次采样之间的
  增量记在采样所在的小时（UTC+8）。所以单小时只作粗检，标定用整天、已结束且落库完成的窗口。

## 6. OpenAI 上游不声明压缩（单独一个提交）

**目的**是消除与官方 Codex 的一处可识别差异：官方客户端发 `/responses` 不带 `Accept-Encoding`，网关现在因为 Go 的默认行为带着
`Accept-Encoding: gzip`。上游对 SSE 本来就不压缩，去掉它对带宽没有影响，`down` 也因此就是线上的消息体字节。范围只求
`/responses`，不追求网关发往 chatgpt.com 的全部请求都对齐。

**做法**：Go 没有"这一个请求不声明"的开关——请求里不带这个头 transport 就补 gzip，只能在 transport 上开 `DisableCompression`。
`/responses` 的请求（透传与转换两条路径）都在 context 里标了 `HTTPUpstreamProfileOpenAI`，`httpUpstream.Do` / `DoWithTLS` 按 profile
取连接池，所以：

- `repository/http_upstream.go` 的 `poolSettings` 加一项 `kongDisableCompression`，`applyProfilePoolSettings` 对 OpenAI profile 打开它，
  `buildPoolKey` 把它算进配置键（`poolKey` 只用来判断已缓存的客户端配置是否变了）；`buildUpstreamTransport` 与
  `buildUpstreamTransportWithTLSFingerprint` 各一行设 `transport.DisableCompression`。
- **连接池并存靠缓存键**：`s.clients` 按 `cacheKey` 索引。普通路径的 `cacheKey` 含协议模式，OpenAI profile 总是 `openai_*`、默认
  profile 是 `default`，本来就分开。TLS 指纹路径（`getClientEntryWithTLS`）的 `cacheKey` 固定用默认协议模式、不含 profile，同账号同
  代理交替发 OpenAI 与默认 profile 的 TLS 请求时，`poolKey` 不符会删掉旧条目重建，而不是两个池并存（现在两者的响应头超时就
  不同，已有这种替换；新开关会让它在其余设置相同时也发生）。所以 TLS 路径的 `cacheKey` 要加一个压缩策略的区分项（一行），
  两者各占一个客户端缓存名额；`proxyKey`、协议模式与 HTTP/2 回退状态的语义都不变。
- 不压缩时 `decompressResponseBody` 照旧只在上游主动发来 `Content-Encoding` 时生效。

**波及**：同一 profile 下的其他请求也不再声明压缩——alpha search（每天约 4 千次，JSON 很小；官方 Codex 发这类请求也不带）、
图片、embeddings、计数等（线上没有流量）。给客户端拉模型目录时，OAuth 账号走的是 `httpclient.GetClient` 的共享客户端，不经这个
连接池，照旧由 Go 自动 gzip（目录约 555 KB，gzip 后约 106 KB，每天约 1,300 次），不受影响；管理端的上游模型列表
（`upstream_models.go` 经 `DoWithTLS`）不标 OpenAI profile，也照旧。

**不追的路径**：WS 握手（Go 对升级请求同样自动加 gzip）、agent identity、PAT、刷新令牌与查额度用的客户端。

**发布形态**：独立于字节计数的一个 fork 提交，FORK-NOTES 单独一个小节；上游若新增 OpenAI 上游请求，要标 OpenAI profile 才在
覆盖范围内。

## 7. 不做的事

- **WS**：不是主要场景，WS 的用量行没有 `bytes`。
- **其他 OpenAI 端点**（chat completions、messages、图片、embeddings）：线上没有流量，不接。
- **cyber 拒绝行**（`request_type = 4`）：写行的 goroutine 用 `context.Background()`，要带字节得改上游 handler 两三处，7 天只有
  4 行，不接。
- **非 OpenAI 平台**：Responses 路由会把多个平台交给同一个 handler，只有账号平台为 OpenAI 的行写 `bytes`。
- **以错误结束、没有用量行的请求**：它们的字节无处落地，计入未解释差额。
- **插件发送的字节**：网关量不到，只记次数。
- **重定向的中间响应**：在 `http.Client` 内部读掉，看不到。
- **合并时仍在进行的读取**：只在上游提前报错时出现，不加等待在途读取的收尾协议（§3.4）。
- **协议开销**：按 §5 的经验系数换算，不逐请求计。
- **库内按小时 × 账号的汇总表**：需要库里长期留存时再加。
- **对下游的压缩**：客户端声明 `Accept-Encoding` 时由网关压缩响应（给我们自己的客户端，例如本地代理，省客户端一侧的流量）。
  官方 Codex 不声明，所以只对定制客户端有用；要处理 SSE 的逐事件刷新、心跳与 `out` 的口径（压缩前还是压缩后），另行设计。

## 8. 改动面与测试

- fork 文件：新增 `service/kong_request_bytes.go`（容器、计数 reader、计数后发送的辅助函数 `kongDoUpstreamCounted`、响应计数入口
  `KongWithUpstreamDownCounter` / `KongCountUpstreamResponseBody`、合并 `KongMergeRequestBytes`）与
  `server/routes/kong_request_bytes.go`（中间件）；`kong_request_features.go`（`Bytes` 字段、`IsEmpty`）；
  `kong_openai_request_zstd.go`（压缩前取明文长度，`send()` 改调辅助函数）。
- 上游文件里的单行挂点（共 9 处）：`routes/gateway.go`（包 `bodyLimit`、`textBodyLimit` 各一行）；`service/openai_plugin_transport.go`
  （`sendOpenAIUpstream` 里 `httpUpstream.Do` 换成辅助函数、`handled` 分支记插件次数，各一行）；`repository/http_upstream.go`
  （`doUpstreamRequest` 里解压前包响应计数，一行）；`handler/openai_gateway_handler.go`（读体后、提交前各一行）；
  `handler/openai_alpha_search.go`（同上）。
- 测试（`kong_*_test.go`）：
  - 计数 reader 与容器；`IsEmpty` 对只有字节数的特征为假，写库与回读不丢；JSON 形状（顶层各键都在、0 输出为 0、`others`
    与 `plugin` 只在需要时出现）。
  - 入站：明文、zstd、gzip 三种，`in` 与 `body` 各自正确；经分组白名单中间件预读时 `in` 不变；带 BOM、控制字符归一化与组合
    路由改模型时 `body` 符合定义。
  - 上游：发送前失败（建连失败）后换号成功——失败那次 `up` / `up_wire` 为 0、`sends` 计 1；账号 A 经出口 X 失败、B 经 Y 成功，
    分别落在 `others` 与顶层，合计等于请求总量；之后把 B 的代理改到 Z，已写的行不变；307 / 308 重放计入 `up_wire`；zstd 被拒
    后明文重发，`sends` 为 2、两次的字节都计；415 被拒响应（未压缩、正文未读）`down` 计 0；400 探读前缀只计一次。
  - 明文与压缩长度：明文长度 `L`、压缩长度 `C`——压缩成功时 `up = L`、`up_wire = C`；压缩被拒、两次正文都被完整读取的明文重发时
    `up = 2L`、`up_wire = C + L`、`sends = 2`；建连失败时 `up` 与 `up_wire` 都为 0。
  - 上游主动压缩：不发 `Accept-Encoding`、假上游仍回 gzip——业务拿到解压后的正文，`down` 等于实际读到的压缩字节；400 探读与
    回填仍只计一次。
  - 插件：在预判与实际分流之间切入插件——这次发送只加 `plugin`、不计字节；切出插件、实际走 HTTP——计入 HTTP 各项、不加
    `plugin`；两种情况下原发送行为不变。
  - 重定向：假上游先回带正文的 307、再回 200——两次上行都计入 `up_wire`，中间响应的下行不计（按 §3.3 的边界）。
  - 下行与客户端：心跳字节计入 `out`；客户端断开后排空的字节计入 `down`。
  - alpha search：`/v1/alpha/search`、`/alpha/search`、`/backend-api/codex/alpha/search` 三个入口，明文与压缩请求都带 `bytes`，`in`
    只计一次（含分组白名单预读）。非 OpenAI 平台账号的行没有 `bytes`。
- FORK-NOTES 新增一个小节：挂点；复核项——上游新增 OpenAI 发送点要经 `doOpenAIUpstream`，新增的 OpenAI 用量提交点要合并字节数，
  改 `httpUpstream.Do` 的重放或解压方式时要复核 §3.3。
- §6 是单独的提交：`repository/http_upstream.go` 约 6 行（`poolSettings` 字段、`applyProfilePoolSettings`、`buildPoolKey`、两个
  transport 构建函数、TLS 路径的 `cacheKey`）。测试：
  - 假上游记录收到的请求头：OpenAI profile 的请求（普通与 TLS 指纹两种 transport，普通路径含 HTTP/2 回退前后）不带
    `Accept-Encoding`；默认 profile 仍带 `gzip` 并照旧透明解压；OpenAI profile 收到上游主动发来的 `Content-Encoding: gzip` 时仍由
    `decompressResponseBody` 解开。
  - 同账号同代理、其余设置相同的 TLS 请求按 OpenAI → 默认 → OpenAI 交替：各自的请求头正确，第三次复用第一次的客户端；缓存
    上限、活跃条目保护与空闲淘汰照旧。
  - FORK-NOTES 单独一个小节。

## 9. 上线与回滚

- 与跟进 v0.2.17 一起作为 `v0.2.17-kong.1` 发布。没有迁移：回滚直接换回旧版本，旧版本读到带 `bytes` 的行只是忽略这个键。
- 上线后确认：
  - 完整性只查已接入的那部分：近一小时 `/v1/responses` 与 `/v1/alpha/search` 中，账号平台为 OpenAI、`request_type` 不是 3（WS）
    也不是 4（cyber）的用量行都带 `bytes`。
  - 端口用受控请求核对：对一个账号发一个请求，看那一行的 `port` 等于发送时该账号代理的端口；把它改派到另一个出口后再发一个，
    新行是新端口、旧行不变。不拿账号的当前配置去比历史行。
  - 用一整天的窗口按 §5 与 mihomo 对账，记下经验系数与未解释差额。
  - §6：网关发往上游的请求头从外部看不到（`aeprobe` 只能观测官方客户端），是否生效以单元测试为准；上线后确认 `/responses`、
    alpha search 与模型目录拉取都照常。
