# OpenAI 请求体内存：少留副本、少做复制

**状态**：已实现（`v0.2.15-kong.8`）；只用于本 fork，不向上游提 PR。各项挂在请求体快速路径的开关下（§3），仓库组织与发布流程见 `FORK-NOTES.md`。

本文是通用设计说明，§0 的剖析数据来自一个部署实例。快速路径（`DESIGN-openai-request-body-fastpath.md`）减的是
每个请求的 CPU，本文减的是每个请求占用与分配的内存，两者的约束相同（§1）。

## 0. 背景：内存随在途请求数 × 请求体大小线性增长

主要流量与快速路径相同：codex CLI 走 HTTP `POST /v1/responses`，流式，落在 OpenAI OAuth 账号上走透传
（`forwardOpenAIPassthrough`），出站请求体按 zstd 压缩发往 ChatGPT 后端；客户端经本地代理上来，请求体也是
zstd 压缩的（`EncodeAll`，帧头带原始大小）。

**线上现象**。一段时间里有一部分请求在对话历史中带图片，base64 随每一轮原样重发：平均出站请求体从约 1 MB 涨到
2.6–3.4 MB，token 数基本不变，出站压缩率从 0.47 变差到 0.66。请求数没变，内存却顶到了 `GOMEMLIMIT`：

- 每轮 GC 标记后的存活堆 570–744 MiB（`GOMEMLIMIT` 896 MiB），约每 5 秒一轮 GC，cgroup 开始换页；
- 32 KiB 以上的分配每分钟 1.94 GB，约每个请求 43 MB；
- 进程约 0.9 核里，后台 GC 占 26%，分配 4%，内核态 22%（含缺页）；双核宿主 CPU 跑满后，请求链路里的数据库
  事务也推进不动，连接池被占满。

存活堆与分配点用 bpftrace 的 uprobe 量出（`runtime.(*gcControllerState).resetLive` 的参数是标记后的存活字节数，
`runtime.mallocgcLarge` 的参数是大块分配的大小，按调用栈汇总）。一分钟内的大块分配：

| 来源 | 每分钟 | 占比 |
|---|---|---|
| item id 清洗（`sanitizeOpenAIResponsesInputItemIDs`：逐项复制、`sjson.DeleteBytes`、`replaceOpenAIRawInput`） | 663 MiB | 34% |
| 入站读取与解压（读块再拼接、每请求新建解码器、`io.ReadAll` 扩容） | 约 512 MiB | 26% |
| 身份改写（`applyCodexAccountIdentityClientMetadataRaw` 的两次 sjson 改写） | 330 MiB | 17% |
| 出站 zstd（`GetBody` 再读一份明文、输出缓冲扩容） | 约 322 MiB | 17% |

item id 清洗之所以几乎每个请求都要重写：codex 给历史里的 `custom_tool_call_output` 带 `ctco_` 前缀的 id，上游规则
要求这一类以 `fc` 开头（见 `openAIResponsesInputItemIDPrefix` 的注释），于是每项工具输出都删 id。线上 30 秒、37
次调用里删了 397 个 id；工具输出正是请求体的大头，等于每次把整个 input 复制四遍。

**离线复现**（`handler` 包的 `TestKongOpenAIBodyMemory`，量法见 §4）。一份 2.78 MB、入站压缩后 2.10 MB 的合成请求
（历史项都带线上形态的 id，夹 6 张 300 KB 的图片，入站 zstd），经 handler → `Forward` → 透传发往假上游；假上游像
真实 transport 一样边读边丢弃请求体、把请求挂在 `Response.Request` 上，在流式响应第一次被读时停住。

流式期间请求持有的存活约 4 份请求体（4.04 份），由 inuse 堆剖析定位：

| 存活的副本 | 为什么还活着 | 必要吗 |
|---|---|---|
| 入站解压出的版本 | handler 持有，failover 换号要从它重新派生 | 必要 |
| item id 清洗后的版本 | `Forward` 清洗后用 `newOpenAIRequestView` 重取 `reqModel` 等字段；视图是零拷贝的，字段是这份请求体的子串，`reqModel` 一路用到请求结束，拖住整块 | 不必要 |
| 透传的最终版本 | 流式结束后还要用（compact 回退重试、`extractOpenAIServiceTierFromBody`） | 必要 |
| 出站 zstd 输出 | 经 `Response.Request` 的 `Body` 与 `GetBody` 保活到流式结束；输出缓冲按明文一半预留，压不动时扩容到与明文同大 | 不必要 |

