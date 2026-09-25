# Codex 的 x-reasoning-included：让客户端不再重复计 reasoning

**状态**：已实现，默认开启；`KONG_CODEX_REASONING_INCLUDED=false` 关闭。仓库组织与发布流程见 `FORK-NOTES.md`。

本文是通用设计说明，不涉及具体部署实例。

## 0. 背景：codex 把历史 reasoning 算了两遍

codex CLI 判断是否自动压缩上下文时，比较的是一个客户端内部计数，而不是 API 回报的用量
（`codex-rs/core/src/context_manager/history.rs` 的 `get_total_token_usage`）：

```
internal = last_token_usage.total_tokens                      # API 回报
         + est(最后一条模型生成项之后的本地新项)                 # 待发送的工具输出等，通常很小
         + [server_reasoning_included == false 时]
           est(最后一条用户轮边界之前的全部 encrypted reasoning)
```

- `server_reasoning_included` 每回合开始重置为 false，响应带 `x-reasoning-included` 头时置 true。
  SSE 从 HTTP 响应头读（`codex-api/src/sse/responses.rs`），WS 从握手响应头读（`endpoint/responses_websocket.rs`）。
- 每个 reasoning 块按 `ceil((base64 长度 × 3/4 − 650) / 4)` 估算 token。
- 这个标志在 codex 里只用于这一处计数，不改变发送的内容，也不影响 reasoning 是否随请求回传。

ChatGPT codex 后端的 HTTP SSE 响应不带这个头，但它回报的 `input_tokens` 已经含历史 reasoning：相邻请求的
`input_tokens` 增量约等于上一次的全部 `output_tokens`（含 `reasoning_output_tokens`）加上新的非 reasoning 项。
客户端再估一遍就是重复计数，估算值本身还比真实用量高约 20%。

**线上数据**（一个部署实例的六次自动压缩，按上式复算；阈值 = 872,000 × 90% = 784,800）：

| API total | 估出的历史 reasoning | 复算的内部计数 |
|---|---|---|
| 617,543 | 161,526 | 779,069 |
| 587,292 | 195,574 | 782,866 |
| 580,129 | 208,806 | 788,935 |
| 605,336 | 181,338 | 786,674 |
| 626,742 | 153,768 | 780,510 |
| 690,939 | 246,484 | 937,423 |

前五次正好卡在阈值附近；最后一次是新的用户消息到达、此前全部 reasoning 一次性归入估算项造成的跳变。
会话实际用量在 58–69 万时就被压缩，本可以用到约 78.5 万。

## 1. 目标与约束

- 发往 codex 客户端的 Responses 成功响应带上 `x-reasoning-included`，覆盖流式与非流式。
- 上游给了这个头就沿用上游的值，响应里只有一份。
- 不改请求体、上游请求头，也不动 `x-codex-turn-state` 的写入、清除与溯源登记。
- 一个开关即可回到现状。标志每回合重置，没有持久状态。

错误响应带不带无所谓：codex 对非 2xx 响应在读头之前就返回错误（`codex-rs/http-client/src/transport.rs`），
这个头只在成功的流式响应与成功的 WS 握手上读，而且只看在不在。§2 的写法因此允许错误响应也带上它。

## 2. 写入时机与取值

**响应头可能在选定上游之前就提交**，这决定了写入时机：

- 流式请求排队等用户槽或账号槽时，handler 每隔一段时间写一次排队心跳并 Flush（`gateway_helper.go` 的
  `waitForSlotWithPingTimeout`），响应头随第一次心跳提交。
- OpenAI 账号的非透传流式响应先把账号相关的头暂存起来，见到首个语义输出才提交；首输出前的 keepalive 会先把
  响应头提交出去，暂存的头从此作废（`handleStreamingResponseWithReasoning`）。

这两种情况在长推理、账号并发打满时都很常见。所以分两步写：

1. **handler 先写 `1`**：Responses 在排队等用户槽之前，WS 入口在 `coderws.Accept` 之前（握手发生在拨上游之前）。
2. **各 Responses 成功出口按本次上游响应重写一次**：上游响应头带这个头就换成上游的值，并把白名单 `Add`
   进来的那一份合成一份。响应头已经提交时这一步不再生效，客户端收到的是第 1 步的 `1`。

值一律用 `Set` 写入，failover 时被放弃的 attempt 留下的值也被覆盖。codex 只看这个头在不在，上游给的是别的值
也原样转发。

各通路的结果：

| 通路 | 客户端收到的值 |
|---|---|
| 下游 HTTP，上游 HTTP（透传与非透传） | 上游响应头有就用它的值，否则 `1`；响应头提前提交时为 `1` |
| 下游 HTTP，上游 WS（`forwardOpenAIWSV2`） | `1`：这条通路只把握手里的 `x-codex-turn-state` 写给客户端，其余握手头不下发，第 1 步的值原样送达 |
| 下游 HTTP，协议转换（Responses 转 Anthropic Messages / Chat Completions） | `1`，同上 |
| 下游 WS（`GET /responses` 升级） | `1` |

**只对 codex 客户端**：判据复用 `openai.IsCodexOfficialClientByHeaders(User-Agent, originator)`，与透传路径识别
官方客户端的口径一致（UA 前缀集、`Codex ` 家族、UA 尾部 `(name; version)` 兜底、originator 精确集合）。
其它客户端不注入，HTTP 上游通路仍按白名单转发上游的这个头，与现状相同。下游 WS 以升级请求的请求头判定。

