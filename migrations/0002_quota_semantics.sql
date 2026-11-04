-- =============================================================
-- 明确 daily_cap 的语义，并在数据库层挡住非法值。
--
--   daily_cap > 0  : 每日调用上限
--   daily_cap = 0  : 不限额（Redis 里仍然计数，方便展示今日用量）
--   daily_cap < 0  : 非法，由下面的 CHECK 约束拒绝
--
-- 旧代码里 0 会被当成"上限为 0"，导致该租户所有请求都 429，
-- 与 0001 建表脚本里"0 表示不限制"的注释自相矛盾。
--
-- ⚠ 注意：sui_tenants.daily_cap 的默认值是 0，也就是说"漏填 daily_cap 的租户"
-- 现在等于不限额。如果你希望"漏填 = 必须显式指定"，可以改成
--   ALTER TABLE sui_tenants ALTER COLUMN daily_cap DROP DEFAULT;
-- 让 INSERT 漏填时直接报错，而不是悄悄放开额度。
--
-- 执行方式同 0001：
--   docker exec -i gateway-postgres psql -U gateway -d gateway_db \
--     < migrations/0002_quota_semantics.sql
-- =============================================================

ALTER TABLE sui_tenants
    DROP CONSTRAINT IF EXISTS sui_tenants_daily_cap_check;

ALTER TABLE sui_tenants
    ADD CONSTRAINT sui_tenants_daily_cap_check CHECK (daily_cap >= 0);

COMMENT ON COLUMN sui_tenants.daily_cap IS
    '每日调用配额上限；0 表示不限额（仍计数）；负数非法';
