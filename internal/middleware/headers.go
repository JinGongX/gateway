package middleware

import (
	"math"
	"time"
)

// retryAfterForRate 返回令牌桶"补回一个令牌"所需的整数秒（向上取整，至少 1）。
// 限流器只回答"放行/拒绝"，不返回精确的剩余等待时间，所以这里按 1/rps 估算：
// 对客户端来说"至少等这么久再重试"是个保守且够用的值。
func retryAfterForRate(rps float64) int {
	if rps <= 0 {
		return 1
	}
	s := int(math.Ceil(1 / rps))
	if s < 1 {
		return 1
	}
	return s
}

// secondsUntil 返回距离 reset 还有多少整秒（向上取整，至少 1）。
// reset 为零值表示"不知道"，返回 0，调用方据此不输出 Retry-After。
func secondsUntil(reset, now time.Time) int {
	if reset.IsZero() {
		return 0
	}
	s := int(math.Ceil(reset.Sub(now).Seconds()))
	if s < 1 {
		return 1
	}
	return s
}
