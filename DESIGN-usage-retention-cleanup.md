# 明细保留清理不挡住明细写入

上游的请求明细保留清理（`usage_logs`）在删除期间和删除之后都会长时间锁住分组用量汇总的状态行，挡住所有新明细的写入，
写入超时的明细直接丢失。本设计让保留清理不再持这把锁，删完只对账边界附近的分组日桶。只改上游文件，做成可提给
上游的修复。

## 1. 现状与问题

### 1.1 分组用量日桶的维护方式

- `usage_group_daily_rollups` 存每天每个分组的费用，`usage_group_rollup_state`（单行，`id = 1`）记两条边界：
  `closed_before`（之前的日子已发布进日桶）与 `retained_from`（最早一条明细的时刻）。
- 读取（`getAllGroupUsageSummaryFromRollups`）：日桶只取 `[retained_from 那天, closed_before)`，`closed_before` 之后
  直接汇总明细。
- `usage_logs` 上三个触发器（迁移 222、223）在明细变动落在已发布的日子时把 `closed_before` 降到那一天：
  - 行级删除 / 更新触发器 `invalidate_group_usage_rollup_state()`：对状态行 `SELECT … FOR UPDATE`；
  - 语句级插入触发器 `invalidate_group_usage_rollup_state_after_insert()`：对状态行 `SELECT … FOR KEY SHARE`，
    插入的行落在已发布的日子时再 `UPDATE` 降水位。
- 同步 `SyncGroupUsageRollups`（每个聚合周期之后、启动时、清理后）：`FOR UPDATE` 读状态行；`closed_before` 已是今天
  就直接返回；否则删掉 `retained_from` 那天之前的日桶，从 max(`retained_from` 那天, `closed_before`) 起把日桶重算到
  今天，最后把 `closed_before` 设为今天、`retained_from` 设为最早一条明细。整个重算都在这把锁里。

### 1.2 保留清理怎么做

`DashboardAggregationService.maybeCleanupRetention` 在聚合周期末尾每 6 小时一轮（失败则下个周期重试），共用周期的
2 分钟超时，调 `CleanupUsageLogs(now − 保留天数)`：

1. 不分区的表走 `cleanupUsageLogsBatches`：每批一个事务，`lockGroupUsageRollupState`（状态行 `FOR UPDATE`）→
   删 1 万行（每行触发删除触发器）→ `invalidateGroupUsageRollupsAt(本批最早删除时刻)` → 提交，直到不足一批；
2. 分区表先整月 `DROP` 过期分区（同样先锁状态行、把水位降到该月月初），边界分区再走第 1 步；
3. 最后 `SyncGroupUsageRollups`。

### 1.3 后果

- `FOR UPDATE` 与插入触发器的 `FOR KEY SHARE` 互斥，**每批删除的整个事务期间，所有新明细的写入都在排队**。
- 删完 `closed_before` 被降到最早被删的日子，随后的同步**从保留期最早那天起把日桶重算到今天**，扫遍整个保留期的明细，
  同样持锁，写入继续排队。
- 写明细（`Create usage log`）有超时，排队超时的明细丢失（计费照常扣）。
- 稳态下每一轮清理都这样：每轮删掉几小时的旧数据，`closed_before` 就被降到保留期最早那天，下一次同步重算整个
  保留期。保留期越长，重算越久。

实测（2 核 VPS，PostgreSQL 18，库里约 110 万行明细，保留期从 180 改为 31 天后的第一轮清理）：一批 1 万行约 27 秒，
随后的同步约 40 秒；期间大量 `Create usage log failed: context deadline exceeded`，2 分钟内 43 条明细没写进库。

### 1.4 为什么不必重算整个保留期

保留清理每批删的是"早于 cutoff 的最早一批行"，所以已提交的删除是明细的一段前缀（例外是保留边界之前的迟到写入，
见 3）。删完之后：

