-- Codex 票据验证与注入：票缓存、事件、指纹探测记录三张表。
--
-- 编号占 9xx 独立段：上游在 238_ 附近递增，900 以后永不撞号。表名带 kong_ 前缀，
-- 与上游将来可能的同名实现隔开。三张表都是本 fork 独有的新表，与上游 schema 无交集。
--
-- 刻意不走 ent：上游把 ent 生成代码全部入库，新增 schema 会重新生成
-- ent/migrate/schema.go、ent/mutation.go 等上游每周改动数次的文件，rebase 代价过高。
-- 账号级的三项配置同理存在 accounts.extra (jsonb) 里，本迁移不碰 accounts。
--
-- 全部语句幂等，可重入。

-- ---------------------------------------------------------------------------
-- 1. 票缓存：短生命周期，按 expires_at 清理
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS kong_ticket_cache (
    id                BIGSERIAL PRIMARY KEY,
    account_id        BIGINT      NOT NULL,
    -- 最终上游模型名（不是客户端传来的别名）。票与验证结论按 (account_id, model) 绑定。
    model             TEXT        NOT NULL,
    -- 票原值。注入与验证都要用它；随票过期一起清理，不长期留存。
    state             TEXT        NOT NULL,
    state_len         INTEGER     NOT NULL,
    -- observed = 业务响应头带回；fetch = 经票据出口主动取
    source            TEXT        NOT NULL CHECK (source IN ('observed', 'fetch')),
    status            TEXT        NOT NULL CHECK (status IN ('unverified', 'verified', 'rejected')),
    -- 归因结果与概率；unverified 时为 NULL。fingerprint_model 是最像的那一个，
    -- fingerprint_p 是它的概率——两者是**证据**，不是采纳结论。
    fingerprint_model TEXT,
    fingerprint_p     DOUBLE PRECISION,
    -- 归因的完整分布（模型 → 概率）。
    --
    -- 采纳判据是「白名单内各归因结果的概率之和 ≥ 置信度」，白名单来自环境变量、可以改。只存
    -- argmax 的话，改了白名单就没法重判存量票：要么拿旧结论放行（可能放行白名单外的档位），
    -- 要么一律作废（把合格票也丢掉）。存下整个分布，任何时候都能按**当前**白名单重判。
    fingerprint_probs JSONB,
    -- 本段无票期内是否已被跳过（候选未被上游接受后留存但不再选中）
    skip_until_new    BOOLEAN     NOT NULL DEFAULT FALSE,
    captured_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL,
    -- expires_at 的来源：parsed = 从票本身解析；estimated = 按收到时刻 + TTL 推算。
    -- estimated 是下界估计（被动票可能已用掉部分寿命），续期提前量必须覆盖该误差。
    expires_at_source TEXT        NOT NULL DEFAULT 'estimated'
                                  CHECK (expires_at_source IN ('parsed', 'estimated')),
    -- 采集事实：这张票是用哪个出口、在多长静默之后采到的。只有 fetch 票有（observed 票是业务
    -- 响应顺带带回来的，没有「取票出口」这回事）。
    --
    -- 必须随票行存，不能等验证时按当前配置重建：取票与验证之间可以隔着一次重启或一次配置改动，
    -- 那时重建出来的是**现在**的出口，而探测记录声称的是采集时的——记下一个错的事实比没有更糟。
    capture_egress    TEXT,
    capture_idle_seconds BIGINT
);

-- 同一账号同一模型下，票原值唯一。
--
-- 重复收到同一张票不是新信息：没有这条约束时它会新增一行、按「收到时刻 + TTL」重算出一个更晚的
-- 过期时刻（凭空延长寿命），还会被当成新信息去解除候选的跳过标记、触发一次诊断探测。
CREATE UNIQUE INDEX IF NOT EXISTS kong_ticket_cache_state_uq
    ON kong_ticket_cache (account_id, model, state);

CREATE INDEX IF NOT EXISTS kong_ticket_cache_pick_idx
    ON kong_ticket_cache (account_id, model, status, expires_at DESC);
CREATE INDEX IF NOT EXISTS kong_ticket_cache_expiry_idx
    ON kong_ticket_cache (expires_at);

