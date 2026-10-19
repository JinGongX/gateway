// Package metrics 提供网关的运行指标（/metrics 端点），供 Prometheus 采集。
//
// 网关最该被监控的四类信号：
//  1. 请求量与状态码分布（requests_total）：异常时第一个该看的东西
//  2. 延迟分布（request_duration_seconds）：流式请求的耗时量级和普通 API 完全不同
//  3. 被限流/被配额拒绝的次数：区分"用户太多"和"下游挂了"
//  4. 鉴权失败次数：被扫 key / key 配置错误都能从这里看出来
//
// 关于标签基数（label cardinality）：标签值的组合数决定了时间序列的数量，
// 每个序列都要占服务端内存。所以这里**故意不打 path 标签**——/api/* 的路径
// 千变万化，一旦打上去就是基数爆炸。租户标签只用在"被拒绝"这类低频且基数
// 有界的指标上（租户数量是可控的）。
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets 是延迟直方图的分桶。
// 默认的 prometheus.DefBuckets 最大只到 10 秒，而一次 LLM 生成动辄几十秒，
// 用默认分桶会把绝大多数请求都堆进 +Inf，等于看不到分布。
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// namespace 是所有指标的统一前缀，方便在 Prometheus 里按 gateway_* 过滤。
const namespace = "gateway"

// Metrics 持有全部指标和一个独立的注册表。
//
// 为什么自己 new 一个 registry，而不是用 prometheus.MustRegister 往默认
// 全局注册表里塞：全局注册表是进程级共享状态，一旦"某个包的 init 里注册了
// 同名指标"，或者测试里重复构造 Metrics，就会直接 panic（重复注册）。
// 自己持有 registry 之后，想建几个实例就建几个，测试之间也互不干扰。
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inflight        prometheus.Gauge
	panics          prometheus.Counter
	authFailures    *prometheus.CounterVec
	rateLimited     *prometheus.CounterVec
	quotaRejected   *prometheus.CounterVec
	quotaRefunds    *prometheus.CounterVec
}

// New 创建指标集合，并注册 Go 运行时/进程自身的指标
// （goroutine 数、GC、内存、文件描述符等，排查"是不是泄漏了"很有用）。
func New() *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		registry: registry,

		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "requests_total",
			Help:      "按 HTTP 方法和响应状态码统计的请求总数",
		}, []string{"method", "status"}),

		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "request_duration_seconds",
			Help:      "请求处理耗时（秒），含流式响应直到写完为止",
			Buckets:   latencyBuckets,
		}, []string{"method", "status"}),

		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "inflight_requests",
			Help:      "当前正在处理中的请求数（含长连接流式请求）",
		}),

		panics: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "panics_total",
			Help:      "被 Recovery 中间件捕获的 panic 次数",
		}),

		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "auth_failures_total",
			Help:      "鉴权失败次数，按原因分类（missing_token / invalid_key / store_unavailable）",
		}, []string{"reason"}),

		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rate_limited_total",
			Help:      "被令牌桶限流拒绝的请求数（按租户）",
		}, []string{"tenant"}),

		quotaRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "quota_rejected_total",
			Help:      "因每日配额耗尽被拒绝的请求数（按租户）",
		}, []string{"tenant"}),

		quotaRefunds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "quota_refunds_total",
			Help:      "配额退款次数，按结果分类（ok / failed）。failed 说明 Redis 有问题，账目可能不准",
		}, []string{"result"}),
	}

	registry.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.inflight,
		m.panics,
		m.authFailures,
		m.rateLimited,
		m.quotaRejected,
		m.quotaRefunds,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler 返回 /metrics 的 HTTP 处理器（Prometheus 文本格式）。
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// 采集出错时不要把内部错误细节暴露给抓取方，只记录在响应里
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// ObserveRequest 记录一次请求的状态码与耗时。
func (m *Metrics) ObserveRequest(method string, status int, seconds float64) {
	code := statusLabel(status)
	m.requestsTotal.WithLabelValues(method, code).Inc()
	m.requestDuration.WithLabelValues(method, code).Observe(seconds)
}

// IncInflight / DecInflight 维护"处理中请求数"这个瞬时值。
func (m *Metrics) IncInflight() { m.inflight.Inc() }
func (m *Metrics) DecInflight() { m.inflight.Dec() }

// IncPanic 记录一次被捕获的 panic（同时由 Recovery 记一条 500 状态码）。
func (m *Metrics) IncPanic() { m.panics.Inc() }

// IncAuthFailure 记录鉴权失败。reason 取值有限（见下面的常量），基数可控。
func (m *Metrics) IncAuthFailure(reason string) {
	m.authFailures.WithLabelValues(reason).Inc()
}

// IncRateLimited 记录一次限流拒绝。
func (m *Metrics) IncRateLimited(tenant string) {
	m.rateLimited.WithLabelValues(tenant).Inc()
}

// IncQuotaRejected 记录一次配额拒绝。
func (m *Metrics) IncQuotaRejected(tenant string) {
	m.quotaRejected.WithLabelValues(tenant).Inc()
}

// IncQuotaRefund 记录一次配额退款（ok=false 表示退款失败，需要告警）。
func (m *Metrics) IncQuotaRefund(ok bool) {
	result := "ok"
	if !ok {
		result = "failed"
	}
	m.quotaRefunds.WithLabelValues(result).Inc()
}

// 鉴权失败的原因分类（故意用有限枚举而不是把错误信息当标签）：
const (
	ReasonMissingToken     = "missing_token"
	ReasonInvalidKey       = "invalid_key"
	ReasonStoreUnavailable = "store_unavailable"
)

// statusLabel 把状态码转成标签值。
// 用数字而不是 "2xx" 这种归类：状态码本身数量有限（业务上就那几个），
// 但数字能直接区分 400 和 404，排查时信息量更大。
func statusLabel(status int) string {
	if status <= 0 {
		// 请求被异常中断（例如客户端提前断开）时状态码可能还是 0，
		// 归一成 unknown，避免出现 "0" 这种看不懂的序列
		return "unknown"
	}
	return strconv.Itoa(status)
}
