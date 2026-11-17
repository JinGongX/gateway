// AI 网关：统一代理多个模型后端的请求，支持鉴权、限流、配额、
// 多模型路由和流式转发。
//
// 启动方式：go run .          （注意是整个目录，不是 main.go：引擎装配在 engine.go）
// 默认读取 config/config.yaml，可通过 -config 参数指定其他路径；
// 配置里的 ${VAR} 占位符从 .env 或环境变量取值，见 internal/config/env.go。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"gateway/internal/config"
	"gateway/internal/health"
	"gateway/internal/logger"
	"gateway/internal/metrics"
	"gateway/internal/pgstore"
	"gateway/internal/proxy"
	"gateway/internal/ratelimit"
	"gateway/internal/redisclient"
	"gateway/internal/router"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	flag.Parse()

	// ---------- 1. 加载配置 ----------
	cfg, err := config.Load(*configPath)
	if err != nil {
		// 此时日志器还没建起来（日志级别/格式本身来自配置），
		// 所以启动早期的错误用标准库输出到 stderr
		log.Fatalf("加载配置失败: %v", err)
	}

	// ---------- 2. 初始化日志器 ----------
	// 放到最前面：后面的每一步初始化都要能被记录，
	// 而且日志级别/格式来自配置和 .env，必须等配置加载完才能确定。
	appLogger, err := logger.New(cfg.Log)
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	slog.SetDefault(appLogger) // 让所有 slog.Default() 兜底（含 logger.FromContext）都走这里

	appLogger.Info("配置加载完成",
		"config", *configPath,
		"port", cfg.Server.Port,
		"log_level", cfg.Log.Level,
		"log_format", cfg.Log.Format,
	)

	// ---------- 3. 初始化各个组件 ----------

	// 初始化 Redis 客户端。它内部会主动 Ping 一次，连不上会在启动阶段
	// 就直接失败退出，而不是留到线上流量进来才报错。
	// 参数映射统一收在 redisclient.FromSettings 里，保证 cmd/seed 和
	// 网关用的是同一套超时/连接池参数。
	redisClient, err := redisclient.FromSettings(
		cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB,
		cfg.Redis.DialTimeoutMs, cfg.Redis.ReadTimeoutMs, cfg.Redis.WriteTimeoutMs,
		cfg.Redis.PoolSize,
	)
	if err != nil {
		fatal(appLogger, "初始化 Redis 失败", err)
	}
	defer redisClient.Close() // 进程退出前释放连接池资源

	// 初始化 PostgreSQL 连接池，同样是"启动阶段就验证连接"的思路。
	// 学习点：这里正是你之前问到的地方——pgstore.NewPool 现在只接收
	// 一个 dsn string 参数，DSN 由 cfg.Postgres.DSN() 拼好传进来，
	// main.go 不需要知道 DSN 的具体拼接格式，这个细节封装在 config 包里。
	pgPool, err := pgstore.NewPool(cfg.Postgres.DSN())
	if err != nil {
		fatal(appLogger, "初始化 PostgreSQL 失败", err)
	}
	defer pgPool.Close()

	// 租户存储：组合 PostgreSQL 连接池 + Redis 客户端，
	// 内部实现"先查缓存、未命中查库、查到后回填缓存"的完整逻辑。
	// 注意这里不再需要像以前那样从 config.yaml 里循环 Register 租户了——
	// 租户数据已经预先通过 cmd/seed 工具写进了 PostgreSQL。
	tenantStore := pgstore.NewTenantStore(pgPool, redisClient)

	modelRouter := router.NewRouter()
	for _, m := range cfg.Models {
		modelRouter.Register(m.Name, m.Backend)
	}

	// 限流和配额都基于同一个 Redis 客户端，各自用不同的 key 前缀
	// （ratelimit:xxx 和 quota:xxx）区分，互不干扰。
	// rateLimiter := ratelimit.NewRedisLimiter(redisClient)
	// loc, err := time.LoadLocation("Asia/Shanghai") // 容器里需要有 tzdata，或 import _ "time/tzdata"
	// if err != nil {
	// 	fatal(appLogger, "加载时区失败", err)
	// }
	// quotaTracker := ratelimit.NewRedisQuotaTracker(redisClient, ratelimit.WithQuotaLocation(loc))
	// //quotaTracker := ratelimit.NewRedisQuotaTracker(redisClient)
	// proxyPool := proxy.NewPool()

	// 指标集合（/metrics 端点，供 Prometheus 抓取）
	appMetrics := metrics.New()
	secs := func(n int) time.Duration { return time.Duration(n) * time.Second }
	secsOr := func(n, def int) time.Duration {
		if n <= 0 {
			n = def
		}
		return secs(n)
	}

	// Redis 故障时的降级：限流和配额都退化为"每个副本在内存里各限 1/N"，
	// 不再是无限放行。每次降级记指标；日志按 10 秒采样，避免故障期间刷屏。
	var lastFallbackLog atomic.Int64
	onFallback := func(component string) ratelimit.FallbackNotify {
		return func(tenantID string, err error) {
			appMetrics.IncRedisFallback(component)
			now := time.Now().UnixNano()
			last := lastFallbackLog.Load()
			if now-last >= int64(10*time.Second) && lastFallbackLog.CompareAndSwap(last, now) {
				appLogger.Warn("Redis 不可用，已降级为本地限流/配额",
					"component", component, "tenant", tenantID, "err", err)
			}
		}
	}
	replicas := cfg.Server.ExpectedReplicas
	rateLimiter := ratelimit.NewFallbackLimiter(ratelimit.NewRedisLimiter(redisClient), replicas, onFallback("ratelimit"))
	quotaTracker := ratelimit.NewFallbackQuota(ratelimit.NewRedisQuotaTracker(redisClient), replicas, onFallback("quota"))

	proxyPool := proxy.NewPool(
		proxy.WithTransport(proxy.NewTransport(proxy.TransportConfig{
			DialTimeout:           time.Duration(cfg.Upstream.DialTimeoutMs) * time.Millisecond,
			ResponseHeaderTimeout: secs(cfg.Upstream.ResponseHeaderTimeoutSeconds),
			IdleConnTimeout:       secs(cfg.Upstream.IdleConnTimeoutSeconds),
			MaxIdleConnsPerHost:   cfg.Upstream.MaxIdleConnsPerHost,
			MaxConnsPerHost:       cfg.Upstream.MaxConnsPerHost,
		})),
		proxy.WithBodyIdleTimeout(secs(cfg.Upstream.StreamIdleTimeoutSeconds)),
	)

	drainer := &health.Drainer{}

	// // 指标集合（/metrics 端点，供 Prometheus 抓取）
	// appMetrics := metrics.New()

	// 就绪探针的检查项。
	// 超时 2 秒：依赖卡住时探针要能及时返回失败，而不是跟着一起挂住；
	// 缓存 1 秒：K8s 的多个探针可能以很高频率打过来，避免每次都真去 Ping。
	// 注意这里只查"关键依赖"，模型后端不在其中 —— 模型服务挂了只影响
	// 对应模型的请求，不应该让整个网关实例被摘出负载均衡。
	prober := health.NewProber(2*time.Second, time.Second,
		health.Check{
			Name: "redis",
			Fn:   func(ctx context.Context) error { return redisClient.Ping(ctx).Err() },
		},
		health.Check{
			Name: "postgres",
			Fn:   func(ctx context.Context) error { return pgPool.Ping(ctx) },
		},
	)

	// ---------- 4. 组装 Gin 引擎和中间件链 ----------
	// 具体的路由与中间件顺序放在 buildEngine（engine.go），
	// 这样这段"顺序敏感"的逻辑可以被单元测试直接覆盖。
	r := buildEngine(engineDeps{
		tenantStore:    tenantStore,
		rateLimiter:    rateLimiter,
		quotaTracker:   quotaTracker,
		modelRouter:    modelRouter,
		proxyPool:      proxyPool,
		metrics:        appMetrics,
		prober:         prober,
		maxBodyBytes:   cfg.Server.MaxBodyBytes,
		trustedProxies: cfg.Server.TrustedProxies,

		drainer:         drainer,
		streamWriteIdle: secsOr(cfg.Server.StreamWriteTimeoutSeconds, 30),
		maxStream:       secsOr(cfg.Server.MaxStreamSeconds, 900),
	})

	// ---------- 5. 启动 HTTP 服务，并支持优雅关闭 ----------
	// 学习点：企业级服务不能用 r.Run() 简单启动完事——进程收到停止信号
	// （比如 K8s 滚动更新时发的 SIGTERM）时，应该拒绝新请求，但把正在
	// 处理中的请求（尤其是长时间的流式响应）处理完再退出，而不是
	// 粗暴地直接杀掉进程导致用户请求半途中断。
	// srv := &http.Server{
	// 	Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
	// 	Handler:      r,
	// 	ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSeconds) * time.Second,
	// 	WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSeconds) * time.Second,
	// }
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second, // 防 slowloris：请求头必须在 10 秒内发完
		ReadTimeout:       secs(cfg.Server.ReadTimeoutSeconds),
		WriteTimeout:      secs(cfg.Server.WriteTimeoutSeconds), // 0 = 关闭，由 DeadlineWriter 接管
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		appLogger.Info("网关启动", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal(appLogger, "服务启动失败", err)
		}
	}()

	// 监听系统信号，收到 Ctrl+C（SIGINT）或者 kill（SIGTERM）时触发优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	// <-quit
	// appLogger.Info("收到关闭信号，开始优雅关闭")

	// // 给正在处理中的请求最多 10 秒时间完成，超时则强制关闭
	// ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// defer cancel()

	// if err := srv.Shutdown(ctx); err != nil {
	// 	fatal(appLogger, "优雅关闭超时，强制退出", err)
	// }
	// appLogger.Info("网关已安全关闭")
	<-quit

	grace := secs(cfg.Server.ShutdownGraceSeconds)
	if cfg.Server.ShutdownGraceSeconds <= 0 {
		grace = secsOr(cfg.Server.MaxStreamSeconds, 900) + 30*time.Second
	}
	if err := gracefulShutdown(srv, drainer, secsOr(cfg.Server.DrainSeconds, 5), grace, appLogger); err != nil {
		fatal(appLogger, "优雅关闭失败", err)
	}
	appLogger.Info("网关已安全关闭")
}

// fatal 记录错误并退出。
//
// 注意 os.Exit 不会执行 defer（比如 defer redisClient.Close()），
// 这是启动阶段失败时的刻意选择：这时候连接池里通常什么都没有，
// 而"带着坏配置继续跑"的风险远大于"少关几个连接"。
func fatal(logger *slog.Logger, msg string, err error) {
	logger.Error(msg, "err", err)
	os.Exit(1)
}
