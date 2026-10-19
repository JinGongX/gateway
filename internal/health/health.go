// Package health 实现"就绪探针"（readiness probe）：检查网关依赖的
// 外部组件（Redis / PostgreSQL / 模型后端）现在能不能用。
//
// 为什么要跟存活探针分开：
//   - 存活探针（/healthz）只回答"进程还活着吗"。它必须非常轻、绝不依赖外部组件，
//     否则 Redis 抖一下，K8s 就会以为整个容器死了，把所有副本一起重启 ——
//     依赖故障变成了自己制造的一次全面雪崩。
//   - 就绪探针（/readyz）回答"现在能接流量吗"。依赖挂了就应该返回失败，
//     让负载均衡把这个实例摘出去，但**不要重启它**，等依赖恢复后自动重新加入。
package health

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Check 是一项被检查的依赖。Fn 返回 nil 表示健康。
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Result 是单项检查的结果，会以 JSON 形式返回给探针调用方。
type Result struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Prober 按顺序执行一组检查，并把结果短暂缓存。
type Prober struct {
	checks  []Check
	timeout time.Duration // 单个检查的超时
	ttl     time.Duration // 结果缓存时长（<=0 表示不缓存）

	// now 抽出来是为了让测试能控制时间，不用真的 sleep
	now func() time.Time

	mu       sync.Mutex
	cached   []Result
	cachedOK bool
	expires  time.Time
}

// NewProber 创建探针。
//
// ttl 的意义：K8s 可能同时有多个探针（liveness/readiness/startup）以很高的
// 频率打过来，外面还可能有监控系统在轮询。没有缓存的话，每次探测都要真的去
// Ping 一遍 Redis 和数据库，依赖紧张的时候等于自己给自己加压。
// 缓存 1 秒左右既能挡住这些重复探测，又不会让"依赖刚恢复"这个消息迟到太久。
func NewProber(timeout, ttl time.Duration, checks ...Check) *Prober {
	return &Prober{
		checks:  checks,
		timeout: timeout,
		ttl:     ttl,
		now:     time.Now,
	}
}

// Run 执行全部检查，返回每一项的结果以及"是否全部健康"。
//
// 注意：这里故意不做"遇到第一个失败就返回"，而是把每一项都跑完 ——
// 运维最需要的信息是"到底哪几个依赖挂了"，一次探完比一个个试要快得多。
func (p *Prober) Run(ctx context.Context) ([]Result, bool) {
	if results, ok, hit := p.fromCache(); hit {
		return results, ok
	}

	results := make([]Result, 0, len(p.checks))
	healthy := true

	for _, check := range p.checks {
		// 每一项检查都有独立的超时：Redis 卡住不应该让整次探测一直挂着，
		// 探针超时本身也会被 K8s 判为失败，但那样就看不到其他依赖的状态了。
		checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
		err := check.Fn(checkCtx)
		cancel()

		result := Result{Name: check.Name, OK: err == nil}
		if err != nil {
			result.Error = err.Error()
			healthy = false
		}
		results = append(results, result)
	}

	p.store(results, healthy)
	return results, healthy
}

func (p *Prober) fromCache() ([]Result, bool, bool) {
	if p.ttl <= 0 {
		return nil, false, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached == nil || p.now().After(p.expires) {
		return nil, false, false
	}
	// 返回副本，避免调用方（或编码 JSON 的过程）改到缓存内容
	return append([]Result(nil), p.cached...), p.cachedOK, true
}

func (p *Prober) store(results []Result, healthy bool) {
	if p.ttl <= 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.cached = append([]Result(nil), results...)
	p.cachedOK = healthy
	p.expires = p.now().Add(p.ttl)
}

// String 方便在日志里一眼看出哪项失败了。
func (p *Prober) String(results []Result, healthy bool) string {
	if healthy {
		return fmt.Sprintf("healthy (%d checks)", len(results))
	}
	failed := make([]string, 0, len(results))
	for _, r := range results {
		if !r.OK {
			failed = append(failed, r.Name)
		}
	}
	return fmt.Sprintf("unhealthy, failed: %v", failed)
}
