package limit

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/valkey-io/valkey-go"
)

// Rate buckets: a per-minute allowance of requests and tokens, refilled
// continuously and holding at most a minute's worth (a provider's quota,
// metered as providers meter it). A take needs a request and the bucket's
// tokens not in debt: a request larger than what is left is admitted, and
// its tokens put the bucket in debt, so a large request is never starved
// and the next waits until the debt is repaid. A correction gives tokens
// back, or takes more, never past a minute's worth.

// Rate is a bucket's allowance.
type Rate = admission.Rate

// rateBucket is a bucket's state: what it holds, as of at.
type rateBucket struct {
	requests, tokens float64
	at               time.Time
}

// refill brings b to now under r; a new bucket starts full.
func (b *rateBucket) refill(now time.Time, r Rate) {
	if b.at.IsZero() {
		b.requests, b.tokens, b.at = float64(r.RequestsPerMinute), float64(r.TokensPerMinute), now
		return
	}
	minutes := max(now.Sub(b.at).Minutes(), 0)
	b.requests = min(float64(r.RequestsPerMinute), b.requests+float64(r.RequestsPerMinute)*minutes)
	b.tokens = min(float64(r.TokensPerMinute), b.tokens+float64(r.TokensPerMinute)*minutes)
	b.at = now
}

// take takes a request and tokens, or reports the wait until it can.
func (b *rateBucket) take(r Rate, tokens int64) (bool, time.Duration) {
	var wait time.Duration
	if r.RequestsPerMinute > 0 && b.requests < 1 {
		wait = max(wait, minutesFor((1-b.requests)/float64(r.RequestsPerMinute)))
	}
	if r.TokensPerMinute > 0 && b.tokens <= 0 {
		wait = max(wait, minutesFor(-b.tokens/float64(r.TokensPerMinute))+time.Millisecond)
	}
	if wait > 0 {
		return false, wait
	}
	if r.RequestsPerMinute > 0 {
		b.requests--
	}
	if r.TokensPerMinute > 0 {
		b.tokens -= float64(tokens)
	}
	return true, 0
}

func (b *rateBucket) adjust(r Rate, tokens int64) {
	if r.TokensPerMinute > 0 {
		b.tokens = min(float64(r.TokensPerMinute), b.tokens+float64(tokens))
	}
}

func minutesFor(m float64) time.Duration {
	return time.Duration(math.Ceil(m*float64(time.Minute/time.Millisecond))) * time.Millisecond
}

// Take takes from a bucket (Store's).
func (m *Mem) Take(_ context.Context, key string, r Rate, tokens int64) (bool, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, 0, ErrUnavailable
	}
	b := m.bucket(key)
	b.refill(m.Now(), r)
	ok, wait := b.take(r, tokens)
	return ok, wait, nil
}

// Adjust corrects a bucket's tokens (Store's).
func (m *Mem) Adjust(_ context.Context, key string, r Rate, tokens int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	b := m.bucket(key)
	b.refill(m.Now(), r)
	b.adjust(r, tokens)
	return nil
}

// bucket is key's bucket; mu is held.
func (m *Mem) bucket(key string) *rateBucket {
	if m.buckets == nil {
		m.buckets = map[string]*rateBucket{}
	}
	b := m.buckets[key]
	if b == nil {
		b = &rateBucket{}
		m.buckets[key] = b
	}
	return b
}

// rateFill is the Lua form of rateBucket.refill: KEYS[1] the bucket, ARGV
// requests and tokens a minute; it leaves r, tok and nowMs set, the store's
// clock (one clock for every gateway).
const rateFill = nowMillis + `
local key, rpm, tpm = KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2])
local v = redis.call('HMGET', key, 'r', 't', 'at')
local r, tok, at = tonumber(v[1]), tonumber(v[2]), tonumber(v[3])
if not at then r, tok, at = rpm, tpm, nowMs end
local dt = math.max(nowMs - at, 0) / 60000
r, tok = math.min(rpm, r + rpm * dt), math.min(tpm, tok + tpm * dt)
local function save()
  redis.call('HSET', key, 'r', tostring(r), 't', tostring(tok), 'at', nowMs)
  -- Kept while it is not full: a minute's refill, or its debt's repayment.
  local life = 120000
  if tpm > 0 and tok < 0 then life = life + math.ceil(-tok / tpm * 60000) end
  redis.call('PEXPIRE', key, life)
end
`

