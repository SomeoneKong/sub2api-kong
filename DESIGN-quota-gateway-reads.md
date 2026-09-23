# 额度服务改由网关读取账号与实时占用

**状态**：已实现，随 `0.2.15-kong.10` 发布。

额度服务（工作区 `sub2api-quota/`，宿主上的 `sub2api-quota.service`，额度页与下游 `/quota` 的数据源）现在直接读网关的
postgres 与 redis：每次都 `docker exec` 进容器跑 `psql` / `redis-cli`。本设计把其中最频繁的两项——账号列表与每个账号此刻的
并发、排队、活跃会话——改为经网关的管理接口读取。账号计划边车已经只走网关接口，不在本设计范围内。

## 1. 现状与动机

| 读取 | 方式 | 频率 |
|---|---|---|
| 全部 OAuth 账号的列（套餐、到期、停调阈值、extra、代理、限流 / 暂停字段）与系统设置里的停调阈值 | `psql`（`SQL_ALL_ACCOUNTS`） | 采集每分钟 1 次、额度页每次刷新 1 次 |
| 每个账号的并发槽、实时租约、排队数、活跃会话 | `redis-cli`（`attach_concurrency`，逐账号 `ZCOUNT` / `GET`） | 同上，另加每次 `/quota` |
| 各账号 5h / 7d 窗口内的明细聚合 | `psql`（`SQL_WINDOW_STATS`，缓存 10 分钟） | 约 10 分钟 1 次 |
| 下游 `/quota` 验 key、取该 key 分组下的账号 | `psql`（`SQL_KEY`、`SQL_ACCOUNTS`） | 每次 `/quota` 2 次 |

动机不是 CPU：新宿主空载时 docker 与 containerd 合计约 2.7% 个核（含容器健康检查），额度服务自身 0.8%。要解决的是：

- **算法是照网关抄的**：当前并发 = 常规槽位（按 30 分钟槽位 TTL 判过期）+ 实时租约（按 60 秒判过期），活跃会话 =
  `session_limit:account:<id>` 里最后活跃晚于"现在 − 空闲超时"的成员，空闲超时默认 5 分钟；会话只对生效会话上限大于 0 的
  账号读，而生效上限要按计划条目、账号设置与热账号限速重算一遍（`_session_read_args`、`effective_session_limits`）。网关或上游
  改了 redis 键、TTL 或会话登记规则，额度页会静默算错。
- **SQL 绑着表结构**：凭据 JSON 里的子键名、`settings` 的键名、`proxies` 的列名都写死在额度服务里。
- **权限**：能 `docker exec` 即等于 root，还能读整库；本服务只是靠自觉不取凭据列。

## 2. 范围

本设计只做前两项（账号列表、实时占用），它们占了绝大部分调用。窗口用量（10 分钟一次）与 `/quota` 的 key 校验保留 `psql`，
所以额度服务仍需 docker 权限；要彻底去掉时另做：网关给出"按账号与起点聚合用量"的接口、key 校验交给网关。

## 3. 网关：`kong-plan/accounts` 每项加实时占用

`GET /api/v1/admin/kong-plan/accounts` 的每一项（`KongPlanAccountView`）加一个字段：

```json
"live": {"concurrency": 3, "waiting": 0, "sessions": 2}
```

- `concurrency` / `waiting`：`ConcurrencyService.GetAccountsLoadBatch` 的 `CurrentConcurrency` / `WaitingCount`，与选号读的是
  同一份负载（常规槽位 + 实时租约、排队计数）。入参的并发上限取这一项的 `max_concurrency`（只影响负载率，这里不输出）。
- `sessions`：这一项的 `max_sessions` 大于 0 时，用 `GetActiveSessionCountBatch` 按 `kongSessionIdleTimeout(account)` 读活跃会话
  数；`max_sessions` 不大于 0（不限）时为 `null`——网关不对不限的账号登记会话，读出来的数没有意义。视图的上限由
  `kongEffectiveLimitsOf` 得到，会话登记的 `kongEffectiveMaxSessions` 也调用同一个函数；按本项已求出的上限决定是否查询，
  不在查询前重算另一份计划或热度。空闲超时同样用登记侧的函数，未配置或配置无效时是 5 分钟。
