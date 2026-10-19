// 租户查找的核心实现：cache-aside 模式。
//
// 为什么鉴权路径要加一层 Redis 缓存：这是每个请求都要走的高频路径，
// 如果每次都直接查 PostgreSQL，数据库连接数和延迟都扛不住。
// 缓存策略：
//  1. 先查 Redis：tenant:{api_key的哈希} -> 命中就直接用，不碰数据库
//  2. 没命中：查 PostgreSQL，查到后写回 Redis（带 TTL），下次请求就能命中缓存
//  3. 查不到（key 不存在或已吊销）：短 TTL 缓存一个"未找到"标记，
//     防止有人拿着无效 key 疯狂重试，每次都精准打到数据库上
//     （这类攻击叫 cache penetration / 缓存穿透，是真实存在的风险）
package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gateway/internal/auth"
	"gateway/internal/logger"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	cacheKeyPrefix   = "tenant:"
	cacheTTL         = 5 * time.Minute  // 正常命中的缓存有效期
	negativeCacheTTL = 10 * time.Second // "未找到"标记的缓存有效期，防止缓存穿透
	negativeMarker   = "__NOT_FOUND__"
)

type TenantStore struct {
	pool  *pgxpool.Pool
	redis *redis.Client
}

func NewTenantStore(pool *pgxpool.Pool, redisClient *redis.Client) *TenantStore {
	return &TenantStore{pool: pool, redis: redisClient}
}

// hashKey 对 API Key 做 SHA-256 哈希，数据库和缓存里都只存哈希值，
// 不存明文 key，即使数据库或 Redis 泄露，也拿不到原始 key。
func hashKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// Lookup 实现 middleware.TenantLookup 接口，供 AuthMiddleware 调用。
// 学习点：这个方法对外只暴露"查这个 key 对应哪个租户"这一件事，
// 调用方（中间件）完全不需要知道背后是先查 Redis 还是查数据库，
// 这是"关注点分离"的具体体现——中间件只关心结果，不关心实现细节。
func (s *TenantStore) Lookup(ctx context.Context, apiKey string) (*auth.Tenant, error) {
	hash := hashKey(apiKey)
	cacheKey := cacheKeyPrefix + hash

	// ---------- 第一步：查 Redis 缓存 ----------
	cached, err := s.redis.Get(ctx, cacheKey).Result()
	if err == nil {
		if cached == negativeMarker {
			return nil, auth.ErrTenantNotFound
		}
		var tenant auth.Tenant
		if jsonErr := json.Unmarshal([]byte(cached), &tenant); jsonErr != nil {
			// 缓存里的数据损坏了，不应该让整个请求失败，降级为"当作未命中"，
			// 直接去查数据库拿到正确结果。
			// 注意这里的 ctx 来自请求链路，所以这条日志会自动带上 request_id
			// 和租户信息（见 middleware 里把日志器挂进 context 的写法）。
			logger.FromContext(ctx).Warn("解析租户缓存失败，回退查询数据库", "err", jsonErr)
		} else {
			return &tenant, nil
		}
	} else if !errors.Is(err, redis.Nil) {
		// Redis 出错了（不是"没有这个 key"这种正常情况，而是连接失败之类的真错误），
		// 这里选择继续往下查数据库兜底，而不是直接报错——
		// 缓存只是加速手段，缓存不可用不应该导致整个鉴权失败。
		logger.FromContext(ctx).Warn("查询租户缓存失败，回退查询数据库", "err", err)
	}

	// ---------- 第二步：缓存未命中，查 PostgreSQL ----------
	tenant, err := s.queryFromDB(ctx, hash)
	if err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			// 查不到也要写缓存（负向缓存），防止无效 key 反复穿透到数据库
			_ = s.redis.Set(ctx, cacheKey, negativeMarker, negativeCacheTTL).Err()
			return nil, auth.ErrTenantNotFound
		}
		// 数据库真的出错了（连接失败等），这个错误要原样往上抛，
		// 让中间件决定怎么处理（鉴权场景下应该 fail-closed，拒绝请求）
		return nil, fmt.Errorf("查询数据库失败: %w", err)
	}

	// ---------- 第三步：把查到的结果写回缓存，下次请求就能命中 ----------
	if data, marshalErr := json.Marshal(tenant); marshalErr == nil {
		if setErr := s.redis.Set(ctx, cacheKey, data, cacheTTL).Err(); setErr != nil {
			// 写缓存失败不影响这次请求正常返回结果，只是下次请求还得再查一次数据库
			logger.FromContext(ctx).Warn("写入租户缓存失败", "err", setErr)
		}
	}

	return tenant, nil
}

