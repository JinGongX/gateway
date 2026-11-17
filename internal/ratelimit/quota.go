// 配额（每日累计调用次数）的 Redis 实现。
//
// 这一版相对旧实现的核心变化：
//  1. 「检查 + 扣减 + 补过期时间」合并进一个 Lua 脚本，在 Redis 端原子完成；
//  2. 超限被拒绝的请求不再自增计数（旧实现 INCR 后才比较，被拒请求会一直把计数往上推，
//     导致后续的失败退款名义上"退了"、实际额度仍然是满的）；
//  3. Consume 返回 Reservation（本次扣的是哪个 key），Refund 按它退款，
//     跨零点时不会把款退到"新一天"的 key 上；
//  4. 日期与过期时间统一按配置的时区（默认 UTC）计算，多副本不会因为机器时区不同而各记各的账。
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// consumeScript 原子地完成：读当前值 -> 判断是否超限 -> 自增 -> 保证 key 带过期时间。
//
// ARGV[1] = dailyCap；<= 0 表示「不限额」（仍然计数，方便展示今日用量）
// ARGV[2] = 绝对过期时间（unix 秒）
//
// 返回 {是否放行(1/0), 当前计数}。被拒绝时计数保持不变。
//
// "cur == 1 或 TTL < 0 就补 EXPIREAT" 的原因：
//   - cur == 1：key 刚创建，设置过期时间；
//   - TTL < 0：key 存在但没有过期时间（比如上一次进程在 INCR 与 EXPIRE 之间崩溃，
//     旧实现会留下永不过期的 key），这里顺手修复，实现自愈。
const consumeScript = `
local key = KEYS[1]
local cap = tonumber(ARGV[1])
local expireAt = tonumber(ARGV[2])

local cur = tonumber(redis.call("GET", key) or "0")
if cap > 0 and cur >= cap then
    return {0, cur}
end

cur = redis.call("INCR", key)
if cur == 1 or redis.call("TTL", key) < 0 then
    redis.call("EXPIREAT", key, expireAt)
end
return {1, cur}
`

// refundScript 退还一次预扣：只在 key 存在且 > 0 时才 DECR。
//
// 对比旧版"先 DECR，小于 0 再 SET 0 + EXPIREAT"：
//   - key 不存在时旧版会先创建一个 -1 的 key 再修补；新版根本不碰它，
//     不会产生任何新 key，也就不存在"永不过期的僵尸 key"；
//   - 判断与 DECR 在同一个脚本里，没有"DECR 到 -1 后被别人 INCR 成 0"的竞态窗口；
//   - DECR 不改变已有的 TTL，过期时间保持不变。
const refundScript = `
local cur = tonumber(redis.call("GET", KEYS[1]) or "0")
if cur > 0 then
    return redis.call("DECR", KEYS[1])
end
return 0
`

// expireGrace 是在「次日零点」之外多留的缓冲时间。
// key 名里已经带了日期，到了第二天自然不会被复用，所以多留一会儿没有副作用；
// 它的作用是：一个在 23:59:59 开始、00:00:05 才失败的请求，退款时 key 还在，
// 账才退得回去。
const expireGrace = time.Hour

// Reservation 记录一次成功预扣"扣在了哪里"，退款时原样传回。
type Reservation struct {
	Key     string    // 本次扣减的 Redis key（含租户和日期）
	Used    int64     // 扣减后的今日累计用量（含本次）
	ResetAt time.Time // 今日配额的重置时刻（下一个零点），用于 429 的 Retry-After
}

// QuotaOption 用来在构造 tracker 时调整行为。
type QuotaOption func(*RedisQuotaTracker)

// WithQuotaLocation 指定"一天"的边界所在的时区。默认 UTC。
// 如果配额按租户的自然日（比如北京时间）结算，传 time.LoadLocation("Asia/Shanghai")。
// 同一个集群的所有副本必须使用同一个时区，否则各副本会写到不同的 key。
func WithQuotaLocation(loc *time.Location) QuotaOption {
	return func(q *RedisQuotaTracker) {
		if loc != nil {
			q.loc = loc
		}
	}
}