- 视图本身只列状态为 active 的 OpenAI OAuth 账号（`ListByPlatform`），边车也读它，不扩大这个集合；停用、出错的账号
  因此没有实时占用。它们不接新请求，额度服务的池子汇总也只算能服务的账号，页面上这些账号的并发与会话显示为未知。
- 全部账号合成一次负载读取、一次会话读取。负载读取报错时所有项的 `live` 为 `null`；只有会话读取报错时所有项的
  `live.sessions` 为 `null`。还必须检查结果 map 是否包含所请求的账号：`GetActiveSessionCountBatch` 会忽略失败的单项，
  即使整批失败也可能返回空 map 和 nil error；缺项只将该账号的 `sessions` 置 `null`，绝不按 map 零值写成 0。
  负载 map 缺项或值为 nil 时也将该账号的 `live` 置 `null`。依赖未装配按读不到处理，不 panic。
  接口照常返回 200：定层与上限是主内容，实时占用是附加读数，边车不读它。
- 代码只在 fork 文件里：`KongPlanAccounts` 增加一个读取实时占用的参数（接口类型，两个方法分别包住负载读取与会话读取），由
  `OpenAIGatewayService` 实现（它已持有 `concurrencyService` 与 `kongSessionLimit`）；handler 传 `h.gateway`，传 `nil` 时每项的
  `live` 都为 `null`（`live` 字段总是输出）。只修改现有 fork service 与 handler 的调用；路由与依赖装配不变。适配器要兼容 nil 网关指针，避免传入接口后的
  typed nil 被误当成已装配。
- 测试：`live` 的取值与 `max_sessions` 为 0 / 大于 0 时 `sessions` 的有无、两种读取失败时的置空、`nil` 读取器；契约测试
  还覆盖会话结果部分缺项、整批空 map 且 nil error、合法的 0 与负载缺项。`admin_kong_plan_contract_test.go` 现有用例没有
  实际请求 `/accounts`，需新增带账号数据的路由响应断言，检查 JSON 的 0、null 与省略字段。
- 发布 `0.2.15-kong.10`，FORK-NOTES「账号选择与 credits」一节补一句 `/accounts` 带实时占用。

## 4. 额度服务

### 4.1 账号列表改读管理接口

采集与额度页（`collect_once`、`/api/quota`）不再跑 `SQL_ALL_ACCOUNTS`，改为：

- `GET /api/v1/admin/accounts?platform=openai&type=oauth&page_size=1000`：上游自带的账号列表，不需要 fork 改动；
- `GET /api/v1/admin/settings` 里的 `account_scheduling_thresholds`（系统级停调阈值）。

原 `SQL_ALL_ACCOUNTS` 只过滤 `type=oauth`、不限平台，本节请求与 `kong-plan/accounts` 的实时视图只覆盖 OpenAI OAuth。库里的 OAuth
账号目前全是 OpenAI 的（12 个），两者等价；以后接入其他平台的 OAuth 账号时，再扩展这两处读取。切换前的对照仍核对新旧账号 id 集合一致。

两次都用已有的 `_gateway_get`（管理员 key 取边车那一份、每次请求 3 秒超时），从解包后的 `data.items` 取账号、`data.total`
检查完整性。列表不传 status / group / search / privacy_mode 过滤，停用、停调与限流账号也要保留。
当前 `lite=1` 仍包含本表所需的 credentials、extra 与 proxy，可用它省去 groups / account_groups；两种响应的字段都按本表处理。
不传 `include_scheduler_score`，它默认关闭；lite 不会跳过列表原有的并发、会话与适用账号的窗口费用读取，这些附加结果不用于本服务。
返回的每个账号转换成原来 SQL 行的形状，业务判定仍交给 `build()`：

