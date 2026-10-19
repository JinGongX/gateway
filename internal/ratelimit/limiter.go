// Package ratelimit 提供基于 Redis 的分布式限流和配额实现。
//
// 为什么要从内存版换成 Redis 版：
// 内存版的 map[tenantID]*rate.Limiter 只在单个进程内有效。如果网关
// 部署了多个副本（比如 k8s 里 replicas: 3），每个副本各自维护一份
// 独立的令牌桶，同一个租户的请求可能被负载均衡分散到 3 个副本上，
// 每个副本都认为"这个租户还有额度"，实际上加起来早就超限了。
// Redis 是所有副本共享的外部存储，限流状态统一存一份，才能做到
// "无论请求落在哪个副本，限流结果都是一致的"。
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript 用 Lua 脚本在 Redis 端原子执行"读取令牌数 -> 按经过
// 时间补充 -> 判断是否够扣 -> 扣减 -> 写回"整个流程。
//
// 关键点：如果拆成多条 Go 代码分别执行 HMGET 再 HMSET，中间会有
// 竞态窗口——比如两个并发请求同时 HMGET 读到"还剩 1 个令牌"，
// 都各自认为可以通过，实际上应该只放行 1 个。Lua 脚本在 Redis 里
// 是单线程原子执行的，脚本跑完之前不会被其他命令打断，从根本上
// 消除了这类竞态条件。
const tokenBucketScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])       -- 每秒补充速率 (RPS)
local burst = tonumber(ARGV[2])      -- 桶容量 (Burst)
local now = tonumber(ARGV[3])        -- 当前时间戳（秒，带小数）
local requested = 1

local bucket = redis.call("HMGET", key, "tokens", "last_refill")
local tokens = tonumber(bucket[1])
local last_refill = tonumber(bucket[2])

-- 第一次见到这个租户，桶是满的
if tokens == nil then
    tokens = burst
    last_refill = now
end

-- 按经过的时间线性补充令牌，不超过桶容量上限
local elapsed = now - last_refill
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
if tokens >= requested then
    tokens = tokens - requested
    allowed = 1
end

redis.call("HMSET", key, "tokens", tokens, "last_refill", now)
-- 一小时没有新请求就让 Redis 自动过期回收这个 key，
-- 防止长期不活跃的租户在 Redis 里堆积大量僵尸 key
redis.call("EXPIRE", key, 3600)

return allowed
`

// RedisLimiter 是限流器的 Redis 实现，替代内存版 RateLimiter。
type RedisLimiter struct {
	client *redis.Client
	script *redis.Script
}

func NewRedisLimiter(client *redis.Client) *RedisLimiter {
	return &RedisLimiter{
		client: client,
		script: redis.NewScript(tokenBucketScript),
	}
}

// Allow 判断这个租户当前请求是否被放行。
// 返回值：(是否放行, 是否发生了 Redis 层面的错误)
// 调用方需要显式处理"Redis 出错"和"限流拒绝"这两种不同的情况，
// 不能混为一谈——见 middleware/ratelimit.go 里的降级策略。
func (rl *RedisLimiter) Allow(ctx context.Context, tenantID string, rps float64, burst int) (bool, error) {
	key := fmt.Sprintf("ratelimit:%s", tenantID)
	now := float64(time.Now().UnixNano()) / 1e9

	result, err := rl.script.Run(ctx, rl.client, []string{key}, rps, burst, now).Result()
	if err != nil {
		return false, fmt.Errorf("执行限流 Lua 脚本失败: %w", err)
	}

	allowed, ok := result.(int64)
	if !ok {
		return false, fmt.Errorf("限流脚本返回了意料之外的类型: %T", result)
	}
	return allowed == 1, nil
}
