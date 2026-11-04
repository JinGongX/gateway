package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeClock 同时驱动 tracker 的时钟和 miniredis 的"当前时间"
// （miniredis 用它来解释 EXPIREAT 的绝对时间）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
	mr *miniredis.Miniredis
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
	c.mr.SetTime(t)
}

// today 返回"今天 UTC 零点"。测试时间都基于真实日期推算而不是写死年份：
// 这样即使 miniredis 版本用真实时钟解释 EXPIREAT，过期时间也一定在未来。
func today() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func newTracker(t *testing.T, start time.Time, opts ...QuotaOption) (*RedisQuotaTracker, *miniredis.Miniredis, *fakeClock) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	clk := &fakeClock{mr: mr}
	clk.set(start)
	all := append([]QuotaOption{WithQuotaClock(clk.now)}, opts...)
	return NewRedisQuotaTracker(client, all...), mr, clk
}

// 被拒绝的请求不能再推高计数。
func TestConsumeRejectsWithoutCounting(t *testing.T) {
	qt, _, _ := newTracker(t, today().Add(10*time.Hour))
	ctx := context.Background()

	allowed := 0
	for i := 0; i < 10; i++ {
		_, ok, err := qt.Consume(ctx, "tenant-a", 3)
		if err != nil {
			t.Fatalf("Consume 出错: %v", err)
		}
		if ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("cap=3 应放行 3 次，实际 %d", allowed)
	}
	if used, _ := qt.Usage(ctx, "tenant-a"); used != 3 {
		t.Errorf("被拒绝的请求不应计入用量，期望 3，实际 %d", used)
	}
}

// 旧实现的真实 bug：cap=1，A 通过、B 被拒（旧版会把计数推到 2），
// A 失败退款后计数仍是 1，C 被误拒，尽管租户今天实际用量是 0。
func TestRefundAfterRejectionFreesRealCapacity(t *testing.T) {
	qt, _, _ := newTracker(t, today().Add(10*time.Hour))
	ctx := context.Background()

	a, ok, err := qt.Consume(ctx, "t", 1)
	if err != nil || !ok {
		t.Fatalf("A 应被放行: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := qt.Consume(ctx, "t", 1); ok {
		t.Fatal("B 应因配额耗尽被拒绝")
	}
	if _, err := qt.Refund(ctx, a); err != nil {
		t.Fatalf("退款失败: %v", err)
	}
	if _, ok, _ := qt.Consume(ctx, "t", 1); !ok {
		t.Error("A 退款后额度应该真正回到 0，C 应被放行")
	}
}

// 对一个不存在的 key 退款：不能创建新 key（旧实现会先造出一个 -1 的 key 再修补）。
func TestRefundOnMissingKeyCreatesNothing(t *testing.T) {
	qt, mr, clk := newTracker(t, today().Add(10*time.Hour))
	key := quotaKey("ghost", clk.now())

	left, err := qt.Refund(context.Background(), Reservation{Key: key})
	if err != nil || left != 0 {
		t.Fatalf("期望 (0, nil)，实际 (%d, %v)", left, err)
	}
	if mr.Exists(key) {
		t.Error("退款不应该创建 key")
	}
}

func TestRefundNeverGoesNegative(t *testing.T) {
	qt, mr, _ := newTracker(t, today().Add(10*time.Hour))
	ctx := context.Background()

	res, _, _ := qt.Consume(ctx, "t", 5)
	for i := 0; i < 3; i++ { // 退款次数多于扣款次数
		if _, err := qt.Refund(ctx, res); err != nil {
			t.Fatalf("退款失败: %v", err)
		}
	}
	if got, _ := mr.Get(res.Key); got != "0" {
		t.Errorf("计数应钳位在 0，实际 %q", got)
	}
}

func TestEmptyReservationRefundIsNoop(t *testing.T) {
	qt, _, _ := newTracker(t, today().Add(10*time.Hour))
	if left, err := qt.Refund(context.Background(), Reservation{}); err != nil || left != 0 {
		t.Errorf("空 Reservation 应是 no-op，实际 (%d, %v)", left, err)
	}
}

// 23:59:59 预扣、次日 00:00:01 才失败退款：款必须退回昨天的 key，
// 而不是在新一天的 key 上凭空造一个。
func TestRefundTargetsReservationKeyAcrossMidnight(t *testing.T) {
	qt, mr, clk := newTracker(t, today().Add(23*time.Hour+59*time.Minute+59*time.Second))
	ctx := context.Background()

	res, ok, err := qt.Consume(ctx, "t", 10)
	if err != nil || !ok {
		t.Fatalf("Consume 失败: ok=%v err=%v", ok, err)
	}

	clk.set(today().Add(24*time.Hour + time.Second)) // 次日 00:00:01
	mr.FastForward(2 * time.Second)                  // miniredis 的 TTL 不会自己走，手动推进

	left, err := qt.Refund(ctx, res)
	if err != nil || left != 0 {
		t.Fatalf("昨天的 key 应还在并被退到 0，实际 (%d, %v)", left, err)
	}
	if mr.Exists(quotaKey("t", clk.now())) {
		t.Error("退款不应该在新一天的 key 上留下任何东西")
	}
}

// dailyCap <= 0 的语义：不限额，但仍然计数。
func TestZeroCapMeansUnlimitedButStillCounted(t *testing.T) {
	qt, _, _ := newTracker(t, today().Add(10*time.Hour))
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		if _, ok, err := qt.Consume(ctx, "t", 0); err != nil || !ok {
			t.Fatalf("cap=0 应永远放行，第 %d 次: ok=%v err=%v", i+1, ok, err)
		}
	}
	if used, _ := qt.Usage(ctx, "t"); used != 50 {
		t.Errorf("不限额也应计数，期望 50，实际 %d", used)
	}
}