| SQL 行的键 | 来源（账号列表的一项） |
|---|---|
| `id`、`name`、`platform`、`status`、`schedulable`、`priority`、`concurrency`、`extra`、`error_message`、`auto_pause_on_expired`、`proxy_id`、`temp_unschedulable_reason` | 同名字段 |
| `rate_limit_reset_at`、`rate_limited_at`、`temp_unschedulable_until`、`overload_until` | 同名字段（RFC 3339 字符串，`build()` 现有的解析接受） |
| `expires_at` | null 保持 null；其余 Unix 秒转成带时区的 ISO 字符串，不能用真假值判断而漏掉 0。DTO 只有秒精度，原数据库时间的小数秒不会保留 |
| `plan`、`plan_expires_at`、`scheduling_threshold` | `credentials` 的 `plan_type`、`subscription_expires_at`、`account_scheduling_threshold`（这三项不在凭据黑名单里）。须恢复 SQL `->>` 的文本形状：null 为 None，字符串保持不变，数字、布尔等按 JSON 文本转换，不能将数字直接传给会调用 `.strip()` 的 `scheduling_threshold()`；例如 95 转为 `"95"`，false 转为 `"false"` |
| `platform_scheduling_thresholds` | 设置接口的 `account_scheduling_thresholds` 序列化成 JSON 字符串。这是网关补全并归一化后的有效配置，并非原 `settings.value` 原文；未配置或非法配置时原输出可能为 null，新输出为 100，二者都表示不停调 |
| `proxy_name`、`proxy_protocol`、`proxy_host`、`proxy_port`、`proxy_status` | `proxy` 对象的 `name`、`protocol`、`host`、`port`、`status`（不含密码） |
| `proxy_deleted_at` | 列表不返回已软删的代理，也无法区分软删与记录缺失；不伪造删除时刻。`proxy_id` 非空但无 `proxy` 时，所有 `proxy_*` 展示字段为 None，现有 `proxy_info()` 输出 `issue=missing`、name / endpoint 为 null，页面显示代理不存在；不再保证显示原来的“代理已删” |

- 列表接口默认按名字排序，转换后按 `id` 排（与原 SQL 的 `order by a.id` 一致，页面卡片位置按 id 稳定）。
- `total` 大于返回条数时报错，不静默截断（当前 12 个账号）。
- 转换层只保留表中字段及三个凭据子键，丢弃其余账号字段和代理 username，不回显完整上游响应；代理密码不在 DTO 中。
- 读取失败的处理与现在 `psql` 失败相同：采集记一轮失败（`collection_runs.ok = 0`）、额度页回 503。网关重启的几秒内可能丢一轮采集。
- 不另加缓存：采集与额度页各每分钟一次，合计每分钟约 4 个请求。
- 账号 HTTP 读取是主数据源，不由 `PLAN_ENABLED` 的可选计划展示开关决定。非默认 `QUOTA_PG_DB` 的实例须显式配置指向同一实例的
  `QUOTA_GATEWAY_BASE` 和 `QUOTA_ADMIN_KEY_FILE`，缺少时明确报配置错误，不能将生产网关账号与另一库的窗口用量、key 校验混用。
  不保留 SQL 账号列表作为备用路径；部署时同时核对 PG 容器 / 库与网关地址属于同一实例。

### 4.2 实时占用改读网关视图

`attach_concurrency` 不再跑 `redis-cli`，改为从 `gateway_view()` 的 `accounts[*].live` 取：视图读成功且不是沿用的旧视图
（`stale` 为假）时，`concurrency = {"current": live.concurrency, "waiting": live.waiting}`、`sessions = {"current": live.sessions}`
（`live.sessions` 为 `null` 时 `sessions` 为 `None`）；视图读不到、是沿用的旧视图或这一项的 `live` 缺失 / 为 null 时两者都为 None。
映射必须显式区分 0 与 null，不能把 null 写成 `{"current": null, "waiting": null}` 冒充成功读取。
`gateway_view()` 本就缓存 15 秒，额度页、采集与 `/quota` 共用；缓存从四个 GET 全部完成后开始计时，所以读数年龄为最多 15 秒缓存
加本次读取耗时（网关负载服务本身还有极短缓存），不是严格的 15 秒。任一个计划 GET 失败都会使整份视图 stale，实时占用也随之置空。

