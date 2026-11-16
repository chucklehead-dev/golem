// Package guard bounds what a caller can make the gateway do before it
// knows who is calling (security-and-correctness.md): requests and failed
// authentications per source, and connections. Authenticated callers'
// limits are the limiter's (gateway/limit); this is the floor beneath them,
// for a flood of requests, or of invalid keys and tokens, that never
// authenticates.
package guard

import (
	"cmp"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Guard keeps two token buckets per source: requests (Rate a second, up to
// Burst), taken before authentication (Admit), and failed authentications
// (FailRate, FailBurst; Failed). A source whose failures have emptied its
// bucket is Failing until it refills: the request path then refuses each
// of its requests that fails authentication too, and an Entra token before
// its signature is checked (each costs more than a refusal), but still
// serves a valid golem key (a cheap lookup), so a flood from a shared
// address (a sidecar's) cannot lock out its valid callers. Safe for
// concurrent use; a nil Guard admits everything.
type Guard struct {
	// SourceHeader, if set, names a header the proxy in front of the
	// gateway sets to the caller's address, its last comma-separated value
	// taken (X-Forwarded-For, appended by the proxy last). Unset, the
	// source is the connection's address: behind a sidecar every caller's
	// is the sidecar's, and the budgets are this gateway's.
	SourceHeader string
	// Rate and Burst are each source's requests before authentication; 0:
	// no limit.
	Rate, Burst float64
	// FailRate and FailBurst are each source's failed authentications; 0:
	// no limit.
	FailRate, FailBurst float64
	// MaxSources bounds the sources kept (default 65536): past it, while
	// no kept source can be forgotten, new sources are not limited at all
	// (logged at most once a minute): one bucket they shared could be
	// drained by any of them, refusing every other.
	MaxSources int
	// Now is the clock (tests set it).
	Now func() time.Time
	// Log is where an overflowing source table is reported (default
	// slog's).
	Log *slog.Logger

	mu       sync.Mutex
	sources  map[string]*source
	swept    time.Time
	logged   time.Time // the last overflow logged
	overflow int       // sources unlimited since
	report   func()    // an overflow to log once mu is released
	// sweepMax bounds the sources one sweep looks at (default
	// sweepBatch): the sweep runs on a request's path, under mu.
	sweepMax int
}

// sweepBatch is how many sources a sweep looks at, at most: a full table's
// 65,536 under the lock every request takes would stall them all. Map
// iteration starts at a random place, so successive sweeps look at
// different sources.
const sweepBatch = 4096

// unlock releases mu, then logs an overflow found under it: no request
// waits on the log's write.
func (g *Guard) unlock() {
	report := g.report
	g.report = nil
	g.mu.Unlock()
	if report != nil {
		report()
	}
}

type source struct {
	req, fail bucket
}

// bucket is a token bucket, its tokens as of at.
type bucket struct {
	tokens float64
	at     time.Time
}

// fill brings b to now at rate, up to burst; a new bucket starts full.
func (b *bucket) fill(now time.Time, rate, burst float64) {
	if b.at.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+rate*now.Sub(b.at).Seconds())
	}
	b.at = now
}

// wait is how long b takes to hold one token at rate.
func (b *bucket) wait(rate float64) time.Duration {
	return time.Duration(math.Ceil((1-b.tokens)/rate)) * time.Second
}

