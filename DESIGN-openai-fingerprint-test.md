# 账号指纹测试：主动看一个 OpenAI 账号此刻被服务的模型

**状态**：已实现，无配置项。仓库组织与发布流程见 `FORK-NOTES.md`；codex 票的被动观测见 `DESIGN-codex-ticket.md`。

管理员在 `/admin/accounts` 上对单个账号发起，经账号自己的代理、用账号自己的凭据发 1～3 份挑战（**不带任何
票**），用指纹库归因，并对照上游回报的模型给出结论。每一份的原始证据与整次的结论都落库长期留存，事后只凭
库里的记录就能解释当时为什么得出那个结论。

## 0. 结论能说明什么

结果回答的是"这几次挑战观察到了什么"，是相对可比的信号，**不是绝对档位判定**：

- 闭集归因：库外的模型会被归到最像的候选上。
- 校准库取自不带 instructions 的环境，而 codex 端点的请求带着默认 instructions。
- 上游回报的模型名是上游的声明，与指纹是两类独立证据。
- 不带票的各份请求不保证被路由到同一个模型，所以只有各份都指向同一个候选时才下结论（§2 规则 3、4）。

这几条在弹窗里写明。

## 1. 范围

- **账号**：走 codex 协议的 OpenAI 账号（OAuth 与 setup token）。两类除外，前后端都拒绝：影子账号没有自己的
  凭据，测它的母账号即可；Agent Identity 账号不持有 access token，每个请求都要现场签名，而签名流程会登记或恢复
  task、改动账号状态。
- **目标模型与归因候选分开**：指纹库（`kong_data/unified_bank.json`）有 16 个候选，8 个 GPT、8 个 Claude。
  可选的目标是其中 `family = gpt` 的候选；Claude 只是归因的干扰项，发不到 codex 端点。服务端同样校验，不在其中
  的目标在发出任何请求之前就拒绝。参与归因的仍是全部 16 个。
- **执行环境在开始时固定**：开始时读一次账号（凭据、代理），这次测试的每一份都用它；中途改了账号或代理不影响
  进行中的测试，记录里写的是开始时的代理。凭据就是账号上存着的 access token，与"测试连接"同口径，**不经
  token provider**：那条路会刷新凭据、读实时缓存，凭据不可用时还会把账号置为错误状态。代理解析失败时直接结束，**不退回直连**（那会把请求从服务器的真实
  IP 发出去）。
- **与"测试连接"一致**：不占账号并发槽，不写用量行，不计费，不改变账号的调度状态。

## 2. 执行与判定

**每一份挑战**：三条挑战题按顺序取用（`kong_fingerprint_challenges.go`，与建库时逐字一致）。

1. 经账号的代理向 codex responses 端点发挑战，读回完整正文与上游回报的模型名，单份超时 180 秒。
2. 解析回答里的数字序列，与此前各份有效回答一起归因：各份分别评分、在分数层面平均，按有效份数选校准温度。
3. 这一份立即写入 `kong_fingerprint_probes`，然后推一条进度事件。

**判定规则**：每一份完成后按顺序检查，命中即停；三份用完仍未命中就停在"判不准"。

| 顺序 | 条件 | 执行结果 | 模型结论 |
|---|---|---|---|
| 1 | 这一份发送失败、上游返回 4xx / 5xx、超时，或管理员已取消 | 失败 / 已取消 | 无 |
| 2 | 上游回报了模型名，且与目标对不上 | 完成 | 不一致 |
| 3 | 累计归因最高者的概率 ≥ 0.9，且各份有效回答的单份归因都指向它 | 完成 | 最高者是目标 → 一致；否则不一致 |
| 4 | 累计归因最高者的概率 ≥ 0.9，但各份的单份归因不同 | 完成 | 判不准 |
| 5 | 三份用完仍未命中以上 | 完成 | 判不准 |

- **模型名比较**：大小写无关地相等，或回报值去掉 `目标-` 前缀后以数字开头（快照与日期后缀，实测有
  `gpt-5.4-mini` → `gpt-5.4-mini-2026-03-17`）。纯前缀匹配会让 `gpt-6` 吞掉 `gpt-6-astra`，不用。
- 上游没回报模型名不算对不上，只是少一项证据。
- 2xx 之后读流出错（截断）、数字太少、含非 ASCII 数字、打分失败的回答：算用掉一份，不计入归因，照常留档。
  截断的那一份回报的模型照样作数。
- 执行结果（完成 / 已取消 / 失败）与模型结论（一致 / 不一致 / 判不准）分开表达，界面上两者都显示。

**结束原因**（`end_reason`）：`reported_model_mismatch`、`fingerprint_confident`、`parts_disagree`、
`parts_exhausted`、`client_cancelled`、`proxy_unavailable`、`upstream_status`、`upstream_error`、`timeout`。