池子保持预热时，每个请求分配约 39 份请求体：item id 清洗的逐项复制（语料里要删 id 的工具输出比线上还多）、每个
请求新建的解码器、`io.ReadAll` 的扩容、读块后的拼接、出站的明文副本与输出扩容、身份改写的两次复制。

## 1. 目标与约束

- **流式期间请求自己持有的存活从 4 份降到 2 份**（只剩入站与最终版本；出站正文在上传完成后放掉，上传期间仍在），
  **池子预热时每个请求的分配降到约 6 份**（§0 的语料下原来约 39 份）。冷启动与池化的工作缓冲（入站解码器、
  出站编码器）不属于某个请求，单独计量（§4）。
- 约束沿用快速路径：
  - 发往上游的请求与现状逐字节一致（含压缩后的字节），每一次重试、每一次换号都算；
  - 对客户端的可见行为一致：状态码、错误响应、流式输出；
  - 观测面一致：用量记录、ops 抓取、日志内容不变，不新增逐请求日志；
  - 每一项都能单独关掉，关掉后走原代码。
- 上游文件里只加单行接入点或函数头几行，带 `[kong]` 注释；规则判定调用上游自己的函数，不在 fork 里复制规则。
- 不追求更少：流水线上必须存在的版本（入站、最终出站）照留，只去掉多余的复制与多余的保活。

## 2. 各项设计

### 2.1 请求视图的字段不拖住请求体（存活 −1 份）

**现状**：`newOpenAIRequestView` 用 `parseRawJSONView` 零拷贝解析，取出的 `Model`、`PromptCacheKey`、
`PreviousResponseID`、`ServiceTier`、`ReasoningEffort` 是请求体的子串。对入站版本无所谓，它本来就活到请求结束；
但 `Forward` 在 reasoning 回放归一化、item id 清洗改写之后都会对新版本重建视图，把字段赋给 `reqModel`、
`promptCacheKey`、`originalModel`，这些字符串传进透传、写进结果，于是中间版本整块保活。

**做法**：`newOpenAIRequestView` 返回前把这五个字符串换成独立的副本（`strings.Clone`），每次只多几十字节的分配。
视图自己的 `body` 字段不动：视图是局部值，没有被长期持有（离线实验里清空 `Forward` 的 `requestView` 与几个请求体
变量都没有效果，复制这几个字符串才把那份副本放掉）。

**接入**：`newOpenAIRequestView` 的 `return view` 前一行。

### 2.2 出站 zstd：流式压缩、一次分配、上传完就放掉（存活 −1 份，分配 −2 份）

只改 fork 自己的 `kong_openai_request_zstd.go`。

- **不再复制明文**。现在 `kongZstdReadBody` 经 `GetBody` 读出一整份明文，只为三件事：判长度、判是不是 JSON、交给
  编码器。改为：长度用 `ContentLength`（我们的出站请求都由字节切片构造，长度已知；未知时走原来的读法）；从
  `GetBody` 的 reader 读出开头的空白与第一个非空白字节判 JSON，连同其余部分直接交给编码器（`io.MultiReader` +
  `Encoder.ReadFrom`）。被拒后的明文重发改用压缩前保存的原 `GetBody` 与 `ContentLength`，不再需要那份明文。
  klauspost 的编码器按块大小切块，与写入的切分方式无关，压缩结果逐字节不变（由对照测试守住，§4）。
- **输出一次分配**。现在按明文一半预留、压不动时扩容，扩容后的整块容量随正文一起保活。改为按上界一次分配：
  不可压缩的数据按原样存进块里，输出最多是明文长度加每块（最大 128 KiB）3 字节块头、帧头，以及 `Flush` 之后
  `Close` 写的 3 字节结尾空块；取保守的"明文长度 + 明文长度 / 256 + 64"，保证不扩容。可压缩的请求会多占
  一段容量，但正文上传完就放掉（下一条），不随流式保活，所以不另做"暂存池 + 精确长度复制"。
