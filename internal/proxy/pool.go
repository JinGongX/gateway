// Package proxy 负责真正把请求转发到下游模型服务（Ollama / vLLM 等）。
// 对应【阶段一：流式转发】+【阶段四：多后端支持】
package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// Pool 按后端地址缓存 ReverseProxy 实例，避免每次请求都重新创建。
// 学习点：httputil.ReverseProxy 本身是可以安全并发复用的，没必要
// 每个请求都 new 一个新的，复用能省掉重复解析 URL 等开销。
type Pool struct {
	mu        sync.Mutex
	proxies   map[string]*httputil.ReverseProxy
	transport http.RoundTripper
}

// Option 用来在构造 Pool 时微调行为（目前只有 Transport 一项）。
// 用可变参数而不是一堆字段，是为了让最常见的调用保持
// proxy.NewPool() 这种零配置写法。
type Option func(*Pool)

// WithTransport 替换底层 HTTP 传输实现。
//
// 两个用途：
//  1. 生产环境调上游连接池、握手/响应头超时（默认 http.DefaultTransport
//     的连接复用参数不一定适合长连接流式转发，需要时从这里注入定制版）；
//  2. 单元测试注入一个假的 RoundTripper，不启真实监听端口就能模拟
//     "下游返回 200" 和 "下游连不上" 两种结果。
func WithTransport(rt http.RoundTripper) Option {
	return func(p *Pool) { p.transport = rt }
}

func NewPool(opts ...Option) *Pool {
	p := &Pool{
		proxies:   make(map[string]*httputil.ReverseProxy),
		transport: http.DefaultTransport,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Get 按后端地址取出（或创建）一个 ReverseProxy 实例。
func (p *Pool) Get(backend string) (*httputil.ReverseProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if rp, ok := p.proxies[backend]; ok {
		return rp, nil
	}

	target, err := url.Parse(backend)
	if err != nil {
		return nil, fmt.Errorf("invalid backend url %q: %w", backend, err)
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = p.transport

	// 关键配置一：流式转发的开关。
	// FlushInterval 设为负数，表示每收到一点数据就立刻 flush 给客户端，
	// 不等 buffer 攒满。这是 SSE / NDJSON 流式响应能实时到达的核心。
	rp.FlushInterval = -1 * time.Nanosecond

	// 关键配置二：后端出错时的处理（企业级网关必须有，之前几版没写）。
	// 如果 Ollama/vLLM 挂了或者连不上，默认情况下 ReverseProxy 只会记一条
	// 日志然后让客户端连接挂起或收到裸的连接失败，用户体验很差。
	// 这里显式捕获错误，返回统一格式的 502，明确告诉调用方"下游不可用"。
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"error":"upstream backend unavailable","detail":%q}`, err.Error())))
	}

	p.proxies[backend] = rp
	return rp, nil
}