// WithQuotaClock 注入时钟，主要给测试用（模拟跨零点）。
func WithQuotaClock(now func() time.Time) QuotaOption {
	return func(q *RedisQuotaTracker) {
		if now != nil {
			q.now = now
		}
	}
}

type RedisQuotaTracker struct {
	client  *redis.Client
	consume *redis.Script
	refund  *redis.Script
	now     func() time.Time
	loc     *time.Location
}

func NewRedisQuotaTracker(client *redis.Client, opts ...QuotaOption) *RedisQuotaTracker {
	qt := &RedisQuotaTracker{
		client:  client,
		consume: redis.NewScript(consumeScript),
		refund:  redis.NewScript(refundScript),
		now:     time.Now,
		loc:     time.UTC,
	}
	for _, opt := range opts {
		opt(qt)
	}
	return qt
}

// quotaKey 拼出"租户 + 日期"维度的 Redis key。t 必须已经转换到配额时区。
func quotaKey(tenantID string, t time.Time) string {
	return fmt.Sprintf("quota:%s:%s", tenantID, t.Format("2006-01-02"))
}

// nextMidnight 返回 t 所在自然日的下一个零点（与 t 同时区）。
func nextMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

// Consume 尝试为这个租户预扣一次今日配额。
//
// dailyCap <= 0 表示不限额：永远放行，但仍然计数。
// 返回的 Reservation 只有在 (allowed == true, err == nil) 时才应该被拿去退款；
// 出错或被拒绝时没有任何东西被扣，调用方不应退款。
func (qt *RedisQuotaTracker) Consume(ctx context.Context, tenantID string, dailyCap int) (Reservation, bool, error) {
	now := qt.now().In(qt.loc)
	reset := nextMidnight(now)
	res := Reservation{Key: quotaKey(tenantID, now), ResetAt: reset}
	expireAt := reset.Add(expireGrace).Unix()

	out, err := qt.consume.Run(ctx, qt.client, []string{res.Key}, dailyCap, expireAt).Slice()
	if err != nil {
		return Reservation{}, false, fmt.Errorf("配额计数失败: %w", err)
	}
	if len(out) != 2 {
		return Reservation{}, false, fmt.Errorf("配额脚本返回了意料之外的长度: %d", len(out))
	}
	allowed, ok1 := out[0].(int64)
	used, ok2 := out[1].(int64)
	if !ok1 || !ok2 {
		return Reservation{}, false, fmt.Errorf("配额脚本返回了意料之外的类型: %T, %T", out[0], out[1])
	}

	res.Used = used
	return res, allowed == 1, nil
}

// Refund 按 Reservation 退还一次预扣，用于"预扣成功但请求最终失败（5xx）"的场景。
//
// 这不等于"绕过配额"：它只退还这一次预扣，计数不会被减到 0 以下，
// 失败一次退一次，账目始终是平的。空 Reservation 是 no-op。
func (qt *RedisQuotaTracker) Refund(ctx context.Context, res Reservation) (int64, error) {
	if res.Key == "" {
		return 0, nil
	}
	left, err := qt.refund.Run(ctx, qt.client, []string{res.Key}).Int64()
	if err != nil {
		return 0, fmt.Errorf("配额退款失败: %w", err)
	}
	return left, nil
}

// Usage 查询某个租户今日已使用的配额。
func (qt *RedisQuotaTracker) Usage(ctx context.Context, tenantID string) (int64, error) {
	key := quotaKey(tenantID, qt.now().In(qt.loc))

	val, err := qt.client.Get(ctx, key).Int64()
	if err == redis.Nil {
		return 0, nil // 今天还没有任何请求，用量是 0
	}
	if err != nil {
		return 0, fmt.Errorf("查询配额用量失败: %w", err)
	}
	return val, nil
}