- **上传完就放掉**。出站正文由一个持有者统一提供：
  - `Body` 与每次 `GetBody` 返回的 reader 各自持有正文切片，读到 EOF 或被 Close 时丢掉引用；`Read` 与 `Close`
    可能来自不同协程（net/http 的约定），用锁保护；
  - `kongSendOpenAIUpstreamCompressed` 决定返回某个响应之后（不再需要明文重发时）释放持有者，此后 `GetBody`
    返回错误；已经交给 transport 的 reader 不受影响，照常读完。
  - 所以正文在"上传完成（reader 读到 EOF 或被 Close）且持有者已释放"之后才不再被引用。HTTP/1.1 的 transport
    并发地写请求与读响应，HTTP/2 在拿到 2xx 响应头之后也可能还在发正文，`Do` 返回时正文未必已经发完，这时它照常
    发完再放掉。

  `GetBody` 在 `Do` 返回之后不会再被调用：net/http 的连接级重试与重定向（307 / 308 的重放）都发生在 `Do` 返回
  之前；`HTTPUpstream` 自己唯一重读 `GetBody` 的地方是 Grok 的回退，不经过这里；票据观测在压缩之前记下的是明文的
  `GetBody`，指向最终版本，那一份本来就活到请求结束；透传的各种重试（被拒字段、agent task、compact 回退）都从
  `body` 重新构造请求，不复用旧请求。

**接入**：无新增。开关见 §3；`KONG_OPENAI_REQUEST_ZSTD=false` 照旧整体关闭压缩。

### 2.3 入站 zstd：不拼接、复用解码器、按帧头预分配（分配 −4 份，存活去掉扩容富余）

**现状**（`httputil.ReadRequestBodyWithPrealloc`）：按 `Content-Length` 预分配，但单块上限 1 MiB，压缩体超过
1 MiB 就读成两块再拼成一份；`decompressRequestBody` 每次 `zstd.NewReader`（默认并发 = min(4, GOMAXPROCS)，流式
解码要起协程、分配块缓冲），再 `io.ReadAll(io.LimitReader(dec, 64 MiB))` 从 512 字节开始扩容。

**做法**：只接管 `Content-Encoding` 为 `zstd` 的请求，其余照旧。

- 读块的方式与上游相同（同样的初始容量与 1 MiB 上限、同样的错误原样返回），只是不拼接，把各块用
  `io.MultiReader` 交给解码器。
- 解码器从池里取：选项与 `zstd.NewReader` 的默认相同，只是并发设为 1（同步解码，不起协程），并用
  `WithDecoderMaxMemory` 把帧窗口限在 8 MiB；用完 `Reset(nil)` 放掉对输入的引用再放回池。解码器的历史缓冲按
  窗口分配（约窗口 + 1 MiB），`Reset` 不会缩小，所以限的是窗口而不是输出长度：声明大窗口却只解出几个字节的帧，
  照样会撑大缓冲。这个上限对每一帧都检查，也覆盖单段帧（它的窗口就是原始大小）；超限的帧在帧头就报错，按下一条
  交回原代码，所以超过 8 MiB 的单段帧照旧能解，只是不走快路径。
- 输出缓冲：第一个帧头带原始大小时，预分配"原始大小与压缩体长度 × 32 中较小者，再 + 1"（多 1 字节是为了读到
  EOF 时不必扩容），不超过 64 MiB；帧头里的原始大小没有经过验证，只用作上限之内的提示，十几字节的输入不会因为
  声明了大尺寸就触发大分配。没有原始大小时从 512 字节开始。读取循环照抄 `io.ReadAll`，外面同样套 64 MiB 的
  `LimitReader`，超出预分配就照常扩容。
- **结果与原来一致**：同步与异步解码只在两种情况下可能交出不同的结果：解码出错（异步路径可能先交出损坏块里的
  数据），以及输出达到 64 MiB 截断线（异步路径交出截断线之前的数据就停了，后面的校验错误不会被读到；同步路径
  可能先报出那个错误）。所以快路径**一旦出错，或输出达到 64 MiB**，就把各块拼起来交给原来的
  `decompressRequestBody` 重新解一遍，返回它的结果，错误与截断由原代码决定。代价只落在出错与超大请求上。
