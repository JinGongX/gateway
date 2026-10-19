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
	"context"
	"errors"
	"net/http"
	"strings"

	"gateway/internal/auth"
	"gateway/internal/metrics"

	"github.com/gin-gonic/gin"
)

// TenantLookup 是鉴权中间件依赖的接口，而不是直接依赖 pgstore.TenantStore
// 这个具体类型。
//
// 学习点：这是 Go 里"依赖接口而不是依赖具体实现"的典型用法。
// 好处：1）中间件包不需要 import pgstore 包，避免了不必要的耦合；
// 2）写单元测试时可以传入一个假的（mock）实现，不需要真的连一个
// PostgreSQL 才能测试鉴权逻辑；3）以后如果租户存储换成别的方案，
// 只要新实现满足这个接口，中间件代码完全不用改。
type TenantLookup interface {
	Lookup(ctx context.Context, apiKey string) (*auth.Tenant, error)
}

// AuthMiddleware 校验请求头里的 API Key，找到对应租户后存入 context。
// 找不到或者格式不对，直接 401 并终止后续处理（c.Abort 系列方法）。
func AuthMiddleware(store TenantLookup, m *metrics.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := loggerFromContext(c)

		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			m.IncAuthFailure(metrics.ReasonMissingToken)
			// 鉴权失败用 Warn 级别：单次是噪音，但如果突然大批量出现，
			// 那就是"有人在扫 key"或者"某个调用方 key 配错了"，
			// 靠 gateway_auth_failures_total 这个指标能立刻看出来
			log.Warn("鉴权失败", "reason", metrics.ReasonMissingToken, "path", c.Request.URL.Path)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing or malformed authorization header",
			})
			return
		}
		key := strings.TrimPrefix(header, "Bearer ")

		tenant, err := store.Lookup(c.Request.Context(), key)
		if err != nil {
			if errors.Is(err, auth.ErrTenantNotFound) {
				m.IncAuthFailure(metrics.ReasonInvalidKey)
				// 注意：这里不能把 key 本身写进日志（日志泄露等于密钥泄露），
				// 只记录"哪个客户端 IP 用了无效 key"这类可追溯信息
				log.Warn("鉴权失败", "reason", metrics.ReasonInvalidKey, "client_ip", c.ClientIP())
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": "invalid api key",
				})
				return
			}

			// 关键设计：鉴权路径必须 fail-closed。
			// 这跟限流/配额中间件的 fail-open 策略正好相反——
			// 限流失效顶多是多放行了几个请求，可以接受；
			// 但如果鉴权查找出错时选择"默认放行"，就等于给所有人开了后门，
			// 数据库故障期间任何人不带 key 或带错误的 key 都能畅通无阻。
			// 所以这里查找出错时统一拒绝请求，返回 503 表示
			// "不是你的 key 有问题，是鉴权服务本身暂时不可用"。
			m.IncAuthFailure(metrics.ReasonStoreUnavailable)
			log.Error("鉴权服务不可用，已拒绝请求（fail-closed）", "err", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error":      "auth service temporarily unavailable",
				"request_id": requestIDFromContext(c),
			})
			return
		}

		c.Set(TenantContextKey, tenant)
		c.Next()
	}
}
