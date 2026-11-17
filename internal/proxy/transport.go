package proxy

import (
	"net"
	"net/http"
	"time"
)

// TransportConfig 是到模型后端的连接池与超时参数。零值字段使用下面注释里的默认值。
type TransportConfig struct {
	DialTimeout           time.Duration // 建连超时，默认 3s
	KeepAlive             time.Duration // TCP keepalive 间隔，默认 30s
	TLSHandshakeTimeout   time.Duration // TLS 握手超时，默认 5s
	ResponseHeaderTimeout time.Duration // 发完请求到收到响应头的最长等待，默认 300s（见下方说明）
	IdleConnTimeout       time.Duration // 空闲连接保留时间，默认 90s
	MaxIdleConns          int           // 全局空闲连接上限，默认 256
	MaxIdleConnsPerHost   int           // 单后端空闲连接上限，默认 64
	MaxConnsPerHost       int           // 单后端总连接上限，默认 0（不限制）
}

func (c TransportConfig) withDefaults() TransportConfig {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 3 * time.Second
	}
	if c.KeepAlive <= 0 {
		c.KeepAlive = 30 * time.Second
	}
	if c.TLSHandshakeTimeout <= 0 {
		c.TLSHandshakeTimeout = 5 * time.Second
	}
	if c.ResponseHeaderTimeout <= 0 {
		c.ResponseHeaderTimeout = 300 * time.Second
	}
	if c.IdleConnTimeout <= 0 {
		c.IdleConnTimeout = 90 * time.Second
	}
	if c.MaxIdleConns <= 0 {
		c.MaxIdleConns = 256
	}
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = 64
	}
	return c
}

// NewTransport 构造一个适合"长连接、流式、少量后端"场景的 Transport。
//
// 与 http.DefaultTransport 的差异（每一条都有理由）：
//   - MaxIdleConnsPerHost 从 2 提到 64：默认值下，并发稍高就会频繁建连/断连，
//     网关到同一个后端的连接数本来就集中，这是最该调的一个参数；
//   - DisableCompression=true：Transport 默认会替客户端加 Accept-Encoding: gzip 并
//     透明解压，对流式响应意味着多一层缓冲、还会吞掉 Content-Encoding。网关应原样透传；
//   - Proxy=nil：网关到后端是内网直连，不应受 HTTP_PROXY 环境变量影响
//     （一个全局代理变量就能让所有推理流量绕路，排查起来很痛苦）；
//   - 有 DialTimeout：后端宕机时快速失败（走 502），而不是等操作系统的 TCP 超时。
//
// 关于 ResponseHeaderTimeout 的取值：
// 流式请求在第一个 token 出来时就有响应头，但 stream:false 的请求要等整段生成完才有响应头，
// 所以它必须 >= 你允许的最长单次生成耗时。默认 300s 与现有 write_timeout 对齐；
// 想对非流式请求收紧，应该在路由层按请求区分，而不是把这个全局值调小。
func NewTransport(c TransportConfig) *http.Transport {
	c = c.withDefaults()
	dialer := &net.Dialer{Timeout: c.DialTimeout, KeepAlive: c.KeepAlive}

	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true, // 自定义 DialContext 时需要显式打开，https 后端才会协商 h2
		TLSHandshakeTimeout:   c.TLSHandshakeTimeout,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       c.IdleConnTimeout,
		MaxIdleConns:          c.MaxIdleConns,
		MaxIdleConnsPerHost:   c.MaxIdleConnsPerHost,
		MaxConnsPerHost:       c.MaxConnsPerHost,
		DisableCompression:    true,
	}
}
