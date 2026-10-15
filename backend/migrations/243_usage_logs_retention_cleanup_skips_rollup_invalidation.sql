-- 保留清理删掉的明细都在新的保留边界之前：整天被删的日桶读取时本来就不取，边界那天由清理之后单独重算。
-- 若删除触发器照旧把 closed_before 降到被删的日子，下一次同步会从保留期最早那天起把日桶重算到今天，
-- 期间与每一批删除一样锁着 usage_group_rollup_state，挡住插入触发器、进而挡住所有新明细的写入。
-- 保留清理的事务里设置 sub2api.usage_logs_retention_cleanup = 'on'（SET LOCAL），删除触发器见到它就直接放行；
-- 其余删除（级联删除、后台用量清理任务）不设置，照旧失效日桶。

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    affected_date DATE;
    published_before DATE;
    configured_timezone TEXT := current_setting('TimeZone');
BEGIN
    IF TG_OP = 'DELETE'
        AND current_setting('sub2api.usage_logs_retention_cleanup', true) = 'on' THEN
        RETURN OLD;
    END IF;

    IF TG_OP = 'DELETE' THEN
        affected_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
    ELSE
        IF OLD.group_id IS NULL THEN
            affected_date := (NEW.created_at AT TIME ZONE configured_timezone)::date;
        ELSIF NEW.group_id IS NULL THEN
            affected_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
        ELSE
            affected_date := LEAST(
                (OLD.created_at AT TIME ZONE configured_timezone)::date,
                (NEW.created_at AT TIME ZONE configured_timezone)::date
            );
        END IF;
    END IF;

    SELECT closed_before
    INTO published_before
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR UPDATE;

    IF published_before > affected_date THEN
        UPDATE usage_group_rollup_state
        SET closed_before = LEAST(closed_before, affected_date),
            updated_at = NOW()
        WHERE id = 1;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
