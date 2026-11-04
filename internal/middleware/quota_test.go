package middleware

import (
	"net/http"
	"testing"
)

// 把计费口径钉死：谁改了 shouldRefundQuota，这里会第一个报警。
func TestShouldRefundQuota(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{http.StatusOK, false, "正常"},
		{http.StatusFound, false, "3xx 正常"},
		{http.StatusBadRequest, false, "调用方参数问题，照常计费"},
		{http.StatusNotFound, false, "下游透传的 404，照常计费"},
		{http.StatusTooManyRequests, false, "下游透传的 429，照常计费"},
		{499, false, "客户端提前断开：上游可能已在计算，照常计费"},
		{http.StatusInternalServerError, true, "网关内部错误"},
		{http.StatusBadGateway, true, "下游不可用"},
		{http.StatusServiceUnavailable, true, "下游过载"},
		{http.StatusGatewayTimeout, true, "上游超时"},
	}
	for _, tc := range cases {
		if got := shouldRefundQuota(tc.status); got != tc.want {
			t.Errorf("status=%d（%s）：期望退款=%v，实际 %v", tc.status, tc.why, tc.want, got)
		}
	}
}
