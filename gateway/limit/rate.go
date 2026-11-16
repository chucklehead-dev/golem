package limit

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/valkey-io/valkey-go"
)

// Rate buckets: requests a minute, refilled continuously and holding at
// most a minute's worth (as providers meter their quotas). Requests
// only: tokens are the provider's own 429s to enforce, which the attempt
// loop fails over on, and budgets' to account (ADR 0031).

// Meter is a bucket a request is charged to.
type Meter = admission.Meter

// rateBucket is a bucket's state: the requests it holds, as of at.
type rateBucket struct {
	requests float64
	at       time.Time
}

// refill brings b to now at perMinute; a new bucket starts full.
func (b *rateBucket) refill(now time.Time, perMinute int64) {
	if b.at.IsZero() {
		b.requests, b.at = float64(perMinute), now
		return
	}
	b.requests = min(float64(perMinute), b.requests+float64(perMinute)*max(now.Sub(b.at).Minutes(), 0))
	b.at = now
}

// wait is how long until b holds a request: 0 if it does.
func (b *rateBucket) wait(perMinute int64) time.Duration {
	if b.requests >= 1 {
		return 0
	}
	ms := math.Ceil((1 - b.requests) / float64(perMinute) * float64(time.Minute/time.Millisecond))
	return time.Duration(ms) * time.Millisecond
}

// Take takes a request from every meter or none (Store's).
func (m *Mem) Take(_ context.Context, ms []Meter) (bool, int, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, 0, 0, ErrUnavailable
	}
	now := m.Now()
	for i, mt := range ms {
		b := m.bucket(mt.Key)
		b.refill(now, mt.PerMinute)
		if wait := b.wait(mt.PerMinute); wait > 0 {
			return false, i, wait, nil
		}
	}
	for _, mt := range ms {
		m.bucket(mt.Key).requests--
	}
	return true, 0, 0, nil
}

// bucket is key's bucket; mu is held.
func (m *Mem) bucket(key string) *rateBucket {
	b := m.buckets[key]
	if b == nil {
		b = &rateBucket{}
		m.buckets[key] = b
	}
	return b
}

// takeScript is Take: every bucket in KEYS (ARGV its requests a minute)
// has a request, or none is taken from; the store's clock is every
// gateway's. It returns {1, 0, 0}, or {0, the first without one (from 0),
// the wait in ms}.
var takeScript = valkey.NewLuaScript(nowMillis + `
local bs = {}
for i, key in ipairs(KEYS) do
  local rpm = tonumber(ARGV[i])
  local v = redis.call('HMGET', key, 'r', 'at')
  local r, at = tonumber(v[1]), tonumber(v[2])
  if not at then r, at = rpm, nowMs end
  bs[i] = {key = key, rpm = rpm, r = math.min(rpm, r + rpm * math.max(nowMs - at, 0) / 60000)}
end
local out = {1, 0, 0}
for i, b in ipairs(bs) do
  if b.r < 1 then out = {0, i - 1, math.ceil((1 - b.r) / b.rpm * 60000)}; break end
end
for _, b in ipairs(bs) do
  if out[1] == 1 then b.r = b.r - 1 end
  redis.call('HSET', b.key, 'r', tostring(b.r), 'at', nowMs)
  redis.call('PEXPIRE', b.key, 120000) -- refilled within a minute: kept two
end
return out`)

// Take takes a request from every meter or none (Store's).
func (v *Valkey) Take(ctx context.Context, ms []Meter) (bool, int, time.Duration, error) {
	c, err := v.client()
	if err != nil {
		return false, 0, 0, err
	}
	keys, args := make([]string, len(ms)), make([]string, len(ms))
	for i, mt := range ms {
		keys[i], args[i] = v.prefix+"rate:"+mt.Key, i64(mt.PerMinute)
	}
	res, err := takeScript.Exec(ctx, c, keys, args).AsIntSlice()
	if err != nil {
		return false, 0, 0, wrap(err)
	}
	if len(res) != 3 {
		return false, 0, 0, fmt.Errorf("limit: the take script returned %d values", len(res))
	}
	return res[0] == 1, int(res[1]), time.Duration(res[2]) * time.Millisecond, nil
}

// Take takes a request from every meter of ms or none (admission.Meters). While
// the store is unreachable it admits: a rate limit fails open (prd.md), and a
// provider's own 429 is the backstop.
func (l *Limiter) Take(ctx context.Context, ms []Meter) (bool, int, time.Duration) {
	if len(ms) == 0 || l.Down() {
		return true, 0, 0
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	ok, over, wait, err := l.store.Take(sctx, ms)
	if err != nil {
		if ctx.Err() == nil {
			l.MarkDown(err)
			l.failedOpen(err)
		}
		return true, 0, 0
	}
	return ok, over, wait
}

// failedOpen logs, at most once a minute, a rate limit admitted unchecked.
func (l *Limiter) failedOpen(err error) {
	now := l.now()
	if last := l.openLogged.Load(); last != 0 && now.Sub(time.Unix(0, last)) < time.Minute {
		return
	}
	l.openLogged.Store(now.UnixNano())
	l.log().Warn("limit: the site store is unreachable; admitting without rate limit checks (they fail open)", "err", err)
}
