package middleware

import (
	"log/slog"

	"gateway/internal/auth"

	"github.com/gin-gonic/gin"
)

// 学习点：c.Set/c.Get 用的 key 是普通字符串，如果到处手写 "tenant"、"backend"
// 这种字面量，一旦某处拼错（比如写成 "Tenant"），编译器发现不了，
// 只会在运行时 c.Get 返回 false，排查起来很痛苦。
// 统一定义成常量，全项目引用这几个常量，从根源上消除这类拼写错误。
const (
	TenantContextKey    = "tenant"     // 存放 *auth.Tenant
	BackendContextKey   = "backend"    // 存放本次请求应转发到的后端地址 string
	RequestIDContextKey = "request_id" // 存放本次请求的唯一 ID，用于日志追踪
	LoggerContextKey    = "logger"     // 存放带 request_id 的 *slog.Logger
)

// tenantFromContext 取出鉴权中间件写进 context 的租户信息。
//
// 为什么要有这个函数：以前每个中间件都写一遍
//
//	tenant := c.Get(TenantContextKey).(*auth.Tenant)
//
// 这种裸类型断言。只要中间件顺序被改错（比如把限流放到鉴权前面），
// context 里没有 tenant，断言就会直接 panic；虽然 RecoveryMiddleware
// 能兜住不至于崩进程，但客户端收到的是含糊的 500，日志里是一堆堆栈，
// 定位成本高。统一走这个返回 (值, ok) 的函数，可以让每个调用点
// 给出"中间件顺序错了"这种一眼能懂的提示。
func tenantFromContext(c *gin.Context) (*auth.Tenant, bool) {
	val, exists := c.Get(TenantContextKey)
	if !exists {
		return nil, false
	}
	tenant, ok := val.(*auth.Tenant)
	if !ok {
		return nil, false
	}
	return tenant, true
}

// loggerFromContext 取出 RequestIDMiddleware 放进 context 的日志器。
// 它已经预置了 request_id 字段，所以后续任何一条日志都天然可追溯到具体请求。
// 理论上不会取不到（只要中间件顺序没被改错），兜底返回默认日志器。
func loggerFromContext(c *gin.Context) *slog.Logger {
	if val, exists := c.Get(LoggerContextKey); exists {
		if l, ok := val.(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}
