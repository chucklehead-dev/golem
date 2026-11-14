// Package lead elects golem-control's leader (control-plane.md §1, ADR
// 0049; model/leadership.md's "fence" design). Leadership is two objects:
//
//   - the lease, an object of its own (LeaseKey), taken when free, released
//     or lapsed, and renewed by its holder every RenewEvery. It holds no
//     authority: losing it or rolling it back only delays a takeover.
//   - the fence, in the state document (state.Document.Fence): who leads,
//     at which epoch. A replica that has acquired the lease at epoch E
//     claims the fence, only if the fence's epoch is below E, and leads
//     from then on. Every leader-only write checks the fence in its own
//     compare-and-swap (state.FencedUpdate), so a deposed leader writes
//     nothing whatever it believes.
//
// A new epoch is one above the higher of the lease's and the fence's, so a
// rolled-back lease object cannot reuse an epoch. Renewals rewrite only the
// small lease object, not the document.
//
// Clocks decide only liveness. A taker judges a lease lapsed when it has
// seen the same lease object, unchanged, for the lease's duration on its
// own clock (as client-go does), so skew between replicas does not matter;
// a holder stops believing when its own clock passes its last renewal plus
// the duration, before any taker can judge the lease lapsed.
package lead

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// LeaseKey is the lease object's key in the state bucket.
const LeaseKey = "lead/lease.json"

// lease is the lease object.
type lease struct {
	Holder    string    `json:"holder"`
	Epoch     uint64    `json:"epoch"`
	RenewedAt time.Time `json:"renewedAt"` // diagnostic: takers judge by their own clock
	// DurationMillis is how long a renewal holds it (encoding/json/v2 has
	// no default form for a time.Duration).
	DurationMillis int64 `json:"durationMillis"`
	Released       bool  `json:"released,omitempty"` // given up: free at once
}

// Try's outcomes in which this replica does not lead, which are not
// failures: it steps down and reports epoch 0 with no error.
var (
	errHeld = errors.New("lead: the lease is held")
	errLost = errors.New("lead: another replica claimed a later epoch")
)

// Elector is one replica's view of leadership.
type Elector struct {
	State  *state.Store  // the document, which holds the fence
	Bucket bucket.Bucket // the lease object's; default the state's bucket
	ID     string        // the replica: its pod name
	// Duration is how long a renewal holds the lease (default 30 s);
	// RenewEvery how often its holder renews (default a third of it), and
	// RetryEvery how soon a failed try is retried (default 2 s, jittered).
	Duration, RenewEvery, RetryEvery time.Duration
	Now                              func() time.Time

	mu      sync.Mutex
	epoch   uint64    // the epoch this replica leads at; 0: it does not
	renewed time.Time // when it last renewed, by its own clock
	pending uint64    // acquired, not yet claimed
	seenTag string    // the lease object as this replica last saw another's
	seenAt  time.Time // when it first saw it so, by its own clock
}

func (e *Elector) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Elector) duration() time.Duration {
	if e.Duration > 0 {
		return e.Duration
	}
	return 30 * time.Second
}

func (e *Elector) bucket() bucket.Bucket {
	if e.Bucket != nil {
		return e.Bucket
	}
	return e.State.Bucket
}

// Epoch is the epoch this replica leads at, or 0. It is 0 from the moment
// the replica learns another leads, and from the moment its own clock
// passes its last renewal plus the lease's duration.
func (e *Elector) Epoch() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epochLocked()
}

// Fence is the leadership this replica acts under, for its leader-only
// writes; ok is false if it does not lead.
func (e *Elector) Fence() (state.Fence, bool) {
	epoch := e.Epoch()
	return state.Fence{Holder: e.ID, Epoch: epoch}, epoch != 0
}

// epochLocked is Epoch with e.mu held: belief ends once the clock passes
// the last renewal plus the duration.
func (e *Elector) epochLocked() uint64 {
	if e.epoch != 0 && !e.now().Before(e.renewed.Add(e.duration())) {
		e.epoch = 0
	}
	return e.epoch
}

// Try renews the lease if this replica holds it, or takes it if it is
// free, released or lapsed, and claims the fence; it returns the epoch the
// replica leads at (0 if another leads, or holds the lease).
func (e *Elector) Try(ctx context.Context) (uint64, error) {
	now := e.now() // stamped before any write: a slow write only shortens belief
	l, tag, err := e.read(ctx)
	if err != nil {
		return e.Epoch(), err
	}
	switch {
	case e.mine(l):
		err = e.renew(ctx, l, tag, now)
	case l.Holder != "" && !l.Released && !e.lapsed(tag, l, now):
		err = errHeld // noticed: another holds it
	default:
		err = e.take(ctx, l, tag, now)
	}
	if errors.Is(err, errHeld) || errors.Is(err, errLost) {
		e.stepDown()
		return 0, nil
	}
	// On another error the lease was not renewed: belief lapses by the
	// clock (Epoch), not now, so a store blip shorter than the lease does
	// not drop leadership.
	return e.Epoch(), err
}

// mine reports whether l is this replica's lease: held at the epoch it
// leads at, or at the one it acquired and has yet to claim.
func (e *Elector) mine(l lease) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return l.Holder == e.ID && !l.Released && (l.Epoch == e.epoch || l.Epoch == e.pending)
}

// stepDown ends this replica's belief that it leads, and any claim it has
// pending.
func (e *Elector) stepDown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.epoch, e.pending = 0, 0
}

