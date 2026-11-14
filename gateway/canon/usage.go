// Package canon is the canonical model every dialect's adapters translate
// to and from (canonical-model.md). The request path needs only usage so
// far.
package canon

// Usage is what a call consumed, as the provider reported it: the input to
// pricing (contracts.md §4).
type Usage struct {
	Input      int64 // uncached input tokens
	Output     int64 // output tokens, reasoning included
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64 // of Output, where the provider says
}
