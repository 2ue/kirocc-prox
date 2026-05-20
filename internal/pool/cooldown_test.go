package pool

import (
	"testing"
	"time"
)

func TestNextBackoff_ExponentialSchedule(t *testing.T) {
	cases := []struct {
		level int
		want  time.Duration
	}{
		{0, 30 * time.Second},
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 240 * time.Second},
		{4, 480 * time.Second},
		{5, 960 * time.Second},
		{6, 1800 * time.Second}, // 30 min cap
		{7, 30 * time.Minute},   // clamped at max
		{-1, 30 * time.Second},  // negative treated as zero
	}
	for _, c := range cases {
		got := NextBackoff(c.level, 0)
		if got != c.want {
			t.Errorf("NextBackoff(%d,0) = %s want %s", c.level, got, c.want)
		}
	}
}

func TestNextBackoff_RetryAfterOverride(t *testing.T) {
	// Below floor: clamped to base.
	if got := NextBackoff(0, 5*time.Second); got != DefaultBaseBackoff {
		t.Errorf("retryAfter=5s expected clamp to base, got %s", got)
	}
	// Above ceiling: clamped to max.
	if got := NextBackoff(0, 2*time.Hour); got != DefaultMaxBackoff {
		t.Errorf("retryAfter=2h expected clamp to max, got %s", got)
	}
	// In range: returned as-is.
	in := 2 * time.Minute
	if got := NextBackoff(3, in); got != in {
		t.Errorf("retryAfter=%s should win over level=3, got %s", in, got)
	}
}
