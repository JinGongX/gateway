package middleware

import (
	"time"

	"gateway/internal/metrics"

	"github.com/gin-gonic/gin"
)

// MetricsPath 是指标端点的路径。这个路径上的请求不参与统计。
const MetricsPath = "/metrics"

// MetricsMiddleware 记录每个请求的"处理中数量、状态码、耗时"。
//
// 放在中间件链的最前面（紧挨 RequestID），原因有两个：
//  1. 耗时统计要覆盖整条链路，包括后面所有中间件的开销；
//  2. 状态码要等业务处理完才确定，放在最外层才能拿到最终值。
//
// 已知边界：如果某个中间件 panic，panic 会跳过本中间件 c.Next() 之后的
// 代码（Go 的 panic 会跳过普通语句），这条请求就不会在这里记账。
// 这种情况由 RecoveryMiddleware 补记一次 500 和 panic 计数，不会漏账。
func MetricsMiddleware(m *metrics.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 不统计指标端点自己：
		//  1. 抓取请求会把 inflight 抬成 1，让"当前并发"这个指标永远回不到 0，
		//     真正想看的高峰反而被这个固定噪音掩盖；
		//  2. 抓取频率（Prometheus 每 15s 一次 × 多副本）和业务流量没有关系，
		//     混进 QPS 里会让容量规划得出错误结论。
		if c.Request.URL.Path == MetricsPath {
			c.Next()
			return
		}

		m.IncInflight()
		start := time.Now()

		// 用 defer 而不是写在 c.Next() 之后：中途 return（比如被别的中间件
		// Abort）时同样能保证 inflight 被减掉，否则这个指标会只增不减
		defer func() {
			m.DecInflight()
			m.ObserveRequest(c.Request.Method, c.Writer.Status(), time.Since(start).Seconds())
		}()

		c.Next()
	}
}
