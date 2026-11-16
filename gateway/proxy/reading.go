package proxy

import (
	"cmp"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/lib/contract"
)

// Reading bounds the request bodies a gateway reads at once, before any
// admission: one valid key could otherwise hold many stalled uploads of
// up to MaxBody each, each buffered for up to BodyTimeout, and take the
// pod's memory. The bytes are counted as they arrive (not as claimed: a
// claim of 32 MiB that sends two bytes holds two), against MaxBytes for
// the gateway, and the bodies against PerPrincipal for each principal;
// past either the request is refused limit.concurrency, retryable in a
// second. A body's bytes are freed
// as its reading ends: after it, admission bounds the call. The gateway's
// handlers share one; safe for concurrent use, the zero value ready with
// the defaults.
type Reading struct {
	// MaxBytes is the request-body bytes being read at once (default 512
	// MiB; negative: no bound).
	MaxBytes int64
	// PerPrincipal is the bodies one principal may have being read at once
	// (default 8; negative: no bound).
	PerPrincipal int

	mu          sync.Mutex
	bytes       int64
	byPrincipal map[string]int
}

// Reading's defaults.
const (
	defaultMaxReading          = 512 << 20
	defaultReadingPerPrincipal = 8
)

// The refusals of a body past Reading's bounds: retryable soon.
var ( // read-only
	readingFull = &refusal{code: contract.LimitConcurrency, retryAfter: time.Second,
		msg: "the gateway is reading too many request bodies at once; retry shortly"}
	readingYours = &refusal{code: contract.LimitConcurrency, retryAfter: time.Second,
		msg: "too many of your request bodies are being sent at once; retry when one has been sent"}
)

func (g *Reading) maxBytes() int64 { return cmp.Or(g.MaxBytes, defaultMaxReading) }

// start begins reading one body of principal's: the body's reader is to
// be wrapped by the returned bodyRead's, which counts what arrives, and
// end frees it all.
func (g *Reading) start(principal string) (*bodyRead, *refusal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := cmp.Or(g.PerPrincipal, defaultReadingPerPrincipal); n > 0 && g.byPrincipal[principal] >= n {
		return nil, readingYours
	}
	if m := g.maxBytes(); m > 0 && g.bytes >= m {
		return nil, readingFull
	}
	if g.byPrincipal == nil {
		g.byPrincipal = map[string]int{}
	}
	g.byPrincipal[principal]++
	return &bodyRead{g: g, principal: principal}, nil
}

// take counts n more bytes of a body being read, reporting false if they
// do not fit the gateway's bound.
func (g *Reading) take(n int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m := g.maxBytes(); m > 0 && g.bytes+n > m {
		return false
	}
	g.bytes += n
	return true
}

// Bytes reports the request-body bytes being read now (metrics, tests).
func (g *Reading) Bytes() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bytes
}

// bodyRead is one body's reading, and its bytes counted.
type bodyRead struct {
	g         *Reading
	principal string
	held      int64 // bytes counted against the gateway's bound
	r         io.Reader
}

// reader wraps r, counting what arrives.
func (b *bodyRead) reader(r io.Reader) io.Reader {
	b.r = r
	return b
}

func (b *bodyRead) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		if !b.g.take(int64(n)) {
			return n, errReadingFull
		}
		b.held += int64(n)
	}
	return n, err
}

// end frees the body's bytes and its place.
func (b *bodyRead) end() {
	g := b.g
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bytes -= b.held
	if n := g.byPrincipal[b.principal] - 1; n > 0 {
		g.byPrincipal[b.principal] = n
	} else {
		delete(g.byPrincipal, b.principal)
	}
}

// errReadingFull ends a body's reading past the gateway's bound.
var errReadingFull = errors.New("proxy: past the gateway's bound on request bodies being read") // read-only
