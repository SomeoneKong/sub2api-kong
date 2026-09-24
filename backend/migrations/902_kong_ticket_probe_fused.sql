-- 902: 探测记录标出「融合样本」，并给被丢弃的观测留一个原因字段。
--
-- 融合样本来自**取票请求本身**：它在票据出口上产生、且当时没有注入票，与常规挑战（流量出口、
-- 带票）的产生条件不同。不标出来，事后分析会把两类混算，而"哪个出口取到好票"正是要靠这张表回答
-- 的问题。
--
-- 默认 FALSE：历史记录全部是常规样本，这个取值对它们为真。
ALTER TABLE kong_fingerprint_probes
    ADD COLUMN IF NOT EXISTS fused BOOLEAN NOT NULL DEFAULT FALSE;

-- 被丢弃的融合样本要留着（它花了额度），但不能参与最终平均——否则离线按 counted_in_average 重算
-- 会把它混回去，得出与线上不同的结论。丢弃原因记在这里，跟 invalid_reason 分开：那一列是"这份
-- 回答本身无效"，而丢弃是"回答有效但产生条件不同，不采用"。
ALTER TABLE kong_fingerprint_probes
    ADD COLUMN IF NOT EXISTS discarded_reason TEXT;
