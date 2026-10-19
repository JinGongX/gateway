-- =============================================================
-- 租户与 API Key 的建表脚本（对应 internal/pgstore 里的 SQL）
--
-- 执行方式：
--   psql "$POSTGRES_DSN" -f migrations/0001_init_tenants.sql
-- 或者：
--   docker exec -i gateway-postgres psql -U gateway -d gateway_db \
--     < migrations/0001_init_tenants.sql
--
-- 表名前缀 sui_ 是历史遗留，代码里的 SQL 写死了这个前缀，
-- 改名要同时改 internal/pgstore/tenant_store.go。
-- =============================================================

-- 租户：限流/配额参数挂在租户上，鉴权后从这里取
CREATE TABLE IF NOT EXISTS sui_tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT        NOT NULL,
    rps        DOUBLE PRECISION NOT NULL DEFAULT 1,   -- 令牌桶速率：每秒补充的令牌数
    burst      INTEGER     NOT NULL DEFAULT 1,        -- 令牌桶容量：允许的瞬时突发
    daily_cap  INTEGER     NOT NULL DEFAULT 0,        -- 每日调用配额上限（0 表示不限制由业务侧判断）
    status     TEXT        NOT NULL DEFAULT 'active', -- active / suspended；只有 active 才放行
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT sui_tenants_status_check CHECK (status IN ('active', 'suspended'))
);

-- API Key：只存明文 key 的 SHA-256，泄露数据库也拿不到原始 key
CREATE TABLE IF NOT EXISTS sui_api_keys (
    key_hash   TEXT PRIMARY KEY,                      -- sha256(明文 key) 的 hex
    tenant_id  TEXT        NOT NULL REFERENCES sui_tenants (id) ON DELETE CASCADE,
    label      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ                            -- 非 NULL 表示已吊销，鉴权时会被排除
);

-- 鉴权 SQL 的过滤条件是 key_hash + revoked_at，key_hash 是主键已经够用；
-- 这个索引服务于"按租户列出/吊销该租户的全部 key"这类管理操作。
CREATE INDEX IF NOT EXISTS idx_sui_api_keys_tenant_id ON sui_api_keys (tenant_id);
