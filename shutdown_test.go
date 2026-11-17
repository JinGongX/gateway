package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"gateway/internal/health"
)

func startTestServer(t *testing.T, d *health.Drainer, streamChunks int, chunkEvery time.Duration) (*http.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Draining() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < streamChunks; i++ {
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(chunkEvery)
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 核心场景：下线时在途的流式响应必须完整结束；摘流量期间 /readyz 为 503 而服务仍可用。
func TestGracefulShutdownLetsInflightStreamFinish(t *testing.T) {
	d := &health.Drainer{}
	srv, base := startTestServer(t, d, 6, 80*time.Millisecond) // 流约持续 480ms
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	streamDone := make(chan string, 1)
	go func() {
		resp, err := client.Get(base + "/stream")
		if err != nil {
			streamDone <- "ERR:" + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		streamDone <- string(b)
	}()
	time.Sleep(100 * time.Millisecond) // 确保流已经开始

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- gracefulShutdown(srv, d, 200*time.Millisecond, 3*time.Second, discardLogger()) }()
	time.Sleep(50 * time.Millisecond) // 处于摘流量阶段

	resp, err := client.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("摘流量阶段服务仍应可访问: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("下线开始后 /readyz 应为 503，实际 %d", resp.StatusCode)
	}

	if got := <-streamDone; got != "xxxxxx" {
		t.Errorf("在途流必须完整结束，期望 6 个字节，实际 %q", got)
	}
	if err := <-shutdownErr; err != nil {
		t.Errorf("在宽限期内完成，不应报错: %v", err)
	}
}

// 宽限期用完仍有请求没结束：强制断开并返回错误（进程不能无限悬挂）。
func TestGracefulShutdownForcesCloseAfterGrace(t *testing.T) {
	d := &health.Drainer{}
	srv, base := startTestServer(t, d, 50, 100*time.Millisecond) // 流约 5s，远超宽限期
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	streamDone := make(chan string, 1)
	go func() {
		resp, err := client.Get(base + "/stream")
		if err != nil {
			streamDone <- ""
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		streamDone <- string(b)
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	err := gracefulShutdown(srv, d, 10*time.Millisecond, 200*time.Millisecond, discardLogger())

	if err == nil {
		t.Fatal("宽限期内没结束，应返回错误")
	}
	if !strings.Contains(err.Error(), "强制") {
		t.Errorf("错误信息应说明是强制断开，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("应在宽限期后很快返回，实际 %v", elapsed)
	}
	if got := <-streamDone; len(got) >= 50 {
		t.Errorf("流应被强制中断，实际收到 %d 字节", len(got))
	}
}
