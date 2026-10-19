package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunReportsEveryCheck(t *testing.T) {
	p := NewProber(time.Second, 0,
		Check{Name: "redis", Fn: func(context.Context) error { return nil }},
		Check{Name: "postgres", Fn: func(context.Context) error { return errors.New("connection refused") }},
	)

	results, healthy := p.Run(context.Background())

	if healthy {
		t.Error("有检查失败时整体不应判定为健康")
	}
	if len(results) != 2 {
		t.Fatalf("两项检查都应被报告（不能遇到失败就短路），实际 %d 项", len(results))
	}
	if !results[0].OK || results[0].Name != "redis" {
		t.Errorf("redis 检查结果异常: %+v", results[0])
	}
	if results[1].OK || results[1].Error == "" {
		t.Errorf("postgres 应该失败并带上原因: %+v", results[1])
	}
}

func TestRunTimesOutSlowCheck(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)

	p := NewProber(20*time.Millisecond, 0, Check{
		Name: "redis",
		Fn: func(ctx context.Context) error {
			select {
			case <-blocked:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})

	start := time.Now()
	_, healthy := p.Run(context.Background())

	if healthy {
		t.Error("超时的检查应判定为失败")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("探针应该按超时时间返回，实际耗时 %s", elapsed)
	}
}

func TestCacheAvoidsRepeatedProbing(t *testing.T) {
	calls := 0
	p := NewProber(time.Second, 500*time.Millisecond, Check{
		Name: "redis",
		Fn:   func(context.Context) error { calls++; return nil },
	})

	now := time.Now()
	p.now = func() time.Time { return now }

	p.Run(context.Background())
	p.Run(context.Background())
	if calls != 1 {
		t.Errorf("缓存期内不应重复探测，实际执行了 %d 次", calls)
	}

	// 推进到缓存过期之后，应该重新探测
	now = now.Add(time.Second)
	p.Run(context.Background())
	if calls != 2 {
		t.Errorf("缓存过期后应重新探测，实际执行了 %d 次", calls)
	}
}

func TestCachedSliceIsCopied(t *testing.T) {
	p := NewProber(time.Second, time.Minute, Check{
		Name: "redis", Fn: func(context.Context) error { return nil },
	})

	results, _ := p.Run(context.Background())
	results[0].OK = false // 篡改调用方拿到的副本

	again, healthy := p.Run(context.Background())
	if !again[0].OK || !healthy {
		t.Error("调用方改到返回值不应该污染缓存内容")
	}
}

func TestStringSummarisesFailure(t *testing.T) {
	p := NewProber(time.Second, 0)
	summary := p.String([]Result{{Name: "redis", OK: false}, {Name: "postgres", OK: true}}, false)
	if !strings.Contains(summary, "redis") {
		t.Errorf("摘要里应该列出失败的检查名，实际: %s", summary)
	}
}
