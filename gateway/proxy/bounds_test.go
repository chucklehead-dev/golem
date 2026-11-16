package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chucklehead-dev/golem/gateway/route"
)

// TestBoundsReuse: a call's alarms are reused by the next call when they
// did not fire, and replaced when they did, so a fired alarm (whose
// callback may still be running) never cancels a later call. The alarms
// run on synctest's clock; the bounds never leave the bubble.
func TestBoundsReuse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBounds()
		quiet := &route.Route{MaxDuration: time.Hour, Headers: time.Minute, Idle: time.Hour}
		call, headers, idle := b.call, b.headers, b.idle
		b.bound(t.Context(), time.Now(), quiet)
		b.gotHeaders()
		b.end()
		if b.call != call || b.headers != headers || b.idle != idle {
			t.Fatal("alarms that did not fire were replaced: a call would allocate")
		}

		ctx := b.bound(t.Context(), time.Now(), &route.Route{MaxDuration: time.Hour, Idle: 10 * time.Millisecond})
		b.gotHeaders()
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), errIdle) {
			t.Fatalf("cause %v, want the idle bound", context.Cause(ctx))
		}
		b.end()
		if b.idle == idle || b.call != call {
			t.Fatal("the fired idle alarm must be replaced, the others kept")
		}

		ctx = b.bound(t.Context(), time.Now(), quiet)
		b.gotHeaders()
		time.Sleep(50 * time.Millisecond)
		synctest.Wait() // any stale alarm's callback has run
		if ctx.Err() != nil {
			t.Fatalf("the next call was cancelled: %v", context.Cause(ctx))
		}
		b.end()
	})
}

// TestBoundsRace: many short calls whose alarms fire as they end, under
// the race detector: no call is cancelled by another's alarm.
func TestBoundsRace(t *testing.T) {
	b := newBounds()
	for i := range 2000 {
		short := i%2 == 0
		rt := &route.Route{MaxDuration: time.Hour, Idle: time.Hour}
		if short {
			rt.Idle = time.Microsecond * time.Duration(1+i%50)
		}
		ctx := b.bound(t.Context(), time.Now(), rt)
		b.gotHeaders()
		if short {
			time.Sleep(time.Duration(i%3) * 20 * time.Microsecond) // end as it fires, or just after
		} else {
			time.Sleep(100 * time.Microsecond) // time for a stale alarm to fire
			if ctx.Err() != nil {
				t.Fatalf("call %d cancelled by %v", i, context.Cause(ctx))
			}
		}
		b.end()
	}
}

// TestIdleAlarm: reads within the idle period keep a call alive however
// long it runs; a read after the alarm fired does not re-arm it, so the
// fired alarm is replaced, not reused with a callback pending. The alarms
// run on synctest's clock.
func TestIdleAlarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBounds()
		ctx := b.bound(t.Context(), time.Now(), &route.Route{MaxDuration: time.Hour, Idle: 20 * time.Millisecond})
		b.gotHeaders()
		body := b.bodyOf(strings.NewReader(strings.Repeat("x", 100)))
		one := make([]byte, 1)
		for range 10 { // 50 ms of reads, 5 ms apart: never idle
			time.Sleep(5 * time.Millisecond)
			synctest.Wait()
			body.Read(one)
		}
		if ctx.Err() != nil {
			t.Fatalf("a call reading every 5 ms was cut: %v", context.Cause(ctx))
		}
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), errIdle) {
			t.Fatalf("cause %v", context.Cause(ctx))
		}
		idle := b.idle
		body.Read(one) // a read after the cut, as a draining caller makes
		b.end()
		if b.idle == idle {
			t.Fatal("the fired idle alarm was kept: a read re-armed it, and its stale callback could cancel the next call")
		}
	})
}