**取消与期限**：管理员关掉弹窗（连接断开）即取消，正在进行的那一份立即中止，不再发后续的份，已完成的份已经
落库；读代理时被取消同样记为取消。取消只打断还没完成的那一份：一份挑战已经完整拿到之后才断开的，这一份照常
参与判定，命中规则就照常给出结论（证据是完整的，记录里是"完成"）。整次测试没有单独的总期限：最多 3 份、每份 180 秒，加上每次落库 10 秒的上限。

**同一账号同时只允许一个测试**：进程内按账号互斥，第二个请求直接返回 409。每份挑战都是一次真实的上游请求，
重复点击会白白消耗额度。互斥在这次测试彻底结束（包括最后的落库）时释放。

## 3. 留存

**落库用独立的 context**（10 秒期限）：管理员断开时已经取得的证据照样留下。落库失败不改变结论，但结论里带上
`persist_error`，提示库里的记录不全。

`kong_fingerprint_probes`（迁移 900 建表，906 加 `reported_model` 列），一份一行：

| 列 | 内容 |
|---|---|
| `verification_id`、`part_index` | 所属测试与第几份 |
| `account_id`、`target_model`、`verify_egress` | 账号、目标、出口（`proxy:<id>` 或 `direct`） |
| `challenge_id`、`digits`、`digit_count` | 挑战题与解析出的完整数字序列（归因的完整输入，换算法或校准表后能重算） |
| `scores`、`part_attribution` | 这一份对各模型的分数（按模型名存）与它自己最像的模型 |
| `cum_probability`、`temperature_tier` | 累计到这一份的最高者概率与所用的校准档 |
| `library_version` | 当时所用指纹库的版本与内容摘要 |
| `parse_valid`、`counted_in_average`、`invalid_reason` | 这一份是否可用、是否计入、不计入的原因 |
| `latency_ms`、`output_tokens`、`reported_model` | 耗时、输出 token 数、上游回报的模型 |

旧版本验票时写的 `ticket_*`、`capture_egress`、`idle_seconds` 等列，新记录一律为空。

`kong_fingerprint_tests`（迁移 906），一次测试一行：`verification_id`、`account_id`、`target_model`、开始时的
`proxy_id`、`started_at` / `finished_at`、`execution`、`end_reason`、`verdict`、`rule_version`。`execution` 为空
表示进程在测试中途退出、没来得及收尾。**改动 §2 的判定顺序或条件时，`rule_version` 要加一**，否则新旧结论
混在一起分不清。

## 4. 管理接口与前端

- `GET /api/v1/admin/kong/fingerprint/targets`：可选的目标模型。
- `POST /api/v1/admin/accounts/:id/kong-fingerprint-test`，请求体 `{"model": "..."}`。校验失败按普通 JSON 错误
  返回（目标不合法 400、账号不合格 404、已有测试在进行 409、指纹库不可用 503）；通过后以 SSE 推送
  `started`、每份的 `part_started` 与 `part`、最后的 `done`（带结论）。单份挑战 p95 约 80 秒，普通请求会撞上
  反向代理的超时，所以用 SSE，并**每 15 秒发一条保活注释**，让一份挑战运行期间也有下行数据；响应头带
  `X-Accel-Buffering: no`。部署前要核对反向代理对 SSE 的空闲超时、总时长上限与缓冲设置。
- 前端在 `frontend/src/features/openai-fingerprint-test/`：账号行菜单里"测试连接"下方的菜单项，以及挂在账号页
  上的弹窗宿主（菜单关闭时菜单组件随之卸载，弹窗不能挂在菜单里）。弹窗逐份显示进度与证据，最后分别显示
  执行结果与模型结论，并写明 §0 的局限。

## 5. 参数与实现位置

置信阈值 0.9、最多 3 份、单份超时 180 秒、落库期限 10 秒、保活间隔 15 秒都是代码常量，不设配置项。指纹库
可以用环境变量 `KONG_FINGERPRINT_BANK` 指向外部文件覆盖（换资料的步骤见 `kong_data/SOURCE.md`）；加载失败时
指纹测试整体不可用，接口返回 503，其余功能不受影响。

| 位置 | 作用 |
|---|---|
| `service/kong_fingerprint_tester.go` | 校验、互斥、逐份执行、判定、落库 |
| `service/kong_fingerprint_upstream.go` | 经代理发挑战、读 SSE 正文与回报的模型 |
| `service/kong_fingerprint.go`、`kong_fingerprint_bank.go`、`kong_fingerprint_challenges.go` | ModelTrace 归因算法的 Go 移植、指纹库、挑战题（由 golden file 回归测试锁住） |
| `handler/admin/kong_fingerprint_handler.go`、`server/routes/admin_kong_fingerprint.go` | 管理接口 |
| `repository/kong_codex_ticket_repo.go` | 落库 |
