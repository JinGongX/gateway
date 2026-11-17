package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errRedisDown = errors.New("redis down")

type fakeLimiter struct {
	allow bool
	err   error
	calls int
}

func (f *fakeLimiter) Allow(ctx context.Context, tenantID string, rps float64, burst int) (bool, error) {
	f.calls++
	return f.allow, f.err
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestFallbackLimiterDelegatesWhenPrimaryHealthy(t *testing.T) {
	primary := &fakeLimiter{allow: false}
	notified := 0
	fl := NewFallbackLimiter(primary, 3, func(string, error) { notified++ })

	ok, err := fl.Allow(context.Background(), "t", 10, 10)
	if err != nil || ok {
		t.Errorf("primary 健康时应原样采用它的决定（拒绝），实际 ok=%v err=%v", ok, err)
	}
	if notified != 0 {
		t.Error("没有降级就不应通知")
	}
}

func TestFallbackLimiterBoundsTrafficWhenRedisDown(t *testing.T) {
	primary := &fakeLimiter{err: errRedisDown}
	var notifiedTenants []string
	fl := NewFallbackLimiter(primary, 2, func(tenant string, err error) {
		notifiedTenants = append(notifiedTenants, tenant)
		if !errors.Is(err, errRedisDown) {
			t.Errorf("通知里应带上原始错误，实际 %v", err)
		}
	})
	clk := &clock{t: time.Now()}
	fl.now = clk.now

	// rps=10 burst=4，2 个副本 -> 本地 rate=5、容量=2
	var got []bool
	for i := 0; i < 4; i++ {
		ok, err := fl.Allow(context.Background(), "t", 10, 4)
		if err != nil {
			t.Fatalf("降级后不应再向上返回错误: %v", err)
		}
		got = append(got, ok)
	}
	if want := []bool{true, true, false, false}; !equalBools(got, want) {
		t.Errorf("降级后应被限在本地容量内，期望 %v 实际 %v", want, got)
	}
	if len(notifiedTenants) != 4 || notifiedTenants[0] != "t" {
		t.Errorf("每次降级都应通知，实际 %v", notifiedTenants)
	}

	clk.add(400 * time.Millisecond) // 5/s * 0.4s = 2 个令牌
	for i := 0; i < 2; i++ {
		if ok, _ := fl.Allow(context.Background(), "t", 10, 4); !ok {
			t.Errorf("过了 400ms 应补回令牌，第 %d 次被拒", i+1)
		}
	}
	if ok, _ := fl.Allow(context.Background(), "t", 10, 4); ok {
		t.Error("补回的令牌用完后应再次拒绝")
	}
}

func TestFallbackLimiterNeverFullyBlocksATenant(t *testing.T) {
	fl := NewFallbackLimiter(&fakeLimiter{err: errRedisDown}, 100, nil)
	// burst=2、100 个副本：缩放后不足 1，容量应至少为 1
	if ok, _ := fl.Allow(context.Background(), "t", 1, 2); !ok {
		t.Error("容量至少为 1，首个请求应被放行")
	}
}

func TestFallbackLimiterIsolatesTenants(t *testing.T) {
	fl := NewFallbackLimiter(&fakeLimiter{err: errRedisDown}, 1, nil)
	_, _ = fl.Allow(context.Background(), "a", 1, 1)
	if ok, _ := fl.Allow(context.Background(), "a", 1, 1); ok {
		t.Fatal("a 的本地桶应已空")
	}
	if ok, _ := fl.Allow(context.Background(), "b", 1, 1); !ok {
		t.Error("b 不应受 a 影响")
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------

type fakeQuotaBackend struct {
	err                       error
	consumeCalls, refundCalls int
	lastRefund                Reservation
}

func (f *fakeQuotaBackend) Consume(ctx context.Context, tenantID string, dailyCap int) (Reservation, bool, error) {
	f.consumeCalls++
	if f.err != nil {
		return Reservation{}, false, f.err
	}
	return Reservation{Key: "quota:" + tenantID + ":primary", Used: 1}, true, nil
}

func (f *fakeQuotaBackend) Refund(ctx context.Context, res Reservation) (int64, error) {
	f.refundCalls++
	f.lastRefund = res
	return 0, nil
}

func newFallbackQuota(primary QuotaBackend, replicas int, clk *clock, notify FallbackNotify) *FallbackQuota {
	return NewFallbackQuota(primary, replicas, notify, WithFallbackClock(clk.now))
}

func TestFallbackQuotaDelegatesWhenPrimaryHealthy(t *testing.T) {
	primary := &fakeQuotaBackend{}
	fq := newFallbackQuota(primary, 2, &clock{t: time.Now()}, nil)

	res, ok, err := fq.Consume(context.Background(), "t", 10)
	if err != nil || !ok || res.Key != "quota:t:primary" {
		t.Errorf("应采用 primary 的结果，实际 res=%+v ok=%v err=%v", res, ok, err)
	}
	if _, err := fq.Refund(context.Background(), res); err != nil || primary.refundCalls != 1 {
		t.Errorf("primary 的 reservation 应退给 primary，refundCalls=%d err=%v", primary.refundCalls, err)
	}
}

func TestFallbackQuotaCapsPerReplicaWhenRedisDown(t *testing.T) {
	primary := &fakeQuotaBackend{err: errRedisDown}
	notified := 0
	fq := newFallbackQuota(primary, 2, &clock{t: time.Now()}, func(string, error) { notified++ })

	// dailyCap=10、2 个副本 -> 本地上限 5
	allowed := 0
	var last Reservation
	for i := 0; i < 8; i++ {
		res, ok, err := fq.Consume(context.Background(), "t", 10)
		if err != nil {
			t.Fatalf("降级后不应再向上返回错误: %v", err)
		}
		if ok {
			allowed++
			last = res
		}
	}
	if allowed != 5 {
		t.Errorf("本地上限应为 ceil(10/2)=5，实际放行 %d", allowed)
	}
	if notified != 8 {
		t.Errorf("每次降级都应通知，实际 %d", notified)
	}

	// 本地预扣的款退回本地，空出一个名额；且不能碰 primary
	if _, err := fq.Refund(context.Background(), last); err != nil {
		t.Fatal(err)
	}
	if primary.refundCalls != 0 {
		t.Error("本地 reservation 绝不能退到 Redis 上")
	}
	if _, ok, _ := fq.Consume(context.Background(), "t", 10); !ok {
		t.Error("退款后应再空出一个名额")
	}
}

func TestFallbackQuotaZeroCapIsUnlimited(t *testing.T) {
	fq := newFallbackQuota(&fakeQuotaBackend{err: errRedisDown}, 3, &clock{t: time.Now()}, nil)
	for i := 0; i < 100; i++ {
		if _, ok, _ := fq.Consume(context.Background(), "t", 0); !ok {
			t.Fatalf("dailyCap=0 表示不限额，第 %d 次被拒", i+1)
		}
	}
}

func TestFallbackQuotaLocalLedgerResetsAtMidnight(t *testing.T) {
	n := time.Now().UTC()
	clk := &clock{t: time.Date(n.Year(), n.Month(), n.Day(), 23, 0, 0, 0, time.UTC)}
	fq := newFallbackQuota(&fakeQuotaBackend{err: errRedisDown}, 1, clk, nil)

	if _, ok, _ := fq.Consume(context.Background(), "t", 1); !ok {
		t.Fatal("首个请求应放行")
	}
	if _, ok, _ := fq.Consume(context.Background(), "t", 1); ok {
		t.Fatal("当天第二个请求应被拒")
	}
	clk.add(2 * time.Hour) // 跨过零点
	res, ok, _ := fq.Consume(context.Background(), "t", 1)
	if !ok {
		t.Error("次日应重新计数")
	}
	if res.ResetAt.IsZero() || !res.ResetAt.After(clk.now()) {
		t.Errorf("ResetAt 应是未来时刻，实际 %v", res.ResetAt)
	}
}

func TestFallbackQuotaRefundOfUnknownLocalKeyIsNoop(t *testing.T) {
	fq := newFallbackQuota(&fakeQuotaBackend{}, 1, &clock{t: time.Now()}, nil)
	if left, err := fq.Refund(context.Background(), Reservation{Key: "local:quota:gone:2000-01-01"}); err != nil || left != 0 {
		t.Errorf("期望 (0, nil)，实际 (%d, %v)", left, err)
	}
}

var (
	_ Limiter      = (*RedisLimiter)(nil)
	_ QuotaBackend = (*RedisQuotaTracker)(nil)
	_ Limiter      = (*FallbackLimiter)(nil)
	_ QuotaBackend = (*FallbackQuota)(nil)
)
