package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
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
// headers, and an idle alarm reset by every read. Each cancels the upstream
// context with its cause. A zero limit is none.
//
// The bounds live in the pooled exchange and their timers are reused from
// call to call, so a call allocates only its context.
type bounds struct {
	cancel              context.CancelCauseFunc
	call, headers, idle *alarm
	idleFor             time.Duration
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
}

func newAlarm(cause error) *alarm {
	a := &alarm{cause: cause}
	a.t = time.AfterFunc(time.Hour, a.fire)
	a.t.Stop()
	return a
}

func (a *alarm) fire() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel(a.cause)
	}
}

func (a *alarm) arm(cancel context.CancelCauseFunc, d time.Duration) {
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	a.t.Reset(d)
}

// disarm stops the alarm; it reports false if the alarm fired.
func (a *alarm) disarm() (reusable bool) {
	a.mu.Lock()
	armed := a.cancel != nil
	a.cancel = nil
	a.mu.Unlock()
	return a.t.Stop() || !armed
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
		b.call.arm(b.cancel, time.Until(started.Add(rt.MaxDuration)))
	}
	if rt.Headers > 0 && (rt.MaxDuration == 0 || rt.Headers < rt.MaxDuration) { // else the call's alarm is the bound
		b.headers.arm(b.cancel, rt.Headers)
	}
	return ctx
}

// gotHeaders ends the wait for headers and arms the idle alarm.
func (b *bounds) gotHeaders() {
	stopAlarm(&b.headers)
	if b.idleFor > 0 {
		b.idle.arm(b.cancel, b.idleFor)
	}
}

// bodyOf is the response body, each read resetting the idle alarm.
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
		ir.b.idle.t.Reset(ir.b.idleFor)
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
