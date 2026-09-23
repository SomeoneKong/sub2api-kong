-- 901: 事件的 outcome 增加 inconclusive 档。
--
-- 为什么单独一个文件而不是改 900：迁移框架按文件内容算 checksum，改已应用过的文件会让所有现存
-- 数据库在启动时校验失败。900 已经在生产上应用过。
--
-- 只是**加**一个允许值：旧枚举保留、旧数据不动，回滚到旧二进制也只是不再写新值。
--
-- 「测了但没测出来」（前提中途失效、没有有效回答、证据写库失败）与「测出不合格」必须分开：前者对
-- 票什么都没说，据它作废票就是无谓拒服。此前这些情况混在 skipped 与 failure 里。
--
-- ⚠️ 不加约束就写不进去：CHECK 会让每一条 inconclusive 事件 INSERT 失败，而那正是要求保存的处置
-- 记录——普通路径只留一行错误日志，最终事件路径会把整次验证判成"记录失败"。
ALTER TABLE kong_ticket_events
    DROP CONSTRAINT IF EXISTS kong_ticket_events_outcome_check;

ALTER TABLE kong_ticket_events
    ADD CONSTRAINT kong_ticket_events_outcome_check
    CHECK (outcome IN ('success', 'failure', 'inconclusive', 'skipped', 'info'));