// takeScript is rateBucket.take: ARGV[3] the tokens. It returns {1, 0},
// or {0, the wait in ms}.
var takeScript = valkey.NewLuaScript(rateFill + `
local need, wait = tonumber(ARGV[3]), 0
if rpm > 0 and r < 1 then wait = math.max(wait, math.ceil((1 - r) / rpm * 60000)) end
if tpm > 0 and tok <= 0 then wait = math.max(wait, math.ceil(-tok / tpm * 60000) + 1) end
if wait > 0 then save(); return {0, wait} end
if rpm > 0 then r = r - 1 end
if tpm > 0 then tok = tok - need end
save()
return {1, 0}`)

// adjustScript is rateBucket.adjust: ARGV[3] the tokens given back.
var adjustScript = valkey.NewLuaScript(rateFill + `
if tpm > 0 then tok = math.min(tpm, tok + tonumber(ARGV[3])) end
save()
return 1`)

// Take takes from a bucket (Store's).
func (v *Valkey) Take(ctx context.Context, key string, r Rate, tokens int64) (bool, time.Duration, error) {
	c, err := v.client()
	if err != nil {
		return false, 0, err
	}
	res, err := takeScript.Exec(ctx, c, []string{v.prefix + "rate:" + key},
		[]string{i64(r.RequestsPerMinute), i64(r.TokensPerMinute), i64(tokens)}).AsIntSlice()
	if err != nil {
		return false, 0, wrap(err)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("limit: the take script returned %d values", len(res))
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond, nil
}

// Adjust corrects a bucket's tokens (Store's).
func (v *Valkey) Adjust(ctx context.Context, key string, r Rate, tokens int64) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	return wrap(adjustScript.Exec(ctx, c, []string{v.prefix + "rate:" + key},
		[]string{i64(r.RequestsPerMinute), i64(r.TokensPerMinute), i64(tokens)}).Error())
}

// QuotaAccount is a deployment's quota bucket's key.
func QuotaAccount(deployment string) string { return "q:" + deployment }

// TakeQuota takes an attempt's request and estimated tokens from a
// deployment's quota share (admission.Quotas). While the store is
// unreachable it admits: a rate limit fails open (PRD §5.5), the
// provider's own 429 the backstop, and the attempt is retried elsewhere.
func (l *Limiter) TakeQuota(ctx context.Context, deployment string, r Rate, tokens int64) (bool, time.Duration) {
	if l.Down() {
		return true, 0
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	ok, wait, err := l.store.Take(sctx, QuotaAccount(deployment), r, tokens)
	if err != nil {
		if ctx.Err() == nil {
			l.markDown(err)
			l.failedOpen("provider quota", err)
		}
		return true, 0
	}
	return ok, wait
}

// AdjustQuota corrects a deployment's quota share by tokens
// (admission.Quotas); with the store unreachable, the correction is lost
// (the bucket refills within a minute regardless).
func (l *Limiter) AdjustQuota(ctx context.Context, deployment string, r Rate, tokens int64) {
	if tokens == 0 || l.Down() {
		return
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Adjust(sctx, QuotaAccount(deployment), r, tokens); err != nil && ctx.Err() == nil {
		l.markDown(err)
	}
}

// failedOpen logs, at most once a minute, a rate limit admitted unchecked.
func (l *Limiter) failedOpen(what string, err error) {
	now := l.now()
	if last := l.openLogged.Load(); last != 0 && now.Sub(time.Unix(0, last)) < time.Minute {
		return
	}
	l.openLogged.Store(now.UnixNano())
	slog.Warn("limit: the site store is unreachable; admitting without the "+what+" check (rate limits fail open)", "err", err)
}
