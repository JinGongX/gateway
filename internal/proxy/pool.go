// Package proxy 负责真正把请求转发到下游模型服务（Ollama / vLLM 等）。
// 对应【阶段一：流式转发】+【阶段四：多后端支持】
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"gateway/internal/logger"
)

// Pool 按后端地址缓存 ReverseProxy 实例，避免每次请求都重新创建。
// 学习点：httputil.ReverseProxy 本身是可以安全并发复用的，没必要
// 每个请求都 new 一个新的，复用能省掉重复解析 URL 等开销。
type Pool struct {
	mu           sync.Mutex
	proxies      map[string]*httputil.ReverseProxy
	transport    http.RoundTripper
	upstreamAuth func(backend string) string
}

// Option 用来在构造 Pool 时微调行为。
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

// WithUpstreamAuthorization 让网关用「自己的」凭证访问某个后端。
// fn 接收后端地址，返回完整的 Authorization 头取值（例如 "Bearer sk-xxx"）；
// 返回空串表示该后端不需要凭证。
//
// 与下面"剥离租户凭证"配套：租户的 key 永远不会到达上游，
// 上游（比如开了 --api-key 的 vLLM）认的是网关自己的凭证。
func WithUpstreamAuthorization(fn func(backend string) string) Option {
	return func(p *Pool) { p.upstreamAuth = fn }
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

// clientCredentialHeaders 是不允许从客户端一路透传到上游的凭证类请求头。
var clientCredentialHeaders = []string{
	"Authorization",       // 租户的 API Key：转给上游等于把密钥写进上游的访问日志
	"Proxy-Authorization", // 同理
	"Cookie",              // 网关与上游之间没有会话语义，带上只会泄露
	"X-Api-Key",           // 另一种常见的 key 传递方式
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
	// 没有 scheme/host 的地址（比如漏写了 http://）url.Parse 并不会报错，
	// 但转发时会得到莫名其妙的结果，所以在这里直接拒绝。
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, fmt.Errorf("invalid backend url %q: 需要形如 http(s)://host:port", backend)
	}

	var upstreamAuth string
	if p.upstreamAuth != nil {
		upstreamAuth = p.upstreamAuth(backend)
	}

	rp := &httputil.ReverseProxy{
		Transport: p.transport,

		// 关键配置一：流式转发的开关。
		// FlushInterval 设为负数，表示每收到一点数据就立刻 flush 给客户端，
		// 不等 buffer 攒满。这是 SSE / NDJSON 流式响应能实时到达的核心。
		FlushInterval: -1 * time.Nanosecond,

		// 用 Rewrite 而不是 NewSingleHostReverseProxy 的 Director：
		//   - Rewrite 在调用前就已经剥掉了入站请求里的 Forwarded / X-Forwarded-* 头，
		//     客户端没法伪造"我是从某某 IP 来的"传给上游；
		//   - SetURL 会把出站请求的 Host 改成目标后端的 Host（Director 版本保留的是
		//     客户端访问网关时的 Host，上游如果按域名做虚拟主机或证书校验会出问题）。
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()

			for _, h := range clientCredentialHeaders {
				pr.Out.Header.Del(h)
			}
			if upstreamAuth != "" {
				pr.Out.Header.Set("Authorization", upstreamAuth)
			}

			// 保持与旧行为一致：客户端没带 User-Agent 就不要替它编一个
			// （否则 Go 的 transport 会补上 "Go-http-client/x.y"）。
			if _, ok := pr.Out.Header["User-Agent"]; !ok {
				pr.Out.Header.Set("User-Agent", "")
			}
		},

		// 关键配置二：后端出错时的处理（企业级网关必须有）。
		// 错误细节只进日志，不返回给调用方：err.Error() 里常常包含内网 IP、端口、
		// 主机名（"dial tcp 10.1.2.3:11434: connect: connection refused"），
		// 对租户是纯粹的信息泄露。调用方靠响应头里的 X-Request-ID 找我们排查即可。
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status, msg := classifyUpstreamError(err)

			log := logger.FromContext(r.Context())
			if status == statusClientClosedRequest {
				log.Info("客户端在上游响应前断开", "backend", backend)
			} else {
				log.Warn("上游请求失败", "backend", backend, "status", status, "err", err)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		},
	}

	p.proxies[backend] = rp
	return rp, nil
}

// statusClientClosedRequest 是 nginx 约定俗成的 499：客户端在服务端响应之前关闭了连接。
const statusClientClosedRequest = 499

// classifyUpstreamError 把底层错误归类成对外的状态码和固定文案。
//   - 客户端取消       -> 499（不是上游的错，不该污染 5xx 告警）
//   - 超时（含 ctx 超时、拨号超时）-> 504
//   - 其余（连接被拒、DNS 失败、TLS 错误……）-> 502
func classifyUpstreamError(err error) (int, string) {
	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "client closed request"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return http.StatusGatewayTimeout, "upstream backend timeout"
	}
	return http.StatusBadGateway, "upstream backend unavailable"
}
