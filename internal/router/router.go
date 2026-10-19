// Package router 维护"模型名 -> 后端地址"的映射表。
// 对应【阶段四：多模型路由】
package router

import "sync"

type ModelRoute struct {
	Backend string
}

// Router 是模型名到后端地址的查找表。
// 学习点：这里同样用 RWMutex，因为路由表在运行期几乎只被读取
// （查找模型对应哪个后端），几乎不会被修改，读写锁比普通锁更合适。
type Router struct {
	mu     sync.RWMutex
	routes map[string]ModelRoute
}

func NewRouter() *Router {
	return &Router{routes: make(map[string]ModelRoute)}
}

func (r *Router) Register(model string, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[model] = ModelRoute{Backend: backend}
}

func (r *Router) Resolve(model string) (ModelRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[model]
	return route, ok
}