- 整天都被删掉的日子：日桶在新的最早一行那天之前，删掉即可；
- 新的最早一行那天（可能删掉了一部分）：只有这一个日桶需要按剩下的明细重算；
- 之后的日子：明细没变，日桶仍然正确。

所以只需要：删掉新最早日之前的日桶、重算新最早日、更新 `retained_from`；`closed_before` 不必动。

## 2. 改法

### 2.1 保留清理时，删除触发器不降水位

新迁移 `243_usage_logs_retention_cleanup_skips_rollup_invalidation.sql`，`CREATE OR REPLACE FUNCTION
invalidate_group_usage_rollup_state()`：在函数开头加一个分支——`TG_OP = 'DELETE'` 且当前事务设置了
`sub2api.usage_logs_retention_cleanup = 'on'` 时直接 `RETURN OLD`，不读、不锁、不改状态行。函数其余部分与迁移 223
定稿的版本逐字相同。

- 判断用 `current_setting('sub2api.usage_logs_retention_cleanup', true)`：没设置过返回 NULL，不报错。
- 自定义设置名带点号，任何数据库用户都能 `SET LOCAL`；`SET LOCAL` 只在当前事务内有效，事务结束即恢复，连接池复用
  连接不会把它带到别的事务。
- 只有保留清理会设置它。用户级联删除、后台用量清理任务（`usage_cleanup_repo.go`）、更新触发器都不受影响。

### 2.2 清理批次不锁状态行

每批的语句改为 `SET LOCAL … = 'on'` → 按原来的 `victims` 语句删一批 → `SET LOCAL … = 'off'`。去掉
`lockGroupUsageRollupState` 与 `invalidateGroupUsageRollupsAt`，不再需要 `RETURNING created_at`。批次大小不变（1 万行）：
批次事务只锁被删的行，不再和写入冲突。

- `r.sql` 是 `*sql.DB` 时每批自开事务、提交（生产路径）。
- `r.sql` 是调用方的 `*sql.Tx` 时在其中执行同样三条语句；删完显式设回 `off`，同一事务里之后的普通删除照常失效日桶。
  任何一步出错，调用方必须回滚整个事务（开关可能还开着）。目前只有测试走这条路径。

### 2.3 每一轮清理都以对账收尾

`CleanupUsageLogs` 改为：

1. 删除阶段（`deleteExpiredUsageLogs`）：探测是否分区；分区表先按月份先后整月 `DROP` 过期分区，再按 2.2 分批删。
   任何一步出错（包括探测本身）就停止删除，记下错误。批次删除不足一批时再查一次
   `EXISTS (SELECT 1 FROM usage_logs WHERE created_at < cutoff)`，还有就接着删——另一轮并发的清理可能先删掉了同一批行，
   不足一批不等于删完。删除阶段给出这一轮的删除可能涉及的最晚时刻 `deletedThrough`：全部删完是 `cutoff`；没删完是
   已删分区里最晚的上界（按分区名的月份），没删分区则为零值；
2. **不论删除阶段成败**都对账一次（见下），把 `deletedThrough` 传给它；对账用独立于清理 ctx 的 30 秒时限——清理的 ctx
   可能已经超时（周期聚合与清理共用 2 分钟）；
3. 对账出错：返回删除与对账的错误；删除阶段出错：返回它；都成功才照旧 `SyncGroupUsageRollups`。

清理返回错误时服务不记成功时间，下一个聚合周期整轮重试（现有行为）。进程在批次提交与对账之间退出时，成功时间只在
内存里，重启后的第一个周期就会再跑一轮清理、再对账。

`r.sql` 是 `*sql.DB`（生产路径）时，对账自开一个短事务。`r.sql` 是调用方的 `*sql.Tx` 时，对账在同一事务里执行：
独立时限换不回已经失败或被取消的事务，出错后由调用方整体回滚，这条路径不提供独立恢复，状态行的锁也要等调用方
结束事务才释放。

对账 `reconcileGroupUsageRollupsWithRetention(ctx, todayStart, cutoff)`，一个短事务：

