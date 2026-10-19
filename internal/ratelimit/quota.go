// 配额（每日累计调用次数）的 Redis 实现。
//
// 跟限流不同，配额不需要复杂脚本——Redis 的 INCR 命令本身就是原子的，
// "读取当前值 -> 加一 -> 写回"这个过程 Redis 内部保证了原子性，
// 不需要像令牌桶那样手动编排多个命令的原子执行。
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"gateway/internal/logger"

	"github.com/redis/go-redis/v9"
)

// refundScript 退还一次已经预扣的配额，并保证计数不会被退成负数。
//
// 为什么要用 Lua，而不是"DECR 完在 Go 里判断是否小于 0 再 SET 0"：
// 这两种操作之间有一个竞态窗口——退款线程刚 DECR 到 -1、还没 SET 回 0，
// 另一个请求的 INCR 就把它加成了 0，于是"钳位"把本该是 1 的计数写成了 0，
// 平白送出去一次额度。Lua 在 Redis 端原子执行，不存在这个窗口。
//
// 为什么小于 0 时要补 EXPIREAT，而不是直接 SET 0 了事：
// 如果退款时 key 已经不存在（比如过期了，或者预扣根本没成功），
// DECR 会新建一个值为 -1 的 key，此时它的 TTL 是 -1（永不过期）。
// 这样就会在 Redis 里留下一个"永远不会过期的 0 计数"，
// 第二天这个 key 仍然存在，计数就再也回不到正常的每日重置逻辑上。
// 所以这里显式把它设置成 0 并补上"今天结束"的过期时间。
const refundScript = `
local key = KEYS[1]
local expireAt = tonumber(ARGV[1])

local left = redis.call("DECR", key)
if left < 0 then
    redis.call("SET", key, 0)
    redis.call("EXPIREAT", key, expireAt)
    left = 0
end
return left
`

type RedisQuotaTracker struct {
	client *redis.Client
	script *redis.Script
}

func NewRedisQuotaTracker(client *redis.Client) *RedisQuotaTracker {
	return &RedisQuotaTracker{
		client: client,
		script: redis.NewScript(refundScript),
	}
}

// quotaKey 拼出"租户 + 日期"维度的 Redis key。
// 用日期做 key 的一部分，天然实现"第二天从 0 开始"，不依赖定时任务清零。
func quotaKey(tenantID string, now time.Time) string {
	return fmt.Sprintf("quota:%s:%s", tenantID, now.Format("2006-01-02"))
}

// Consume 尝试为这个租户消耗一次今日配额。
// 返回 (是否允许通过, error)。
func (qt *RedisQuotaTracker) Consume(ctx context.Context, tenantID string, dailyCap int) (bool, error) {
	now := time.Now()
	key := quotaKey(tenantID, now)

	count, err := qt.client.Incr(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("配额计数失败: %w", err)
	}

	// 学习点：只在第一次自增（count==1）时设置过期时间，而不是每次都设置。
	// 原因：如果每次请求都重新 EXPIRE，某个租户如果一直高频请求，
	// 这个 key 的过期时间会被不断往后推，永远不会在"今天结束"时过期，
	// 导致第二天的计数错误地叠加在昨天的基础上。只在 key 刚被创建
	// （第一次自增到 1）时设置一次到"今天结束"的绝对过期时间，
	// 之后的自增操作不再触碰过期时间，保证每天定时准确清零。
	if count == 1 {
		if err := qt.client.ExpireAt(ctx, key, endOfDay(now)).Err(); err != nil {
			// 设置过期时间失败不应该导致这次请求直接失败——
			// 顶多是这个 key 不会自动清理，用降级思路处理：记日志但不阻断请求。
			logger.FromContext(ctx).Warn("设置配额 key 过期时间失败",
				"tenant", tenantID,
				"err", err,
			)
		}
	}

	return count <= int64(dailyCap), nil
}

// Refund 退还一次配额，用于"预扣成功但请求最终失败（5xx）"的场景。
//
// 注意这不等于"绕过配额"：它只退还这一次预扣，不会把计数减到 0 以下的
// 负数（见 refundScript 里的钳位逻辑），所以反复重试失败请求也不会
// 变相获得无限额度——失败一次退一次，账目始终是平的。
func (qt *RedisQuotaTracker) Refund(ctx context.Context, tenantID string) (int64, error) {
	key := quotaKey(tenantID, time.Now())

	left, err := qt.script.Run(ctx, qt.client, []string{key}, endOfDay(time.Now()).Unix()).Int64()
	if err != nil {
		return 0, fmt.Errorf("配额退款失败: %w", err)
	}
	return left, nil
}

// Usage 查询某个租户今日已使用的配额（用于给调用方展示"还剩多少额度"这类场景）
func (qt *RedisQuotaTracker) Usage(ctx context.Context, tenantID string) (int64, error) {
	key := quotaKey(tenantID, time.Now())

	val, err := qt.client.Get(ctx, key).Int64()
	if err == redis.Nil {
		return 0, nil // 今天还没有任何请求，用量是 0
	}
	if err != nil {
		return 0, fmt.Errorf("查询配额用量失败: %w", err)
	}
	return val, nil
}

// endOfDay 返回指定时间所在自然日的 23:59:59，用作 Redis key 的绝对过期时间。
// 传入 now 而不是在函数内部取 time.Now()，是为了让"算出来的过期时间"
// 和"拼 key 时用的日期"一定属于同一天（跨零点的一瞬间不会错配）。
func endOfDay(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())
}
