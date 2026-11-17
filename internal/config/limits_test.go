package config

import (
	"strings"
	"testing"
)

func TestValidateNonNegative(t *testing.T) {
	if err := validateNonNegative(map[string]int{"a": 0, "b": 5}); err != nil {
		t.Errorf("0 和正数都合法: %v", err)
	}
	if err := validateNonNegative(nil); err != nil {
		t.Errorf("空集合合法: %v", err)
	}

	err := validateNonNegative(map[string]int{
		"server.max_stream_seconds": -1,
		"upstream.dial_timeout_ms":  -5,
	})
	if err == nil {
		t.Fatal("负数应被拒绝")
	}
	// 排序后第一个出错的字段是确定的
	if !strings.Contains(err.Error(), "server.max_stream_seconds") {
		t.Errorf("应指出具体字段，实际: %v", err)
	}
}
