package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/gateway/route"
)

// Why an upstream call was cut (context.Cause of its context).
var (
	errHeadersTimeout = errors.New("the provider sent no response headers within the route's headers timeout")
	errMaxDuration    = errors.New("the call reached the route's maximum duration")
	errIdle           = errors.New("the provider sent nothing for the route's idle timeout")
)

// bounds are a call's route limits over its upstream request, in the
// usual forms (as nginx's proxy_read_timeout, Envoy's stream_idle_timeout
// and max_stream_duration): an alarm for the whole call, one for the
// headers, and an idle alarm. Each cancels the upstream context with its
// cause. A zero limit is none.
//
// A read never resets the idle alarm's timer: it stamps the time, and the
// alarm, when it fires, re-arms itself for the rest of the idle period if
// a read came since. Resetting from the reader could re-arm a timer that
// had just fired, whose callback was still to run: disarm would then see a
// pending timer, keep the alarm, and the stale callback would cancel the
// next call.
//
// The bounds live in the pooled exchange and their timers are reused from
// call to call, so a call allocates only its context.
type bounds struct {
	cancel              context.CancelCauseFunc
	call, headers, idle *alarm
	idleFor             time.Duration
	lastRead            atomic.Int64 // UnixNano of the last read that returned bytes
	body                idleReader
}

// alarm is a reusable timer that cancels the call it is armed for. One
// that fired is never armed again: its callback may still be running, and
// would cancel the next call. disarm says so, and it is replaced (the one
// allocation, and only after a timeout).
type alarm struct {
	t      *time.Timer
	cause  error
	mu     sync.Mutex
	cancel context.CancelCauseFunc // nil: not armed
	// extend, if set, is how much longer the alarm should wait when it
	// fires (the idle alarm's rest of its period); > 0 re-arms it instead.
	extend func() time.Duration
}

func newAlarm(cause error) *alarm {
	a := &alarm{cause: cause}
	a.t = time.AfterFunc(time.Hour, a.fire)
	a.t.Stop()
	return a
}

// fire cancels the call the alarm is armed for, or re-arms it if extend
// says to wait longer. The re-arm is under the lock that disarm takes, so
// it never outlives the call.
func (a *alarm) fire() {
	a.mu.Lock()
	cancel := a.cancel
	if cancel != nil && a.extend != nil {
		if d := a.extend(); d > 0 {
			a.t.Reset(d)
			a.mu.Unlock()
			return
		}
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel(a.cause)
	}
}

func (a *alarm) arm(cancel context.CancelCauseFunc, d time.Duration, extend func() time.Duration) {
	a.mu.Lock()
	a.cancel, a.extend = cancel, extend
	a.mu.Unlock()
	a.t.Reset(d)
}

// disarm stops the alarm; it reports false if the alarm fired.
func (a *alarm) disarm() (reusable bool) {
	a.mu.Lock()
	armed := a.cancel != nil
	a.cancel, a.extend = nil, nil
	stopped := a.t.Stop()
	a.mu.Unlock()
	return stopped || !armed
}

// stopAlarm disarms *a, replacing it if it fired.
func stopAlarm(a **alarm) {
	if !(*a).disarm() {
		*a = newAlarm((*a).cause)
	}
}

func newBounds() bounds {
	return bounds{call: newAlarm(errMaxDuration), headers: newAlarm(errHeadersTimeout), idle: newAlarm(errIdle)}
}

// bound derives the upstream's context from the request's and arms the
// call's and the headers' alarms.
func (b *bounds) bound(ctx context.Context, started time.Time, rt *route.Route) context.Context {
	ctx, b.cancel = context.WithCancelCause(ctx)
	b.idleFor = rt.Idle
	if rt.MaxDuration > 0 {
		b.call.arm(b.cancel, time.Until(started.Add(rt.MaxDuration)), nil)
	}
	if rt.Headers > 0 && (rt.MaxDuration == 0 || rt.Headers < rt.MaxDuration) { // else the call's alarm is the bound
		b.headers.arm(b.cancel, rt.Headers, nil)
	}
	return ctx
}

// nextAttempt readies the bounds for another attempt of the call: the
// last one's idle alarm off, the headers' armed afresh (the call's alarm
// runs on, over all its attempts).
func (b *bounds) nextAttempt(rt *route.Route) {
	stopAlarm(&b.idle)
	stopAlarm(&b.headers)
	if rt.Headers > 0 && (rt.MaxDuration == 0 || rt.Headers < rt.MaxDuration) {
		b.headers.arm(b.cancel, rt.Headers, nil)
	}
}

// gotHeaders ends the wait for headers and arms the idle alarm.
func (b *bounds) gotHeaders() {
	stopAlarm(&b.headers)
	if b.idleFor > 0 {
		b.lastRead.Store(time.Now().UnixNano())
		b.idle.arm(b.cancel, b.idleFor, b.idleLeft)
	}
}

// idleLeft is how much of the idle period remains since the last read.
func (b *bounds) idleLeft() time.Duration {
	return time.Until(time.Unix(0, b.lastRead.Load()).Add(b.idleFor))
}

// bodyOf is the response body, each read stamping the idle alarm's clock.
func (b *bounds) bodyOf(r io.Reader) io.Reader {
	if b.idleFor <= 0 {
		return r
	}
	b.body = idleReader{r, b}
	return &b.body
}

type idleReader struct {
	r io.Reader
	b *bounds
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.b.lastRead.Store(time.Now().UnixNano())
	}
	return n, err
}

// end disarms the alarms (replacing any that fired) and releases the
// upstream context.
func (b *bounds) end() {
	stopAlarm(&b.call)
	stopAlarm(&b.headers)
	stopAlarm(&b.idle)
	b.body = idleReader{}
	b.cancel(nil)
	b.cancel = nil
}
