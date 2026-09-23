-- 账号消耗节奏的决策记录：旧版选号每次进入第 2 层写一行，只用于事后分析（不进管理界面）。
-- 设计见仓库根的 DESIGN-openai-account-pace.md 第 4.4 节。
--
-- 候选明细与各轮顺序放 JSONB（rounds）：候选数随分组变化，逐列展开没有意义；需要按账号统计时
-- 用 jsonb_array_elements 展开。关联 usage_logs：request_id / client_request_id 对应
-- usage_logs.request_id 的 local:<…> / client:<…> 两种写法；session_hash 是粘性绑定用的会话哈希。
--
-- 保留期由配置文件的 decision_log.retention_days 决定，节奏组件每天按 created_at 分批清理。
-- 全部语句幂等，可重入。

CREATE TABLE IF NOT EXISTS kong_openai_pace_decisions (
    id                 BIGSERIAL   PRIMARY KEY,
    created_at         TIMESTAMPTZ NOT NULL,
    request_id         TEXT,
    client_request_id  TEXT,
    user_id            BIGINT,
    group_id           BIGINT,
    model              TEXT,
    session_hash       TEXT,
    -- new_session / sticky_spillover / no_session
    reason             TEXT        NOT NULL,
    -- 是否实际重排；否的原因：disabled / snapshot_missing / snapshot_stale / no_available / load_error /
    -- mixed_segment（各轮的每个优先级段都混有其他类型账号，没有一段按节奏排序）
    applied            BOOLEAN     NOT NULL,
    not_applied_reason TEXT,
    config_version     TEXT,
    -- 这次计算实际用到的配置
    config             JSONB,
    snapshot_age_ms    BIGINT,
    -- 每轮的候选明细、节奏顺序、最终尝试顺序与抢到者位置
    rounds             JSONB,
    -- acquired / not_acquired / no_available
    outcome            TEXT        NOT NULL,
    account_id         BIGINT,
    attempt_index      INTEGER
);

CREATE INDEX IF NOT EXISTS kong_openai_pace_decisions_created_idx
    ON kong_openai_pace_decisions (created_at);
CREATE INDEX IF NOT EXISTS kong_openai_pace_decisions_account_idx
    ON kong_openai_pace_decisions (account_id, created_at);
