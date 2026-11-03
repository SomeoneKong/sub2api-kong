# API Key 项目统计页

新增一个页面，按 API key 查看用量：每个 key 用了哪些模型、每个模型里各档位（标准 / Fast 等）占多少，以及 token 用量曲线。
本部署里一个 key 对应一个项目或一台设备，所以页面叫"项目统计"。

## 1. 现状与依据

### 1.1 已有能力为什么不够

| 已有 | 能给什么 | 缺什么 |
|---|---|---|
| 用量页 `views/user/UsageView.vue` 的"API 密钥使用分布"（`/usage/dashboard/kong-api-key-stats`） | 各 key 的请求、token、费用合计 | 没有模型与档位 |
| `/usage/dashboard/models?api_key_id=` | 单个 key 的模型分布 | 不拆档位；一次一个 key |
| `/usage/dashboard/trend?api_key_id=` | 单个 key 的 token 趋势 | 走 `parseUserUsageFilters` 里的 `apiKeyService.GetByID`，而它用 `activeQuery` 不查软删除的 key，已删除的 key 会返回 404 |

### 1.2 线上数据（2026-11-03，`usage_logs` 全部 31 天，约 199 万行）

- 1 个用户、13 个有用量的 key，其中 8 个名字带 `[project]` 前缀，其余是设备或测试用途。
- `service_tier` 只有两种取值：NULL 197.8 万行，`priority` 7,944 行（占 token 的 0.2%）。
- `priority` 行的来源：`pc-main-自有账号` 6,073 行（10-29 起），`[project] math arnold-t2` 1,872 行（只在 10-30）。
- 全部 `priority` 行的 `inbound_endpoint` 都是 `/v1/responses`，`openai_ws_mode = false`，主路径确认有记录。WS 路径线上只有 30 行且全是 NULL，
  这条路径只读过代码（`resolvedOpenAIUpstreamServiceTierFromObserver`），没有线上样本。
- token 构成：缓存读 96%，输入 3.5%，输出 0.5%，缓存写约 0。
- `/v1/alpha/search` 有 8.7 万行，token 为 0，费用共 $868（每次 $0.01），`model` 记成调用它的模型。
- `requested_model` 与 `model` 在全部行上相同。

## 2. 范围

- **做**：一个页面，一个查询端点；时间范围选择；总览表；每个 key 的模型 × 档位表和 token 曲线。
- **不做**：
  - 超过明细保留期的累计用量。明细只留 31 天，页面注明，不另建汇总表。
  - 管理员看其他用户的 key。现在所有 key 都属于同一个用户，管理员用量页仍可按用户和 key 筛选明细。
  - 按 `[project]` 前缀过滤。线上大部分 Fast 用量来自不带前缀的 key。
  - 按模型或档位拆分曲线、自动刷新、导出。

## 3. 口径

| 项 | 取法 | 说明 |
|---|---|---|
| 范围 | 当前登录用户的 `usage_logs`，按 `created_at` | 与用量页统计卡片相同的筛选写法，同一范围内合计对得上 |
| 模型 | `resolveModelDimensionExpression(ModelSourceRequested)` | 用户侧现有接口只支持请求模型 |
| 档位 | 用量记录的 `service_tier`，归类见下表 | 分类与 `serviceTierCostMultiplier` 一致，比较前转小写并去空白 |
| Token 合计 | `input + output + cache_creation + cache_read` | 与统计卡片一致，因此以缓存读为主 |
| 请求数 | `COUNT(*)`，即用量记录数 | 含中断后仍记了用量的请求，以及 `/v1/alpha/search` 这类 0 token 的调用；不等于全部调用次数或成功次数。页面加一句说明 |
| 费用 | 实际 `actual_cost`、标准 `total_cost` | 与现有分布图相同的两列 |
| 时间桶 | `created_at` 换算到有效时区后按小时截断 | 有效时区与日期筛选相同：请求的 `timezone` 合法就用它，否则用服务器配置的时区（`timezone.Name()`） |

| `service_tier`（归一化后） | 档位 | 页面显示 |
|---|---|---|
| `priority`、`fast` | `fast` | Fast |
| `ultrafast` | `ultrafast` | Ultrafast |
| `flex` | `flex` | Flex |
| NULL、空串、`default`、`auto`、`scale`、其他 | `standard` | 标准 |