**端点**：`/v1/responses`、`/responses`、`/backend-api/codex/responses` 及其 `/responses/*` 子路径。compact 不单独
排除：codex 只在 responses 流上读这个头，compact 响应带上它没有副作用，不必为此多一条路径判断。

**不按上游区分**：OAuth、透传、grok 账号与协议转换通路一律按上述规则处理。前提是上游回报的 `input_tokens`
如实反映上下文占用，见 §5。

## 3. 接入点

helper 在 `backend/internal/service/kong_codex_reasoning_included.go`：

- `KongApplyCodexReasoningIncluded(c, upstream)`：写进 `c.Writer.Header()`；handler 传 `nil`。
- `kongApplyCodexReasoningIncludedUnstaged(staged, c, upstream)`：首输出暂存路径用。这个头与账号无关，不随暂存头
  延迟提交，直接写 writer；同时从暂存集合里删掉白名单带进来的那一份，否则提交时逐项 `Add` 会再写一份。

路径与状态码不在 helper 里判断，由调用点保证：第 1 步只在 Responses 与 WS 入口，第 2 步只在 Responses 成功出口。

| 步骤 | 位置 | `upstream` |
|---|---|---|
| 1 | `OpenAIGatewayHandler.Responses`，`acquireResponsesUserSlot` 之前 | `nil` |
| 1 | `OpenAIGatewayHandler.ResponsesWebSocket`，鉴权与连接数检查之后、`coderws.Accept` 之前 | `nil` |
| 2 | 透传流式 `handleStreamingResponsePassthrough`，`writeOpenAIPassthroughResponseHeaders` 之后 | `resp.Header` |
| 2 | 透传非流式 `handleNonStreamingResponsePassthrough`、`handlePassthroughSSEToJSON`，同上 | `resp.Header` |
| 2 | 非透传流式 `handleStreamingResponseWithReasoning`：OpenAI 账号在 `stageOpenAICodexTurnState` 之后（`Unstaged`），其它平台在 `relayOpenAICodexTurnState` 之后 | `resp.Header` |
| 2 | 非透传非流式 `handleNonStreamingResponse`、`handleSSEToJSON`，`relayOpenAICodexTurnState` 之后 | `resp.Header` |

HTTP→WS 与协议转换通路没有第 2 步：它们不转发上游的这个头，第 1 步写下的值不会被重复或改写。
`coderws.Accept` 会把 `c.Writer.Header()` 里已有的头随 101 一起发出。

## 4. 开关

环境变量 `KONG_CODEX_REASONING_INCLUDED`，写法与 `KONG_OPENAI_REQUEST_ZSTD` 相同：

| 取值 | 行为 |
|---|---|
| 未设置 | 开启 |
| `strconv.ParseBool` 认作真的值（`1`、`true`、`TRUE` 等） | 开启 |
| `strconv.ParseBool` 认作假的值（`0`、`false`、`FALSE` 等） | 关闭 |
| 空值或其它值 | 记一条错误日志，关闭 |

`sync.OnceValue` 在首次使用时读一次，开启时打一行 info。配错时关闭而不回落到默认值：默认行为比上游更激进，
不应由一个写错的值触发。每次注入不打日志。

关闭时行为与现状完全一致：HTTP 上游通路照旧按白名单转发上游的这个头，其它通路照旧不带。

## 5. 风险与已知局限

- **语义前提**：这个头的含义是「服务端回报的用量就是上下文的真实占用，已含 reasoning」。§0 只在 ChatGPT codex
  上游实测过。其它上游（第三方中转、grok、协议转换）只要 `input_tokens` 如实反映上下文占用，前提同样成立；
  若某个上游漏计了实际占用上下文的历史 reasoning，客户端计数会偏低，可能顶到 872,000 × 95% = 828,400 的硬顶
  才压缩，或请求超出真实窗口报错。出现这种迹象时关掉开关即可。
- **响应头提前提交时用不上上游的值**：客户端收到的是第 1 步的 `1`。codex 只看在不在，结果相同。
- **上游 WS 握手带不带这个头尚未确认**。HTTP→WS 通路本就不下发握手头，客户端收到的都是 `1`，不受影响。
- 自动压缩点能推到的上限是 `min(context_window × 90%, model_auto_compact_token_limit)`。要更高得另外调整
  模型目录的 `max_context_window`，不在本功能范围内。

## 6. 验证

- 单元测试：`kong_codex_reasoning_included_test.go`（开关解析、取值规则、各第 2 步出口、首输出前 keepalive、
  HTTP→WS 保留第 1 步的值）、`handler/openai_gateway_handler_kong_test.go`（排队心跳先于选号时已带上、下游 WS 的
  101 响应）。
- 部署后：带 `originator: codex_cli_rs` 的 curl 打 `/v1/responses`，响应头应有 `X-Reasoning-Included: 1`；
  去掉 originator 再打，不应出现。按用量行的 `openai_ws_mode` 找出上游 WS 与上游 HTTP 的 codex 请求各一条，
  确认两者的客户端都收到了这个头。
- 效果：codex 会话的自动压缩点应从 API total 58–69 万推到约 78.5 万。
