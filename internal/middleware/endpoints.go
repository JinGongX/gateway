package middleware

import (
	"net/http"
	"strings"

	"gateway/internal/router"

	"github.com/gin-gonic/gin"
)

// EndpointMiddleware 只放行白名单里的「方法 + 路径」，其余在读 body、扣配额之前就拒绝。
//
// 在链中的位置：Auth -> RateLimit -> Endpoint -> Routing -> Quota。
//   - 在 Auth 之后：未鉴权的人看不到"哪些路径存在"这种信息，统一先拿到 401；
//   - 在 RateLimit 之后：沿用"任何到达网关的请求都占用容量"的语义，
//     拿着合法 key 狂扫 /api/pull 之类路径的行为同样会被限流；
//   - 在 Routing/Quota 之前：被拒的请求既不读 body，也绝不消耗配额。
func EndpointMiddleware(policy *router.EndpointPolicy) gin.HandlerFunc {
	return func(c *gin.Context) {
		verdict, allowed := policy.Check(c.Request.Method, c.Request.URL.Path)
		if verdict == router.Allowed {
			c.Next()
			return
		}

		// 访问了未开放的接口多半是探测或误用，值得留痕；
		// 日志里不带 key，只带租户、方法、路径。
		attrs := []any{"method", c.Request.Method, "path", c.Request.URL.Path}
		if tenant, ok := tenantFromContext(c); ok {
			attrs = append(attrs, "tenant", tenant.ID)
		}
		loggerFromContext(c).Warn("请求了未开放的接口", attrs...)

		if verdict == router.MethodNotAllowed {
			c.Header("Allow", strings.Join(allowed, ", "))
			c.AbortWithStatusJSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
			return
		}
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "endpoint not found"})
	}
}
