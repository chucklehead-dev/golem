package proxy

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// InFlight is a gateway's admitted calls in flight. Each entry keeps what
// authenticated the call and the cancellation of its upstream request, so
// a revocation ends the calls it withdraws rather than letting them stream
// on (security-and-correctness.md).
//
// The gateway calls Sweep when it applies a snapshot that revokes a key or
// removes a principal, and when the emergency denylist gains an entry.
// Each call the keys no longer honour is ended. A stream gets its
// dialect's error event, a call waiting for headers gets its dialect's
// error, and a whole response being written is cut. The authorization
// lease's expiry is not a sweep, since it stops only new requests.
//
// The request path pays two shard-lock round trips per call and allocates
// nothing. The entry lives in the call's pooled exchange, on one shard's
// intrusive list, chosen by the request ID's random bits. A sweep asks the
// current keys about every call in flight (auth.Keys.Honours), so it is
// linear in the calls in flight. ADR 0041.
type InFlight struct {
	// Keys is the authentication in force: the applied snapshot's keys,
	// with the site's denylist (nil keys: nothing is checked).
	Keys func() *auth.Keys

	// gen counts sweeps: a call authenticated before a sweep began and
	// registered after it passed its shard checks itself (add).
	gen      atomic.Uint64
	sweeping sync.Mutex // one sweep at a time, each with the keys current when it began
	shards   [inFlightShards]inFlightShard
}

const inFlightShards = 16

type inFlightShard struct {
	mu   sync.Mutex
	head *inFlightCall
	n    int
	_    [40]byte // a cache line each: shards locked by different cores
}

// inFlightCall is one call's entry, in its exchange.
type inFlightCall struct {
	prev, next *inFlightCall
	shard      *inFlightShard
	grant      auth.Grant
	cancel     context.CancelCauseFunc
	ended      bool // a sweep ended it (counted once)
}

// end cancels the call's upstream request with the refusal its keys now
// give it.
func (c *inFlightCall) end(no *auth.Refusal) *errTerminated {
	t := &errTerminated{no.Code, no.Msg}
	c.cancel(t)
	c.ended = true
	return t
}

// errTerminated is the cause of an upstream request a sweep cancelled: the
// refusal a new request of the same caller would get.
type errTerminated struct {
	code contract.Code
	msg  string
}

func (e *errTerminated) Error() string { return "proxy: " + e.message() }

// message is what the caller is told.
func (e *errTerminated) message() string {
	return "golem ended this call in flight: " + e.msg
}

// terminatedBy is the sweep that cancelled ctx, if one did.
func terminatedBy(ctx context.Context) *errTerminated {
	t, _ := context.Cause(ctx).(*errTerminated)
	return t
}

// generation is the count of sweeps begun, read before a call
// authenticates.
func (f *InFlight) generation() uint64 {
	if f == nil {
		return 0
	}
	return f.gen.Load()
}

// add registers a call: g is what authenticated it, cancel ends its
// upstream request, pick chooses its shard, gen is generation's value
// before it authenticated. A sweep that began since may have passed its
// shard before it was added, so it then asks the current keys itself; it
// returns the termination if they refuse it.
func (f *InFlight) add(c *inFlightCall, g *auth.Grant, cancel context.CancelCauseFunc, pick byte, gen uint64) *errTerminated {
	c.grant, c.cancel, c.ended = *g, cancel, false
	s := &f.shards[pick%inFlightShards]
	c.shard = s
	s.mu.Lock()
	c.prev, c.next = nil, s.head
	if s.head != nil {
		s.head.prev = c
	}
	s.head = c
	s.n++
	defer s.mu.Unlock()
	// A sweep's keys are in force before its generation is counted, and
	// its pass over this shard, if this call missed it, was over before
	// the add took the shard's lock: a changed generation here sees them.
	if f.gen.Load() == gen {
		return nil
	}
	if k := f.keys(); k != nil {
		if no := k.Honours(&c.grant); no != nil {
			return c.end(no)
		}
	}
	return nil
}

// remove takes a call off the list as it ends, before its upstream
// context is released: no sweep cancels it after.
func (f *InFlight) remove(c *inFlightCall) {
	s := c.shard
	s.mu.Lock()
	if c.prev != nil {
		c.prev.next = c.next
	} else {
		s.head = c.next
	}
	if c.next != nil {
		c.next.prev = c.prev
	}
	s.n--
	s.mu.Unlock()
	*c = inFlightCall{} // the pooled exchange keeps no principal or credential
}

func (f *InFlight) keys() *auth.Keys {
	if f.Keys == nil {
		return nil
	}
	return f.Keys()
}

// Sweep ends every call in flight the current keys no longer honour, and
// returns how many it ended. Call it after each snapshot is applied and
// each denylist change is put in force, never before: a call that
// registers while it runs relies on the new keys being current.
func (f *InFlight) Sweep() (ended int) {
	f.sweeping.Lock()
	defer f.sweeping.Unlock()
	f.gen.Add(1)
	k := f.keys()
	if k == nil {
		return 0
	}
	for i := range f.shards {
		s := &f.shards[i]
		s.mu.Lock()
		for c := s.head; c != nil; c = c.next {
			if c.ended {
				continue // ended by an earlier sweep, still settling
			}
			if no := k.Honours(&c.grant); no != nil {
				// A snapshot applied since k was read may honour a call
				// that authenticated under it (a key minted a moment ago,
				// unknown to k): the current keys must refuse it too.
				// Revocations are never undone, so they refuse what k
				// refused for that reason.
				if cur := f.keys(); cur != nil && cur != k && cur.Honours(&c.grant) == nil {
					continue
				}
				c.end(no)
				ended++
			}
		}
		s.mu.Unlock()
	}
	return ended
}

// Len is the number of calls in flight (for metrics and tests).
func (f *InFlight) Len() int {
	n := 0
	for i := range f.shards {
		s := &f.shards[i]
		s.mu.Lock()
		n += s.n
		s.mu.Unlock()
	}
	return n
}
