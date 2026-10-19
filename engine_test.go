package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gateway/internal/auth"
	"gateway/internal/config"
	"gateway/internal/health"
	"gateway/internal/metrics"
	"gateway/internal/middleware"
	"gateway/internal/proxy"
	"gateway/internal/router"

	"github.com/gin-gonic/gin"
)

// ---------- 假实现：让"顺序"和"计费语义"可以被直接断言，不需要真的连 Redis/PG ----------

type fakeTenantStore struct {
	tenant *auth.Tenant
	err    error
}

func (f *fakeTenantStore) Lookup(ctx context.Context, apiKey string) (*auth.Tenant, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.tenant == nil || apiKey != "test-key" {
		return nil, auth.ErrTenantNotFound
	}
	return f.tenant, nil
}

type fakeLimiter struct {
	allowed bool
	err     error
	calls   int
}

func (f *fakeLimiter) Allow(ctx context.Context, tenantID string, rps float64, burst int) (bool, error) {
	f.calls++
	return f.allowed, f.err
}

type fakeQuota struct {
	allowed                   bool
	err                       error
	consumeCalls, refundCalls int
}

func (f *fakeQuota) Consume(ctx context.Context, tenantID string, dailyCap int) (bool, error) {
	f.consumeCalls++
	return f.allowed, f.err
}

func (f *fakeQuota) Refund(ctx context.Context, tenantID string) (int64, error) {
	f.refundCalls++
	return 0, nil
}

type fakeProber struct {
	results []health.Result
	healthy bool
}

func (f *fakeProber) Run(ctx context.Context) ([]health.Result, bool) {
	return f.results, f.healthy
}

// 假 RoundTripper：直接决定"上游给出什么结果"，不经过任何真实网络。
// 这样测试既快又不受环境限制（没网、端口被占用都不影响）。
type stubTransport struct {
	status int
	body   string
	err    error
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

// upstreamOK 模拟一个正常返回的下游模型服务
func upstreamOK() *proxy.Pool {
	return proxy.NewPool(proxy.WithTransport(&stubTransport{body: `{"response":"ok"}`}))
}

// upstreamDown 模拟下游模型服务连不上（Ollama/vLLM 挂了）
func upstreamDown() *proxy.Pool {
	return proxy.NewPool(proxy.WithTransport(&stubTransport{err: errors.New("connection refused")}))
}

// upstreamStatus 模拟下游返回某个非 2xx 状态码（比如模型名不存在时的 404）
func upstreamStatus(status int, body string) *proxy.Pool {
	return proxy.NewPool(proxy.WithTransport(&stubTransport{status: status, body: body}))
}

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	// 测试里不需要看日志输出（日志格式本身由 internal/logger 的测试覆盖）
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Run()
}

// testEnv 把一次测试需要的所有可观测组件都暴露出来，方便断言。
type testEnv struct {
	engine  *gin.Engine
	limiter *fakeLimiter
	quota   *fakeQuota
	metrics *metrics.Metrics
	prober  *fakeProber
}

func newTestEnv(t *testing.T, pool *proxy.Pool) *testEnv {
	t.Helper()

	env := &testEnv{
		limiter: &fakeLimiter{allowed: true},
		quota:   &fakeQuota{allowed: true},
		metrics: metrics.New(),
		prober: &fakeProber{
			results: []health.Result{{Name: "redis", OK: true}, {Name: "postgres", OK: true}},
			healthy: true,
		},
	}

	modelRouter := router.NewRouter()
	modelRouter.Register("qwen2.5", "http://upstream.invalid")

	env.engine = buildEngine(engineDeps{
		tenantStore: &fakeTenantStore{tenant: &auth.Tenant{
			ID: "tenant-a", Name: "测试租户", RPS: 10, Burst: 10, DailyCap: 100,
		}},
		rateLimiter:  env.limiter,
		quotaTracker: env.quota,
		modelRouter:  modelRouter,
		proxyPool:    pool,
		metrics:      env.metrics,
		prober:       env.prober,
		maxBodyBytes: 10 << 20,
	})
	return env
}

