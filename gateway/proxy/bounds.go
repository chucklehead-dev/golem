package proxy

import (
	"context"
	"errors"
	"io"
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
// and max_stream_duration): a deadline for the whole call, a timer for the
// headers, and an idle timer reset by every read. Each cancels the
// upstream context with its cause. A zero limit is none.
type bounds struct {
	cancel  context.CancelCauseFunc
	release context.CancelFunc // the deadline's
	headers *time.Timer
	idle    *time.Timer
	idleFor time.Duration
}

// bound derives the upstream's context from the request's.
func bound(ctx context.Context, started time.Time, rt *route.Route) (context.Context, *bounds) {
	ctx, cancel := context.WithCancelCause(ctx)
	b := &bounds{cancel: cancel, release: func() {}, idleFor: rt.Idle}
	if rt.MaxDuration > 0 {
		ctx, b.release = context.WithDeadlineCause(ctx, started.Add(rt.MaxDuration), errMaxDuration)
	}
	if rt.Headers > 0 && (rt.MaxDuration == 0 || rt.Headers < rt.MaxDuration) { // else the deadline is the bound
		b.headers = time.AfterFunc(rt.Headers, func() { cancel(errHeadersTimeout) })
	}
	return ctx, b
}

// gotHeaders ends the wait for headers and starts the idle bound.
func (b *bounds) gotHeaders() {
	if b.headers != nil {
		b.headers.Stop()
	}
	if b.idleFor > 0 {
		b.idle = time.AfterFunc(b.idleFor, func() { b.cancel(errIdle) })
	}
}

// body is the response body, each read resetting the idle bound.
func (b *bounds) body(r io.Reader) io.Reader {
	if b.idle == nil {
		return r
	}
	return idleReader{r, b}
}

type idleReader struct {
	r io.Reader
	b *bounds
}

func (ir idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.b.idle.Reset(ir.b.idleFor)
	}
	return n, err
}

// end stops the timers and releases the upstream context.
func (b *bounds) end() {
	if b.headers != nil {
		b.headers.Stop()
	}
	if b.idle != nil {
		b.idle.Stop()
	}
	b.release()
	b.cancel(nil)
}