func (g *Guard) on() bool { return g != nil && (g.Rate > 0 || g.FailRate > 0) }

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Source is r's source: its SourceHeader's last value, or the connection's
// host. A long value is cut, so a key costs at most 64 bytes.
func (g *Guard) Source(r *http.Request) string {
	if !g.on() {
		return ""
	}
	if g.SourceHeader != "" {
		if vs := r.Header.Values(g.SourceHeader); len(vs) > 0 {
			v := vs[len(vs)-1]
			if i := strings.LastIndexByte(v, ','); i >= 0 {
				v = v[i+1:]
			}
			if v = strings.TrimSpace(v); v != "" {
				return v[:min(len(v), 64)]
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Refusal is a request Admit refused: why, said to the caller, and when it
// may retry.
type Refusal struct {
	Msg        string
	RetryAfter time.Duration
}

// Admit takes one request of src's, or refuses it past the source's
// request rate. Its failures are Failing's.
func (g *Guard) Admit(src string) *Refusal {
	if g == nil || g.Rate <= 0 {
		return nil
	}
	g.mu.Lock()
	defer g.unlock()
	now := g.now()
	s := g.source(src, now)
	if s == nil {
		return nil // past MaxSources: not limited
	}
	s.req.fill(now, g.Rate, cmp.Or(g.Burst, g.Rate))
	if s.req.tokens < 1 {
		return &Refusal{Msg: "too many requests from this address; retry later", RetryAfter: s.req.wait(g.Rate)}
	}
	s.req.tokens--
	return nil
}

// Failing is the refusal of src's requests while it is past its failed
// authentications (with when to retry), or nil: what the request path
// tells such a source whose request fails, or whose credential would cost
// a signature check.
func (g *Guard) Failing(src string) *Refusal {
	if g == nil || g.FailRate <= 0 {
		return nil
	}
	g.mu.Lock()
	defer g.unlock()
	now := g.now()
	s := g.source(src, now)
	if s == nil {
		return nil
	}
	g.fillFail(s, now)
	if s.fail.tokens < 1 {
		return &Refusal{Msg: "too many failed authentications from this address: check the key or token; retry later", RetryAfter: s.fail.wait(g.FailRate)}
	}
	return nil
}

// Failed records one failed authentication of src's: one that was the
// credential's fault, not the gateway's or Entra's (the caller decides).
func (g *Guard) Failed(src string) {
	if g == nil || g.FailRate <= 0 {
		return
	}
	g.mu.Lock()
	defer g.unlock()
	now := g.now()
	s := g.source(src, now)
	if s == nil {
		return
	}
	g.fillFail(s, now)
	s.fail.tokens = max(s.fail.tokens-1, 0)
}

// fillFail brings s's failures bucket to now.
func (g *Guard) fillFail(s *source, now time.Time) {
	s.fail.fill(now, g.FailRate, cmp.Or(g.FailBurst, g.FailRate))
}

// source is src's buckets; at MaxSources, once a second, the sources whose
// buckets have refilled are forgotten (among at most sweepBatch looked at) (a
// forgotten source starts full, as it would be), and if none were, src has none
// (nil): it is not limited, rather than sharing a bucket any one source could
// drain for all.
func (g *Guard) source(src string, now time.Time) *source {
	if s := g.sources[src]; s != nil {
		return s
	}
	if g.sources == nil {
		g.sources = map[string]*source{}
	}
	limit := cmp.Or(g.MaxSources, 65536)
	if len(g.sources) >= limit {
		if now.Sub(g.swept) >= time.Second {
			g.swept = now
			n := cmp.Or(g.sweepMax, sweepBatch)
			for k, s := range g.sources {
				if n--; n < 0 {
					break
				}
				if s.req.full(now, g.Rate, g.Burst) && s.fail.full(now, g.FailRate, g.FailBurst) {
					delete(g.sources, k)
				}
			}
		}
		if len(g.sources) >= limit {
			g.overflowed(now, limit)
			return nil
		}
	}
	s := &source{}
	g.sources[src] = s
	return s
}

// overflowed reports a source left unlimited for want of room, at most once
// a minute.
func (g *Guard) overflowed(now time.Time, limit int) {
	g.overflow++
	if now.Sub(g.logged) < time.Minute {
		return
	}
	log, unlimited := cmp.Or(g.Log, slog.Default()), g.overflow
	g.report = func() {
		log.Warn("golem-gateway: the guard's source table is full; new sources are not limited before authentication until kept ones refill",
			"sources", limit, "unlimited", unlimited)
	}
	g.logged, g.overflow = now, 0
}

// full reports whether b would be full now (a bucket with no rate is).
func (b *bucket) full(now time.Time, rate, burst float64) bool {
	if rate <= 0 || b.at.IsZero() {
		return true
	}
	return b.tokens+rate*now.Sub(b.at).Seconds() >= cmp.Or(burst, rate)
}
