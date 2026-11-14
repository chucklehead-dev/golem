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
	"context"
	"encoding/json"
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
	// ErrFenced: the record was written under a later epoch or from a newer
	// state; this publisher has lost its lease or reads stale state.
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
}

// Record is the pool's publication record as last read.
func (p *Publisher) Record(ctx context.Context) (state.Record, error) {
	if err := p.State.Refresh(ctx); err != nil {
		return state.Record{}, err
	}
	return p.State.Current().Publications[p.Pool], nil
}

// Publish builds the next publication with build, given its sequence
// number, and publishes it under the leadership epoch from the state
// version source. The snapshot is written first (content-addressed), then
// the record by the state's compare-and-swap, then the checkpoint, then
// the pointer. A publisher that dies after the record leaves the
// checkpoint and pointer behind it, and the next publication (the lease
// renewal, within a minute) moves them on; until then a gateway restarted
// with an empty disk takes the older checkpoint as its floor.
func (p *Publisher) Publish(ctx context.Context, epoch, source uint64, build func(seq uint64) (*snapshot.Snapshot, error)) (snapshot.Pointer, error) {
	rec, err := p.Record(ctx)
	if err != nil {
		return snapshot.Pointer{}, err
	}
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
	cp, _ := json.Marshal(snapshot.Checkpoint{Pool: p.Pool, PublishSeq: seq})
	if _, err := p.Bucket.Put(ctx, snapshot.CheckpointKey(p.Pool), cp, ""); err != nil {
		return snapshot.Pointer{}, err
	}
	ptr := snapshot.Pointer{Pool: p.Pool, PublishSeq: seq, Epoch: epoch, Source: source, IssuedAt: p.now(), Hash: hash}
	signed, err := snapshot.SignPointer(ctx, ptr, p.Signer)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if _, err := p.Bucket.Put(ctx, snapshot.PointerKey(p.Pool), signed, ""); err != nil {
		return snapshot.Pointer{}, err
	}
	return ptr, nil
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
