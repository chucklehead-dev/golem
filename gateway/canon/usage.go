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
	CacheWrite int64 // every cache write, whatever its lifetime
	// CacheWrite1h is of CacheWrite, the writes cached for an hour, where
	// the provider says (Anthropic's cache_creation breakdown); the rest
	// are the five-minute writes (CacheWrite5m).
	CacheWrite1h int64
	Reasoning    int64 // of Output, where the provider says
}

// CacheWrite5m is the cache writes that are not CacheWrite1h's: the
// five-minute writes, the default lifetime.
func (u Usage) CacheWrite5m() int64 { return max(u.CacheWrite-u.CacheWrite1h, 0) }