**档位的含义**：页面统计的是用量记录里的档位，只回答"记录上写的是哪一档"，不推断别的。

- 它是网关经策略改写和上游声明调整之后落库的值。上游声明的档位更低时记录会跟着降（`ResolveBillingServiceTier`）；OAuth 账号上游报 `default` 时不采信，
  记录保留发出去的档位（`ResolveOpenAIServiceTierBilling`）。所以记为 Fast 不能证明上游实际提速。
- 它也不等于实际收费：免费 Fast 保留 `priority` 与 Fast 的 `total_cost`，`actual_cost` 按标准档重算。实际扣费看费用列。
- 标准档包含未指定档位的请求（NULL、未知值），不代表上游确认走了标准档。

页面说明写成白话："Fast 按用量记录统计，实际扣费看费用列，不能据此确认上游是否提速。标准含未指定档位的请求。"

**缺值与可查范围**：响应带一个服务端给出的可查范围（第 4 节 `coverage`）。范围内没有记录的时段确实没有用量，按 0 画；范围之外的日期明细已清理或还没发生，不画，也不说成没有用量。
加载失败与没有数据是两种状态，分开显示（5.4 节）。

## 4. 后端

全部放在新文件里，查询是仓库的可选方法，经接口断言取得，不加进上游的 `UsageLogRepository` 接口（与 `kong_usage_api_key_stats.go` 相同的做法）。

| 文件 | 内容 |
|---|---|
| `repository/kong_usage_api_key_projects.go` | `KongGetAPIKeyProjectStats` 查询 |
| `service/kong_usage_api_key_projects.go` | 结果结构（带 `json` tag）、可选接口、`UsageService.KongGetAPIKeyProjectStats` |
| `handler/kong_usage_api_key_projects.go` | `UsageHandler.KongDashboardAPIKeyProjects`、有效时区解析 |

**端点**：`GET /api/v1/usage/dashboard/kong-api-key-projects`，参数与 `/usage/stats` 相同，走 `parseUserUsageFilters(c, true)`。前端只传 `start_date`、`end_date`
（`timezone` 由 `apiClient` 自动附加），不传 `api_key_id`，所以不经过 `GetByID`，已删除的 key 不受影响。挂在 `usage` 路由组下。

**有效时区**：`parseUserUsageFilters` 不返回时区，handler 用同样的规则解析一次：`time.LoadLocation` 接受且不是 `Local` 就用请求的值，否则用 `timezone.Name()`。
时区名作为绑定参数传给 SQL。Go 接受而 PostgreSQL 不认识的名字会让查询报错，页面显示加载失败，不会返回错的桶。

**用量查询**：一条 SQL，筛选条件复用 `kongUsageLogFilterConditions`，`$tz` 是追加在筛选参数之后的绑定参数。

```sql
WITH r AS MATERIALIZED (
  SELECT api_key_id, requested_model, model, service_tier,
         date_trunc('hour', created_at AT TIME ZONE $tz) AS hour,
         COUNT(*) AS requests,
         COALESCE(SUM(input_tokens), 0) AS input_tokens,
         COALESCE(SUM(output_tokens), 0) AS output_tokens,
         COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
         COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
         COALESCE(SUM(total_cost), 0) AS cost,
         COALESCE(SUM(actual_cost), 0) AS actual_cost
  FROM usage_logs
  <筛选条件>
  GROUP BY 1, 2, 3, 4, 5
)
SELECT r.api_key_id, COALESCE(k.name, ''), COALESCE(k.deleted_at IS NOT NULL, FALSE),
       <请求模型表达式，别名 r> AS model,
       CASE LOWER(TRIM(COALESCE(r.service_tier, '')))
         WHEN 'priority' THEN 'fast' WHEN 'fast' THEN 'fast'
         WHEN 'ultrafast' THEN 'ultrafast' WHEN 'flex' THEN 'flex'
         ELSE 'standard' END AS tier,
       TO_CHAR(r.hour, 'YYYY-MM-DD HH24:00') AS bucket,
       SUM(r.requests), SUM(r.input_tokens), SUM(r.output_tokens),
       SUM(r.cache_creation_tokens), SUM(r.cache_read_tokens), SUM(r.cost), SUM(r.actual_cost)
FROM r
LEFT JOIN api_keys k ON k.id = r.api_key_id
GROUP BY 1, 2, 3, 4, 5, 6
ORDER BY 1, 6, 4, 5
```

