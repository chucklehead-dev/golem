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

// wait is how long until b has room for a request: 0 if it has.
func (b *rateBucket) wait(r Rate) time.Duration {
	var wait time.Duration
	if r.RequestsPerMinute > 0 && b.requests < 1 {
		wait = max(wait, minutesFor((1-b.requests)/float64(r.RequestsPerMinute)))
	}
	if r.TokensPerMinute > 0 && b.tokens <= 0 {
		wait = max(wait, minutesFor(-b.tokens/float64(r.TokensPerMinute))+time.Millisecond)
	}
	return wait
}

// take takes a request and tokens from b, which has room.
func (b *rateBucket) take(r Rate, tokens int64) {
	if r.RequestsPerMinute > 0 {
		b.requests--
	}
	if r.TokensPerMinute > 0 {
		b.tokens -= float64(tokens)
	}
}

func (b *rateBucket) adjust(r Rate, tokens int64) {
	if r.TokensPerMinute > 0 {
		b.tokens = min(float64(r.TokensPerMinute), b.tokens+float64(tokens))
	}
}

func minutesFor(m float64) time.Duration {
	return time.Duration(math.Ceil(m*float64(time.Minute/time.Millisecond))) * time.Millisecond
}

// Meter is a bucket a request is charged to.
type Meter = admission.Meter

// Take takes a request and tokens from every meter or none (Store's).
func (m *Mem) Take(_ context.Context, ms []Meter, tokens int64) (bool, int, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, 0, 0, ErrUnavailable
	}
	now := m.Now()
	for i, mt := range ms {
		b := m.bucket(mt.Key)
		b.refill(now, mt.Rate)
		if wait := b.wait(mt.Rate); wait > 0 {
			return false, i, wait, nil
		}
	}
	for _, mt := range ms {
		m.bucket(mt.Key).take(mt.Rate, tokens)
	}
	return true, 0, 0, nil
}

