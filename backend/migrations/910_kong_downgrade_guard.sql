-- 降档哨兵（fork 专有）：事件表，以及保护 accounts.extra 里两个哨兵键的触发器。设计见 DESIGN-account-downgrade-guard.md。
-- 全部语句幂等，可重入。

-- ---------------------------------------------------------------------------
-- 1. 事件表
-- ---------------------------------------------------------------------------
-- hold（与关闭同一事务）、false_alarm（与误报撤销同一事务）、fp_start（写成功才发起指纹测试）是恢复控制状态
-- 所需的正式记录；impact 由续跑任务保证最终写入；其余是诊断事件。
CREATE TABLE IF NOT EXISTS kong_downgrade_guard_events (
    id          BIGSERIAL   PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    account_id  BIGINT      NOT NULL,
    -- 哨兵关闭操作的操作号；与某次关闭无关的事件为空串
    op          TEXT        NOT NULL DEFAULT '',
    kind        TEXT        NOT NULL,
    detail      JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS kong_downgrade_guard_events_account_idx
    ON kong_downgrade_guard_events (account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS kong_downgrade_guard_events_kind_idx
    ON kong_downgrade_guard_events (kind, created_at DESC);
-- 每个关闭操作只有一份影响汇总，重算用 upsert 覆盖。
CREATE UNIQUE INDEX IF NOT EXISTS kong_downgrade_guard_events_impact_uq
    ON kong_downgrade_guard_events (op) WHERE kind = 'impact';

-- ---------------------------------------------------------------------------
-- 2. accounts.extra 里的两个哨兵键
-- ---------------------------------------------------------------------------
-- kong_downgrade_guard_hold：哨兵持有关闭期间的记录，有它才表示"这次关闭是哨兵的"。
-- kong_schedulable_rev：调度开关版本，schedulable 每变一次（或状态变为 error）加一。
--
-- 只有哨兵能写这两个键：哨兵在自己的事务里 set_config('kong.guard_write', 'on', true)。其他写入（账号编辑、
-- 用量快照、边车写回、批量更新……）不管带来什么值，都还原成写入前的值；新建、复制账号时去掉。
--
-- schedulable 真正变化、或状态变为 error 时，版本加一；若不是哨兵的写入，同时删掉持有记录——人或别的流程
-- 动了开关，哨兵就失去这次关闭的所有权，进行中的结果作废。放在触发器里而不是逐个改上游的写入点，是为了覆盖
-- 上游现在与将来所有改动 schedulable 的路径。
CREATE OR REPLACE FUNCTION kong_downgrade_guard_accounts_bu()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    guard_write BOOLEAN := COALESCE(current_setting('kong.guard_write', true), '') = 'on';
    old_extra   JSONB := COALESCE(OLD.extra, '{}'::jsonb);
    old_rev     BIGINT := 0;
BEGIN
    IF old_extra ? 'kong_schedulable_rev' AND jsonb_typeof(old_extra -> 'kong_schedulable_rev') = 'number' THEN
        old_rev := (old_extra ->> 'kong_schedulable_rev')::BIGINT;
    END IF;

    IF NOT guard_write THEN
        NEW.extra := COALESCE(NEW.extra, '{}'::jsonb) - 'kong_downgrade_guard_hold' - 'kong_schedulable_rev';
        IF old_extra ? 'kong_downgrade_guard_hold' THEN
            NEW.extra := NEW.extra || jsonb_build_object('kong_downgrade_guard_hold', old_extra -> 'kong_downgrade_guard_hold');
        END IF;
        IF old_extra ? 'kong_schedulable_rev' THEN
            NEW.extra := NEW.extra || jsonb_build_object('kong_schedulable_rev', old_extra -> 'kong_schedulable_rev');
        END IF;
    END IF;

    IF NEW.schedulable IS DISTINCT FROM OLD.schedulable
       OR (NEW.status = 'error' AND OLD.status IS DISTINCT FROM 'error') THEN
        NEW.extra := COALESCE(NEW.extra, '{}'::jsonb) || jsonb_build_object('kong_schedulable_rev', old_rev + 1);
        IF NOT guard_write THEN
            NEW.extra := NEW.extra - 'kong_downgrade_guard_hold';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION kong_downgrade_guard_accounts_bi()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF COALESCE(current_setting('kong.guard_write', true), '') <> 'on' THEN
        NEW.extra := COALESCE(NEW.extra, '{}'::jsonb) - 'kong_downgrade_guard_hold' - 'kong_schedulable_rev';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS kong_downgrade_guard_accounts_bu ON accounts;
CREATE TRIGGER kong_downgrade_guard_accounts_bu
    BEFORE UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION kong_downgrade_guard_accounts_bu();

DROP TRIGGER IF EXISTS kong_downgrade_guard_accounts_bi ON accounts;
CREATE TRIGGER kong_downgrade_guard_accounts_bi
    BEFORE INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION kong_downgrade_guard_accounts_bi();
