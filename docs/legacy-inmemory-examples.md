# 旧的内存版实现存档（仅作学习对照，不参与编译）

以下三份代码是限流/配额/鉴权还是单进程内存版时的实现，
在升级到 Redis 共享状态后已经退役。保留在这里是为了对照
"内存版 vs Redis 版"的差别，不参与编译，可以安全忽略。

## internal/middleware/auth.go（内存版）

```go
// 对应【阶段二：鉴权中间件】
//
// 学习点回顾：这里验证的是 API Key，不是 JWT。
//   - JWT 适合"验证身份"：令牌自带签名，服务端不查库就能验证真伪。
//   - API Key 适合"查找配置"：网关需要查出这个租户的限流/配额参数，
//     这些是动态数据，光解析一个令牌拿不到，必须查表。
//
// 两者不是二选一的竞争关系，是解决不同问题的工具。
package middleware

import (
	"net/http"
	"strings"

	"gateway/internal/auth"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware 校验请求头里的 API Key，找到对应租户后存入 context。
// 找不到或者格式不对，直接 401 并终止后续处理（c.Abort 系列方法）。
func AuthMiddleware(store *auth.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing or malformed authorization header",
			})
			return
		}

		key := strings.TrimPrefix(header, "Bearer ")
		tenant, ok := store.Lookup(key)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid api key",
			})
			return
		}

		c.Set(TenantContextKey, tenant)
		c.Next()
	}
}
```

## internal/middleware/ratelimit.go（内存版）

```go
// 对应【阶段三：限流】
//
// 学习点回顾：限流控制的是"瞬时速率"——同一时刻涌入太多请求要拒绝一部分。
// 验证限流效果必须用并发请求测试，顺序 for 循环打 curl 因为请求间隔
// 往往大于令牌补充周期，会看不出限流效果，这个坑你已经亲自踩过一次了。
package middleware

import (
	"net/http"
	"sync"

	"gateway/internal/auth"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimiter 按租户 ID 维护独立的令牌桶，做到"按 API Key 隔离"。
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{limiters: make(map[string]*rate.Limiter)}
}

// getLimiter 惰性创建：第一次见到这个租户时才创建对应的令牌桶，
// 之后复用同一个实例（否则每次请求都 new 一个新桶，等于限流形同虚设）。
func (rl *RateLimiter) getLimiter(tenantID string, rps float64, burst int) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if lim, ok := rl.limiters[tenantID]; ok {
		return lim
	}
	lim := rate.NewLimiter(rate.Limit(rps), burst)
	rl.limiters[tenantID] = lim
	return lim
}

func RateLimitMiddleware(rl *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantVal, ok := c.Get(TenantContextKey)
		if !ok {
			// 这里理论上不会发生，因为 AuthMiddleware 必须在这之前执行。
			// 但企业级代码不能假设"理论上不会发生"，要显式处理防御一下。
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "tenant not found in context, check middleware order",
			})
			return
		}
		tenant := tenantVal.(*auth.Tenant)

		lim := rl.getLimiter(tenant.ID, tenant.RPS, tenant.Burst)
		if !lim.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
			})
			return
		}
		c.Next()
	}
}
```

## internal/middleware/quota.go（内存版）

```go
// 对应【阶段三：配额】
//
// 学习点回顾：配额和限流是两个独立的机制。
// - 限流：这一秒能过几个请求（瞬时速率）
// - 配额：今天总共能用多少次（累计总量）
// 两者互不依赖，一个请求要同时通过限流检查和配额检查才能放行。
package middleware

import (
	"net/http"
	"sync"
	"time"

	"gateway/internal/auth"

	"github.com/gin-gonic/gin"
)

type quotaEntry struct {
	count     int
	resetDate string // 格式 "2006-01-02"，Go 的参考时间写法，不是随便写的占位日期
}

// QuotaTracker 按租户 ID 记录当日已用次数，日期变化时自动重置。
type QuotaTracker struct {
	mu      sync.Mutex
	entries map[string]*quotaEntry
}

func NewQuotaTracker() *QuotaTracker {
	return &QuotaTracker{entries: make(map[string]*quotaEntry)}
}

// consume 尝试消耗一次配额，成功返回 true，配额耗尽返回 false。
func (q *QuotaTracker) consume(tenantID string, dailyCap int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	e, ok := q.entries[tenantID]
	if !ok || e.resetDate != today {
		// 学习点：这里没有单独起一个定时任务去"清零"，而是惰性重置——
		// 只有当这个租户真的发起新请求时，才检查日期是否变化并重置。
		// 好处是不需要额外的后台 goroutine，坏处是如果某个租户一整天
		// 都没有请求，它的计数条目会一直留在内存里不被清理。
		// 生产环境如果租户数量很大，可以加一个定时清理任务防止内存缓慢增长。
		e = &quotaEntry{count: 0, resetDate: today}
		q.entries[tenantID] = e
	}
	if e.count >= dailyCap {
		return false
	}
	e.count++
	return true
}

func QuotaMiddleware(q *QuotaTracker) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantVal, ok := c.Get(TenantContextKey)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "tenant not found in context, check middleware order",
			})
			return
		}
		tenant := tenantVal.(*auth.Tenant)

		if !q.consume(tenant.ID, tenant.DailyCap) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "daily quota exceeded",
			})
			return
		}
		c.Next()
	}
}
```