// Adjust corrects every meter's tokens (Store's).
func (m *Mem) Adjust(_ context.Context, ms []Meter, tokens int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	for _, mt := range ms {
		b := m.bucket(mt.Key)
		b.refill(m.Now(), mt.Rate)
		b.adjust(mt.Rate, tokens)
	}
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

// rateFill is the Lua form of rateBucket.refill and its save, for the
// buckets in KEYS, ARGV holding each one's requests and tokens a minute
// (from ARGV[2]; ARGV[1] the tokens); nowMs is the store's clock (one
// clock for every gateway).
const rateFill = nowMillis + `
local need, bs = tonumber(ARGV[1]), {}
for i, key in ipairs(KEYS) do
  local rpm, tpm = tonumber(ARGV[2*i]), tonumber(ARGV[2*i+1])
  local v = redis.call('HMGET', key, 'r', 't', 'at')
  local r, tok, at = tonumber(v[1]), tonumber(v[2]), tonumber(v[3])
  if not at then r, tok, at = rpm, tpm, nowMs end
  local dt = math.max(nowMs - at, 0) / 60000
  bs[i] = {key = key, rpm = rpm, tpm = tpm, r = math.min(rpm, r + rpm * dt), tok = math.min(tpm, tok + tpm * dt)}
end
local function save()
  for _, b in ipairs(bs) do
    redis.call('HSET', b.key, 'r', tostring(b.r), 't', tostring(b.tok), 'at', nowMs)
    -- Kept while it is not full: a minute's refill, or its debt's repayment.
    local life = 120000
    if b.tpm > 0 and b.tok < 0 then life = life + math.ceil(-b.tok / b.tpm * 60000) end
    redis.call('PEXPIRE', b.key, life)
  end
end
`

// takeScript is Take: every bucket has room, or none is taken from. It
// returns {1, 0, 0}, or {0, the first bucket without room (from 0), the
// wait in ms}.
var takeScript = valkey.NewLuaScript(rateFill + `
for i, b in ipairs(bs) do
  local wait = 0
  if b.rpm > 0 and b.r < 1 then wait = math.max(wait, math.ceil((1 - b.r) / b.rpm * 60000)) end
  if b.tpm > 0 and b.tok <= 0 then wait = math.max(wait, math.ceil(-b.tok / b.tpm * 60000) + 1) end
  if wait > 0 then save(); return {0, i - 1, wait} end
end
for _, b in ipairs(bs) do
  if b.rpm > 0 then b.r = b.r - 1 end
  if b.tpm > 0 then b.tok = b.tok - need end
end
save()
return {1, 0, 0}`)

// adjustScript is Adjust: ARGV[1] the tokens given back to each.
var adjustScript = valkey.NewLuaScript(rateFill + `
for _, b in ipairs(bs) do
  if b.tpm > 0 then b.tok = math.min(b.tpm, b.tok + need) end
end
save()
return 1`)

// meterArgs are a script's KEYS and ARGV for ms.
func (v *Valkey) meterArgs(ms []Meter, tokens int64) ([]string, []string) {
	keys, args := make([]string, len(ms)), append(make([]string, 0, 1+2*len(ms)), i64(tokens))
	for i, mt := range ms {
		keys[i] = v.prefix + "rate:" + mt.Key
		args = append(args, i64(mt.Rate.RequestsPerMinute), i64(mt.Rate.TokensPerMinute))
	}
	return keys, args
}

// Take takes a request and tokens from every meter or none (Store's).
func (v *Valkey) Take(ctx context.Context, ms []Meter, tokens int64) (bool, int, time.Duration, error) {
	c, err := v.client()
	if err != nil {
		return false, 0, 0, err
	}
	keys, args := v.meterArgs(ms, tokens)
	res, err := takeScript.Exec(ctx, c, keys, args).AsIntSlice()
	if err != nil {
		return false, 0, 0, wrap(err)
	}
	if len(res) != 3 {
		return false, 0, 0, fmt.Errorf("limit: the take script returned %d values", len(res))
	}
	return res[0] == 1, int(res[1]), time.Duration(res[2]) * time.Millisecond, nil
}

// Adjust corrects every meter's tokens (Store's).
func (v *Valkey) Adjust(ctx context.Context, ms []Meter, tokens int64) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	keys, args := v.meterArgs(ms, tokens)
	return wrap(adjustScript.Exec(ctx, c, keys, args).Error())
}

// Take takes a request and tokens from every meter of ms or none
// (admission.Meters). While the store is unreachable it admits: a rate
// limit fails open (PRD §5.5), and a provider's own 429 is the quota's
// backstop.
func (l *Limiter) Take(ctx context.Context, ms []Meter, tokens int64) (bool, int, time.Duration) {
	if len(ms) == 0 || l.Down() {
		return true, 0, 0
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	ok, over, wait, err := l.store.Take(sctx, ms, tokens)
	if err != nil {
		if ctx.Err() == nil {
			l.markDown(err)
			l.failedOpen(err)
		}
		return true, 0, 0
	}
	return ok, over, wait
}

// Adjust corrects every meter of ms by tokens (admission.Meters); with the
// store unreachable the correction is lost (a bucket refills within a
// minute regardless).
func (l *Limiter) Adjust(ctx context.Context, ms []Meter, tokens int64) {
	if len(ms) == 0 || tokens == 0 || l.Down() {
		return
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Adjust(sctx, ms, tokens); err != nil && ctx.Err() == nil {
		l.markDown(err)
	}
}

// failedOpen logs, at most once a minute, a rate limit admitted unchecked.
func (l *Limiter) failedOpen(err error) {
	now := l.now()
	if last := l.openLogged.Load(); last != 0 && now.Sub(time.Unix(0, last)) < time.Minute {
		return
	}
	l.openLogged.Store(now.UnixNano())
	slog.Warn("limit: the site store is unreachable; admitting without rate limit and quota checks (they fail open)", "err", err)
}