func doRequest(engine *gin.Engine, body string, withAuth bool) *httptest.ResponseRecorder {
	// 必须显式给请求挂一个"可取消的 context"。
	// 原因：httptest.NewRequest 造出来的请求，Context().Done() 是 nil，
	// 而 ReverseProxy 发现 Done 为 nil 时会回退去用早已被废弃的
	// http.CloseNotifier 接口，gin 的 ResponseWriter 不实现它（直接 panic）。
	// 真实服务器的请求 context 一定有 Done 通道，所以这里必须模拟真实情况，
	// 否则测的就不是线上那条代码路径了。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer test-key")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// scrapeMetrics 抓一次 /metrics 文本，用来断言指标真的被记录了。
func scrapeMetrics(t *testing.T, engine *gin.Engine) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 期望 200，得到 %d", rec.Code)
	}
	return rec.Body.String()
}

// ---------- 鉴权：fail-closed ----------

func TestAuthRejectsMissingKey(t *testing.T) {
	env := newTestEnv(t, upstreamDown())

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, false)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("期望 401，得到 %d", rec.Code)
	}
	// 鉴权不过就绝对不能走到后面的限流/配额，否则未鉴权的流量也能消耗额度
	if env.limiter.calls != 0 || env.quota.consumeCalls != 0 {
		t.Errorf("鉴权失败后不应继续执行，限流调用=%d 配额调用=%d", env.limiter.calls, env.quota.consumeCalls)
	}
	if !strings.Contains(scrapeMetrics(t, env.engine), `gateway_auth_failures_total{reason="missing_token"} 1`) {
		t.Error("鉴权失败次数应该被计入指标")
	}
}

func TestAuthFailsClosedWhenStoreUnavailable(t *testing.T) {
	env := newTestEnv(t, upstreamDown())
	env.engine = buildEngine(engineDeps{
		// 模拟数据库故障：不是"查不到"，而是"查不了"
		tenantStore:  &fakeTenantStore{err: errors.New("数据库连接失败")},
		rateLimiter:  env.limiter,
		quotaTracker: env.quota,
		modelRouter:  router.NewRouter(),
		proxyPool:    upstreamDown(),
		metrics:      env.metrics,
		prober:       env.prober,
		maxBodyBytes: 10 << 20,
	})

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("鉴权服务故障时期望 503（fail-closed），得到 %d", rec.Code)
	}
}

// ---------- 顺序与计费语义 ----------

func TestUnknownModelDoesNotConsumeQuota(t *testing.T) {
	env := newTestEnv(t, upstreamDown())

	rec := doRequest(env.engine, `{"model":"not-exist","prompt":"hi"}`, true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d", rec.Code)
	}
	// 这是本次改造的核心断言：请求根本没走到后端，就不该占用租户的每日额度
	if env.quota.consumeCalls != 0 {
		t.Errorf("未知模型不应消耗配额，实际消耗了 %d 次", env.quota.consumeCalls)
	}
	// 但限流仍然计数：它保护的是网关自身的容量，任何到达网关的请求都占用了容量
	if env.limiter.calls != 1 {
		t.Errorf("限流应按请求次数计数（1 次），实际 %d 次", env.limiter.calls)
	}
}

func TestUpstreamFailureRefundsQuota(t *testing.T) {
	env := newTestEnv(t, upstreamDown())

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，得到 %d", rec.Code)
	}
	if env.quota.consumeCalls != 1 {
		t.Errorf("配额应被预扣 1 次（用于并发下防止超发），实际 %d 次", env.quota.consumeCalls)
	}
	// 租户什么都没拿到（502），这次调用必须退还额度
	if env.quota.refundCalls != 1 {
		t.Errorf("下游 502 应退还配额，实际退款 %d 次", env.quota.refundCalls)
	}
	if !strings.Contains(scrapeMetrics(t, env.engine), `gateway_quota_refunds_total{result="ok"} 1`) {
		t.Error("退款次数应该被计入指标")
	}
}

