package price

import (
	"math"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

func TestCost(t *testing.T) {
	p := snapshot.Price{InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000, CacheReadPerMTok: 300_000, CacheWritePerMTok: 3_750_000}
	// 1000 in ($0.003), 500 out ($0.0075), 10000 cache read ($0.003), 200 cache write ($0.00075)
	if got := Cost(canon.Usage{Input: 1000, Output: 500, CacheRead: 10000, CacheWrite: 200}, p); got != 14250 {
		t.Fatalf("cost %d micro-units, want 14250", got)
	}
	for _, tc := range []struct{ n, want int64 }{{500_000, 0}, {1_500_000, 2}, {2_500_000, 2}, {2_500_001, 3}, {499_999, 0}} {
		if got := halfEven(tc.n, 1_000_000); got != tc.want {
			t.Errorf("halfEven(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
	if _, ok := Worst(1, math.MaxInt64, 2, p); ok {
		t.Error("Worst accepted maxOutput * completions overflow")
	}
	if c := Cost(canon.Usage{Output: 1 << 40}, snapshot.Price{OutputPerMTok: 75_000_000}); c != MaxCost {
		t.Errorf("an absurd usage report costs %d, want MaxCost: it must saturate, not wrap", c)
	}
}
