package health

import "sync/atomic"

// Drainer 表示"本实例正在下线，不要再给我派新流量"。
//
// 优雅关闭的第一步不是 srv.Shutdown，而是先让 /readyz 返回 503，
// 等负载均衡 / K8s Endpoints 把本实例摘掉之后，再开始 Shutdown。
// 顺序反过来的话，在"收到 SIGTERM"到"LB 感知到摘除"之间的几秒钟里，
// 仍会有新请求打进来，被 Shutdown 拒掉，用户看到的就是连接被重置。
//
// 零值可用；对 nil 指针调用 Draining() 恒为 false，方便测试里不传。
type Drainer struct {
	draining atomic.Bool
}

// Start 进入下线状态，不可逆。
func (d *Drainer) Start() { d.draining.Store(true) }

// Draining 报告是否已进入下线状态。
func (d *Drainer) Draining() bool { return d != nil && d.draining.Load() }
