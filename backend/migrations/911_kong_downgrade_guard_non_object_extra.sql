-- 降档哨兵（fork 专有）：accounts.extra 不是 JSON 对象时的处理。设计见 DESIGN-account-downgrade-guard.md。
-- 910 的触发器对 extra 直接做 `- 'key'` 与 `||`：extra 是标量（含 JSON null）时 `-` 报 cannot delete from scalar，
-- 是数组时 `||` 把它改成数组拼接。应用自己只写对象，OpenAI 账号的 extra 还由上游迁移 175 的触发器保证是对象；
-- 非对象只会出现在其他平台的账号上、来自手工 SQL（上游的测试会这样写）。
-- 这里只在不牵涉哨兵状态时原样放行：旧值里没有两个哨兵键、不是哨兵的写入、也不改调度开关或置错误。其余情况仍然
-- 报错拒绝——非对象里放不下持有记录与版本，放行会丢掉它们，版本一旦清零，作废了的旧检测结果、旧编辑就会重新生效。
-- 旧值不是对象时按空对象处理。其余逻辑与 910 相同。全部语句幂等，可重入；两个触发器沿用 910 建的，只替换函数。

CREATE OR REPLACE FUNCTION kong_downgrade_guard_accounts_bu()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    guard_write BOOLEAN := COALESCE(current_setting('kong.guard_write', true), '') = 'on';
    old_extra   JSONB := COALESCE(OLD.extra, '{}'::jsonb);
    old_rev     BIGINT := 0;
BEGIN
    IF jsonb_typeof(old_extra) <> 'object' THEN
        old_extra := '{}'::jsonb;
    END IF;

    IF jsonb_typeof(COALESCE(NEW.extra, '{}'::jsonb)) <> 'object' THEN
        IF guard_write
           OR old_extra ?| ARRAY['kong_downgrade_guard_hold', 'kong_schedulable_rev']
           OR NEW.schedulable IS DISTINCT FROM OLD.schedulable
           OR (NEW.status = 'error' AND OLD.status IS DISTINCT FROM 'error') THEN
            RAISE EXCEPTION 'accounts.extra must be a JSON object to keep the downgrade guard state (account %)', OLD.id;
        END IF;
        RETURN NEW;
    END IF;

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
    IF COALESCE(current_setting('kong.guard_write', true), '') <> 'on'
       AND jsonb_typeof(COALESCE(NEW.extra, '{}'::jsonb)) = 'object' THEN
        NEW.extra := COALESCE(NEW.extra, '{}'::jsonb) - 'kong_downgrade_guard_hold' - 'kong_schedulable_rev';
    END IF;
    RETURN NEW;
END;
$$;
