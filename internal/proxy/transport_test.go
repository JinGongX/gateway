package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewTransportDefaultsAndSafetyFlags(t *testing.T) {
	tr := NewTransport(TransportConfig{})

	if tr.MaxIdleConnsPerHost != 64 {
		t.Errorf("MaxIdleConnsPerHost 默认应为 64（net/http 默认的 2 会导致频繁建连），实际 %d", tr.MaxIdleConnsPerHost)
	}
	if !tr.DisableCompression {
		t.Error("网关应原样透传，不能让 Transport 自作主张加 gzip 并解压")
	}
	if tr.Proxy != nil {
		t.Error("到内网后端的连接不应受 HTTP_PROXY 环境变量影响")
	}
	if tr.ResponseHeaderTimeout != 300*time.Second || tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("默认超时不符合预期: header=%v idle=%v", tr.ResponseHeaderTimeout, tr.IdleConnTimeout)
	}
}

func TestNewTransportHonoursOverrides(t *testing.T) {
	tr := NewTransport(TransportConfig{
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConnsPerHost:   7,
		MaxConnsPerHost:       3,
	})
	if tr.ResponseHeaderTimeout != 5*time.Second || tr.MaxIdleConnsPerHost != 7 || tr.MaxConnsPerHost != 3 {
		t.Errorf("显式配置应生效: %+v", tr)
	}
}

// 上游迟迟不回响应头：应按超时失败，并被归类成 504（而不是 502 或一直挂着）。
func TestResponseHeaderTimeoutSurfacesAs504(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	pool := NewPool(WithTransport(NewTransport(TransportConfig{ResponseHeaderTimeout: 100 * time.Millisecond})))
	rp, err := pool.Get(slow.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	start := time.Now()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("期望 504，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("应在响应头超时后很快返回，实际耗时 %v", elapsed)
	}
}

// 后端根本连不上：快速失败成 502，而不是等系统级 TCP 超时。
func TestDialFailureSurfacesAs502(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // 关掉，端口上没人监听

	pool := NewPool(WithTransport(NewTransport(TransportConfig{DialTimeout: 500 * time.Millisecond})))
	rp, _ := pool.Get(addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("连接被拒应为 502，实际 %d", rec.Code)
	}
}
