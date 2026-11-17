package middleware

import (
	"testing"
	"time"
)

func TestRetryAfterForRate(t *testing.T) {
	cases := []struct {
		rps  float64
		want int
	}{
		{10, 1}, {2, 1}, {1, 1}, {3, 1},
		{0.5, 2}, {0.1, 10}, {0.3, 4},
		{0, 1}, {-5, 1},
	}
	for _, tc := range cases {
		if got := retryAfterForRate(tc.rps); got != tc.want {
			t.Errorf("rps=%v 期望 %d，实际 %d", tc.rps, tc.want, got)
		}
	}
}

func TestSecondsUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		reset time.Time
		want  int
	}{
		{"未知", time.Time{}, 0},
		{"1.2 秒后向上取整", now.Add(1200 * time.Millisecond), 2},
		{"整 90 秒", now.Add(90 * time.Second), 90},
		{"一小时", now.Add(time.Hour), 3600},
		{"已过期至少给 1", now.Add(-time.Minute), 1},
		{"不足 1 秒至少给 1", now.Add(10 * time.Millisecond), 1},
	}
	for _, tc := range cases {
		if got := secondsUntil(tc.reset, now); got != tc.want {
			t.Errorf("%s: 期望 %d，实际 %d", tc.name, tc.want, got)
		}
	}
}
