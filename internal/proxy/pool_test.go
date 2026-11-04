package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingTransport 记录上游"实际收到"的请求，并可以按需返回错误。
type recordingTransport struct {
	err  error
	last *http.Request
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.last = req
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"response":"ok"}`)),
		Request:    req,
	}, nil
}

// serve 走一遍 ReverseProxy。请求要挂一个可取消的 context：
// 真实服务器的请求一定有 Done 通道，测试里要模拟同样的路径。
func serve(t *testing.T, pool *Pool, backend string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	rp, err := pool.Get(backend)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)
	return rec
}

func TestClientCredentialsAreNotForwarded(t *testing.T) {
	rt := &recordingTransport{}
	pool := NewPool(WithTransport(rt))

	serve(t, pool, "http://upstream.invalid:11434", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer tenant-secret-key")
		r.Header.Set("Proxy-Authorization", "Basic abc")
		r.Header.Set("Cookie", "session=abc")
		r.Header.Set("X-Api-Key", "tenant-secret-key")
		r.Header.Set("X-Request-ID", "trace-1")
		r.Header.Set("X-Forwarded-For", "6.6.6.6") // 客户端伪造的来源 IP
	})

	if rt.last == nil {
		t.Fatal("请求没有到达上游")
	}
	for _, h := range clientCredentialHeaders {
		if v := rt.last.Header.Get(h); v != "" {
			t.Errorf("%s 不应被转发给上游，实际 %q", h, v)
		}
	}
	if got := rt.last.Header.Get("X-Request-ID"); got != "trace-1" {
		t.Errorf("X-Request-ID 应透传给上游以便串联日志，实际 %q", got)
	}
	if xff := rt.last.Header.Get("X-Forwarded-For"); strings.Contains(xff, "6.6.6.6") {
		t.Errorf("客户端伪造的 X-Forwarded-For 不应传给上游，实际 %q", xff)
	}
	if rt.last.URL.Host != "upstream.invalid:11434" {
		t.Errorf("应转发到目标后端，实际 %q", rt.last.URL.Host)
	}
}

func TestGatewayUsesItsOwnUpstreamCredential(t *testing.T) {
	rt := &recordingTransport{}
	pool := NewPool(
		WithTransport(rt),
		WithUpstreamAuthorization(func(backend string) string {
			if backend == "http://secured.invalid" {
				return "Bearer gateway-upstream-token"
			}
			return ""
		}),
	)

	serve(t, pool, "http://secured.invalid", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer tenant-secret-key")
	})
	if got := rt.last.Header.Get("Authorization"); got != "Bearer gateway-upstream-token" {
		t.Errorf("上游应收到网关自己的凭证，实际 %q", got)
	}

	serve(t, pool, "http://open.invalid", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer tenant-secret-key")
	})
	if got := rt.last.Header.Get("Authorization"); got != "" {
		t.Errorf("没配凭证的后端不应收到任何 Authorization，实际 %q", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

func TestUpstreamErrorsAreClassifiedAndRedacted(t *testing.T) {
	const internal = "dial tcp 10.1.2.3:11434: connect: connection refused"

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{"连接被拒", errors.New(internal), http.StatusBadGateway, "upstream backend unavailable"},
		{"客户端取消", context.Canceled, 499, "client closed request"},
		{"包装过的取消", errors.Join(errors.New("round trip"), context.Canceled), 499, "client closed request"},
		{"上下文超时", context.DeadlineExceeded, http.StatusGatewayTimeout, "upstream backend timeout"},
		{"网络层超时", timeoutError{}, http.StatusGatewayTimeout, "upstream backend timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := NewPool(WithTransport(&recordingTransport{err: tc.err}))
			rec := serve(t, pool, "http://10.1.2.3:11434", nil)

			if rec.Code != tc.wantStatus {
				t.Errorf("状态码期望 %d，实际 %d", tc.wantStatus, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantMsg) {
				t.Errorf("响应体应包含 %q，实际 %s", tc.wantMsg, body)
			}
			// 内网地址、底层错误原文绝不能返回给调用方
			for _, leaked := range []string{"10.1.2.3", "dial tcp", "connection refused", "i/o timeout"} {
				if strings.Contains(body, leaked) {
					t.Errorf("响应体泄露了内部信息 %q: %s", leaked, body)
				}
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type 期望 application/json，实际 %q", ct)
			}
		})
	}
}

func TestGetRejectsMalformedBackend(t *testing.T) {
	pool := NewPool()
	for _, backend := range []string{
		"localhost:11434", // 漏写 scheme
		"http://",         // 没有 host
		"ftp://host:21",   // 不支持的 scheme
		"://bad",          // 根本不是 URL
		"",
	} {
		if _, err := pool.Get(backend); err == nil {
			t.Errorf("后端地址 %q 应被拒绝", backend)
		}
	}
	if _, err := pool.Get("https://vllm.internal:8000"); err != nil {
		t.Errorf("合法地址不应报错: %v", err)
	}
}

func TestGetCachesProxyPerBackend(t *testing.T) {
	pool := NewPool()
	a1, _ := pool.Get("http://a.invalid")
	a2, _ := pool.Get("http://a.invalid")
	b, _ := pool.Get("http://b.invalid")
	if a1 != a2 {
		t.Error("同一个后端应复用同一个 ReverseProxy")
	}
	if a1 == b {
		t.Error("不同后端不应共用实例")
	}
}
