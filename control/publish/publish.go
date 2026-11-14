// Package publish is golem-control's publisher (control-plane.md §4,
// model/publication.md): it writes a pool's snapshot to the bucket, wins
// the pool's publication record in the state document (control/state) by
// a fenced update, writes the pool's trusted checkpoint, then the signed
// pointer to the snapshot.
//
// The record, not the pointer, is the authority on what was last
// published: anyone who can write the pool's objects but not sign can copy
// an old signed pointer back (the model's replayPointer), so numbering
// from the pointer would let a replay set the sequence back. A publisher
// is fenced as the model's FENCE is: it publishes only with a leadership
// epoch and a state version at least the record's.
package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Errors a publish reports.
var (
	// ErrFenced: this publisher does not hold the lease at its epoch, or the
	// record was written under a later epoch or from a newer state, or a
	// later publication's checkpoint or pointer is in place.
	ErrFenced = errors.New("publish: fenced by a newer publication")
	// ErrConflict: another publisher updated the record first; re-read and
	// retry.
	ErrConflict = errors.New("publish: the publication record changed concurrently")
)

// Publisher publishes one pool's snapshots.
type Publisher struct {
	State  *state.Store  // the publication record's home
	Bucket bucket.Bucket // snapshots, checkpoints and pointers
	Signer sign.Signer
	Pool   string
	Now    func() time.Time
	// PointerTTL is how long after its issue a pointer is overdue
	// (snapshot.Pointer.Expires): ten minutes unless set, ten renewals.
	PointerTTL time.Duration
}

// Record is the pool's publication record as last read.
func (p *Publisher) Record(ctx context.Context) (state.Record, error) {
	if err := p.State.Refresh(ctx); err != nil {
		return state.Record{}, err
	}
	return p.State.Current().Publications[p.Pool], nil
}

// Lease is the leadership a publication is made under: the replica that
// believes it holds the lease, and at which epoch (control/lead).
type Lease struct {
	Holder string
	Epoch  uint64
}

// Publish builds the next publication with build, given its sequence
// number, and publishes it under lease from the state version source. The
// snapshot is written first (content-addressed), then the record by the
// state's compare-and-swap, which refuses it unless the state's lease is
// still lease, then the checkpoint, then the pointer. A publisher that
// dies after the record leaves the checkpoint and pointer behind it, and
// the next publication (the lease renewal, within a minute) moves them on;
// until then a gateway restarted with an empty disk takes the older
// checkpoint as its floor.
//
// The record's write checks the holder and epoch, not the lease's expiry:
// while this replica is still the holder no other has taken the lease, so
// none can have published, and a replica stops believing it leads before
// its lease lapses by its own clock (lead.Elector.Epoch).
func (p *Publisher) Publish(ctx context.Context, lease Lease, source uint64, build func(seq uint64) (*snapshot.Snapshot, error)) (snapshot.Pointer, error) {
	rec, err := p.Record(ctx)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if err := holds(p.State.Current().Leader, lease); err != nil {
		return snapshot.Pointer{}, err
	}
	epoch := lease.Epoch
	if epoch < rec.Epoch || source < rec.Source {
		return snapshot.Pointer{}, fmt.Errorf("%w: the record is at epoch %d, state %d; this publisher has epoch %d, state %d", ErrFenced, rec.Epoch, rec.Source, epoch, source)
	}
	seq := rec.Seq + 1
	s, err := build(seq)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if s.PublishSeq != seq {
		return snapshot.Pointer{}, fmt.Errorf("publish: built publication %d, want %d", s.PublishSeq, seq)
	}
	body, err := s.Encode()
	if err != nil {
		return snapshot.Pointer{}, err
	}
	hash := snapshot.Hash(body)
	if _, err := p.Bucket.Put(ctx, snapshot.ObjectKey(p.Pool, hash), body, ""); err != nil {
		return snapshot.Pointer{}, err
	}
	if _, err := p.State.Update(ctx, func(d *state.Document) error {
		if err := holds(d.Leader, lease); err != nil {
			return err
		}
		if d.Publications[p.Pool] != rec {
			return ErrConflict // another publisher won since this one read the record
		}
		if d.Publications == nil {
			d.Publications = map[string]state.Record{}
		}
		d.Publications[p.Pool] = state.Record{Epoch: epoch, Seq: seq, Source: source}
		return nil
	}); err != nil {
		return snapshot.Pointer{}, err
	}
	cp, err := snapshot.SignCheckpoint(ctx, snapshot.Checkpoint{Pool: p.Pool, PublishSeq: seq}, p.Signer)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if err := p.advance(ctx, snapshot.CheckpointKey(p.Pool), seq, cp); err != nil {
		return snapshot.Pointer{}, err
	}
	now := p.now()
	ptr := snapshot.Pointer{Pool: p.Pool, PublishSeq: seq, Epoch: epoch, Source: source, IssuedAt: now,
		Expires: now.Add(cmp.Or(p.PointerTTL, 10*time.Minute)), Hash: hash}
	signed, err := snapshot.SignPointer(ctx, ptr, p.Signer)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if err := p.advance(ctx, snapshot.PointerKey(p.Pool), seq, signed); err != nil {
		return snapshot.Pointer{}, err
	}
	return ptr, nil
}

// holds refuses a publication unless l is the state's lease.
func holds(l *state.Lease, want Lease) error {
	if l == nil || l.Holder != want.Holder || l.Epoch != want.Epoch {
		have := "no lease"
		if l != nil {
			have = fmt.Sprintf("%s holds the lease at epoch %d", l.Holder, l.Epoch)
		}
		return fmt.Errorf("%w: %s; this publisher is %s at epoch %d", ErrFenced, have, want.Holder, want.Epoch)
	}
	return nil
}

// advance writes body, publication seq's object at key, unless a later
// publication's is there: conditionally on the object it read, so a later
// one written meanwhile is not overwritten. A stored object whose sequence
// is later than the record's was not written by golem-control (the record
// is written before either object), and is replaced; so is one that does
// not decode. The stored sequence is read without its signature
// (snapshot.PeekSeq): it is only compared with the record.
func (p *Publisher) advance(ctx context.Context, key string, seq uint64, body []byte) error {
	for range 5 {
		cur, tag, err := p.Bucket.Get(ctx, key, "")
		switch {
		case errors.Is(err, bucket.ErrNotFound):
			tag = bucket.Absent
		case err != nil:
			return err
		default:
			if stored, err := snapshot.PeekSeq(cur); err == nil && stored > seq {
				rec, err := p.Record(ctx)
				if err != nil {
					return err
				}
				if rec.Seq >= stored {
					return fmt.Errorf("%w: %s holds publication %d, this publisher wrote %d", ErrFenced, key, stored, seq)
				}
			}
		}
		_, err = p.Bucket.Put(ctx, key, body, tag)
		if !errors.Is(err, bucket.ErrPrecondition) {
			return err
		}
	}
	return fmt.Errorf("%w: %s kept changing", ErrConflict, key)
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