1. `SELECT closed_before, retained_from, timezone_name FROM usage_group_rollup_state WHERE id = 1 FOR NO KEY UPDATE`；
2. 状态行的时区与当前配置不同：直接结束——交给同步按时区变化整体重建（与现有逻辑一致）；
3. `SELECT MIN(created_at) FROM usage_logs` 得到新的 `retained_from`（没有明细时取今天零点），其日期记为新最早日；
4. 重算右端：max(新最早日, `deletedThrough` 那天)，`deletedThrough` 为零值时就是新最早日；再与 `closed_before` 取小——
   重算范围是 `[新最早日, min(右端 + 1 天, closed_before))`，`closed_before` 及以后的日子由读取端直接汇总明细；
5. `DELETE FROM usage_group_daily_rollups WHERE bucket_date < 新最早日`；
6. 重算范围非空时：删掉范围内的日桶，用与同步相同的聚合语句（抽成常量 `groupUsageDailyRollupsRebuildSQL`）只聚合
   这个范围的明细重新写入；日期边界按配置时区的自然日：下一天按日历日期加一天，某天零点因夏令时被跳过时取切换的
   时刻作为那天的起点；右端截在 `closed_before` 时与读取端一样按 `closed_before` 解析出的时刻切开，日桶与尾段不重不漏；
7. `retained_from` 有变化时更新它（及 `updated_at`）；`closed_before` 不动。

为什么右端这样取：删除全部完成后，早于 `cutoff` 的只剩保留边界之前的迟到写入，`[新最早日, cutoff 那天]` 里只有这些
迟到写入和 `cutoff` 那天剩下的明细，重算很便宜，又能覆盖迟到写入让最早一行早于删除边界的情形。删除没完成时这段
范围里还有大量待删的旧明细（如保留期从 180 改 31 的首轮），不延伸到 `cutoff`：已提交的批次删的正好是一段前缀，只有
新最早日可能删了一部分；整月删掉的分区却未必构成前缀——默认分区或未按月命名的分区里可能留着更早的行，最早一行
仍在已删的月份之前——所以重算要延伸到已删分区的上界。分区上界按分区名的月份（UTC 月初）给出，实际边界若按会话时区
建，相差不到一天，取这一刻所在的日子都能盖住。

**锁**：`FOR NO KEY UPDATE` 与插入触发器的 `FOR KEY SHARE` 兼容，对账期间普通插入不等，无论对账花多久（线上实例的
临时库实测：对账事务持锁、已 `UPDATE` 状态行、另有同步在排队等 `FOR UPDATE` 三种情况下，插入都立即完成）。它与同步、
`RecomputeRange`、后台用量清理任务的 `FOR UPDATE`，以及迟到写入降水位的 `UPDATE` 互斥，这些都串行。

### 2.4 分区表

整月 `DROP` 过期分区时不再锁状态行、降水位，直接 `DROP`；日桶交给 2.3 的对账按 `MIN(created_at)` 对上剩下的明细，
不依赖分区边界恰好落在哪里（新最早日即使只删了一部分也会重算）。边界分区仍走 2.2 的批次删除。

### 2.5 不改的

- `SyncGroupUsageRollups`（包括它把 `retained_from` 设为最早一条明细、删掉那天之前的日桶）、`RecomputeRange`、插入与更新
  触发器、读取逻辑；`CleanupUsageBillingDedup`（去重表没有触发器）。
- **同步自身的持锁重算**：日切时同步把昨天发布进日桶、迟到写入降水位后同步重算，仍在 `FOR UPDATE` 下进行、期间挡插入；
  正常只有一天的量。
- **后台用量清理任务**（管理员手动触发）与保留批次删到同一批行时：任务先 `FOR UPDATE` 锁状态行，再等批次的行锁，
  插入在这期间排队，最长约一个批次的时长。