- 请求头的清理（删 `Content-Encoding`、`Content-Length`，改 `ContentLength`）照旧；空请求体、读不出完整帧头，也
  拼接后交给原来的 `decompressRequestBody`。

**接入**：`ReadRequestBodyWithPrealloc` 里 `PrereadBody` 判断之后一行。`httputil` 不能引用 `service`，开关由
`service` 在包初始化时注入（§3）。

### 2.4 item id 清洗：一次拼出结果（分配 −2 份，线上更多）

**现状**：判定为要清洗时，对每个要删 id 的项做 `[]byte(item.raw)`、`sjson.DeleteBytes`、`string(...)` 三次复制，
最后 `replaceOpenAIRawInput` 再拼一份整体。线上工具输出项占了请求体的大头，所以是整体复制四遍。

**做法**：快速路径的判否门（`kongInputItemIDsClean`）之后，加一个只做"怎么拼"的快路径。它只处理"只需要删 id"
的请求：

- 遍历 input 各项，取值与判定照抄原函数：同样的 gjson 遍历与取值，调用上游的
  `shouldStripOpenAIResponsesInputItemID` 与 `shouldStripOpenAIResponsesNonPairCallID`；
- **任何一项需要删 `call_id`，整个请求交回原函数**（`message` / `reasoning` / `image_generation_call` 带
  `call_id`，少见）。这样不必处理两次删除的先后与区间重叠；
- 一项也不需要清洗时，与原函数一样原样返回请求体，不重新拼接；
- 对要删 id 的项算出删除区间，不复制。区间照抄 sjson 的删除规则：以 `gjson.Get(项, "id")` 定位（sjson 删除时也
  这样定位），从值的起点往回找，前面是逗号就连逗号带键值一起删，前面是 `{` 就改为删掉后面的逗号（跳过中间的
  空白），即移植 `deleteTailItem` 与它后面那几行；
- 最后按 `replaceOpenAIRawInput` 的格式一次分配、拼出整体：input 前的原文、`[`、各项（原样或去掉删除区间）用 `,`
  连接、`]`、input 后的原文。

结果与原函数逐字节相同，由差分与随机变异测试守住（§4）。

**接入**：`sanitizeOpenAIResponsesInputItemIDs` 函数头，紧接在已有的判否门之后一行。

### 2.5 身份改写：两次改写共用一份副本（分配 −1 份）

**现状**：`applyCodexAccountIdentityClientMetadataRaw` 先 `sjson.SetRawBytes` 改 `client_metadata`，再
`sjson.SetBytes` 改 `prompt_cache_key`，两次各复制一整份。线上两处每个请求都会改写。

**做法**：只针对这两个已知的顶层字段，身份的计算仍由上游函数完成，fork 只接管"怎么写回去"：

- 第一处（`client_metadata`）：按 sjson 替换已有顶层键的方式拼出新切片——sjson 就是把 `gjson.Get(全文, 键)`
  定位到的那段值换掉——并多留 256 字节容量；
- 第二处（`prompt_cache_key`）：只有当函数里的局部变量 `changed` 为真（第一处确实改写过，`next` 是本函数刚分配、
  还没交给任何人的私有副本）且容量够时，才在 `next` 上原地挪动尾部完成替换；`changed` 为假时 `next` 就是传进来
  的请求体，照常走 sjson。不靠容量或登记表猜测所有权；
