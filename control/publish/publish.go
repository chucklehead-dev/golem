// Package publish is golem-control's publisher. For each pool it writes
// the snapshot to the bucket, wins the pool's publication record in the
// state document by a fenced update, then writes the pool's trusted
// checkpoint and the signed pointer to the snapshot.
//
// The record, not the pointer, says what was last published. Anyone who
// can write the bucket but not sign can copy an old signed pointer back, so
// numbering from the pointer would let a replay set the sequence back.
// model/publication.md checks this, and the fence, as replayPointer and
// FENCE.
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
	// ErrFenced means this publisher does not hold the fence at its epoch.
	// Or the record was written under a later epoch or from a newer state,
	// or a later publication's checkpoint or pointer is in place.
	ErrFenced = errors.New("publish: fenced by a newer publication")
	// ErrTampered means the pool's trusted checkpoint does not verify, or
	// is missing though a publication was made. Someone other than
	// golem-control wrote the bucket. Nothing is published until a person
	// looks, because numbering past it could lower gateways' floors.
	ErrTampered = errors.New("publish: the pool's trusted checkpoint has been tampered with")
	// ErrConflict means another publisher updated the record first. Read
	// it again and retry.
	ErrConflict = errors.New("publish: the publication record changed concurrently")
	// ErrRegressed means the pool's record, read afresh, is below what the
	// bucket proves was published. Records are written first and restores
	// keep them, so the state document went back. Revocations made since
	// may have revived. The pool is not published until a person restores
	// the newest document, or adopts this one with golem-control bootstrap
	// -adopt.
	ErrRegressed = errors.New("publish: the state document went back: the pool's publication record is below what the bucket proves was published")
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

// Publish is PublishAll for one pool. build makes the publication given
// its sequence number. It is published under fence, from the state
// version source.
//
// The number is one above the highest of the record, the verified
// checkpoint and the signed pointer. A checkpoint that does not verify, or
// is missing after a publication, stops it with ErrTampered.
//
// The writes go in this order: the snapshot (content-addressed), the
// record by a fenced compare-and-swap, the checkpoint, then the pointer. A
// publisher that dies after the record leaves the checkpoint and pointer
// behind, and the next renewal moves them on. Until then a gateway
// restarted with an empty disk takes the older checkpoint as its floor.
func (p *Publisher) Publish(ctx context.Context, fence state.Fence, source uint64, build func(seq uint64) (*snapshot.Snapshot, error)) (snapshot.Pointer, error) {
	ptrs, errs := PublishAll(ctx, p.State, fence, source, []*Publisher{p}, func(_ *Publisher, seq uint64) (*snapshot.Snapshot, error) {
		return build(seq)
	})
	if err := errs[p.Pool]; err != nil {
		return snapshot.Pointer{}, err
	}
	return ptrs[p.Pool], nil
}

