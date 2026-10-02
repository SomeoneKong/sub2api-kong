-- 账号选择与 credits（fork 专有）：边车发布的兜底与快照、人工约束、发布状态、容量视图的慢速部分、
-- 调用方声明，以及网关自己的决策记录。设计见仓库根的 DESIGN-openai-plan-dispatch.md。只加表，旧版本的代码不读不写它们。
--
-- kong_plan_dispatch 一类一行，内容放 JSONB；标量列只是内容里同名字段的副本，方便人查，网关只读 content。
-- 全部语句幂等，可重入。

CREATE TABLE IF NOT EXISTS kong_plan_dispatch (
    -- publish（发布状态）/ dispatch（最近一次接受的发布）/ control（人工约束）/ forecast（慢速部分）
    kind        TEXT        PRIMARY KEY,
    -- publish、dispatch：发布代次
    generation  BIGINT,
    -- publish：最近一次接受的发布序号；dispatch：这份发布的序号
    seq         BIGINT,
    -- control：约束修订号
    revision    BIGINT,
    -- dispatch、control：内容指纹
    sha256      TEXT,
    content     JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 发布状态的初值：代次 1、启用，发版后边车的第一次发布就能被接受。
INSERT INTO kong_plan_dispatch (kind, generation, seq, content)
VALUES ('publish', 1, 0, '{"generation": 1, "enabled": true, "last_seq": 0}'::jsonb)
ON CONFLICT (kind) DO NOTHING;

-- 调用方声明：只有一个调用方，单行，后写覆盖。
CREATE TABLE IF NOT EXISTS kong_pool_caller (
    id                  SMALLINT         PRIMARY KEY CHECK (id = 1),
    -- normal / throttled / paused
    state               TEXT             NOT NULL,
    desired_pp_per_hour DOUBLE PRECISION,
    until_at            TIMESTAMPTZ      NOT NULL,
    note                TEXT             NOT NULL DEFAULT '',
    api_key_id          BIGINT,
    updated_at          TIMESTAMPTZ      NOT NULL
);

-- 决策记录：计划输入生效时的选号按小时累计，网关每分钟累加写入，保留 30 天（每天清理一次）。
CREATE TABLE IF NOT EXISTS kong_dispatch_stats (
    hour              TIMESTAMPTZ NOT NULL,
    account_id        BIGINT      NOT NULL,
    -- subscription / credits / paused；中转为空
    layer             TEXT        NOT NULL,
    -- 第一层的档位组 asap / normal / inactive / standby；其余为空
    grp               TEXT        NOT NULL,
    -- 选中时的段 room / grace / credits_room / overflow / credits_overflow / paused / relay；
    -- credits_reuse 是复用 credits 层会话绑定（只记停留时长）
    seg               TEXT        NOT NULL,
    -- 网关所用的输入 snapshot / fallback
    in_use            TEXT        NOT NULL,
    plan_version      BIGINT      NOT NULL,
    publish_seq       BIGINT      NOT NULL,
    -- 选中次数、确认登记时名额已被占的次数、第 3 层选中后排队等槽的次数
    selected          BIGINT      NOT NULL DEFAULT 0,
    confirm_failed    BIGINT      NOT NULL DEFAULT 0,
    waited            BIGINT      NOT NULL DEFAULT 0,
    -- 选中时账号已有的会话数之和
    sessions_sum      BIGINT      NOT NULL DEFAULT 0,
    -- 选中 credits 层账号或中转时，这个第一层账号接不了的次数：已到上限加宽限 / 负载已满或未知 / 其余
    ahead_limit       BIGINT      NOT NULL DEFAULT 0,
    ahead_busy        BIGINT      NOT NULL DEFAULT 0,
    ahead_unavailable BIGINT      NOT NULL DEFAULT 0,
    -- 复用 credits 层会话时，会话从写下标记起已停留的秒数
    dwell_n           BIGINT      NOT NULL DEFAULT 0,
    dwell_sum_s       BIGINT      NOT NULL DEFAULT 0,
    dwell_max_s       BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, account_id, layer, grp, seg, in_use, plan_version, publish_seq)
);