// 下游返回 4xx：这是调用方自己传错了参数（比如后端没有这个模型），
// 请求确实被处理并得到答复，所以照常计费，不退款。
func TestUpstream4xxIsStillBilled(t *testing.T) {
	env := newTestEnv(t, upstreamStatus(http.StatusNotFound, `{"error":"model not found"}`))

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404（透传下游状态码），得到 %d", rec.Code)
	}
	if env.quota.consumeCalls != 1 || env.quota.refundCalls != 0 {
		t.Errorf("下游 4xx 应照常计费，实际 扣费=%d 退款=%d", env.quota.consumeCalls, env.quota.refundCalls)
	}
}

func TestSuccessConsumesQuotaWithoutRefund(t *testing.T) {
	env := newTestEnv(t, upstreamOK())

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	if env.quota.consumeCalls != 1 || env.quota.refundCalls != 0 {
		t.Errorf("成功请求应计费且不退款，实际 扣费=%d 退款=%d", env.quota.consumeCalls, env.quota.refundCalls)
	}
}

func TestQuotaExceededReturns429(t *testing.T) {
	env := newTestEnv(t, upstreamDown())
	env.quota.allowed = false

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("期望 429，得到 %d", rec.Code)
	}
	// 被配额拦下的请求什么都没扣（也没转发），自然不存在退款
	if env.quota.consumeCalls != 1 || env.quota.refundCalls != 0 {
		t.Errorf("期望 扣费=1 退款=0，实际 扣费=%d 退款=%d", env.quota.consumeCalls, env.quota.refundCalls)
	}
	if !strings.Contains(scrapeMetrics(t, env.engine), `gateway_quota_rejected_total{tenant="tenant-a"} 1`) {
		t.Error("配额拒绝次数应该被计入指标")
	}
}

func TestQuotaFailOpenWhenRedisDown(t *testing.T) {
	env := newTestEnv(t, upstreamDown())
	env.quota.err = errors.New("redis 连接失败")

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	// fail-open：配额系统故障时放行，最终由下游决定结果（这里是 502）
	if rec.Code != http.StatusBadGateway {
		t.Errorf("配额故障应降级放行（最终 502），得到 %d", rec.Code)
	}
	if env.quota.refundCalls != 0 {
		t.Errorf("没扣成功就不该退款，实际退款 %d 次", env.quota.refundCalls)
	}
}

func TestRateLimitRejectionIsCounted(t *testing.T) {
	env := newTestEnv(t, upstreamOK())
	env.limiter.allowed = false

	rec := doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，得到 %d", rec.Code)
	}
	// 被限流就不该走到配额（更不该转发）
	if env.quota.consumeCalls != 0 {
		t.Errorf("限流拒绝后不应消耗配额，实际 %d 次", env.quota.consumeCalls)
	}
	if !strings.Contains(scrapeMetrics(t, env.engine), `gateway_rate_limited_total{tenant="tenant-a"} 1`) {
		t.Error("限流拒绝次数应该被计入指标")
	}
}

