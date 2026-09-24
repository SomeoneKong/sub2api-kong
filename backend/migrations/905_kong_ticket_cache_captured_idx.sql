-- codex 票的被动收票（fork 专有）：按首次收到的时刻保留 30 天，清理按 captured_at 取最早的一批删除。
-- 设计见仓库根的 DESIGN-codex-ticket.md。只加索引，旧版本的代码不受影响。

CREATE INDEX IF NOT EXISTS kong_ticket_cache_captured_idx
    ON kong_ticket_cache (captured_at);
