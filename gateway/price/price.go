// Package price prices a call's usage (contracts.md §4): micro-units from
// integer token counts and rates in micro-units per million tokens, rounded
// half-even once, at the end.
package price

import (
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Cost is usage's cost under p, in micro-units.
func Cost(u canon.Usage, p snapshot.Price) int64 {
	n := u.Input*p.InputPerMTok + u.Output*p.OutputPerMTok + u.CacheRead*p.CacheReadPerMTok + u.CacheWrite*p.CacheWritePerMTok
	return halfEven(n, 1_000_000)
}

// Worst is a call's declared worst-case cost, for the worst-case check
// (ADR 0031, 0038, 0044): every input byte a token at the full input rate,
// and the output cap at the output rate, no cache discount. Bytes bound
// text tokens from above; they do not bound images, audio or files, or
// units billed outside tokens: a declaration, not a ceiling.
func Worst(inputBytes, maxOutput int64, p snapshot.Price) int64 {
	return halfEven(inputBytes*p.InputPerMTok+maxOutput*p.OutputPerMTok, 1_000_000)
}

func halfEven(n, d int64) int64 {
	q, r := n/d, n%d
	switch {
	case 2*r > d, 2*r == d && q%2 == 1:
		q++
	}
	return q
}
