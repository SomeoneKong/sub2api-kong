# 按到期时间指定重置卡的用卡端点

**状态**：已实现。仓库组织与发布流程见 `FORK-NOTES.md`。调用方是账号计划的执行端边车（工作区 `plan-sidecar/`），
执行端的设计见工作区 `DESIGN-account-plan-final.md`。

## 0. 背景

上游的 `POST /api/v1/admin/openai/accounts/:id/reset-quota` 消耗账号**自身到期最早**的那张重置卡，且每次调用自己生成
幂等 id。账号计划的执行端要按卡规则选卡（保留最晚到期的一张、先用订阅前到期的），也要在超时后能安全重试，
这两点无参端点都做不到。服务层已有定向版本 `ResetCreditTargeted(creditID, redeemRequestID)`（自动用卡内部在用），
但上游卡 ID 有意不出服务层（`openAIAutoResetCreditCandidate` 的说明），管理端响应里只有每张卡的到期时间。

## 1. 设计

- 新端点 `POST /api/v1/admin/openai/accounts/:id/kong-reset-quota`，请求体 `{"expires_at": RFC3339, "redeem_request_id": 串}`。
- 服务层 `KongResetCreditByExpiry`：现查一遍卡明细（与 `QueryUsage` 同一条上游查询），把到期时间换成卡 ID，再走
  `resetCredit(..., targeted=true)`。`expires_at` 带小数秒时只认精确相等；是整秒时认到期落在这一秒里的全部卡（调用方
  可能把时间截到了秒），这一秒里多于一张就按歧义拒绝——不因截断落到同一秒的另一张卡上。调用方应原样传卡明细里的到期
  时间串。明细不全、找不到、有歧义、或那张卡没有上游 ID 都返回 409，**不会退而消耗别的卡**。
- `redeem_request_id` 原样交给上游做幂等：调用方超时后用同一个 id 重试不会消耗第二张。id 由调用方按（账号、卡到期、操作）
  稳定生成。重试**拿不回首次的结果**：首次其实已兑换的话，那张卡不再可用，重试按 409 返回；首次没执行完的用卡后处理
  也不会因重试补上。所以超时之后调用方不以重试结果为准，而是按账号的卡数与用量对账，服务是否恢复另判（执行端边车
  把这种用卡记为未决、下周期对账，恢复不了就调管理端的解除限流）。
- 用卡后的处理（解除限流、刷新额度缓存、刷新账号行）与上游 `ResetQuota` 同一个 `RunOpenAIQuotaResetPostProcess`，
  响应形状也相同（`openAIQuotaResetResponse`）。
- 卡 ID 仍不出服务层、不进日志：响应里的 `credit` 去掉 `id`，其余元数据照旧（上游无参端点的响应会带它）。

## 2. 落点

| 文件 | 内容 |
|---|---|
| `backend/internal/service/kong_openai_quota_reset_by_expiry.go` | `KongResetCreditByExpiry`、`kongPickResetCreditByExpiry`、`kongScrubResetCreditID` |
| `backend/internal/handler/admin/kong_openai_reset_credit.go` | `KongResetQuotaByExpiry`；通过接口断言取服务层方法，不改上游的 `openAIQuotaService` 接口 |
| `backend/internal/server/routes/admin_kong_plan.go` | `registerKongPlanRoutes`；`admin.go` 里一行调用 |
| 同名 `*_test.go` | 匹配规则与卡 ID 脱敏；handler 的参数校验、能力缺失、后处理与上游一致 |

碰上游文件的只有 `routes/admin.go` 的一行追加。

## 3. 不做的

- 不给前端加入口：这是给执行端用的，人工用卡仍走面板里的无参端点。
- 不在管理端暴露卡 ID：调用方用到期时间就够了。
