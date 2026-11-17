package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadlineRecorder 是一个"支持写超时"的 ResponseWriter，记录收到的每个截止时间。
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.deadlines = append(r.deadlines, t)
	return nil
}

func TestDeadlineWriterExtendsDeadlineOnEveryWrite(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := NewDeadlineWriter(rec, 30*time.Second, 0)

	before := time.Now()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("a"))
	time.Sleep(20 * time.Millisecond)
	_, _ = w.Write([]byte("b"))

	if len(rec.deadlines) != 3 {
		t.Fatalf("WriteHeader + 2 次 Write 应各续期一次，实际 %d 次", len(rec.deadlines))
	}
	for i, dl := range rec.deadlines {
		if dl.Before(before.Add(30*time.Second)) || dl.After(time.Now().Add(30*time.Second)) {
			t.Errorf("第 %d 次截止时间应约为 now+30s，实际 %v", i, dl)
		}
	}
	if !rec.deadlines[2].After(rec.deadlines[0]) {
		t.Error("持续写出时截止时间应不断往后推（滚动），而不是固定不变")
	}
	if rec.Body.String() != "ab" {
		t.Errorf("数据应原样写出，实际 %q", rec.Body.String())
	}
	if w.Unsupported() {
		t.Error("支持 SetWriteDeadline 的 writer 不应被判定为 unsupported")
	}
}

func TestDeadlineWriterNeverExceedsHardCap(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	w := NewDeadlineWriter(rec, time.Hour, 50*time.Millisecond) // 滚动窗口很长，但总时长只有 50ms

	_, _ = w.Write([]byte("x"))
	time.Sleep(10 * time.Millisecond)
	_, _ = w.Write([]byte("y"))

	hard := start.Add(50 * time.Millisecond)
	for i, dl := range rec.deadlines {
		if dl.After(hard.Add(5 * time.Millisecond)) {
			t.Errorf("第 %d 次截止时间 %v 超过了总时长上限 %v", i, dl, hard)
		}
	}
}

func TestDeadlineWriterHardCapAloneWithoutIdle(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := NewDeadlineWriter(rec, 0, time.Minute)
	_, _ = w.Write([]byte("x"))
	if len(rec.deadlines) != 1 {
		t.Errorf("只配置总时长上限时也应设置截止时间，实际 %d 次", len(rec.deadlines))
	}
}

// 底层不支持写超时（比如 httptest.ResponseRecorder）：数据照常写出，只是保护不生效，并且能被察觉。
func TestDeadlineWriterDegradesGracefullyWhenUnsupported(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewDeadlineWriter(rec, time.Second, time.Minute)

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("写入不应失败: %v", err)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("数据应原样写出，实际 %q", rec.Body.String())
	}
	if !w.Unsupported() {
		t.Error("应报告写超时保护未生效，调用方才能告警")
	}
}

// 包装器必须保持 Flush 能力：ReverseProxy 靠 http.ResponseController 找到它。
func TestDeadlineWriterKeepsFlushReachable(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewDeadlineWriter(rec, time.Second, 0)

	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("Flush 应能穿过包装器到达底层: %v", err)
	}
	if !rec.Flushed {
		t.Error("底层 writer 应收到 Flush")
	}
}

// 端到端：上游吐出第一块数据后卡住，网关应在空闲超时后中断，而不是一直等。
func TestUpstreamBodyIdleTimeoutAbortsStalledStream(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "chunk-1\n")
		w.(http.Flusher).Flush()
		select { // 模拟后端假死：连接还在，但不再出数据
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()

	pool := NewPool(WithBodyIdleTimeout(150 * time.Millisecond))
	rp, err := pool.Get(stalled.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	start := time.Now()
	rp.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("应在约 150ms 空闲后中断，实际等了 %v", elapsed)
	}
	if rec.Body.String() != "chunk-1\n" {
		t.Errorf("已经收到的数据应原样转发给客户端，实际 %q", rec.Body.String())
	}
}

// 正常的流式响应（数据持续到达）不应被空闲超时误伤，且整体时长可以远超空闲窗口。
func TestUpstreamBodyIdleTimeoutDoesNotCutHealthyStream(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 8; i++ { // 总共 ~400ms，远超 200ms 的空闲窗口，但每次间隔只有 50ms
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer healthy.Close()

	pool := NewPool(WithBodyIdleTimeout(200 * time.Millisecond))
	rp, _ := pool.Get(healthy.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Body.String() != "xxxxxxxx" {
		t.Errorf("健康的流不应被截断，实际 %q", rec.Body.String())
	}
}