// 访问日志必须是结构化的：带 status / latency_ms / tenant / request_id 字段，
// 这样日志平台才能按 Tenant、按 Request ID 检索聚合。
func TestAccessLogIsStructured(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	env := newTestEnv(t, upstreamOK())
	doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	line := buf.String()
	for _, want := range []string{
		`"msg":"request"`, `"status":200`, `"tenant":"tenant-a"`,
		`"request_id":`, `"latency_ms":`, `"path":"/api/generate"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("访问日志缺少字段 %s，实际输出:\n%s", want, line)
		}
	}
}

// ---------- 探针 ----------

func TestReadyzReportsUnhealthyDependency(t *testing.T) {
	env := newTestEnv(t, upstreamOK())
	env.prober.healthy = false
	env.prober.results = []health.Result{{Name: "redis", OK: true}, {Name: "postgres", OK: false, Error: "connection refused"}}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	env.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("依赖故障时 /readyz 期望 503，得到 %d", rec.Code)
	}
	// 失败原因要能看出是哪一项挂了（运维排查靠这个）
	if !strings.Contains(rec.Body.String(), "postgres") || !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("响应里应该带上失败项与原因，实际: %s", rec.Body.String())
	}
}

func TestHealthzStaysOKWhenDependencyIsDown(t *testing.T) {
	env := newTestEnv(t, upstreamOK())
	env.prober.healthy = false

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	env.engine.ServeHTTP(rec, req)

	// 存活探针不看依赖：否则 Redis 抖一下，K8s 会把所有副本一起重启，
	// 把"依赖故障"放大成"自己制造的全面雪崩"
	if rec.Code != http.StatusOK {
		t.Errorf("存活探针不应受依赖影响，期望 200，得到 %d", rec.Code)
	}
}

// ---------- 指标与追踪 ----------

func TestMetricsEndpointExposesExpectedSeries(t *testing.T) {
	env := newTestEnv(t, upstreamOK())
	doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	body := scrapeMetrics(t, env.engine)

	for _, want := range []string{
		`gateway_requests_total{method="POST",status="200"} 1`,
		"gateway_request_duration_seconds_bucket", // 直方图分桶
		"gateway_inflight_requests 0",             // 请求结束后应该回到 0
		"gateway_panics_total 0",
		"go_goroutines", // Go 运行时指标（排查泄漏用）
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 输出缺少 %q", want)
		}
	}
}

func TestInflightReturnsToZeroAfterRequest(t *testing.T) {
	env := newTestEnv(t, upstreamDown())
	doRequest(env.engine, `{"model":"qwen2.5","prompt":"hi"}`, true)

	if !strings.Contains(scrapeMetrics(t, env.engine), "gateway_inflight_requests 0") {
		t.Error("请求处理完后 inflight 必须回到 0（否则这个指标会只增不减，掩盖真实并发）")
	}
}

func TestRequestIDIsPropagatedAndSanitised(t *testing.T) {
	env := newTestEnv(t, upstreamOK())

	// 调用方传了合法的 request id：必须原样透传，这样上下游能串成一个链路
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"qwen2.5","prompt":"hi"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("X-Request-ID", "trace-abc-123")
	rec := httptest.NewRecorder()
	env.engine.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "trace-abc-123" {
		t.Errorf("合法 request id 应被透传，得到 %q", got)
	}

	// 调用方塞了一个含换行的非法值（log injection 手法）：必须被丢弃并重新生成
	req2 := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"qwen2.5","prompt":"hi"}`))
	req2.Header.Set("Authorization", "Bearer test-key")
	req2.Header.Set("X-Request-ID", "evil\ninjected")
	rec2 := httptest.NewRecorder()
	env.engine.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("X-Request-ID"); got == "evil\ninjected" || got == "" {
		t.Errorf("非法 request id 应被替换成新生成的值，得到 %q", got)
	}
}

func TestPanicIsRecoveredAndCounted(t *testing.T) {
	m := metrics.New()
	r := gin.New()
	// 中间件顺序与 engine.go 保持一致
	r.Use(
		middleware.RequestIDMiddleware(),
		middleware.RecoveryMiddleware(m),
		middleware.MetricsMiddleware(m),
		middleware.LoggingMiddleware(),
	)
	r.GET("/boom", func(c *gin.Context) { panic("boom") })
	r.GET(middleware.MetricsPath, gin.WrapH(m.Handler()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应被兜成 500，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Errorf("panic 响应体应该是统一 JSON 结构，实际: %s", rec.Body.String())
	}

	metricsRec := httptest.NewRecorder()
	r.ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRec.Body.String(), "gateway_panics_total 1") {
		t.Error("panic 次数应该被计入指标（panic 会跳过 Metrics 中间件的收尾代码，由 Recovery 补记）")
	}
}

// ---------- 配置文件的冒烟测试 ----------

// TestShippedConfigLoads 保证仓库里这份 config/config.yaml 本身是可加载的：
// 严格字段校验 + ${VAR} 插值都通过。配置文件写错在部署时才发现，
// 代价比在测试里发现高得多。
func TestShippedConfigLoads(t *testing.T) {
	cfg, err := config.Load("config/config.yaml")
	if err != nil {
		t.Fatalf("仓库自带的 config/config.yaml 加载失败: %v", err)
	}
	if cfg.Server.Port == 0 || cfg.Redis.Addr == "" || len(cfg.Models) == 0 {
		t.Errorf("配置解析结果不完整: %+v", cfg)
	}
}