// 过期时间丢失（比如旧版本崩溃留下的无 TTL key）会在下一次 Consume 时自愈。
func TestMissingTTLIsHealedByNextConsume(t *testing.T) {
	qt, mr, _ := newTracker(t, today().Add(10*time.Hour))
	ctx := context.Background()

	res, _, _ := qt.Consume(ctx, "t", 10)
	if mr.TTL(res.Key) <= 0 {
		t.Fatal("首次 Consume 后 key 应带过期时间")
	}

	// 模拟"没有 TTL 的僵尸 key"。注意不能用 mr.SetTTL(key, 0)：
	// miniredis 会把它当成"TTL 为 0 的条目"，而不是"没有 TTL"。mr.Set 会清除过期时间。
	if err := mr.Set(res.Key, "1"); err != nil {
		t.Fatalf("构造僵尸 key 失败: %v", err)
	}
	if mr.TTL(res.Key) != 0 {
		t.Fatal("前置条件：key 此时应没有 TTL")
	}
	if _, _, err := qt.Consume(ctx, "t", 10); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if ttl := mr.TTL(res.Key); ttl <= 0 || ttl > 25*time.Hour {
		t.Errorf("TTL 应被修复到约 1 天内，实际 %v", ttl)
	}
}

// 高并发下绝不超发：200 个并发请求抢 50 个额度，恰好 50 个成功。
func TestConcurrentConsumeNeverExceedsCap(t *testing.T) {
	qt, _, _ := newTracker(t, today().Add(10*time.Hour))
	ctx := context.Background()

	const cap, workers = 50, 200
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := qt.Consume(ctx, "t", cap)
			if err != nil {
				t.Errorf("Consume 出错: %v", err)
				return
			}
			if ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != cap {
		t.Errorf("应恰好放行 %d 个，实际 %d", cap, got)
	}
	if used, _ := qt.Usage(ctx, "t"); used != cap {
		t.Errorf("用量应恰好为 %d，实际 %d", cap, used)
	}
}

func TestDayBoundaryFollowsConfiguredLocation(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	base := today().Add(20 * time.Hour) // UTC 20:00 == 北京时间次日 04:00

	utcTracker, _, _ := newTracker(t, base)
	cstTracker, _, _ := newTracker(t, base, WithQuotaLocation(cst))

	utcRes, _, _ := utcTracker.Consume(context.Background(), "t", 10)
	cstRes, _, _ := cstTracker.Consume(context.Background(), "t", 10)

	if want := "quota:t:" + base.UTC().Format("2006-01-02"); utcRes.Key != want {
		t.Errorf("默认应按 UTC 记日期，期望 %s，实际 %s", want, utcRes.Key)
	}
	if want := "quota:t:" + base.In(cst).Format("2006-01-02"); cstRes.Key != want {
		t.Errorf("配置了时区应按该时区记日期，期望 %s，实际 %s", want, cstRes.Key)
	}
	if utcRes.Key == cstRes.Key {
		t.Error("两个时区在这个时刻应落在不同的日期上")
	}
}
