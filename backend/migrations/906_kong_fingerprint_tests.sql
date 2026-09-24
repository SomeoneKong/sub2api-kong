-- 账号指纹测试（fork 专有）：探测记录加上游回报的模型，一次测试一行。
-- 设计见仓库根的 DESIGN-openai-fingerprint-test.md。只加表与可空列，旧版本的代码不读不写它们。

ALTER TABLE kong_fingerprint_probes
    ADD COLUMN IF NOT EXISTS reported_model TEXT;

CREATE TABLE IF NOT EXISTS kong_fingerprint_tests (
    -- 与 kong_fingerprint_probes.verification_id 关联
    verification_id UUID        PRIMARY KEY,
    account_id      BIGINT      NOT NULL,
    target_model    TEXT        NOT NULL,
    -- 测试开始时账号的代理；NULL 表示直连
    proxy_id        BIGINT,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    -- completed / cancelled / failed；NULL 表示没收尾（进程在测试中途退出）
    execution       TEXT,
    end_reason      TEXT,
    -- match / mismatch / inconclusive；执行失败或取消时为 NULL
    verdict         TEXT,
    -- 判定规则的版本，改了判定顺序或条件就换版本
    rule_version    TEXT        NOT NULL
);

CREATE INDEX IF NOT EXISTS kong_fingerprint_tests_account_idx
    ON kong_fingerprint_tests (account_id, started_at DESC);
