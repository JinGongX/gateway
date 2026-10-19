// 学习点：企业级网关必须要有 panic 恢复。
//
// 为什么需要？Go 里如果一个 goroutine 发生 panic 没有被 recover，
// 整个进程会崩溃退出——网关是所有请求的统一入口，如果因为某一个
// 请求的 bug（比如空指针）导致整个网关进程挂掉，会影响所有租户，
// 而不只是触发 panic 的那一个请求。
//
// Gin 内置的 gin.Recovery() 已经做了这件事，但企业级场景通常需要
// 自定义 panic 时的响应格式（统一 JSON 结构）和日志记录方式，
// 所以这里自己实现一遍，方便你理解底层机制，也方便后续按需定制。
package middleware

import (
	"net/http"
	"runtime/debug"

	"gateway/internal/metrics"

	"github.com/gin-gonic/gin"
)

// 为什么要有一个专门的 interface 而不是直接依赖 *metrics.Metrics：
// 中间件只用到"记一次 panic"这一件事，依赖面越小越好测（测试里塞个假实现即可）。
type panicCounter interface {
	IncPanic()
}

// RecoveryMiddleware 捕获后续处理链中的 panic，避免进程崩溃，
// 并返回统一格式的 500 错误，同时把堆栈信息记进日志方便排查。
func RecoveryMiddleware(counter panicCounter) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				if counter != nil {
					counter.IncPanic()
				}

				// 学习点：debug.Stack() 拿到完整调用栈，这是排查 panic
				// 原因最关键的信息。结构化日志里把堆栈放进一个字段，
				// 而不是拼成多行字符串——多行内容在日志平台里会被截断或
				// 拆成多条，反而不好用。
				loggerFromContext(c).Error("panic recovered",
					"error", err,
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"stack", string(debug.Stack()),
				)

				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error":      "internal server error",
					"request_id": requestIDFromContext(c),
				})
			}
		}()
		c.Next()
	}
}

// requestIDFromContext 取请求 ID，取不到就返回空串（日志/响应里都能接受）。
func requestIDFromContext(c *gin.Context) string {
	if id, ok := c.Get(RequestIDContextKey); ok {
		if s, ok := id.(string); ok {
			return s
		}
	}
	return ""
}

// 编译期断言：*metrics.Metrics 必须满足 panicCounter
var _ panicCounter = (*metrics.Metrics)(nil)
