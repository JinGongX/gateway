// 对应【阶段三：限流】
//
// 学习点回顾：限流控制的是"瞬时速率"——同一时刻涌入太多请求要拒绝一部分。
// 验证限流效果必须用并发请求测试，顺序 for 循环打 curl 因为请求间隔
// 往往大于令牌补充周期，会看不出限流效果，这个坑你已经亲自踩过一次了。
package middleware

import (
	"context"
	"net/http"

	"gateway/internal/metrics"
	"gateway/internal/ratelimit"

	"github.com/gin-gonic/gin"
)

// RateLimiter 是限流中间件依赖的接口（跟 TenantLookup 同一个思路）：
// 中间件只关心"这次请求放不放行"，不关心底下是 Redis 令牌桶还是别的实现。
// 好处有两个：单元测试可以直接塞一个假实现，不用真的起一个 Redis；
// 以后换限流算法（滑动窗口、漏桶）也只需要满足这个接口。
type RateLimiter interface {
	Allow(ctx context.Context, tenantID string, rps float64, burst int) (bool, error)
}

// RateLimitMiddleware 按租户做令牌桶限流。
//
// 计费语义：这里统计的是"请求次数"，不是"成功次数"。被路由中间件拒绝的
// 未知模型请求、被配额拒绝的请求，都会消耗令牌。这是刻意的选择——
// 限流保护的是网关自身的处理容量，而任何到达网关的请求都已经占用了容量。
// 真正跟钱挂钩的配额，采用的是另一套语义（见 quota.go）。
func RateLimitMiddleware(rl RateLimiter, m *metrics.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant, ok := tenantFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "tenant not found in context, check middleware order",
			})
			return
		}

		allowed, err := rl.Allow(c.Request.Context(), tenant.ID, tenant.RPS, tenant.Burst)
		if err != nil {
			// fail-open：Redis 出问题时不阻断业务请求，但要能在日志里
			// 追查到这类事件。这里用 Warn 级别，配合 WARN 关键字可以把
			// 历史上所有降级事件捞出来复盘；生产环境还应该把它接到
			// 告警系统（这类"静默降级"最容易被忽略，等发现时已经放了很久）。
			loggerFromContext(c).Warn("限流检查失败，已降级放行",
				"tenant", tenant.ID,
				"err", err,
			)
			c.Next()
			return
		}

		if !allowed {
			// 限流拒绝率是判断"要不要扩容/要不要调额度"的核心指标
			m.IncRateLimited(tenant.ID)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
			})
			return
		}
		c.Next()
	}
}

// 让 *ratelimit.RedisLimiter 自动满足上面的接口。
// 这是一行编译期断言：如果哪天 RedisLimiter 的 Allow 签名改了，
// 这里会立刻编译失败，而不是等运行到 main.go 组装时才报错。
var _ RateLimiter = (*ratelimit.RedisLimiter)(nil)
