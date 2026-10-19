// 对应【阶段三：配额】
//
// 学习点回顾：配额和限流是两个独立的机制。
// - 限流：这一秒能过几个请求（瞬时速率），统计"尝试次数"
// - 配额：今天总共能用多少次（累计总量），统计"真正消耗了后端能力的次数"
// 两者互不依赖，一个请求要同时通过限流检查和配额检查才能放行。
package middleware

import (
	"context"
	"net/http"
	"time"

	"gateway/internal/metrics"
	"gateway/internal/ratelimit"

	"github.com/gin-gonic/gin"
)

// QuotaTracker 是配额中间件依赖的接口（同样为了可测试 + 可替换）。
//
// 为什么是"预扣 + 退款"两个方法，而不是一个 Check 一个 Consume：
// 并发场景下不能先 Check 再 Consume —— 两个请求可能同时 Check 到
// "还剩 1 次"，然后都 Consume，凭空多放行一个。所以 Consume 必须是
// 原子的"先扣再说"，扣完发现这次请求没成功，再用 Refund 还回去。
// 这样既不超发，又不会把失败的调用算进账单。
type QuotaTracker interface {
	Consume(ctx context.Context, tenantID string, dailyCap int) (bool, error)
	Refund(ctx context.Context, tenantID string) (int64, error)
}

// refundTimeout 限制退款操作的耗时上限。
// 退款发生在响应已经写完之后的"收尾阶段"，这时候客户端可能早就断开了，
// 不能让它无限期挂着占住 goroutine。
const refundTimeout = 3 * time.Second

func QuotaMiddleware(qt QuotaTracker, m *metrics.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant, ok := tenantFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "tenant not found in context, check middleware order",
			})
			return
		}

		allowed, err := qt.Consume(c.Request.Context(), tenant.ID, tenant.DailyCap)
		if err != nil {
			// 同样采用 fail-open：配额检查失败不阻断请求，但要留痕方便追查。
			// 这里要特别提醒一点权衡：配额通常跟计费挂钩，如果你的场景是
			// "配额等于付费额度"，fail-open 意味着 Redis 故障期间可能出现
			// 超出付费额度的调用没有被拦下。是否可以接受，需要结合实际
			// 业务规则判断，这不是一个技术问题，是一个业务风险取舍问题。
			//
			// 注意：fail-open 分支不会退款——因为我们压根没扣成功，
			// 也就没有东西可以退。
			loggerFromContext(c).Warn("配额检查失败，已降级放行",
				"tenant", tenant.ID,
				"err", err,
			)
			c.Next()
			return
		}

		if !allowed {
			m.IncQuotaRejected(tenant.ID)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "daily quota exceeded",
			})
			return
		}

		c.Next()

		// 走到这里说明响应已经写完了，可以看最终状态码决定要不要退款。
		// gin 的 c.Writer.Status() 此时拿到的是"真正写给客户端的状态码"，
		// 包括下游反向代理返回的 502。
		if !shouldRefundQuota(c.Writer.Status()) {
			return
		}

		// 关键细节：这里不能用 c.Request.Context()。
		// 它挂在客户端的连接上，客户端一断开（超时、Ctrl+C）就会被取消，
		// 退款请求会当场失败——而"后端 5xx + 客户端等不及断开"恰恰是
		// 最常见的组合。context.WithoutCancel 会保留 request id 之类的值，
		// 但切断取消信号，再配一个自己的超时，退款才可靠。
		refundCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), refundTimeout)
		defer cancel()

		// 退款用的是脱离请求生命周期的 context，所以这里要显式带上请求日志器，
		// 否则这条日志就丢了 request_id，排查时串不起来。
		log := loggerFromContext(c)

		if _, err := qt.Refund(refundCtx, tenant.ID); err != nil {
			// 退款失败意味着"这次调用白扣了一次额度"，是要人工介入的账目问题，
			// 所以记 Error 级别，并且用独立的指标计数（result=failed）方便告警
			m.IncQuotaRefund(false)
			log.Error("配额退款失败，该失败请求会占用一次额度",
				"tenant", tenant.ID,
				"status", c.Writer.Status(),
				"err", err,
			)
			return
		}

		m.IncQuotaRefund(true)
		log.Info("配额已退款",
			"tenant", tenant.ID,
			"status", c.Writer.Status(),
			"reason", "下游或网关错误",
		)
	}
}

// shouldRefundQuota 判断这个最终状态码对应的请求是否应该退还配额。
//
// 语义：只有"网关或下游自己出问题"（5xx）才不算消耗——
//   - 2xx/3xx：正常处理，计费
//   - 4xx：是调用方的问题（参数错、key 没权限），请求确实被处理了，照常计费；
//     注意网关自己产生的 400（未知模型、body 非法）根本走不到这里，
//     因为路由中间件在配额之前就把它们拦掉了。
//   - 5xx：502（Ollama/vLLM 挂了）、500（网关内部 panic），
//     租户没有拿到任何服务，不该让他为这次调用买单。
func shouldRefundQuota(status int) bool {
	return status >= http.StatusInternalServerError
}

var _ QuotaTracker = (*ratelimit.RedisQuotaTracker)(nil)
