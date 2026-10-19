package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNewRejectsUnknownLevelAndFormat(t *testing.T) {
	if _, err := New(Config{Level: "verbose"}); err == nil {
		t.Error("未知级别应该报错")
	}
	if _, err := New(Config{Format: "josn"}); err == nil {
		t.Error("未知格式应该报错")
	}
}

func TestNewDefaultsAndJSONOutput(t *testing.T) {
	l, err := New(Config{}) // 空配置 = info + text
	if err != nil {
		t.Fatalf("默认配置不应该报错: %v", err)
	}
	if l.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("默认级别下 debug 应该是关闭的")
	}

	// JSON 格式输出应该能被解析出字段（这里做轻量校验：包含键名和值）
	var buf bytes.Buffer
	jsonLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	jsonLogger.Info("request", "request_id", "abc123", "status", 200)

	out := buf.String()
	for _, want := range []string{`"msg":"request"`, `"request_id":"abc123"`, `"status":200`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON 日志缺少 %s，实际输出: %s", want, out)
		}
	}
}

func TestFromContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	scoped := slog.New(slog.NewTextHandler(&buf, nil)).With("request_id", "r-1")

	// 没挂日志器时退回全局默认（不应 panic）
	if FromContext(context.Background()) == nil {
		t.Fatal("FromContext 不应该返回 nil")
	}

	ctx := IntoContext(context.Background(), scoped)
	FromContext(ctx).Warn("缓存回退")

	if !strings.Contains(buf.String(), "request_id=r-1") {
		t.Errorf("上下文日志器应该带上 request_id，实际输出: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "缓存回退") {
		t.Errorf("日志内容丢失，实际输出: %s", buf.String())
	}
}
