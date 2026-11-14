// Package lead elects golem-control's leader (control-plane.md §1, ADR
// 0045): a lease in the state document, taken when it is free or has
// lapsed, renewed by its holder. Each change of hands increases the
// lease's epoch, and the publisher is fenced by it (model/publication.md
// §4.2, §6.4): a paused leader that publishes after losing the lease is
// refused by the publication record, whatever it believes.
//
// The lease lives in the state document, not in a Kubernetes Lease: its
// epoch is then written by the same compare-and-swap as the record it
// fences, and golem-control needs no API server to lead (standalone and
// tests). Clock skew between replicas affects only when a lapsed lease
// may be taken; it cannot let two epochs publish, since the record
// refuses any epoch below its own.
package lead

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
)

// errHeld: another replica's lease has not lapsed; errNotHolder: this
// replica does not hold it (Release).
var (
	errHeld      = errors.New("lead: the lease is held")
	errNotHolder = errors.New("lead: not the holder")
)

// Elector is one replica's view of the lease.
type Elector struct {
	State *state.Store
	ID    string // the replica: its pod name
	// Duration is how long a renewal holds the lease (default 15 s);
	// RenewEvery how often its holder renews (default a third of it).
	Duration, RenewEvery time.Duration
	Now                  func() time.Time

	mu      sync.Mutex
	epoch   uint64    // the epoch this replica believes it leads at; 0: it does not
	renewed time.Time // when it last renewed, by its own clock
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
	return 15 * time.Second
}

// Epoch is the epoch this replica leads at, or 0. It is 0 from the moment
// the replica learns another holds the lease, and from the moment its own
// clock passes its last renewal plus the lease's duration: it stops
// believing before another replica may take the lease (by that replica's
// clock), so a pause makes it give up rather than overlap.
func (e *Elector) Epoch() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epochLocked()
}

// Try takes the lease if it is free or lapsed, or renews it if this
// replica holds it, and returns the epoch it leads at (0 if another holds
// it). Taking it from another, or after none, increases the epoch.
func (e *Elector) Try(ctx context.Context) (uint64, error) {
	now := e.now()
	d, err := e.State.Update(ctx, func(doc *state.Document) error {
		l := doc.Leader
		switch {
		case l == nil:
			doc.Leader = &state.Lease{Holder: e.ID, Epoch: 1}
		case l.Holder == e.ID:
			// Renewal: the same epoch.
		case now.Before(l.Expires()):
			return errHeld
		default:
			l.Holder, l.Epoch = e.ID, l.Epoch+1
		}
		doc.Leader.RenewedAt, doc.Leader.DurationMillis = now.UTC(), e.duration().Milliseconds()
		return nil
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case errors.Is(err, errHeld):
		e.epoch = 0 // noticed: another holds it
		return 0, nil
	case err != nil:
		// Not renewed: belief lapses by the clock (Epoch), not now, so a
		// store blip shorter than the lease does not drop leadership.
		return e.epochLocked(), err
	}
	// Belief runs from the time this replica stamped, before its write: a
	// slow write only shortens it.
	e.epoch, e.renewed = d.Leader.Epoch, now
	return e.epoch, nil
}

func (e *Elector) epochLocked() uint64 {
	if e.epoch != 0 && !e.now().Before(e.renewed.Add(e.duration())) {
		e.epoch = 0
	}
	return e.epoch
}

// Release gives up the lease if this replica holds it, so another may take
// it at once rather than when it lapses (a rolling restart then loses no
// publication time). Belief ends first: nothing this replica does after
// Release starts is done as leader. The lease's holder and epoch stay;
// it is marked lapsed, and the next taker's epoch is one more.
func (e *Elector) Release(ctx context.Context) error {
	e.mu.Lock()
	e.epoch = 0
	e.mu.Unlock()
	_, err := e.State.Update(ctx, func(doc *state.Document) error {
		if doc.Leader == nil || doc.Leader.Holder != e.ID {
			return errNotHolder
		}
		doc.Leader.DurationMillis = 0 // lapsed as of its last renewal
		return nil
	})
	if errors.Is(err, errNotHolder) {
		return nil
	}
	return err
}

// Run tries for the lease every RenewEvery until ctx ends.
func (e *Elector) Run(ctx context.Context) {
	every := e.RenewEvery
	if every <= 0 {
		every = e.duration() / 3
	}
	t := time.NewTicker(every)
	defer t.Stop()
	was := uint64(0)
	for {
		epoch, err := e.Try(ctx)
		if err != nil {
			slog.Warn("lead: the lease was not renewed", "replica", e.ID, "err", err)
		}
		if epoch != was {
			slog.Info("lead: leadership changed", "replica", e.ID, "epoch", epoch)
			was = epoch
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