分两层有两个目的：内层只按原始列分组，避开逐行的字符串运算；`MATERIALIZED` 让内层独立规划。不写它时外层的分组需求会传进内层，PostgreSQL 先把全部明细排序再聚合，耗时翻倍（第 6 节）。
实现里给这个关键字写注释说明原因，防止被当成多余的删掉。

`LEFT JOIN` 不过滤 `deleted_at`，软删除的 key 仍有名字。总是按小时返回，由前端汇总成天，切换粒度不再请求。

**可查范围**：用量查询之后再查一次，`MIN(created_at)` 走 `idx_usage_logs_created_at`，线上约 10 毫秒。

```sql
SELECT TO_CHAR(GREATEST($start, MIN(created_at)) AT TIME ZONE $tz, 'YYYY-MM-DD HH24:00'),
       TO_CHAR(LEAST($end - interval '1 microsecond', NOW()) AT TIME ZONE $tz, 'YYYY-MM-DD HH24:00')
FROM usage_logs
```

- 起点是所选起点与全表最早一行中较晚的一个。保留清理是全表的，所以不按用户过滤。空表时 `MIN` 为 NULL，`GREATEST` 忽略 NULL，结果是所选起点。
- 终点是所选终点与当前时刻中较早的一个。放在用量查询之后执行，所以不会早于已返回的最后一个桶。
- 起点晚于终点，说明所选日期完全不在可查范围内（已清理，或是未来的日期）。

**响应**：key 信息只出现一次，由 Go 按首次出现整理。

```json
{
  "coverage": { "start": "2026-10-27 00:00", "end": "2026-11-03 23:00" },
  "api_keys": [{ "api_key_id": 6, "api_key_name": "[project] math-hsft", "deleted": false }],
  "rows": [{
    "api_key_id": 6, "model": "gpt-6.1-sol", "tier": "standard", "bucket": "2026-11-03 14:00",
    "requests": 120, "input_tokens": 1000, "output_tokens": 200,
    "cache_creation_tokens": 0, "cache_read_tokens": 30000, "cost": 1.23, "actual_cost": 0.98
  }]
}
```

没有数据时 `api_keys` 与 `rows` 都是空数组，不是 `null`，`coverage` 仍然返回。

## 5. 前端

放在 `frontend/src/features/api-key-projects/`：

| 文件 | 内容 |
|---|---|
| `api.ts` | 类型、取数函数、`useKongApiKeyProjects` |
| `aggregate.ts` | 纯函数：key 汇总、模型 × 档位表、曲线数据（含补 0 与汇总到天） |
| `ApiKeyProjectsView.vue` | 页面 |

路由 `/key-stats`，`requiresAuth: true`、`requiresAdmin: false`。不放在 `/usage/...` 下，因为侧边栏的 `isActive` 按路径前缀判断，会把"用量"一起高亮。
导航加在 `buildSelfNavItems`，普通用户主菜单和管理员的"我的账户"共用；标记 `hideInSimpleMode: true`，与用量页相同。

文案写在 feature 内，用中文字面量，不加 i18n 键：这个页面只服务本部署。已删除标记复用现有的 `usage.errors.keyDeleted`。

**取数**：`useKongApiKeyProjects` 用 `AbortController`，发新请求和页面卸载时取消旧请求（与 `UsageView` 的列表加载相同），取消不算失败；同时保留序号校验处理竞态。
后端查询用请求的 `context`，取消会传到数据库。`apiClient` 的 30 秒超时够用，不另外调整。

### 5.1 布局