-- ---------------------------------------------------------------------------
-- 2. 事件：取票、收票、验证、注入失效、回捞、冷却、出口配置失效
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS kong_ticket_events (
    id              BIGSERIAL   PRIMARY KEY,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    account_id      BIGINT      NOT NULL,
    model           TEXT,
    event_type      TEXT        NOT NULL,
    -- 成败单独成列而不是塞进 detail：冷却要查「最近一次失败」（§4.3 的 F），
    -- 校准参数要算「成功率 vs 空闲时长」，两者都不该依赖 jsonb 取值。
    outcome         TEXT        NOT NULL DEFAULT 'info'
                                CHECK (outcome IN ('success', 'failure', 'skipped', 'info')),
    -- 上游返回的状态码（若该事件对应一次请求）
    status_code     INTEGER,
    -- 两个出口都要记：出口合并是一类只表现为「一直拿不到合格票」的故障，
    -- 不记下当时判定的两个出口就只能靠猜。
    traffic_egress  TEXT,
    ticket_egress   TEXT,
    state_len       INTEGER,
    ticket_id       BIGINT,
    fingerprint_model TEXT,
    -- 距本系统最后一次使用该出口的秒数。不是「实际静默」——系统外的活动观测不到，
    -- 这个值会高估真实静默。把「成功率 vs 空闲时长」变成一句 SQL 的正是这一列。
    idle_seconds    BIGINT,
    detail          JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS kong_ticket_events_account_idx
    ON kong_ticket_events (account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS kong_ticket_events_type_idx
    ON kong_ticket_events (event_type, created_at DESC);
-- 支撑「该出口最后一次活动 / 最近一次失败」这两个调度判定
CREATE INDEX IF NOT EXISTS kong_ticket_events_egress_idx
    ON kong_ticket_events (ticket_egress, event_type, outcome, created_at DESC);

-- ---------------------------------------------------------------------------
-- 3. 指纹探测记录：长期留存，是本系统唯一可离线重算的证据
-- ---------------------------------------------------------------------------
-- 与票缓存分开的原因：一次验证含 1~3 份探测（份数按置信度递增），是 1:N。
-- 票会随过期被删，所以这里的每一行都自带解释所需的上下文快照，不依赖对票的引用。
CREATE TABLE IF NOT EXISTS kong_fingerprint_probes (
    id                BIGSERIAL   PRIMARY KEY,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 把同一次验证的多份探测串起来
    verification_id   UUID        NOT NULL,
    part_index        SMALLINT    NOT NULL,

    -- --- 解释上下文（快照，不是外键：票缓存会被清理） ---
    account_id        BIGINT      NOT NULL,
    target_model      TEXT        NOT NULL,
    ticket_id         BIGINT,
    ticket_fingerprint TEXT,      -- 票的稳定标识（哈希），票行删除后仍可关联
    ticket_source     TEXT,
    verify_egress     TEXT,       -- 验证请求走的出口（流量出口）
    -- 这张票**被采到时**用的票据出口。与 verify_egress 分开：验证走流量出口，两者通常不同，
    -- 混用会让「哪个出口能取到好票」这个判断失真。observed 票为空（业务响应顺带带回的）。
    capture_egress    TEXT,
    idle_seconds      BIGINT,     -- 采集该票时票据出口的空闲值

    -- --- 归因输入与输出 ---
    challenge_id      TEXT        NOT NULL,
    -- 原始数字序列：归因算法的完整输入。存了它才能在换算法或换校准表后重算历史结论。
    digits            JSONB       NOT NULL,
    digit_count       INTEGER     NOT NULL,
    scores            JSONB,      -- 与各模型中心比较的分数向量
    part_attribution  TEXT,       -- 该份单独的归因结果
    cum_probability   DOUBLE PRECISION, -- 累计到该份的平均分经 softmax 后的概率
    temperature_tier  SMALLINT,   -- 校准温度档（按计入平均的有效份数选）

    -- 归因资料的版本。档 "1" 不是一份永不变的资料——换了校准表，同一序列会算出不同
    -- 概率。没有这些版本标识，历史结论既不能复核也不能重算，而「能重算」正是长期留存的理由。
    library_version   JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- --- 有效性与处置（分开记） ---
    parse_valid       BOOLEAN     NOT NULL,
    counted_in_average BOOLEAN    NOT NULL,
    -- 作废原因不止解析失败：候选未被上游接受、配置变更导致晚到结果作废、验证期间票过期，
    -- 都会留下格式完全正常的数字序列。它们可作观测样本，但不是该候选的有效指纹。
    invalid_reason    TEXT,
    latency_ms        INTEGER,
    output_tokens     INTEGER
);

CREATE INDEX IF NOT EXISTS kong_fingerprint_probes_verification_idx
    ON kong_fingerprint_probes (verification_id, part_index);
CREATE INDEX IF NOT EXISTS kong_fingerprint_probes_account_idx
    ON kong_fingerprint_probes (account_id, created_at DESC);

-- 本表只记「当时系统据以做出决定」的原始判定。离线重算的结果不得写回这里，
-- 否则事后分不清「当初判错了」与「现在用新校准表看法不同」。
