package router

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDefaultEndpointPolicy(t *testing.T) {
	p := DefaultEndpointPolicy()

	cases := []struct {
		name         string
		method, path string
		want         Verdict
		wantAllow    []string
	}{
		{"generate", http.MethodPost, "/api/generate", Allowed, nil},
		{"chat", http.MethodPost, "/api/chat", Allowed, nil},
		{"embed", http.MethodPost, "/api/embed", Allowed, nil},
		{"embeddings", http.MethodPost, "/api/embeddings", Allowed, nil},

		// Ollama 的运维类接口：绝不能对租户开放
		{"pull", http.MethodPost, "/api/pull", NotFound, nil},
		{"push", http.MethodPost, "/api/push", NotFound, nil},
		{"delete", http.MethodDelete, "/api/delete", NotFound, nil},
		{"create", http.MethodPost, "/api/create", NotFound, nil},
		{"copy", http.MethodPost, "/api/copy", NotFound, nil},
		{"tags 泄露已装模型", http.MethodGet, "/api/tags", NotFound, nil},

		// 路径变形：必须是精确匹配
		{"尾部斜杠", http.MethodPost, "/api/generate/", NotFound, nil},
		{"路径穿越", http.MethodPost, "/api/generate/../pull", NotFound, nil},
		{"大小写", http.MethodPost, "/API/generate", NotFound, nil},
		{"空路径", http.MethodPost, "", NotFound, nil},

		// 路径对、方法错
		{"GET generate", http.MethodGet, "/api/generate", MethodNotAllowed, []string{"POST"}},
		{"DELETE chat", http.MethodDelete, "/api/chat", MethodNotAllowed, []string{"POST"}},
		{"OPTIONS embed", http.MethodOptions, "/api/embed", MethodNotAllowed, []string{"POST"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, allow := p.Check(tc.method, tc.path)
			if got != tc.want {
				t.Fatalf("%s %s: 期望 %v，实际 %v", tc.method, tc.path, tc.want, got)
			}
			if !reflect.DeepEqual(allow, tc.wantAllow) {
				t.Errorf("Allow 期望 %v，实际 %v", tc.wantAllow, allow)
			}
		})
	}
}

func TestEndpointPolicyReportsAllMethodsSorted(t *testing.T) {
	p := NewEndpointPolicy(
		Endpoint{http.MethodPost, "/x"},
		Endpoint{http.MethodGet, "/x"},
	)
	verdict, allow := p.Check(http.MethodDelete, "/x")
	if verdict != MethodNotAllowed || !reflect.DeepEqual(allow, []string{"GET", "POST"}) {
		t.Errorf("实际 (%v, %v)", verdict, allow)
	}
}
