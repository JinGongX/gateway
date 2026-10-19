// Package logger 负责创建结构化日志器，并提供"把日志器挂进 context"的能力。
//
// 为什么不用标准库的 log 或者继续 fmt.Printf：
//   - 结构化日志（key=value 或 JSON）才能被日志平台采集、按字段检索
//     （比如"查某个 request_id 的全部日志"、"统计所有 tenant-a 的错误"）
//   - 有级别才能按环境调节噪音：本地 debug 全开，线上只留 info 以上
//   - 把 logger 挂进 context 之后，连 pgstore/ratelimit 这些不认识 gin 的
//     底层包也能自动带上 request_id，不用把参数一层层透传
package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Config 对应 yaml 里的 log 节点。
type Config struct {
	Level  string `yaml:"level"`  // debug / info / warn / error，空值视为 info
	Format string `yaml:"format"` // text（本地可读）/ json（日志平台采集），空值视为 text
}

// New 按配置创建日志器。配置写错会直接返回错误（fail fast），
// 而不是悄悄降级成默认值 —— 写了 LOG_FORMAT=josn 却拿到 text 输出，
// 排查起来比启动失败麻烦得多。
func New(cfg Config) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "", "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		return nil, fmt.Errorf("不支持的日志格式 %q（可选值：text、json）", cfg.Format)
	}

	return slog.New(handler), nil
}

// parseLevel 把字符串级别转成 slog.Level。
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("不支持的日志级别 %q（可选值：debug、info、warn、error）", s)
	}
}

// ctxKey 是 context 里存放日志器的私有 key 类型。
// 用空结构体而不是字符串，避免和别的包用的 key 撞车（这是 Go 里放
// context 值的标准做法，比手写字符串 key 安全）。
type ctxKey struct{}

// IntoContext 把带好字段的日志器放进 context。
func IntoContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext 取出 context 里的日志器；没有就退回全局默认日志器。
//
// 为什么底层包（pgstore / ratelimit）也应该用它而不是直接 slog.Warn：
// 这样"某次请求触发的缓存回退告警"会自动带上这次请求的 request_id 和租户，
// 排查线上问题时可以直接按 request_id 把整条链路的日志拉出来。
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}
