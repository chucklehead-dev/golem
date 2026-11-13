// Package price prices a call's usage (contracts.md §4): micro-units from
// integer token counts and rates in micro-units per million tokens, rounded
// half-even once, at the end.
package price

import (
	"math"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Cost is usage's cost under p, in micro-units.
func Cost(u canon.Usage, p snapshot.Price) int64 {
	n, ok := numerator([2]int64{u.Input, p.InputPerMTok}, [2]int64{u.Output, p.OutputPerMTok},
		[2]int64{u.CacheRead, p.CacheReadPerMTok}, [2]int64{u.CacheWrite, p.CacheWritePerMTok})
	if !ok {
		return MaxCost // an absurd report saturates: never wraps to a small or negative spend
	}
	return min(halfEven(n, 1_000_000), MaxCost)
}

// MaxCost is the most one call is charged (nine billion units of the
// currency): a provider's absurd usage report is charged this, which ends
// any budget, and sums of a few such costs cannot overflow the counters.
const MaxCost = math.MaxInt64 / 1024

// Worst is a call's declared worst-case cost, for the declared-worst-case check
// (ADR 0031, 0038, 0044): every input byte a token at the full input rate,
// and the output cap at the output rate, no cache discount. Bytes bound
// text tokens from above; they do not bound images, audio or files, or
// units billed outside tokens: a declaration, not a ceiling.
func Worst(inputBytes, maxOutput, completions int64, p snapshot.Price) (cost int64, ok bool) {
	output, ok := product(maxOutput, completions)
	if !ok {
		return 0, false
	}
	n, ok := numerator([2]int64{inputBytes, p.InputPerMTok}, [2]int64{output, p.OutputPerMTok})
	if !ok {
		return 0, false
	}
	return halfEven(n, 1_000_000), true
}

func product(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a != 0 && b > math.MaxInt64/a {
		return 0, false
	}
	return a * b, true
}

func numerator(terms ...[2]int64) (int64, bool) {
	var total int64
	for _, term := range terms {
		n, ok := product(term[0], term[1])
		if !ok || n > math.MaxInt64-total {
			return 0, false
		}
		total += n
	}
	return total, true
}

func halfEven(n, d int64) int64 {
	q, r := n/d, n%d
	switch {
	case 2*r > d, 2*r == d && q%2 == 1:
		q++
	}
	return q
}
