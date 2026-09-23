-- Codex 模型目录的版本存档（fork 专有）：目录全文按内容去重，各账号看到哪个版本按连续段记录。
-- 设计见仓库根的 DESIGN-codex-catalog-consensus.md。只加表，旧版本的代码不读不写它们。全部语句幂等，可重入。

CREATE TABLE IF NOT EXISTS kong_codex_catalogs (
    id            BIGSERIAL   PRIMARY KEY,
    -- 原始正文的 SHA-256（hex），全文一致才算同一版本
    sha256        TEXT        NOT NULL UNIQUE,
    -- 按清单顺序的模型名，便于直接查看
    model_slugs   TEXT[]      NOT NULL,
    body_size     INTEGER     NOT NULL,
    -- 网关从上游拿到的原始正文
    body          BYTEA       NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 一行是一个账号在某个客户端版本下，相邻几次采样都看到同一版本、同一套餐的一段。
-- 起止是采样的摘要：两次采样之间、相邻两段之间的状态未知。
CREATE TABLE IF NOT EXISTS kong_codex_catalog_observations (
    id             BIGSERIAL   PRIMARY KEY,
    account_id     BIGINT      NOT NULL,
    client_version TEXT        NOT NULL,
    catalog_id     BIGINT      NOT NULL REFERENCES kong_codex_catalogs (id),
    -- 采样时账号凭据里的 plan_type，只用于观测
    plan_type      TEXT        NOT NULL,
    first_seen_at  TIMESTAMPTZ NOT NULL,
    last_seen_at   TIMESTAMPTZ NOT NULL,
    samples        INTEGER     NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS kong_codex_catalog_observations_latest_idx
    ON kong_codex_catalog_observations (client_version, account_id, last_seen_at DESC);
