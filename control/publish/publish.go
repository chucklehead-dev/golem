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
	"bytes"
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
	// ErrTampered: the pool's trusted checkpoint does not verify, or is
	// missing though a publication was made: someone other than
	// golem-control wrote the bucket. Nothing is published until a person
	// looks (model/leadership.md's VERIFY): numbering past it could lower
	// gateways' floors, or let a forgery set the sequence.
	ErrTampered = errors.New("publish: the pool's trusted checkpoint has been tampered with")
	// ErrConflict: another publisher updated the record first; re-read and
	// retry.
	ErrConflict = errors.New("publish: the publication record changed concurrently")
)

// Publisher publishes one pool's snapshots.
type Publisher struct {
	State  *state.Store  // the publication record's home
	Bucket bucket.Bucket // snapshots, checkpoints and pointers
	Signer sign.Signer
	// Keys are further publishing keys whose checkpoints and pointers are
	// trusted, beside the Signer's own public half (its Verifier, which
	// every signer here provides): during a rotation, the retiring key's
	// (golem-control -trusted-keys). What a retiring key signed counts as
	// golem-control's; the next publication re-signs the checkpoint and
	// pointer with the Signer, so once a lease has passed nothing in the
	// bucket needs the old key.
	Keys sign.Verifier
	Pool string
	Now  func() time.Time
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

// Lease is the leadership a publication is made under: the fence its
// elector claimed (state.Fence, lead.Elector.Fence).
type Lease = state.Fence

// Publish builds the next publication with build, given its sequence
// number, and publishes it under lease from the state version source: it
// is PublishAll for one pool.
//
// The number is one above the highest of the record, the verified
// checkpoint and the signed pointer (model/leadership.md's GUARD and
// VERIFY), and a checkpoint that does not verify, or is missing after a
// publication, stops it with ErrTampered. The snapshot is written first
// (content-addressed), then the record by the state's fenced
// compare-and-swap, which refuses it unless the document's fence is still
// lease, then the checkpoint, then the pointer. A publisher that dies
// after the record leaves the checkpoint and pointer behind it, and the
// next publication (the controller's renewal, within a minute) moves them
// on; until then a gateway restarted with an empty disk takes the older
// checkpoint as its floor.
//
// The record's write checks the fence's holder and epoch, not the lease's
// expiry: while the fence names this replica no other has claimed a later
// epoch, so none can have published, and a replica stops believing it
// leads before its lease lapses by its own clock (lead.Elector.Epoch).
func (p *Publisher) Publish(ctx context.Context, lease Lease, source uint64, build func(seq uint64) (*snapshot.Snapshot, error)) (snapshot.Pointer, error) {
	ptrs, errs := PublishAll(ctx, p.State, lease, source, []*Publisher{p}, func(_ *Publisher, seq uint64) (*snapshot.Snapshot, error) {
		return build(seq)
	})
	if err := errs[p.Pool]; err != nil {
		return snapshot.Pointer{}, err
	}
	return ptrs[p.Pool], nil
}

// PublishAll publishes every pool's publication with one state write: each
// pool's snapshot is numbered and written, then all their records are won
// in a single fenced update (one document version a cycle, whatever the
// number of pools, ADR 0049), then each pool's checkpoint and pointer. A
// pool that fails before the record (tampering, a build error) is left out
// and the others go on; its error is in errs. The publishers share st.
func PublishAll(ctx context.Context, st *state.Store, lease Lease, source uint64, pubs []*Publisher,
	build func(p *Publisher, seq uint64) (*snapshot.Snapshot, error)) (ptrs map[string]snapshot.Pointer, errs map[string]error) {
	ptrs, errs = map[string]snapshot.Pointer{}, map[string]error{}
	fail := func(err error) (map[string]snapshot.Pointer, map[string]error) {
		for _, p := range pubs {
			if errs[p.Pool] == nil {
				errs[p.Pool] = err
			}
		}
		return ptrs, errs
	}
	if err := st.Refresh(ctx); err != nil {
		return fail(err)
	}
	// Checked here as well as by the fenced update below, so a deposed
	// publisher writes no snapshot either.
	if err := holds(st.Current().Fence, lease); err != nil {
		return fail(err)
	}
	var ready []prepared
	for _, p := range pubs {
		pr, err := p.prepare(ctx, st.Current().Publications[p.Pool], lease, source, build)
		if err != nil {
			errs[p.Pool] = err
			continue
		}
		ready = append(ready, pr)
	}
	if len(ready) == 0 {
		return ptrs, errs
	}
	if _, err := st.FencedUpdate(ctx, lease, func(d *state.Document) error {
		for _, pr := range ready {
			if d.Publications[pr.p.Pool] != pr.rec {
				return ErrConflict // another publisher won since this one read the record
			}
		}
		if d.Publications == nil {
			d.Publications = map[string]state.Record{}
		}
		for _, pr := range ready {
			d.Publications[pr.p.Pool] = state.Record{Epoch: lease.Epoch, Seq: pr.seq, Source: source}
		}
		return nil
	}); err != nil {
		if errors.Is(err, state.ErrFenced) {
			err = fmt.Errorf("%w: %w", ErrFenced, err)
		}
		return fail(err)
	}
	for _, pr := range ready {
		ptr, err := pr.p.finish(ctx, pr, lease, source)
		if err != nil {
			errs[pr.p.Pool] = err
			continue
		}
		ptrs[pr.p.Pool] = ptr
	}
	return ptrs, errs
}

// prepared is a pool's publication, numbered and its snapshot written,
// waiting for its record.
type prepared struct {
	p    *Publisher
	rec  state.Record // as read: the record update requires it unchanged
	seq  uint64
	hash string
}

// prepare numbers the pool's next publication and writes its snapshot.
func (p *Publisher) prepare(ctx context.Context, rec state.Record, lease Lease, source uint64,
	build func(*Publisher, uint64) (*snapshot.Snapshot, error)) (prepared, error) {
	if lease.Epoch < rec.Epoch || source < rec.Source {
		return prepared{}, fmt.Errorf("%w: the record is at epoch %d, state %d; this publisher has epoch %d, state %d", ErrFenced, rec.Epoch, rec.Source, lease.Epoch, source)
	}
	// Numbered above the record, the verified checkpoint and the signed
	// pointer: a rolled-back record beside an older checkpoint copied back
	// would otherwise publish below what gateways already trust.
	floor, err := p.floor(ctx, rec)
	if err != nil {
		return prepared{}, err
	}
	seq := max(rec.Seq, floor) + 1
	s, err := build(p, seq)
	if err != nil {
		return prepared{}, err
	}
	if s.PublishSeq != seq {
		return prepared{}, fmt.Errorf("publish: built publication %d, want %d", s.PublishSeq, seq)
	}
	body, err := s.Encode()
	if err != nil {
		return prepared{}, err
	}
	hash := snapshot.Hash(body)
	if _, err := p.Bucket.Put(ctx, snapshot.ObjectKey(p.Pool, hash), body, ""); err != nil {
		return prepared{}, err
	}
	return prepared{p: p, rec: rec, seq: seq, hash: hash}, nil
}

// finish writes a pool's checkpoint, then its pointer, once its record is won.
func (p *Publisher) finish(ctx context.Context, pr prepared, lease Lease, source uint64) (snapshot.Pointer, error) {
	cp, err := snapshot.SignCheckpoint(ctx, snapshot.Checkpoint{Pool: p.Pool, PublishSeq: pr.seq}, p.Signer)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if err := p.advance(ctx, snapshot.CheckpointKey(p.Pool), pr.seq, cp); err != nil {
		return snapshot.Pointer{}, err
	}
	now := p.now()
	ptr := snapshot.Pointer{Pool: p.Pool, PublishSeq: pr.seq, Epoch: lease.Epoch, Source: source, IssuedAt: now,
		Expires: now.Add(cmp.Or(p.PointerTTL, 10*time.Minute)), Hash: pr.hash}
	signed, err := snapshot.SignPointer(ctx, ptr, p.Signer)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if err := p.advance(ctx, snapshot.PointerKey(p.Pool), pr.seq, signed); err != nil {
		return snapshot.Pointer{}, err
	}
	return ptr, nil
}

// holds refuses a publication unless the document's fence, l, is want.
func holds(l *state.Fence, want Lease) error {
	if l == nil || *l != want {
		have := "no lease"
		if l != nil {
			have = fmt.Sprintf("%s holds the lease at epoch %d", l.Holder, l.Epoch)
		}
		return fmt.Errorf("%w: %s; this publisher is %s at epoch %d", ErrFenced, have, want.Holder, want.Epoch)
	}
	return nil
}

// floor is the highest publication the bucket proves was made: the
// verified checkpoint's and the signed pointer's. A checkpoint that does not
// verify, or is missing though the record or a pointer shows one was
// written, is ErrTampered.
func (p *Publisher) floor(ctx context.Context, rec state.Record) (uint64, error) {
	keys := p.keys()
	if keys == nil {
		return 0, errors.New("publish: no Keys to verify the pool's checkpoint with: set Publisher.Keys to the publishing key's public half")
	}
	var ptrSeq uint64
	havePtr := false
	switch raw, _, err := p.Bucket.Get(ctx, snapshot.PointerKey(p.Pool), ""); {
	case errors.Is(err, bucket.ErrNotFound):
	case err != nil:
		return 0, err
	default:
		// An unverifiable pointer proves nothing (gateways refuse it, and
		// this publication replaces it).
		ptrSeq, havePtr = p.verifiedSeq(false, raw)
	}
	key := snapshot.CheckpointKey(p.Pool)
	raw, _, err := p.Bucket.Get(ctx, key, "")
	switch {
	case errors.Is(err, bucket.ErrNotFound):
		// A checkpoint exists from the first publication's on: record 1
		// without one is a publisher that died between the two writes;
		// a higher record, or any signed pointer, means it was deleted.
		if rec.Seq > 1 || havePtr {
			return 0, fmt.Errorf("%w: %s is missing, but publication %d was made; restore it from the bucket's versions, and find who deleted it", ErrTampered, key, max(rec.Seq, ptrSeq))
		}
		return 0, nil
	case err != nil:
		return 0, err
	}
	cp, err := snapshot.OpenCheckpoint(raw, keys)
	if err != nil || cp.Pool != p.Pool {
		return 0, fmt.Errorf("%w: %s does not verify as this pool's (%v). If the publishing key was rotated, give golem-control the previous key's public half in -trusted-keys (docs/operations.md, key rotation); otherwise restore golem-control's from the bucket's versions, and find who wrote it", ErrTampered, key, err)
	}
	return max(cp.PublishSeq, ptrSeq), nil
}

// advance writes body, publication seq's object at key, unless a later
// publication's is there: conditionally on the object it read, so a later
// one written meanwhile (a successor's, while this publisher was paused)
// is not overwritten. A stored checkpoint that does not verify is
// ErrTampered; a stored pointer that does not verify is replaced (it is
// junk to gateways too).
func (p *Publisher) advance(ctx context.Context, key string, seq uint64, body []byte) error {
	isCheckpoint := key == snapshot.CheckpointKey(p.Pool)
	for range 5 {
		cur, tag, err := p.Bucket.Get(ctx, key, "")
		switch {
		case errors.Is(err, bucket.ErrNotFound):
			tag = bucket.Absent
		case err != nil:
			return err
		default:
			stored, ok := p.verifiedSeq(isCheckpoint, cur)
			switch {
			case ok && stored >= seq && !bytes.Equal(cur, body):
				return fmt.Errorf("%w: %s holds publication %d, this publisher wrote %d", ErrFenced, key, stored, seq)
			case !ok && isCheckpoint:
				return fmt.Errorf("%w: %s does not verify; nothing was written over it. If the publishing key was rotated, give golem-control the previous key's public half in -trusted-keys", ErrTampered, key)
			}
		}
		_, err = p.Bucket.Put(ctx, key, body, tag)
		if !errors.Is(err, bucket.ErrPrecondition) {
			return err
		}
	}
	return fmt.Errorf("%w: %s kept changing", ErrConflict, key)
}

// verifiedSeq is a stored checkpoint's (or pointer's) sequence, if it
// verifies as this pool's.
func (p *Publisher) verifiedSeq(isCheckpoint bool, b []byte) (uint64, bool) {
	if isCheckpoint {
		cp, err := snapshot.OpenCheckpoint(b, p.keys())
		return cp.PublishSeq, err == nil && cp.Pool == p.Pool
	}
	ptr, err := snapshot.OpenPointer(b, p.keys())
	return ptr.PublishSeq, err == nil && ptr.Pool == p.Pool
}

// keys is what verifies the pool's checkpoint and pointer: the Signer's
// own public half, and Keys; nil if neither is there (floor refuses then,
// before anything is written).
func (p *Publisher) keys() sign.Verifier {
	var vs anyOf
	if v, ok := p.Signer.(interface{ Verifier() sign.Verifier }); ok {
		vs = append(vs, v.Verifier())
	}
	if p.Keys != nil {
		vs = append(vs, p.Keys)
	}
	switch len(vs) {
	case 0:
		return nil
	case 1:
		return vs[0]
	}
	return vs
}

// anyOf verifies a signature that any of its verifiers does.
type anyOf []sign.Verifier

// Verify implements sign.Verifier: the last verifier's refusal if none
// verifies.
func (a anyOf) Verify(keyID string, msg, sig []byte) error {
	var err error
	for _, v := range a {
		if err = v.Verify(keyID, msg, sig); err == nil {
			return nil
		}
	}
	return err
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