// queryFromDB 从 PostgreSQL 查询指定哈希对应的有效租户。
// 学习点：这条 SQL 同时校验了三件事——key 存在、key 没被吊销、
// 租户状态是 active，任何一个条件不满足都返回"找不到"，
// 调用方不需要关心具体是哪个原因，统一按"鉴权失败"处理。
func (s *TenantStore) queryFromDB(ctx context.Context, keyHash string) (*auth.Tenant, error) {
	const query = `
		SELECT t.id, t.name, t.rps, t.burst, t.daily_cap
		FROM sui_api_keys k
		JOIN sui_tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = $1
		  AND k.revoked_at IS NULL
		  AND t.status = 'active'
	`

	var t auth.Tenant
	err := s.pool.QueryRow(ctx, query, keyHash).Scan(&t.ID, &t.Name, &t.RPS, &t.Burst, &t.DailyCap)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, auth.ErrTenantNotFound
		}
		return nil, err
	}
	return &t, nil
}

// InvalidateCache 主动删除某个 API Key 对应的缓存。
// 用途：吊销一把 key 之后，如果不主动清缓存，最长要等 cacheTTL（5分钟）
// 这把已经吊销的 key 才会真正失效，这段时间里它仍然能通过鉴权，
// 这在安全场景下是不可接受的延迟。吊销操作必须搭配主动失效缓存。
func (s *TenantStore) InvalidateCache(ctx context.Context, apiKey string) error {
	cacheKey := cacheKeyPrefix + hashKey(apiKey)
	return s.redis.Del(ctx, cacheKey).Err()
}

// ---------- 以下是管理类操作，供 cmd/seed 或未来的管理后台调用 ----------

type CreateTenantInput struct {
	ID       string
	Name     string
	RPS      float64
	Burst    int
	DailyCap int
}

// CreateTenant 创建一个新租户。用 ON CONFLICT 处理"重复运行 seed 脚本"
// 这种幂等性需求——重复执行同样的创建操作不应该报错，而是更新为最新值。
func (s *TenantStore) CreateTenant(ctx context.Context, in CreateTenantInput) error {
	const query = `
		INSERT INTO sui_tenants (id, name, rps, burst, daily_cap)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE
		SET name = EXCLUDED.name,
		    rps = EXCLUDED.rps,
		    burst = EXCLUDED.burst,
		    daily_cap = EXCLUDED.daily_cap
	`
	_, err := s.pool.Exec(ctx, query, in.ID, in.Name, in.RPS, in.Burst, in.DailyCap)
	if err != nil {
		return fmt.Errorf("创建/更新租户失败: %w", err)
	}
	return nil
}

// RegisterAPIKey 给指定租户签发一把新的 API Key（传入明文 key，内部转换成哈希存储）
func (s *TenantStore) RegisterAPIKey(ctx context.Context, tenantID, apiKey, label string) error {
	const query = `
		INSERT INTO sui_api_keys (key_hash, tenant_id, label)
		VALUES ($1, $2, $3)
		ON CONFLICT (key_hash) DO NOTHING
	`
	_, err := s.pool.Exec(ctx, query, hashKey(apiKey), tenantID, label)
	if err != nil {
		return fmt.Errorf("注册 API Key 失败: %w", err)
	}
	return nil
}

// RevokeAPIKey 吊销一把 key：数据库标记 + 主动清缓存，两步缺一不可
func (s *TenantStore) RevokeAPIKey(ctx context.Context, apiKey string) error {
	const query = `UPDATE sui_api_keys SET revoked_at = now() WHERE key_hash = $1 AND revoked_at IS NULL`
	_, err := s.pool.Exec(ctx, query, hashKey(apiKey))
	if err != nil {
		return fmt.Errorf("吊销 API Key 失败: %w", err)
	}
	return s.InvalidateCache(ctx, apiKey)
}