- **分区 `DROP`** 要父表的 `ACCESS EXCLUSIVE` 锁，期间挡写入。另外分区按会话时区的本地日期建边界、按 UTC 月份判断是否
  整月过期，在负偏移时区可能删掉 cutoff 之后几小时的行。这两点是上游既有行为，与本修复无关，这里不改。

## 3. 正确性

**一轮删除全部完成后**：早于 `cutoff` 的明细只剩迟到写入。对账删掉新最早日之前的日桶、重算 `[新最早日, cutoff 那天]`
中已发布的日桶；之后的日子没被删动。读取取 `[retained_from 那天, closed_before)` 的日桶加尾段，累计费用等于剩下的
明细之和，与原逻辑"降水位再整体重建"的结果相同。

**一轮删除没完成**（出错、2 分钟超时）：已提交的批次各自删的是当时早于 `cutoff` 的最早一段，合起来仍是一段前缀，
新最早日之后的日子没被删动；对账重算新最早日。同一时刻（`created_at` 相同）的行跨两批也不影响，新最早日总会重算。
整月删掉了分区时，重算延伸到已删分区的上界，默认分区里留着的更早的行不影响结果。例外是这一轮里又有早于删除边界的
迟到写入：最早一行是那条迟到写入，删除边界所在的日子没被重算，读数偏多，直到下一次完成的清理带 `cutoff` 对账。

**"完成"的判定**：删除阶段只有在没有出错、且最后确认已没有早于 `cutoff` 的明细时才算完成。并发的另一轮清理先删了同一批行，
这一轮会接着删，不会把还没删的旧明细当成已删完、去整段重算。

**对账没成功**：对账在每一轮清理末尾都会尝试，但它自己也可能失败（数据库不可用、等锁或执行超过 30 秒），这时清理返回
错误、下一轮再试。进程在批次提交与对账之间退出时，重启后第一轮清理补上。下一轮清理要跑起来还有前提：聚合周期里
聚合本身失败会在清理之前返回，读运行时保留设置失败也会跳过这一轮，这些情况下恢复要等它们恢复正常，没有固定的上限。
保留清理此后被关掉（保留天数改为 0）则不会再对账，最早一天的日桶会一直多算被删的部分——同步日切时会删掉更早的
整天日桶，但不重算最早一天。这些残余情形接受，不另加恢复入口。

**读数偏多的窗口**：从批次提交到对账成功提交，读取会把已删日子的日桶算进累计费用；对账在这一轮成功时，窗口约为
一次清理的时长加上对账本身。其间若恰逢日切同步，它会先删掉更早的整天日桶。

**迟到写入**：落在已发布日子的迟到写入照旧由插入触发器降水位、同步重算——与现状相同；插入触发器的 `UPDATE` 与对账互斥，
对账提交后它再降水位，不会丢失。落在保留边界之前的迟到写入（`created_at` 早于 `cutoff`，正常请求不会出现；包括插入
触发器不处理的无分组明细）由删除全部完成那一轮的带 `cutoff` 对账覆盖，见上。

**并发**：

- 对账与同步、`RecomputeRange`、其他实例的清理对账由状态行锁串行，且都可重复执行，先后顺序不影响结果。
- 对账看不到尚未提交的批次删除（另一实例的清理）：它按删除前的数据算；那一轮清理结束时自己的对账再修正。
- 保留大幅缩短后的第一轮（如 180 → 31）：一次删很多天，`closed_before` 不动；可能要跨好几个聚合周期才删完，
  每个周期结束时对账，日桶随删除进度跟上，每次只重算一天。

## 4. 测试

