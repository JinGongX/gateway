// 本文件负责"组装"：把 HTTP 引擎、路由和中间件链拼起来。
//
// 为什么单独一个文件、单独一个函数，而不是全写在 main() 里：
// 中间件的顺序本身就是一份重要的业务约定（谁先谁后决定了
// "什么算一次计费调用"），把它关在 main() 里就没法测试。
// 拆成 buildEngine 之后，engine_test.go 可以用假实现直接验证顺序，
// 不需要真的连上 Redis / PostgreSQL / 模型后端。
package main

import (
	"context"
	"net/http"

	"gateway/internal/health"
	"gateway/internal/logger"
	"gateway/internal/metrics"
	"gateway/internal/middleware"
	"gateway/internal/proxy"
	"gateway/internal/router"

	"github.com/gin-gonic/gin"
)

// HealthProber 是就绪探针依赖的接口，*health.Prober 满足它。
// 定义成接口是为了让测试能塞一个可控的假探针（比如"恒定不健康"）。
type HealthProber interface {
	Run(ctx context.Context) ([]health.Result, bool)
}

// engineDeps 是组装引擎需要的全部依赖，字段都是接口或现成对象。
type engineDeps struct {
	tenantStore  middleware.TenantLookup
	rateLimiter  middleware.RateLimiter
	quotaTracker middleware.QuotaTracker
	modelRouter  *router.Router
	proxyPool    *proxy.Pool
	metrics      *metrics.Metrics
	prober       HealthProber
	maxBodyBytes int64
}

// buildEngine 创建 gin 引擎并挂上全部路由与中间件。
func buildEngine(deps engineDeps) *gin.Engine {
	// 用 gin.New() 而不是 gin.Default()：Default 会自动带上 gin 内置的
	// Logger 和 Recovery，这里我们自己实现了同类中间件（方便定制格式），
	// 用 New() 避免重复输出两遍日志。
	r := gin.New()

	// 全局中间件：对所有路由生效，包括健康检查。
	//
	// 顺序说明（这几条顺序是有讲究的，改动前请先读）：
	//   RequestID 最先：后面所有日志都要靠它拿 request_id
	//   Recovery  第二：它必须"包住"后面所有中间件和业务处理，
	//             这样连我们自己写的日志代码出 panic 都能被兜住；
	//             代价是 panic 会跳过 Metrics/Logging 的收尾代码，
	//             所以 Recovery 自己补记一次 500 与 panic 计数
	//   Metrics   第三：耗时统计覆盖后续全部环节
	//   Logging   第四：等处理完再写访问日志，才能拿到最终状态码
	r.Use(
		middleware.RequestIDMiddleware(),
		middleware.RecoveryMiddleware(deps.metrics),
		middleware.MetricsMiddleware(deps.metrics),
		middleware.LoggingMiddleware(),
	)

	// 存活探针（liveness）：只回答"进程还在吗"。
	// 这里**故意不查 Redis/PostgreSQL**：如果依赖抖一下就让存活探针失败，
	// K8s 会把所有副本一起重启，把一个依赖故障放大成一次全面雪崩。
	// 探活要轻、要快、只跟自己的进程状态有关。
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 就绪探针（readiness）：回答"现在能不能接流量"。
	// 依赖不可用时返回 503，负载均衡会把本实例摘出去（但不会重启它），
	// 等依赖恢复后自动重新加入。
	r.GET("/readyz", func(c *gin.Context) {
		results, healthy := deps.prober.Run(c.Request.Context())

		if !healthy {
			// 探针失败要能解释清楚"是谁挂了"，同时注意这个端点通常只有
			// 内网（K8s kubelet / 监控系统）访问，不要把细节暴露到公网
			logger.FromContext(c.Request.Context()).Warn("就绪探针失败",
				"checks", len(results),
			)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unavailable",
				"checks": results,
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"checks": results,
		})
	})

	// 指标端点：给 Prometheus 抓取。
	// 同样不要暴露到公网（里面包含租户 ID、访问量等运营信息，
	// 攻击者也能据此判断你的容量水位）。
	r.GET(middleware.MetricsPath, gin.WrapH(deps.metrics.Handler()))

	// 受保护的 API 路由组：只有 /api/* 下的请求才需要鉴权/限流/配额/路由
	// 学习点：用路由组（Group）隔离"要不要走这条中间件链"，
	// 而不是把所有中间件都挂在全局，否则健康检查这种探活请求
	// 也会被要求带 API Key，这不合理。
	// 顺序是刻意排的，改动前请先读这段说明：
	//
	//   1. Auth      鉴权必须最先做：先确认"你是谁"，后面的限流/配额才有租户可依据。
	//                失败是 fail-closed（拒绝），因为放行等于给所有人开后门。
	//   2. RateLimit 限流放在最前面（读 body 之前）：令牌桶只要不查库就很快，
	//                用它先挡住洪峰，避免恶意的超大/垃圾 body 白白走一遍解析。
	//                语义是"按请求次数计"，所以下面 Routing 拒绝掉的请求
	//                同样会消耗令牌 —— 这是刻意的，限流保护的是网关自身的容量。
	//   3. Routing   解析 body 里的 model 决定后端。放在配额之前，是为了让
	//                "压根没走到后端"的请求（未知模型、body 不是 JSON）
	//                不消耗配额。
	//   4. Quota     配额放在紧挨转发的位置，并且"先预扣、转发失败再退还"
	//                （见 middleware/quota.go 的退款逻辑），避免下游 5xx
	//                也算到租户的每日额度上。
	api := r.Group("/api")
	api.Use(
		middleware.AuthMiddleware(deps.tenantStore, deps.metrics),
		middleware.RateLimitMiddleware(deps.rateLimiter, deps.metrics),
		middleware.RoutingMiddleware(deps.modelRouter, deps.maxBodyBytes),
		middleware.QuotaMiddleware(deps.quotaTracker, deps.metrics),
	)
	api.Any("/*path", proxyHandler(deps.proxyPool))

	return r
}

// proxyHandler 是最终的转发处理函数：从 context 里取出路由中间件
// 解析好的后端地址，用对应的 ReverseProxy 转发请求。
func proxyHandler(pool *proxy.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		backendVal, ok := c.Get(middleware.BackendContextKey)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "backend not resolved, check middleware order",
			})
			return
		}
		// 注意这里也做了 ok 判断：如果有人往 context 里塞了别的类型
		// （或者把 key 写错），裸断言会 panic，判断一下只是多两行代码，
		// 但能换回一个明确的错误响应。
		backend, ok := backendVal.(string)
		if !ok || backend == "" {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "backend in context has unexpected type",
			})
			return
		}

		rp, err := pool.Get(backend)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "invalid backend configuration",
			})
			return
		}

		// 转发前的最后一步：记一条 debug 日志（线上默认 info 级别不会输出，
		// 排查问题临时把 LOG_LEVEL 调成 debug 就能看到每次转发的目标地址）
		logger.FromContext(c.Request.Context()).Debug("转发到模型后端",
			"backend", backend,
			"path", c.Request.URL.Path,
		)

		rp.ServeHTTP(c.Writer, c.Request)
	}
}