// read is the lease object and its tag; a missing one is free.
func (e *Elector) read(ctx context.Context) (lease, string, error) {
	raw, tag, err := e.bucket().Get(ctx, LeaseKey, "")
	if errors.Is(err, bucket.ErrNotFound) {
		return lease{}, bucket.Absent, nil
	}
	if err != nil {
		return lease{}, "", fmt.Errorf("lease %s: %w", LeaseKey, err)
	}
	var l lease
	if err := jsonv2.Unmarshal(raw, &l, jsonv2.RejectUnknownMembers(true)); err != nil {
		return lease{}, "", fmt.Errorf("lease %s: %w", LeaseKey, err)
	}
	return l, tag, nil
}

// lapsed reports whether another's lease has gone unrenewed for its
// duration, by this replica's clock: the same object (tag) seen for that
// long.
func (e *Elector) lapsed(tag string, l lease, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if tag != e.seenTag {
		e.seenTag, e.seenAt = tag, now
		return false
	}
	return !now.Before(e.seenAt.Add(time.Duration(l.DurationMillis) * time.Millisecond))
}

// write puts the lease object if it is still the one read (ifMatch); a
// lost race is errHeld.
func (e *Elector) write(ctx context.Context, l lease, ifMatch string) error {
	body, err := jsonv2.Marshal(l)
	if err != nil {
		return err
	}
	_, err = e.bucket().Put(ctx, LeaseKey, body, ifMatch)
	if errors.Is(err, bucket.ErrPrecondition) {
		return errHeld // another wrote the lease first
	}
	return err
}

// renew extends this replica's lease, and claims the fence if its claim is
// still pending (a claim whose reply was lost, or that failed).
func (e *Elector) renew(ctx context.Context, l lease, tag string, now time.Time) error {
	l.RenewedAt, l.DurationMillis = now.UTC(), e.duration().Milliseconds()
	if err := e.write(ctx, l, tag); err != nil {
		return err
	}
	e.mu.Lock()
	pending := e.pending
	if e.epoch != 0 {
		e.renewed = now
	}
	e.mu.Unlock()
	if pending != 0 {
		return e.claim(ctx, pending, now)
	}
	// Still the fence's holder? A replica that claimed a later epoch has
	// deposed this one, whatever the lease object says.
	if f := e.State.Current().Fence; f != nil && *f != (state.Fence{Holder: e.ID, Epoch: l.Epoch}) {
		return errLost
	}
	return nil
}

// take acquires the lease at one above the higher of the lease's and the
// fence's epochs, then claims the fence.
func (e *Elector) take(ctx context.Context, l lease, tag string, now time.Time) error {
	if err := e.State.Refresh(ctx); err != nil {
		return err
	}
	epoch := l.Epoch
	if f := e.State.Current().Fence; f != nil && f.Epoch > epoch {
		epoch = f.Epoch
	}
	epoch++
	next := lease{Holder: e.ID, Epoch: epoch, RenewedAt: now.UTC(), DurationMillis: e.duration().Milliseconds()}
	if err := e.write(ctx, next, tag); err != nil {
		return err
	}
	e.mu.Lock()
	e.pending, e.epoch = epoch, 0
	e.mu.Unlock()
	return e.claim(ctx, epoch, now)
}

// claim writes the fence at epoch, if the fence's epoch is below it; the
// replica leads once it has. A claim whose reply was lost is found by the
// state store's write ID (state.Store.Update), or at the next renewal.
func (e *Elector) claim(ctx context.Context, epoch uint64, now time.Time) error {
	_, err := e.State.Update(ctx, func(d *state.Document) error {
		switch f := d.Fence; {
		case f != nil && *f == (state.Fence{Holder: e.ID, Epoch: epoch}):
			return nil // claimed already: a lost reply, adopted
		case f != nil && f.Epoch >= epoch:
			return errLost
		}
		d.Fence = &state.Fence{Holder: e.ID, Epoch: epoch}
		return nil
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.epoch, e.renewed, e.pending = epoch, now, 0
	e.mu.Unlock()
	return nil
}

// Release gives up the lease if this replica holds it, so another may take
// it at once rather than when it lapses (a rolling restart then loses no
// publication time). Belief ends first: nothing this replica does after
// Release starts is done as leader. The fence stays: the next taker claims
// a later epoch.
func (e *Elector) Release(ctx context.Context) error {
	e.stepDown()
	l, tag, err := e.read(ctx)
	if err != nil || l.Holder != e.ID || l.Released {
		return err
	}
	l.Released = true
	if err := e.write(ctx, l, tag); err != nil && !errors.Is(err, errHeld) {
		return err
	}
	return nil
}

// Run tries every RenewEvery until ctx ends, and a failed try again after
// RetryEvery, jittered, so a store blip does not wait a whole renewal.
func (e *Elector) Run(ctx context.Context) {
	every := e.RenewEvery
	if every <= 0 {
		every = e.duration() / 3
	}
	retry := e.RetryEvery
	if retry <= 0 {
		retry = 2 * time.Second
	}
	was := uint64(0)
	for {
		epoch, err := e.Try(ctx)
		wait := every
		if err != nil {
			slog.Warn("lead: the lease was not renewed", "replica", e.ID, "err", err)
			wait = jitter(retry)
		}
		if epoch != was {
			slog.Info("lead: leadership changed", "replica", e.ID, "epoch", epoch)
			was = epoch
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// jitter is d ± a quarter.
func jitter(d time.Duration) time.Duration {
	var b [8]byte
	rand.Read(b[:])
	q := int64(d) / 4
	if q == 0 {
		return d
	}
	return d - time.Duration(q) + time.Duration(int64(binary.LittleEndian.Uint64(b[:])%uint64(2*q)))
}