// PublishAll publishes every pool with one state write. Each pool's
// snapshot is numbered and written, then all their records are won in one
// fenced update, then each pool's checkpoint and pointer. So a cycle costs
// one document version, whatever the number of pools (ADR 0053).
//
// A pool that fails before the record, by tampering or a build error, is
// left out and the others go on. Its error is in errs. The publishers
// share st.
func PublishAll(ctx context.Context, st *state.Store, fence state.Fence, source uint64, pubs []*Publisher,
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
	if err := holds(st.Current().Fence, fence); err != nil {
		return fail(err)
	}
	var ready []prepared
	for _, p := range pubs {
		pr, err := p.prepare(ctx, st, st.Current().Publications[p.Pool], fence, source, build)
		if err != nil {
			errs[p.Pool] = err
			continue
		}
		ready = append(ready, pr)
	}
	if len(ready) == 0 {
		return ptrs, errs
	}
	if _, err := st.FencedUpdate(ctx, fence, func(d *state.Document) error {
		for _, pr := range ready {
			if d.Publications[pr.p.Pool] != pr.rec {
				return ErrConflict // another publisher won since this one read the record
			}
		}
		if d.Publications == nil {
			d.Publications = map[string]state.Record{}
		}
		for _, pr := range ready {
			d.Publications[pr.p.Pool] = state.Record{Epoch: fence.Epoch, Seq: pr.seq, Source: source}
		}
		return nil
	}); err != nil {
		if errors.Is(err, state.ErrFenced) {
			err = fmt.Errorf("%w: %w", ErrFenced, err)
		}
		return fail(err)
	}
	for _, pr := range ready {
		ptr, err := pr.p.finish(ctx, pr, fence, source)
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
func (p *Publisher) prepare(ctx context.Context, st *state.Store, rec state.Record, fence state.Fence, source uint64,
	build func(*Publisher, uint64) (*snapshot.Snapshot, error)) (prepared, error) {
	if fence.Epoch < rec.Epoch || source < rec.Source {
		return prepared{}, fmt.Errorf("%w: the record is at epoch %d, state %d; this publisher has epoch %d, state %d", ErrFenced, rec.Epoch, rec.Source, fence.Epoch, source)
	}
	floor, err := p.floor(ctx, rec)
	if err != nil {
		return prepared{}, err
	}
	if floor > rec.Seq {
		return prepared{}, p.below(ctx, st, fence, floor)
	}
	seq := rec.Seq + 1
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

// below is the error for a record read below floor, what the bucket proves
// was published. The record is written before the checkpoint and pointer.
// So either another publisher won it since it was read, or the document
// went back (ErrRegressed). Numbering past the floor would publish the
// older document's entitlements, reviving revocations made since.
func (p *Publisher) below(ctx context.Context, st *state.Store, fence state.Fence, floor uint64) error {
	if err := st.Refresh(ctx); err != nil {
		return err
	}
	d := st.Current()
	if err := holds(d.Fence, fence); err != nil {
		return err
	}
	rec := d.Publications[p.Pool]
	if rec.Seq >= floor {
		return fmt.Errorf("%w: pool %s's record moved to %d while this publisher read it", ErrConflict, p.Pool, rec.Seq)
	}
	return fmt.Errorf("%w: pool %s's record (state version %d) is publication %d, but the bucket proves publication %d (its verified checkpoint or pointer). "+
		"The document was put back to an older version or replaced, and revocations since may have revived; nothing is published to this pool. "+
		"Put the newest version of %s back from the bucket's versions and restart golem-control; if it cannot be recovered, redo the revocations since (the audit archive lists them) and run golem-control bootstrap -adopt",
		ErrRegressed, p.Pool, d.StateVersion, rec.Seq, floor, state.DocumentKey)
}

// Floor is the highest publication the bucket proves was made to the pool,
// from its verified checkpoint and signed pointer. The publisher numbers
// above it, and golem-control bootstrap writes records at it. A checkpoint
// that does not verify, or is missing beside a signed pointer, is
// ErrTampered.
func (p *Publisher) Floor(ctx context.Context) (uint64, error) {
	return p.floor(ctx, state.Record{})
}

// finish writes a pool's checkpoint, then its pointer, once its record is won.
func (p *Publisher) finish(ctx context.Context, pr prepared, fence state.Fence, source uint64) (snapshot.Pointer, error) {
	cp, err := snapshot.SignCheckpoint(ctx, snapshot.Checkpoint{Pool: p.Pool, PublishSeq: pr.seq}, p.Signer)
	if err != nil {
		return snapshot.Pointer{}, err
	}
	if err := p.advance(ctx, snapshot.CheckpointKey(p.Pool), pr.seq, cp); err != nil {
		return snapshot.Pointer{}, err
	}
	now := p.now()
	ptr := snapshot.Pointer{Pool: p.Pool, PublishSeq: pr.seq, Epoch: fence.Epoch, Source: source, IssuedAt: now,
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

// Published is the pool's current publication as a gateway would take it:
// the pointer verified (with the signer's own public half or Keys) as this
// pool's, then the snapshot it names, its hash and sequence checked
// against the pointer. bucket.ErrNotFound if nothing is published yet.
func (p *Publisher) Published(ctx context.Context) (*snapshot.Snapshot, error) {
	keys := p.keys()
	if keys == nil {
		return nil, errors.New("publish: no Keys to verify the pool's pointer with: set Publisher.Keys to the publishing key's public half")
	}
	raw, _, err := p.Bucket.Get(ctx, snapshot.PointerKey(p.Pool), "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", snapshot.PointerKey(p.Pool), err)
	}
	ptr, err := snapshot.OpenPointer(raw, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", snapshot.PointerKey(p.Pool), err)
	}
	if ptr.Pool != p.Pool {
		return nil, fmt.Errorf("%s names pool %q", snapshot.PointerKey(p.Pool), ptr.Pool)
	}
	body, _, err := p.Bucket.Get(ctx, snapshot.ObjectKey(p.Pool, ptr.Hash), "")
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", ptr.Hash, err)
	}
	if got := snapshot.Hash(body); got != ptr.Hash {
		return nil, fmt.Errorf("snapshot %s hashes to %s: not the object the pointer signed", ptr.Hash, got)
	}
	s, err := snapshot.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", ptr.Hash, err)
	}
	if s.PublishSeq != ptr.PublishSeq {
		return nil, fmt.Errorf("snapshot %s is publication %d, its pointer says %d", ptr.Hash, s.PublishSeq, ptr.PublishSeq)
	}
	return s, nil
}

// holds refuses a publication unless the document's fence, have, is want.
func holds(have *state.Fence, want state.Fence) error {
	if have == nil || *have != want {
		who := "no one holds the fence"
		if have != nil {
			who = fmt.Sprintf("%s holds the fence at epoch %d", have.Holder, have.Epoch)
		}
		return fmt.Errorf("%w: %s; this publisher is %s at epoch %d", ErrFenced, who, want.Holder, want.Epoch)
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
		// A pointer counts only if it verifies as this pool's: an
		// unverifiable one proves nothing (gateways refuse it, and this
		// publication replaces it), and another pool's signed pointer copied
		// here would otherwise set the floor.
		if seq, ok := p.verifiedSeq(false, raw); ok {
			ptrSeq, havePtr = seq, true
		}
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

// advance writes body, publication seq's object, at key unless a later
// publication's is there. The put is conditional on the object read, so a
// later one written meanwhile is not overwritten. A stored checkpoint that
// does not verify is ErrTampered. A stored pointer that does not verify is
// replaced, as gateways ignore it too.
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
