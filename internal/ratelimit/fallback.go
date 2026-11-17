package ratelimit

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"
)

// 本文件解决的问题：Redis 故障时，限流和配额都选择 fail-open（放行）。
// 这对"保证可用性"是对的，但意味着故障期间租户可以无限调用，GPU 会被直接打满。
//
// 折中做法：Redis 不可用时，不是无条件放行，而是退化成"每个副本各自在内存里限"。
// 每个副本只拿总额度的 1/N（N = 预期副本数），N 个副本加起来大致等于总额度。
// 它不精确（副本数变化、负载不均都会带来误差），但把"无限"变成了"有界"。
//
// 已知局限（有意接受）：
//   - 本地计数在进程重启后丢失；
//   - 降级期间的本地用量不会回写 Redis，恢复后租户会重新获得一份完整额度。
// 要做到精确，需要持久化的 WAL 或回补机制，那属于计费系统的范畴。

// Limiter 是被 FallbackLimiter 包装的限流器接口（*RedisLimiter 满足它）。
type Limiter interface {
	Allow(ctx context.Context, tenantID string, rps float64, burst int) (bool, error)
}

// FallbackNotify 在每次发生降级时被调用，用来记指标和（采样的）日志。
type FallbackNotify func(tenantID string, err error)

// FallbackLimiter：优先用 primary（Redis），primary 出错时退化为本地令牌桶。
// 它永远不会把 primary 的错误向上传递：要么是 primary 的决定，要么是本地的决定。
type FallbackLimiter struct {
	primary Limiter
	share   float64
	notify  FallbackNotify
	now     func() time.Time

	mu      sync.Mutex
	buckets map[string]*localBucket
}

type localBucket struct {
	tokens float64
	last   time.Time
}

// NewFallbackLimiter 创建带兜底的限流器。expectedReplicas 是网关副本数（<1 按 1 处理），
// 本地桶的速率和容量都按 1/expectedReplicas 缩放（容量至少为 1，保证租户不会被完全封死）。
func NewFallbackLimiter(primary Limiter, expectedReplicas int, notify FallbackNotify) *FallbackLimiter {
	if expectedReplicas < 1 {
		expectedReplicas = 1
	}
	return &FallbackLimiter{
		primary: primary,
		share:   1 / float64(expectedReplicas),
		notify:  notify,
		now:     time.Now,
		buckets: make(map[string]*localBucket),
	}
}

func (f *FallbackLimiter) Allow(ctx context.Context, tenantID string, rps float64, burst int) (bool, error) {
	ok, err := f.primary.Allow(ctx, tenantID, rps, burst)
	if err == nil {
		return ok, nil
	}
	if f.notify != nil {
		f.notify(tenantID, err)
	}
	return f.allowLocal(tenantID, rps, burst), nil
}

func (f *FallbackLimiter) allowLocal(tenantID string, rps float64, burst int) bool {
	rate := rps * f.share
	capacity := math.Max(1, math.Ceil(float64(burst)*f.share))
	now := f.now()

	f.mu.Lock()
	defer f.mu.Unlock()

	b, ok := f.buckets[tenantID]
	if !ok {
		b = &localBucket{tokens: capacity, last: now}
		f.buckets[tenantID] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens = math.Min(capacity, b.tokens+elapsed*rate)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// ---------------------------------------------------------------------------

// QuotaBackend 是被 FallbackQuota 包装的配额接口（*RedisQuotaTracker 满足它）。
type QuotaBackend interface {
	Consume(ctx context.Context, tenantID string, dailyCap int) (Reservation, bool, error)
	Refund(ctx context.Context, res Reservation) (int64, error)
}

// FallbackQuota：优先用 primary（Redis），primary 出错时退化为本地每日计数。
// 本地 reservation 的 Key 带 "local:" 前缀，Refund 据此判断该退给本地还是 Redis——
// 即使 Redis 在这次请求处理期间恢复了，本地预扣的款也不会被错误地退到 Redis 上。
type FallbackQuota struct {
	primary  QuotaBackend
	replicas int
	notify   FallbackNotify
	now      func() time.Time
	loc      *time.Location

	mu     sync.Mutex
	day    string
	counts map[string]int64
}

const localKeyPrefix = "local:"

// FallbackQuotaOption 调整 FallbackQuota 的行为。
type FallbackQuotaOption func(*FallbackQuota)

// WithFallbackClock 注入时钟（测试用）。
func WithFallbackClock(now func() time.Time) FallbackQuotaOption {
	return func(f *FallbackQuota) { f.now = now }
}

// WithFallbackLocation 指定"一天"的边界时区，必须与 RedisQuotaTracker 保持一致。
func WithFallbackLocation(loc *time.Location) FallbackQuotaOption {
	return func(f *FallbackQuota) {
		if loc != nil {
			f.loc = loc
		}
	}
}

func NewFallbackQuota(primary QuotaBackend, expectedReplicas int, notify FallbackNotify, opts ...FallbackQuotaOption) *FallbackQuota {
	if expectedReplicas < 1 {
		expectedReplicas = 1
	}
	f := &FallbackQuota{
		primary:  primary,
		replicas: expectedReplicas,
		notify:   notify,
		now:      time.Now,
		loc:      time.UTC,
		counts:   make(map[string]int64),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (f *FallbackQuota) Consume(ctx context.Context, tenantID string, dailyCap int) (Reservation, bool, error) {
	res, ok, err := f.primary.Consume(ctx, tenantID, dailyCap)
	if err == nil {
		return res, ok, nil
	}
	if f.notify != nil {
		f.notify(tenantID, err)
	}
	res, ok = f.consumeLocal(tenantID, dailyCap)
	return res, ok, nil
}

func (f *FallbackQuota) consumeLocal(tenantID string, dailyCap int) (Reservation, bool) {
	now := f.now().In(f.loc)
	day := now.Format("2006-01-02")
	key := localKeyPrefix + quotaKey(tenantID, now)

	// 本地单副本的上限 = ceil(总额度 / 副本数)；dailyCap <= 0 表示不限额。
	var localCap int64
	if dailyCap > 0 {
		localCap = int64(math.Ceil(float64(dailyCap) / float64(f.replicas)))
		if localCap < 1 {
			localCap = 1
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if day != f.day { // 跨天：整个本地账本作废（旧 key 的退款会变成 no-op，没有副作用）
		f.day = day
		f.counts = make(map[string]int64)
	}

	cur := f.counts[key]
	res := Reservation{Key: key, Used: cur, ResetAt: nextMidnight(now)}
	if localCap > 0 && cur >= localCap {
		return res, false
	}
	cur++
	f.counts[key] = cur
	res.Used = cur
	return res, true
}

func (f *FallbackQuota) Refund(ctx context.Context, res Reservation) (int64, error) {
	if !strings.HasPrefix(res.Key, localKeyPrefix) {
		return f.primary.Refund(ctx, res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur := f.counts[res.Key]; cur > 0 {
		f.counts[res.Key] = cur - 1
		return cur - 1, nil
	}
	return 0, nil
}
