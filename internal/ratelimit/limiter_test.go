package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newLimiter(t *testing.T) (*RedisLimiter, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisLimiter(client), mr, client
}

func TestLimiterBurstThenReject(t *testing.T) {
	rl, _, _ := newLimiter(t)
	ctx := context.Background()

	want := []bool{true, true, false, false}
	for i, w := range want {
		got, err := rl.Allow(ctx, "t", 1, 2) // rps=1 burst=2
		if err != nil {
			t.Fatalf("第 %d 次出错: %v", i+1, err)
		}
		if got != w {
			t.Errorf("第 %d 次期望 %v，实际 %v", i+1, w, got)
		}
	}
}

// 令牌按 Redis 的时钟补充：把 Redis 的时间往前拨 2 秒，应补回 2 个令牌。
func TestLimiterRefillsUsingRedisClock(t *testing.T) {
	rl, mr, _ := newLimiter(t)
	ctx := context.Background()
	base := time.Now()
	mr.SetTime(base)

	for i := 0; i < 2; i++ {
		_, _ = rl.Allow(ctx, "t", 1, 2)
	}
	if ok, _ := rl.Allow(ctx, "t", 1, 2); ok {
		t.Fatal("桶应已空")
	}

	mr.SetTime(base.Add(2 * time.Second))
	for i := 0; i < 2; i++ {
		if ok, err := rl.Allow(ctx, "t", 1, 2); err != nil || !ok {
			t.Fatalf("2 秒后应补回 2 个令牌，第 %d 次: ok=%v err=%v", i+1, ok, err)
		}
	}
	if ok, _ := rl.Allow(ctx, "t", 1, 2); ok {
		t.Error("补回的令牌用完后应再次拒绝")
	}
}

func TestLimiterIsolatesTenants(t *testing.T) {
	rl, _, _ := newLimiter(t)
	ctx := context.Background()

	_, _ = rl.Allow(ctx, "a", 1, 1)
	if ok, _ := rl.Allow(ctx, "a", 1, 1); ok {
		t.Fatal("tenant a 的桶应已空")
	}
	if ok, _ := rl.Allow(ctx, "b", 1, 1); !ok {
		t.Error("tenant b 不应受 tenant a 影响")
	}
}

// 过期时间要覆盖"从空桶补满"的 2 倍：rps 很小的租户不能因为 key 提前过期而被白送一桶令牌。
func TestLimiterTTLCoversRefillTime(t *testing.T) {
	rl, mr, _ := newLimiter(t)
	ctx := context.Background()

	_, _ = rl.Allow(ctx, "slow", 0.001, 5) // 补满需要 5000 秒
	if ttl := mr.TTL("ratelimit:slow"); ttl < 5000*time.Second {
		t.Errorf("TTL 应不小于补满时间，实际 %v", ttl)
	}
	_, _ = rl.Allow(ctx, "fast", 100, 10)
	if ttl := mr.TTL("ratelimit:fast"); ttl < 3600*time.Second {
		t.Errorf("TTL 下限应为 3600s，实际 %v", ttl)
	}
}

func TestLimiterReturnsErrorWhenRedisIsDown(t *testing.T) {
	rl, mr, _ := newLimiter(t)
	mr.Close()

	ok, err := rl.Allow(context.Background(), "t", 1, 1)
	if err == nil {
		t.Fatal("Redis 不可用时应返回错误，让上层决定降级策略")
	}
	if ok {
		t.Error("出错时不应同时返回放行")
	}
}
