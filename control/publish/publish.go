// Package publish is golem-control's publisher (control-plane.md §4,
// model/publication.md): it writes a pool's snapshot to the bucket and
// then the signed pointer to it, numbering publications from the pool's
// publication record, which it updates by compare-and-swap.
//
// The record, not the pointer, is the authority on what was last
// published: anyone who can write the bucket but not sign can copy an old
// signed pointer back (the model's replayPointer), so numbering from the
// pointer would let a replay set the sequence back. A publisher is fenced
// as the model's FENCE is: it publishes only with a leadership epoch and a
// state version at least the record's.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Record is a pool's publication record: the last publication's sequence,
// and the epoch and state version it was made under.
type Record struct {
	Epoch  uint64 `json:"epoch"`
	Seq    uint64 `json:"seq"`
	Source uint64 `json:"source"`
}

// RecordKey is the record's object key. In this slice it shares the
// bucket; the design keeps it with the state document (control-plane.md
// §3), where only golem-control writes.
func RecordKey(pool string) string { return "control/" + pool + "/record" }

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
	Bucket bucket.Bucket
	Signer sign.Signer
	Pool   string
	Now    func() time.Time
}

// LoadRecord reads the pool's record; none yet is the zero record.
func (p *Publisher) LoadRecord(ctx context.Context) (Record, string, error) {
	b, tag, err := p.Bucket.Get(ctx, RecordKey(p.Pool), "")
	if errors.Is(err, bucket.ErrNotFound) {
		return Record{}, bucket.Absent, nil
	}
	if err != nil {
		return Record{}, "", err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, "", fmt.Errorf("publication record %s: %w", RecordKey(p.Pool), err)
	}
	return r, tag, nil
}

// Publish builds the next publication with build, given its sequence
// number, and publishes it under the leadership epoch from the state
// version source. The snapshot is written first (content-addressed), then
// the record by compare-and-swap, then the pointer: a publisher that dies
// between the record and the pointer leaves the record ahead, and the next
// publication (the lease renewal, within a minute) moves the pointer on.
func (p *Publisher) Publish(ctx context.Context, epoch, source uint64, build func(seq uint64) (*snapshot.Snapshot, error)) (snapshot.Pointer, error) {
	rec, tag, err := p.LoadRecord(ctx)
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
	next, _ := json.Marshal(Record{Epoch: epoch, Seq: seq, Source: source})
	if _, err := p.Bucket.Put(ctx, RecordKey(p.Pool), next, tag); errors.Is(err, bucket.ErrPrecondition) {
		return snapshot.Pointer{}, ErrConflict
	} else if err != nil {
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
