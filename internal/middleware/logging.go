// 学习点：企业级网关必须有请求日志，原因很直接——出问题的时候
// （比如某个租户投诉"我的请求很慢"或"我被限流了但不知道为什么"），
// 你需要能从日志里查到"哪个租户、什么时候、请求了什么、耗时多久、
// 最后返回了什么状态码"，没有日志就只能两眼一抹黑。
//
// 这里用的是标准库的 log/slog（结构化日志）。相比 fmt.Printf 的好处：
//   - 字段是结构化的，日志平台能按 request_id / tenant / status 检索聚合
//   - 有级别，线上可以只留 warn 以上减少噪音，本地可以开到 debug
//   - 每条日志自动带上本请求的 request_id（见 RequestIDMiddleware）
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"gateway/internal/logger"

	"github.com/gin-gonic/gin"
)

// RequestIDMiddleware 给每个请求确定一个唯一的追踪 ID。
//
// 两件事：
//  1. 尽量复用调用方传来的 X-Request-ID —— 上下游用同一个 ID，
//     一条链路才能在日志系统里串起来（否则网关自己生成一个，
//     对方的日志和我们的日志就对不上了）
//  2. 把这个 ID 同时挂到 gin context（给中间件用）和请求 context 的
//     logger 上（给 pgstore / ratelimit 这些不认识 gin 的底层包用）
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := sanitizeRequestID(c.GetHeader("X-Request-ID"))
		if requestID == "" {
			requestID = newRequestID()
		}

		c.Set(RequestIDContextKey, requestID)
		c.Header("X-Request-ID", requestID) // 回传给调用方，方便对方也能引用这个 ID 排查

		requestLogger := logger.FromContext(c.Request.Context()).With("request_id", requestID)
		c.Set(LoggerContextKey, requestLogger)
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), requestLogger))

		c.Next()
	}
}

// newRequestID 生成一个随机 ID。
// 生产环境通常用 UUID 库或者接入分布式追踪系统（如 OpenTelemetry），
// 这里为了不引入额外依赖，用一个简化的随机 hex 字符串。
func newRequestID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// sanitizeRequestID 校验调用方传来的 X-Request-ID 是否可用。
//
// 为什么要校验而不是直接透传：这个值会原样写进日志和响应头。
// 不加限制的话，调用方可以塞一个几 KB 的字符串把日志撑爆，
// 或者塞一个带换行的内容在日志里**伪造出多条记录**（log injection）——
// 后者是很实际的攻击手法，会让安全审计失去意义。
func sanitizeRequestID(id string) string {
	const maxLen = 64
	if id == "" || len(id) > maxLen {
		return ""
	}
	for i := 0; i < len(id); i++ {
		switch ch := id[i]; {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == '.', ch == ':': // : 用于 "trace:span" 这类格式
		default:
			return "" // 出现任何其他字符就丢弃，重新生成一个
		}
	}
	return id
}

// LoggingMiddleware 记录请求的关键信息。
//
// 注意：这个中间件放在鉴权之前，所以租户信息记录时做好"可能为空"的判断。
// 级别按状态码自动选择：5xx 是 Error（要告警），4xx 是 Warn（多为调用方问题），
// 其余是 Info。这样线上只订阅 error 就能第一时间看到真正的问题。
func LoggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next() // 先放行整个请求链，等处理完了再记日志（这样才能拿到最终状态码）

		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"latency_ms", float64(time.Since(start).Microseconds()) / 1000,
			"client_ip", c.ClientIP(),
		}
		if size := c.Writer.Size(); size >= 0 {
			attrs = append(attrs, "bytes", size)
		}
		if tenant, ok := tenantFromContext(c); ok {
			attrs = append(attrs, "tenant", tenant.ID)
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "errors", c.Errors.String())
		}

		l := loggerFromContext(c)
		switch {
		case status >= 500:
			l.Error("request", attrs...)
		case status >= 400:
			l.Warn("request", attrs...)
		default:
			l.Info("request", attrs...)
		}
	}
}