- 新值按 sjson 的 `appendStringify` 写成 JSON 字符串时需要转义（含控制字符、非 ASCII、`"`、`\`），或者键不存在时，
  交给 sjson 本身。拼接用的就是 sjson 自己的定位方式（同一个 `gjson.Get`），重复键、转义键也照样一致。线上写回的
  是带命名空间前缀的 UUID，不需要转义。

原地修改只发生在本函数刚分配的切片上，不违反"请求体不被原地修改"的不变量（那条约束的是流水线上传递的版本）。

**接入**：函数里两处 sjson 调用各换成同一行的 fork 调用（第二处多传一个 `changed`）。

## 3. 开关

沿用快速路径的开关：`KONG_OPENAI_BODY_FASTPATH`（总开关）与 `KONG_OPENAI_BODY_FASTPATH_OFF`（按名字关闭）。新增
五个名字：

| 名字 | 项 |
|---|---|
| `view_strings` | §2.1 |
| `zstd_out` | §2.2（关掉后回到原来的读法与输出缓冲，压缩照旧） |
| `zstd_in` | §2.3 |
| `item_ids_splice` | §2.4 |
| `identity_splice` | §2.5 |

`httputil` 里留一个默认为开的函数变量，`service` 包初始化时把它设成读取 `zstd_in` 的开关。

## 4. 测试与验证

- **语料**：`kongcorpus.Options` 增加两项，零值时生成的字节与现在完全一样（不多消耗随机数）：
  - `ItemIDs`：历史项带线上形态的 id（`rs_`、`msg_`、`fc_`、`fco_`、`ctc_`、`ctco_`），工具输出项会被清洗；
  - `Images`、`ImageBytes`：夹几张 base64 图片，一半在用户消息里，一半在 view_image 的工具输出里。
- **端到端对照**：在快速路径的对照集里加用例：带 id、带图片、入站 zstd（带原始大小的帧、不带原始大小的流式帧、
  多帧、截断、帧后跟垃圾、空体）、组合，以及叠加 failover、400 重试、压缩被拒；还有一组 id 在边角位置的项（在第
  一个、在最后一个、键值周围有空白、转义的键、重复的 id、同时带 `call_id`）。**先在改动前的代码上重录对照基准**，
  再实施；快速路径全关、全开、逐项关各跑一遍，出站字节（压缩前后）、客户端响应、用量行、gin 上下文、日志都必须
  与基准一致。
- **逐项差分**：
  - §2.4：原函数与快路径在语料、手写边角与随机变异上比较完整字节、`changed` 与错误；边角含合法 / 缺失 / 非字符串
    的 `id`、`call_id:null`、重复与转义的键、`id` 与 `call_id` 的各种排列，无改动时必须返回原切片；
  - §2.5：与原来两次 sjson 调用比较；覆盖只改 `prompt_cache_key` 且原请求体容量富余（断言输入不被修改）、第一处
    交回 sjson、容量不够、重复键、转义键、需要转义的新值；
  - §2.3：与 `decompressRequestBody` 比较返回值、错误文本与请求头的变化：带 / 不带原始大小、多帧、超过 64 MiB、
    截断、损坏的块与校验和、帧后跟垃圾、空体、比帧头还短；在 `GOMAXPROCS=1` 与 `2` 下各跑一遍；读取出错（含
    `MaxBytesError`）原样返回；另测"帧头声明 64 MiB、正文只有几个字节"时的分配量；窗口超限的输入（声明大窗口却
    只解出两个字节、超过 8 MiB 的单段帧、第二帧才声明大窗口）交回原代码、结果一致，之后池里不留下大块，接着处理
    小请求照常；
  - §2.2：压缩结果与原来的"整份写入 + Flush + Close"逐字节相同（含块边界附近的长度与不可压缩数据，且输出缓冲不
    扩容）；被拒重发拿到的是原明文；释放之后 `GetBody` 报错、
    已有 reader 照常读完，读到 EOF 或 Close 之后不再引用切片；用真实的 HTTP/1.1 与 HTTP/2 服务端验证"先回响应头、
    后读正文"与客户端取消，以及 307 / 308 重放。`Read` 与 `Close` 并发的安全性用 CI 的 `kong-race.yml` 跑。
- **内存测量**，分三种口径分别报告，不混在一起：
  - **请求持有的存活**：沿用上面的离线复现（每次测量前强制 GC，池子会被清空，所以量到的正是请求自己引用的版本），
    断言开关全开时不超过 2.3 份；
  - **预热后的分配**：先处理几个请求预热，再连续处理一批同样的请求，取每个请求的平均分配，断言开关全开时不超过
    7 份（实测 5.6–6.0 份，全量测试时会混进少量其他包与后台协程的分配）。从预热到计量结束都暂停 GC：测试进程的
    存活堆很小，GC 每分配几 MB 就跑一轮，`sync.Pool` 隔两轮就被清空，不暂停就量成了冷启动；线上 GC 隔几秒一轮，
    池子是热的。冷启动（池子清空后的第一个请求）单独报告；
  - **池子的保留**：先清空各个池、暂停 GC，让一批请求同时起跑；跑完一轮 GC 后（池里的对象还在）与再两轮 GC 后
    （已清空）的堆之差。只打印，不断言；
  - 三种口径都在同一份语料、同样的预热条件下把开关全关与全开各量一遍。
- 本地闸门照 `DEV-WORKFLOW.md`：`go vet -tags=unit ./internal/...`、`go test -tags=unit ./...`。

## 5. 上线与观测

- **离线实测**（§4 的三种口径，同一语料）：

  | 口径 | 全关 | 全开 |
  |---|---|---|
  | 流式期间请求持有的存活 | 4.04 份 | 2.04 份 |
  | 预热后每请求分配 | 38.5–38.8 份 | 5.1–5.9 份 |
  | 冷启动的那个请求 | 40.3 份 | 9.1 份 |
  | 并发 4 个之后池子保留 | 25.9 MB | 42.3 MB |

  池子多出的约 16 MB 是 4 个入站解码器的历史缓冲（每个约"原始大小 + 1 MiB"，窗口上限 8 MiB）；它随并发的解压数
  增长、不随在途请求数增长，两轮 GC 不用就释放。

- **线上预期**：每个在途请求自己持有的存活少 2 份（上传期间多一份压缩正文）。按 §0 的负载（约 40 个在途、平均
  2.7 MB），存活堆约少 200 MB；线上每请求的大块分配约 16 份（约 43 MB），预计降到 5–6 份，GC 频率随之下降。
- **怎么量**：升级前后各用同一套 bpftrace 量一分钟：标记后的存活堆、GC 次数、大块分配总量与分配点；按日志里
  `kong zstd: 周期计数` 的平均请求体大小与同期的在途数归一后比较。
- **回退**：先用 `KONG_OPENAI_BODY_FASTPATH_OFF` 关掉可疑的那一项（改环境变量要重建容器）；仍有问题换回上一版镜像。
  各项都只改内存的使用方式，不改数据，没有迁移。

## 6. 不做的事

| 方案 | 不做的原因 |
|---|---|
| 不删 `ctco_` 前缀的 id | 改的是发往上游的内容，上游有意这样做 |
| 关掉出站 zstd | 省下的内存远不抵上行流量的增长；而且 §2.2 之后它只占一份压缩体的分配 |
| 放掉最终版本或入站版本 | 流式之后的 compact 回退、failover 换号都要用 |
| 入站改用 `DecodeAll` | 64 MiB 截断与多帧的语义不同，要另写一套判定 |
| 入站保留默认的并发解码 | 每个流都要起协程、分配块缓冲，复用解码器也省不掉；同步解码加上"出错或到截断线交回原代码"就能保持结果一致 |
| 出站用暂存池再复制精确长度 | 正文上传完就放掉，按上界一次分配多出的容量不随流式保活，不值得多一个池 |
| 在线上加 pprof 端点 | bpftrace 已经能量出存活与分配点；内置端点扩大改动面 |
| 调 `GOGC` | 存活降下来之后再看是否需要 |

## 7. 改动面

| 位置 | 改动 |
|---|---|
| `service/openai_gateway_request_body.go` | `newOpenAIRequestView` 返回前 1 行 |
| `service/openai_responses_item_id.go` | `sanitizeOpenAIResponsesInputItemIDs` 判否门之后 1 行 |
| `service/openai_codex_account_identity.go` | 两处 sjson 调用的同行替换 |
| `pkg/httputil/body.go` | `ReadRequestBodyWithPrealloc` 里 1 行 |
| `service/kong_openai_request_zstd.go` | §2.2（fork 自己的文件） |
| 新文件 | `service/kong_openai_body_memory.go`（§2.1、§2.4、§2.5）、`pkg/httputil/kong_request_zstd.go`（§2.3）、开关的
  新名字、语料选项、测试 |
| 文档 | 本文；`FORK-NOTES.md` 快速路径与 zstd 两节补上新的接入点与复核项 |