```
API Key 项目统计                        [时间范围：默认近 7 天] [小时/天]
明细只保留 31 天。请求数按用量记录统计，含中断后仍记了用量的请求和搜索等 0 token 的调用。
Fast 按用量记录统计，实际扣费看费用列，不能据此确认上游是否提速。标准含未指定档位的请求。
┌───────────────────────────────────────────────────────────────────┐
│ 名称                  请求   Token   模型                Fast  实际  标准 │
│ ▼ [project] math-hsft  …      …      gpt-6.1-sol 85% …    —    $…   $… │
│   ┌ 模型分布 ─────────────────────────────────────────────────────┐ │
│   │ 模型 / 档位   请求  输入  输出  缓存  合计  占比  实际  标准  │ │
│   │ gpt-6-astra   …     …     …     …     …    ██ 60%  $…   $…   │ │
│   │   标准        …     …     …     …     …    59.5%   $…   $…   │ │
│   │   Fast        …     …     …     …     …    0.5%    $…   $…   │ │
│   └───────────────────────────────────────────────────────────────┘ │
│   ┌ Token 用量曲线（TokenUsageTrend）─────────────────────────────┐ │
│   └───────────────────────────────────────────────────────────────┘ │
│ ▶ [project] math-sfc   …                                          │
└───────────────────────────────────────────────────────────────────┘
```

- 总览按 token 合计降序，相同时按 `api_key_id` 升序。点一行展开，可同时展开多行对比；默认展开第一行。展开不发请求。
- 切换时间范围时重新请求并保留已展开的 key；新范围里没有的 key 不显示。默认展开只在第一次加载时生效。
- 总览的"模型"列用文字写 token 占比前两名，更多的写"等 N 个"。
- "Fast"列是 Fast 与 Ultrafast 的 token 占该 key 合计的比例，列头提示写明包含 Ultrafast：等于 0 显示 `—`，小于 0.1% 显示 `<0.1%`。
- 名字为空时显示 `#<id>`，已删除的 key 打标记。

### 5.2 模型分布

用表格加行内比例条，不用环形图：线上 Fast 只占 0.2%，在环形图上看不见，数字才有用。

- 模型按 token 合计降序。占比的分母是这个 key 的 token 合计，分母为 0（只有搜索调用）时显示 `—`，不出现 NaN。
- 比例条按档位分段着色。
- 只有某个模型存在非标准档位时才列档位子行，顺序为标准、Fast、Ultrafast、Flex。
- "缓存"列是缓存读加缓存写。

### 5.3 Token 曲线

直接复用 `components/charts/TokenUsageTrend.vue`，不改上游组件。它按输入、输出、缓存写、缓存读、缓存命中率画线，点图例可隐藏某条线，纵轴随之缩放；
缓存读占 96%，要看输入和输出时把它隐藏。

- `aggregate.ts` 把某个 key 的行按桶求和，转成 `TrendDataPoint[]`。
- **粒度**：默认值与用量页相同，范围不超过 1 天按小时，否则按天；可手动切换。按天时取 `bucket` 的前 10 个字符汇总。桶已经按筛选用的时区划分，所以"每天"就是所选的日期。
- **补 0**：图表的横轴是类别轴，缺的桶不补就会把前后两点直接连起来，看不出中间停用过。所以按 `coverage` 从起点到终点逐桶补 0，所有 key 共用同一条时间轴。
  这样单个桶、跨天的空缺、全部 key 在首尾停用都能如实画出来，超出保留期的日期不会画成 0。
  - 个别桶可能落在 `coverage` 之外（两条查询之间保留清理删了最早的明细），时间轴取 `coverage` 与实际桶的并集，不丢数据。
  - 标签按字符串计算（`Date.UTC` 逐小时递增后格式化），不受浏览器时区影响。有夏令时的时区里，拨快的一小时显示为 0，拨回的两小时并在一个桶里。
- **边界提示**：`coverage.start` 晚于所选起点的 00:00 时，图表上方提示"最早可查到 <coverage.start>，更早的明细已清理"。
- **命中率线**：组件在分母为 0 时把缓存命中率算成 0，所以没有用量的桶上命中率也画成 0。这些桶的 token 都是 0，不会和真实的低命中率混淆，不为此改上游组件。

### 5.4 状态

| 状态 | 判据 | 显示 |
|---|---|---|
| 加载中 | 首次加载、换范围 | 加载中（第 6 节的查询耗时加传输），换范围期间不显示旧数据 |
| 失败 | 请求出错，取消除外 | "加载失败"与重试按钮 |
| 不在可查范围 | `rows` 为空且 `coverage.start` 晚于 `coverage.end` | "所选日期没有可查的明细（明细只保留 31 天）" |
| 没有用量 | `rows` 为空且范围有效 | "可查范围内没有用量"（所选范围可能含还没发生的时段，不能说整段都没有） |

## 6. 性能