- **迁移单测**（`//go:build unit`，沿用 222 的文本断言写法）：243 含开关分支，且其余部分与 223 的函数体一致。
- **仓库单测**（sqlmock）：
  - 清理：每批 BEGIN / SET LOCAL on / DELETE / SET LOCAL off / COMMIT，不再锁状态行、不再失效；不足一批后确认没有
    剩余的过期明细，还有就接着删；批次之后是带 `cutoff` 的对账，再是同步；第一批失败、首批删满后第二批失败时都照样
    对账（只重算最早一天）、返回删除的错误、不进入同步；整月删掉一个分区后下一个分区删除失败时，重算延伸到已删分区的
    上界；探测是否分区就失败时照样对账，删除与对账的错误都返回；
  - 对账：时区不一致时不动；最早一天的重算与 `retained_from` 推进；带 `cutoff` 且那天更晚时重算范围延伸到它；
    新最早日不早于 `closed_before` 时只删旧桶；表为空；
  - 同步与 `RecomputeRange` 的现有用例不变。
- **集成测试**（`//go:build integration`，真实 PostgreSQL，沿用 `group_usage_rollup_trigger_integration_test.go`
  的建表方式；本机无 docker，在 CI 跑）：
  1. 清理之后：整天被删的日桶没了、新最早日桶等于剩余明细之和、`retained_from` 更新、`closed_before` 不变，
     `GetAllGroupUsageSummary` 的累计费用等于剩余明细之和；没设开关的删除照旧降水位（回归）；
  2. 上一轮的批次在自己的事务里提交、没对账；下一轮在新的事务里没东西可删，对账照样把日桶与累计费用对上；
  3. 分区表：默认分区里有更早的明细，过期的月分区整月删掉、其余过期行按批删掉，对账后日桶与累计费用对上；
  4. 删完后又有早于 `cutoff` 的无分组迟到写入：带 `cutoff` 对账后各日桶与累计费用对上；
  5. 批次事务未提交时，另一个连接插入明细（`lock_timeout = 1s`）能成功；
  6. 批次在另一个事务里提交之后，新开的对账事务已重算日桶、更新 `retained_from`、未提交时，另一个连接插入明细能成功。

  `*sql.DB` 路径的事务边界（每批、对账各自一个事务，删除失败后照样对账）由仓库单测覆盖；集成测试走调用方事务的路径，
  验证 SQL 在真实数据库上的语义与锁。

## 5. 提交与发布

- 按 `FORK-NOTES.md`：一个"可提给上游的修复"提交（英文 conventional commit，只改上游文件、不引用 fork 符号），
  排在定制清单最前；稳定后向上游提 PR，上游合并后 rebase 时丢掉。设计文档与 FORK-NOTES 的挂点说明放在文档提交里。
- 迁移编号取上游序列的下一个（243）。迁移发布后不再改；上游以后若有迁移重新定义 `invalidate_group_usage_rollup_state()`，
  追加一个后置迁移把开关分支补回，并按"新装（全部迁移按文件名顺序执行）"与"升级（只执行新增的）"两条路径核对函数的
  最终定义。
- **保留天数的有效来源**：`requestRetentionDays` 优先用运行时设置 `ops_runtime_log_config` 里的
  `request_retention_days`，没有这个字段才用 compose 的 `DASHBOARD_AGGREGATION_RETENTION_USAGE_LOGS_DAYS`；后台保存一次
  运行时日志设置就会写进这个字段。线上目前没有它。部署前、每次改保留期前都核对一次，改与回退都操作有效的那个来源。
- 发版 `0.2.15-kong.9`，分两步上线：
  1. 部署前查状态行：`closed_before` 是今天或昨天、`timezone_name` 与配置一致（否则启动同步会先做一次长时间持锁的
     重建，要先处理）；换镜像、保留期仍是 180 天（删不到东西，但这一轮的对账会把最早一天的日桶对上），确认启动后
     没有长时间的同步、写入正常；
  2. 把保留期改为 31 天再重建（约 10 秒中断），看首轮清理：没有 `Create usage log failed`，`closed_before` 不降，
     分组累计费用与剩余明细之和一致，分组日桶从新最早日起连续。出现写入失败就把保留期改回 180 再重建（已删的明细
     不会回来，周归档里有）。
- 计费去重保持上游默认 365 天，31 天以前的由周归档任务删（`sub2api-archive`）。
