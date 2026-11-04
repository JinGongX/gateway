package router

import (
	"net/http"
	"sort"
)

// Endpoint 是一条对外开放的「方法 + 精确路径」。
type Endpoint struct {
	Method string
	Path   string
}

// EndpointPolicy 是网关对外开放的接口白名单。
//
// 为什么需要它：网关用 api.Any("/*path") 把 /api/ 下的一切原样转给后端。
// 对 Ollama 来说，这意味着任何持有合法 key 的租户都能调用
// /api/pull（拉任意镜像撑爆磁盘）、/api/delete（删模型）、/api/create、/api/copy、/api/push。
// 网关的职责之一就是把「推理」和「运维」两类接口隔开：只放行明确列出的推理接口，其余一律拒绝。
//
// 匹配规则是「精确匹配」，不做 path.Clean，也不容忍尾部斜杠：
// /api/generate/ 和 /api/generate/../pull 都不在白名单里。
// 白名单检查和代理转发用的是同一个 URL.Path，所以不存在"检查的是 A、转发的是 B"的歧义。
type EndpointPolicy struct {
	methods map[string]map[string]struct{} // path -> 允许的方法集合
}

func NewEndpointPolicy(eps ...Endpoint) *EndpointPolicy {
	p := &EndpointPolicy{methods: make(map[string]map[string]struct{}, len(eps))}
	for _, ep := range eps {
		set, ok := p.methods[ep.Path]
		if !ok {
			set = make(map[string]struct{})
			p.methods[ep.Path] = set
		}
		set[ep.Method] = struct{}{}
	}
	return p
}

// DefaultEndpointPolicy 只开放 Ollama 的推理类接口。
// 故意不含 /api/tags、/api/show、/api/ps、/api/version：它们会暴露后端装了哪些模型、
// 显存占用等内部信息，而且这类 GET 请求没有 body，也过不了 RoutingMiddleware。
// 接入 OpenAI 兼容路径（/v1/...）时，在这里追加对应的条目即可。
func DefaultEndpointPolicy() *EndpointPolicy {
	return NewEndpointPolicy(
		Endpoint{http.MethodPost, "/api/generate"},
		Endpoint{http.MethodPost, "/api/chat"},
		Endpoint{http.MethodPost, "/api/embed"},
		Endpoint{http.MethodPost, "/api/embeddings"},
	)
}

// Verdict 是白名单检查的结果。
type Verdict int

const (
	Allowed          Verdict = iota // 放行
	NotFound                        // 路径不在白名单
	MethodNotAllowed                // 路径在白名单，但方法不对
)

// Check 返回判定结果；当结果是 MethodNotAllowed 时，同时返回该路径允许的方法（已排序），
// 供调用方填 Allow 响应头。
func (p *EndpointPolicy) Check(method, path string) (Verdict, []string) {
	set, ok := p.methods[path]
	if !ok {
		return NotFound, nil
	}
	if _, ok := set[method]; ok {
		return Allowed, nil
	}
	allowed := make([]string, 0, len(set))
	for m := range set {
		allowed = append(allowed, m)
	}
	sort.Strings(allowed)
	return MethodNotAllowed, allowed
}