线上实测（2026-11-03，用户下全部 key，完整 SQL 含关联、排序与结果输出，在宿主的 psql 里跑，不含 Go 编码与网络传输）：

| 范围 | 行数 | 耗时 | 计划 |
|---|---|---|---|
| 7 天 | 963 | 3.4 秒 | 单进程顺序扫描 + HashAggregate |
| 31 天 | 4,940 | 5.5–5.9 秒 | 同上 |
| 31 天，去掉 `MATERIALIZED` | 4,940 | 10–12 秒 | 2 个 worker 并行扫描，198 万行排序后聚合，PostgreSQL 占满 2 核 |

- 这两个范围的查询都走顺序扫描。31 天基本读完全表，索引帮不上忙；7 天的计划也没选索引。暂不新增索引和预聚合表，新索引还会增加写入成本。
- 只在打开页面和换范围时查询一次，没有自动刷新。展开、切换粒度都不再请求，换范围时取消旧查询。`Heavy()` 限流默认豁免管理员，不能当作并发保护，靠的是取消。
- 同类负载在线上已有：额度服务的窗口用量查询、用量页的统计都是相同量级的扫描。没有设网关延迟阈值，上线时看实际表现（第 9 节）。
- 响应大小估算：31 天约 1 MB，7 天约 200 KB。

## 7. 碰上游的点

| 文件 | 改动 |
|---|---|
| `backend/internal/server/routes/user.go` | 一行路由 |
| `frontend/src/router/index.ts` | 追加一条路由 |
| `frontend/src/components/layout/AppSidebar.vue` | `buildSelfNavItems` 一行 |
| 根 `Makefile` | `FRONTEND_CRITICAL_VITEST` 追加新 spec |
| `FORK-NOTES.md` | 定制清单一条、挂点一节 |

rebase 复核：筛选条件复用 `kongUsageLogFilterConditions`，已有的复核项覆盖这里；有效时区的规则照抄 `timezone.ParseInUserLocation`，上游改那里的回退规则要同步；
`TokenUsageTrend` 的 props 或 `TrendDataPoint` 字段变了要同步 `aggregate.ts`。

## 8. 测试

后端：

- **sqlmock**：筛选条件与参数（含 `$tz` 的位置）、查询含 `MATERIALIZED`、扫描、`api_keys` 去重、空结果是非 nil 的空数组、`coverage` 取值。
- **集成（真 PG）**：
  - 档位分类覆盖 NULL、`default`、`auto`、`scale`、`priority`、`fast`、`ultrafast`、`flex`、未知值及大小写。
  - 只算本用户；软删除的 key 有名字并标 `deleted`；按模型、档位、小时分组正确。
  - 同一筛选条件下各行之和与 `KongGetAPIKeyStatsWithUsageFilters` 相等。
  - 连接时区与 `$tz` 不同（如连接 `Asia/Shanghai`、`$tz` 为 `UTC`）时，边界前后的记录落在 `$tz` 的正确日期里。
  - `coverage`：起点被最早一行夹住；终点被当前时刻夹住；所选日期全部早于最早一行时起点晚于终点；空表。
- **handler**：只查当前用户、非法筛选返回 400、仓库不支持时返回 500、JSON 字段名、有效时区的解析（合法、非法、缺失、`Local`）。

前端：

- **`aggregate.ts`**：汇总与排序、档位子行的出现条件、分母为 0、Fast 占比的显示阈值、补 0（单桶、跨天、首尾停用、桶落在 `coverage` 之外）、按天汇总。
- **页面**：加载中、失败、不在可查范围、没有用量四种状态；边界提示；展开与切换粒度不发请求；已删除标记。
- **取数**：换范围时取消旧请求，取消不显示失败，只保留最后一次的结果；卸载时取消。

## 9. 发布与回滚

- 没有迁移，只换镜像，发布版本接在线上 `v0.2.19-kong.2` 之后。回滚直接换回上一版。
- **上线后确认**：
  - 打开页面，核对某个范围内各 key 的合计与用量页统计卡片一致。
  - 从 `http.access` 日志取端点的 `latency_ms` 与响应字节数，记下接口实际的基线。
  - 换范围时查 `pg_stat_activity`，确认旧查询被取消、不重叠。
  - 打开 31 天范围时看一眼网关的请求延迟和 PostgreSQL 的 CPU 有没有异常。