随之删掉：`REDIS`、`SLOT_TTL_SECONDS`、`LIVE_LEASE_TTL_SECONDS`、redis 读取与解析、`_session_read_args` 的读数参数构造及三个调用点，
以及只被这些调用使用的 `effective_session_limits`。保留展示元数据的补充：自身不限但网关 `max_sessions > 0` 时，仍从该账号 extra
补 `session_idle_timeout_minutes`（未配置为 5），否则自定义空闲超时会在页面提示里丢失。无可用网关视图时，仍为 OpenAI 账号标记
`session_limit_unknown`；沿用的 stale 视图仍可用于展示上限，不能用于展示实时占用。

`build()` 的 `session_limit` / `concurrency_limit` 仍表示账号自身设置，不改写成网关的有效上限；页面 strategy 与 `/quota` 的有效上限
继续使用网关视图，`_effective_limit` 的现有展示用途与本次实时读数替换分开处理。SQLite 的 `session_limit` / `concurrency_limit` 与
`plan_max_sessions` / `plan_max_concurrency` 的含义不变，`session_current` / `concurrency_current` / `concurrency_waiting` 改为缓存读数，
stale 或读取失败时写 SQL NULL，本轮其他数据正常仍记 `collection_runs.ok=1`。

### 4.3 `/quota`

key 校验与取账号仍是 `psql`（第 2 节），实时占用同样改读本次响应共用的那一份网关视图。当前值置空后，现有汇总逻辑会使涉及的
有限账号池 `sessions.known` / `concurrency.known` 为 false、current / free（并发还包括 waiting）为 null；可沿用的上限仍给出。
这比旧版“网关 stale 时继续直读 Redis”更容易返回未知，下游会话节奏判断需按现有 null 约定处理。不限账号的计数不参与有限池汇总。
实现时同步更新 `sub2api-quota.md` 的数据来源与 SQLite 说明，以及 `QUOTA-API.md` 的“每次请求现读”与故障说明。

## 5. 上线与验证

1. 部署网关 `kong.10`：只多一个字段，现行额度服务与边车都忽略它。
2. 保留旧脚本副本，用外部一次性对照脚本分别调用旧、新实现（不监听端口、不写生产 SQLite）；新版服务本身不保留旧读取分支。
   使用同一个 `now`，先核对账号 id 集合，再比对 `build()`、完整页面数据、`_sample_row` 与 `/quota?schema=2` 的账号及池子汇总。
   时间按同一时刻归一化比较，`expires_at` 接受 DTO 的秒精度；接受有效阈值 null → 100，以及软删代理 name / endpoint 丢失、issue 变为 missing。
   占用对照先清空网关视图缓存再读取，注明两个路径的取数顺序；只接受这段时间内可解释的占用变化，不能把系统性差异归为时差。
   新旧脚本不在比较时改生产配置或做 SQL 写入。
3. 用固定响应验证数字 / 字符串阈值、时间 null / Unix 0、空列表 / 超过 1000 条、停用账号、代理软删、计划覆盖自身不限、
   自定义空闲超时、会话 map 部分或全部缺项、负载失败、`live` 缺失 / null、stale 及无历史成功视图。
   对照 SQLite 样本的 NULL 与自身上限、`/quota` 的 known / free，并确认账号 HTTP 失败时采集失败 / 页面 503，而 key 校验失败仍是 401。
4. 对照通过后替换脚本、重启 `sub2api-quota`，看采集每分钟成功、额度页与 `/quota?schema=2` 正常；保留旧脚本供回滚，无 SQLite schema 迁移。

回滚：额度服务换回旧脚本即可（旧脚本不依赖 `live`）；网关回滚到 `kong.9` 前要先回滚额度服务，否则额度页只剩上限、没有占用。

## 6. 不做的事

- 窗口用量与 `/quota` 的 key 校验（第 2 节）。
- 为管理接口另加缓存、批量或重试。
- 边车：已经只读网关接口。
