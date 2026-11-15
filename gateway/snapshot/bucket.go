package snapshot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Bucket is the published source (control-plane.md §4): the pool's signed
// pointer, polled with a conditional get, and the snapshot it names. A
// pointer is applied only if its signature verifies, it is the gateway's
// pool, and its sequence is at least the floor and above what the gateway
// holds (Holder.Apply, model/publication.md's poll); the snapshot object
// must hash to the pointer's hash and carry its sequence. An old signed
// pointer copied back into the bucket is refused by the floor.
type Bucket struct {
	Bucket  bucket.Bucket
	Keys    sign.Verifier
	Pool    string
	Holder  *Holder
	Secrets Secrets
	// Checkpoint, if set, is a file on the gateway's own disk holding the
	// last sequence applied, written atomically after each apply. Start
	// raises the floor to it and to the pool's trusted checkpoint in the
	// bucket (snapshot.CheckpointKey, which only golem-control writes): a
	// restart with an empty disk still has the publisher's floor.
	Checkpoint string
	// LastGood, if set, is a file on the same disk holding the last
	// publication applied, its signed pointer and snapshot, written before
	// the checkpoint. Start applies it, verified as a publication read from
	// the bucket is, so a gateway restarted while the bucket is
	// unreachable serves until the snapshot's lease ends (gateway.md §6).
	LastGood string
	// GatewayID, if set, names the status object written after each apply
	// (status/<pool>/<id>): diagnostic only, never trusted (anyone with the
	// pool's credentials can overwrite it).
	GatewayID string
	Interval  time.Duration // default one second
	// ApplyRetry is how often a verified publication that failed to apply
	// is tried again while it is current (default 30 seconds).
	ApplyRetry time.Duration
	// MaxSkew is how far the publisher's clock may run ahead of this
	// gateway's (default 2 minutes): a pointer issued further in the
	// gateway's future is refused (security-and-correctness.md §3).
	MaxSkew time.Duration
	// MaxLease is the longest authorization lease the pool's publisher
	// grants (default 4 hours, golem-control's -lease default and an
	// internal pool's maximum, ADR 0032). A snapshot whose ValidUntil is
	// more than MaxLease plus MaxSkew ahead is refused, not clamped: a
	// publisher's clock a day ahead must not grant a day-long lease, and a
	// lease the gateway cannot trust is no lease at all (it fails closed:
	// the gateway keeps what it holds, until that expires).
	MaxLease time.Duration
	Log      *slog.Logger

	tag   string    // the entity tag of the last pointer judged (Poll)
	retry time.Time // when to read a pointer whose publication failed to apply again
	// pending is the last-good file as Start read it, while it failed to
	// apply for a reason that may pass (an applyError: a Secret not yet
	// mounted) and nothing has applied since; Poll tries it again at
	// pendingRetry, every ApplyRetry, whether or not the bucket answers.
	pending      []byte
	pendingRetry time.Time
	expires      time.Time // the last verified pointer's Expires
	// overdue is reported, until a pointer that is not overdue; written by
	// Poll's goroutine, read by a metric's collection.
	overdue atomic.Bool
	// fromDisk: the snapshot served is the last-good copy Start applied,
	// not yet replaced by one read from the bucket (for a metric).
	fromDisk atomic.Bool
}

// Start raises the holder's floor to the local checkpoint and the pool's
// trusted checkpoint, whichever is higher. A gateway with neither (an
// empty disk, the bucket unreadable, the checkpoint missing or not
// verifying) does not start: it could not tell a replayed pointer from the
// current one. A pool never published, with no pointer either, has floor
// zero; a pool with a pointer and no checkpoint has lost its floor, and is
// not taken as never published.
func (b *Bucket) Start(ctx context.Context) error {
	local, haveLocal, err := b.localCheckpoint()
	if err != nil {
		return err
	}
	trusted, err := b.trustedCheckpoint(ctx)
	switch {
	case err == nil:
	case haveLocal:
		b.log().Warn("the pool's trusted checkpoint is unreadable; starting from the local one", "pool", b.Pool, "err", err)
	default:
		return fmt.Errorf("no local checkpoint, and the pool's trusted one is unreadable: %w", err)
	}
	b.Holder.RaiseFloor(max(local, trusted))
	b.loadLastGood()
	return nil
}

// lastGood is the last-good file: the publication as the bucket held it.
type lastGood struct {
	Pointer  []byte `json:"pointer"`
	Snapshot []byte `json:"snapshot"`
}

// loadLastGood applies the last-good publication, if there is one that
// verifies, is at or above the floor and whose lease has not ended. One
// that does not is reported and left: the bucket's next publication
// replaces it. One that verifies but does not compile yet (a Secret not
// yet mounted) is retried by Poll (tryLastGood).
func (b *Bucket) loadLastGood() {
	if b.LastGood == "" {
		return
	}
	raw, err := os.ReadFile(b.LastGood)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		b.log().Warn("the last-good snapshot on disk was not read; waiting for the bucket", "path", b.LastGood, "err", err)
		return
	}
	b.tryLastGood(raw)
}

// tryLastGood applies a last-good file. One that failed for a reason that
// may pass (an applyError) is kept for retryLastGood; any other failure is
// final: the bucket's next publication replaces it.
func (b *Bucket) tryLastGood(raw []byte) {
	ptr, err := b.applyLastGood(raw)
	b.pending = nil
	if _, again := errors.AsType[applyError](err); again {
		b.pending, b.pendingRetry = raw, b.Holder.now().Add(b.applyRetry())
		b.log().Warn("the last-good snapshot on disk did not apply; retrying it every ApplyRetry until it does or the bucket's publication applies",
			"path", b.LastGood, "retry", b.applyRetry(), "err", err)
		return
	}
	if err != nil {
		b.log().Warn("the last-good snapshot on disk was not applied; waiting for the bucket", "path", b.LastGood, "err", err)
		return
	}
	b.fromDisk.Store(true)
	b.log().Info("the last-good snapshot applied from disk", "pool", b.Pool, "seq", ptr.PublishSeq, "hash", ptr.Hash[:12])
}

// retryLastGood tries the last-good file again once its retry is due, and
// only while nothing has applied since Start: a publication from the
// bucket supersedes it (read clears it), and one already applied makes it
// refused as not newer.
func (b *Bucket) retryLastGood() {
	if b.pending == nil || b.Holder.now().Before(b.pendingRetry) {
		return
	}
	if b.Holder.cur.Load() != nil {
		b.pending = nil
		return
	}
	b.tryLastGood(b.pending)
}

// applyRetry is ApplyRetry or its default.
func (b *Bucket) applyRetry() time.Duration { return cmp.Or(b.ApplyRetry, 30*time.Second) }

// applyLastGood decodes a last-good file, verifies it as a publication
// read from the bucket would be, and applies it.
func (b *Bucket) applyLastGood(raw []byte) (snapshot.Pointer, error) {
	var lg lastGood
	if err := json.Unmarshal(raw, &lg); err != nil {
		return snapshot.Pointer{}, err
	}
	ptr, s, err := b.verify(lg.Pointer, lg.Snapshot)
	if err != nil {
		return ptr, err
	}
	return ptr, b.Holder.Apply(s, ptr.Hash, b.Secrets)
}

// localCheckpoint is the sequence in the Checkpoint file, and whether
// there was one: a missing file is an empty disk, an unreadable one an
// error.
func (b *Bucket) localCheckpoint() (uint64, bool, error) {
	if b.Checkpoint == "" {
		return 0, false, nil
	}
	raw, err := os.ReadFile(b.Checkpoint)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	seq, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("checkpoint %s: %w (remove it only if the disk is to be treated as empty)", b.Checkpoint, err)
	}
	return seq, true, nil
}

// trustedCheckpoint is the sequence of the pool's signed checkpoint in the
// bucket; zero for a pool never published.
func (b *Bucket) trustedCheckpoint(ctx context.Context) (uint64, error) {
	key := snapshot.CheckpointKey(b.Pool)
	raw, _, err := b.Bucket.Get(ctx, key, "")
	if errors.Is(err, bucket.ErrNotFound) {
		// Never published, unless a pointer says otherwise: the publisher
		// writes the checkpoint before the first pointer.
		_, _, perr := b.Bucket.Get(ctx, snapshot.PointerKey(b.Pool), "")
		switch {
		case errors.Is(perr, bucket.ErrNotFound):
			return 0, nil
		case perr != nil:
			return 0, fmt.Errorf("checkpoint %s missing, and the pointer unreadable: %w", key, perr)
		}
		return 0, fmt.Errorf("checkpoint %s missing though the pool has a pointer: refused (restore the checkpoint, or start from a local one)", key)
	}
	if err != nil {
		return 0, err
	}
	cp, err := snapshot.OpenCheckpoint(raw, b.Keys)
	if err != nil {
		return 0, fmt.Errorf("checkpoint %s: %w", key, err)
	}
	if cp.Pool != b.Pool {
		return 0, fmt.Errorf("checkpoint %s is pool %q's", key, cp.Pool)
	}
	return cp.PublishSeq, nil
}

// Poll reads the pointer and applies a newer publication. An unchanged
// pointer costs one conditional get.
func (b *Bucket) Poll(ctx context.Context) error {
	b.retryLastGood() // first: the bucket may be unreachable
	if !b.retry.IsZero() && !b.Holder.now().Before(b.retry) {
		b.tag, b.retry = "", time.Time{} // read it again, though unchanged
	}
	raw, tag, err := b.Bucket.Get(ctx, snapshot.PointerKey(b.Pool), b.tag)
	if errors.Is(err, bucket.ErrNotModified) {
		b.checkOverdue()
		return nil
	}
	if err != nil {
		return fmt.Errorf("pointer %s: %w", snapshot.PointerKey(b.Pool), err)
	}
	err = b.read(ctx, raw)
	// A pointer judged (applied, held already, or refused) is not read
	// again until it changes; one whose snapshot could not be fetched is,
	// at the next poll; one whose publication failed to apply (a secret not
	// yet mounted, say) every ApplyRetry, as it may apply later.
	if _, transient := errors.AsType[fetchError](err); !transient {
		b.tag = tag
	}
	if _, failed := errors.AsType[applyError](err); failed {
		b.retry = b.Holder.now().Add(b.applyRetry())
	}
	b.checkOverdue()
	return err
}

// checkOverdue reports, once, that the newest pointer seen is past its
// Expires: the publisher has stopped, or the bucket is showing an old
// pointer. The gateway goes on serving until its snapshot's ValidUntil.
func (b *Bucket) checkOverdue() {
	over := !b.expires.IsZero() && b.Holder.now().After(b.expires)
	if over && !b.overdue.Load() {
		b.log().Error("the pool's pointer is overdue: no publication seen since it expired; serving the current snapshot until its lease ends",
			"pool", b.Pool, "expired", b.expires)
	}
	b.overdue.Store(over)
}

// Overdue reports whether the newest pointer seen is past its Expires, as
// of the last poll.
func (b *Bucket) Overdue() bool { return b.overdue.Load() }

// ServingLastGood reports whether the snapshot served is the last-good
// copy from disk (applied at Start, the bucket unread or refused since),
// not a publication read from the bucket.
func (b *Bucket) ServingLastGood() bool { return b.fromDisk.Load() }

// fetchError is a snapshot object the bucket did not return: retried at
// the next poll.
type fetchError struct{ error }

func (e fetchError) Unwrap() error { return e.error }

// applyError is a verified publication the holder did not apply: retried
// every ApplyRetry.
type applyError struct{ error }

func (e applyError) Unwrap() error { return e.error }

// openPointer verifies a pointer, names this pool and is at or above the
// floor.
func (b *Bucket) openPointer(raw []byte) (snapshot.Pointer, error) {
	ptr, err := snapshot.OpenPointer(raw, b.Keys)
	if err != nil {
		return ptr, err
	}
	if ptr.Pool != b.Pool {
		return ptr, fmt.Errorf("pointer for pool %q in pool %q's place: refused", ptr.Pool, b.Pool)
	}
	// Holder.Apply checks the floor too; checking it here as well refuses a
	// replayed pointer before its snapshot is fetched.
	if floor := b.Holder.Floor(); ptr.PublishSeq < floor {
		return ptr, fmt.Errorf("pointer %d is below the floor %d: refused (a replayed pointer, or a stale publisher)", ptr.PublishSeq, floor)
	}
	if now, skew := b.Holder.now(), b.maxSkew(); ptr.IssuedAt.After(now.Add(skew)) {
		return ptr, fmt.Errorf("pointer %d was issued at %s, %s in this gateway's future (more than the %s allowed skew): refused; check the publisher's and this gateway's clocks (NTP)",
			ptr.PublishSeq, ptr.IssuedAt.UTC().Format(time.RFC3339), ptr.IssuedAt.Sub(now).Round(time.Second), skew)
	}
	return ptr, nil
}

// checkLease refuses a snapshot whose lease runs further ahead than the
// longest a publisher grants, plus the skew.
func (b *Bucket) checkLease(s *snapshot.Snapshot) error {
	if s.ValidUntil.IsZero() {
		return nil
	}
	now, bound := b.Holder.now(), cmp.Or(b.MaxLease, 4*time.Hour)+b.maxSkew()
	if left := s.ValidUntil.Sub(now); left > bound {
		return fmt.Errorf("snapshot %d's lease runs to %s, %s from now, longer than the longest lease (%s) plus the skew: refused; check the publisher's clock, or raise -max-lease to the pool's approved maximum",
			s.PublishSeq, s.ValidUntil.UTC().Format(time.RFC3339), left.Round(time.Second), cmp.Or(b.MaxLease, 4*time.Hour))
	}
	return nil
}

func (b *Bucket) maxSkew() time.Duration { return cmp.Or(b.MaxSkew, 2*time.Minute) }

// openSnapshot checks a snapshot object is the one its pointer signed.
func openSnapshot(ptr snapshot.Pointer, body []byte) (*snapshot.Snapshot, error) {
	if got := snapshot.Hash(body); got != ptr.Hash {
		return nil, fmt.Errorf("snapshot %s hashes to %s: refused (not the object the pointer signed)", ptr.Hash, got)
	}
	s, err := snapshot.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", ptr.Hash, err)
	}
	if s.PublishSeq != ptr.PublishSeq {
		return nil, fmt.Errorf("snapshot %s is publication %d, its pointer says %d: refused", ptr.Hash, s.PublishSeq, ptr.PublishSeq)
	}
	return s, nil
}

// verify is openPointer and openSnapshot together.
func (b *Bucket) verify(rawPtr, body []byte) (snapshot.Pointer, *snapshot.Snapshot, error) {
	ptr, err := b.openPointer(rawPtr)
	if err != nil {
		return ptr, nil, err
	}
	s, err := openSnapshot(ptr, body)
	if err == nil {
		err = b.checkLease(s)
	}
	return ptr, s, err
}

// read judges a pointer and applies the publication it names, unless the
// gateway holds it already.
func (b *Bucket) read(ctx context.Context, raw []byte) error {
	ptr, err := b.openPointer(raw)
	if err != nil {
		return err
	}
	if ptr.Expires.After(b.expires) {
		b.expires = ptr.Expires
	}
	if c := b.Holder.cur.Load(); c != nil && ptr.PublishSeq == c.Snapshot.PublishSeq && ptr.Hash == c.Hash {
		return nil
	}
	body, _, err := b.Bucket.Get(ctx, snapshot.ObjectKey(b.Pool, ptr.Hash), "")
	if err != nil {
		return fetchError{fmt.Errorf("snapshot %s: %w", ptr.Hash, err)}
	}
	s, err := openSnapshot(ptr, body)
	if err != nil {
		return err
	}
	if err := b.checkLease(s); err != nil {
		return err
	}
	if err := b.Holder.Apply(s, ptr.Hash, b.Secrets); err != nil {
		return err // an applyError if it may apply later
	}
	b.fromDisk.Store(false)
	b.pending = nil // superseded
	// The last-good copy before the checkpoint: a crash between leaves a
	// copy at or above the floor, never one below it.
	if err := b.saveLastGood(raw, body); err != nil {
		b.log().Error("the last-good snapshot was not written", "path", b.LastGood, "err", err)
	}
	if err := b.saveCheckpoint(ptr.PublishSeq); err != nil {
		// Applied but not persisted: a restart's floor is the older one.
		b.log().Error("snapshot checkpoint not written", "path", b.Checkpoint, "err", err)
	}
	b.writeStatus(ctx, ptr)
	b.log().Info("snapshot applied", "pool", b.Pool, "seq", ptr.PublishSeq, "hash", ptr.Hash[:12])
	return nil
}

// saveLastGood writes the publication applied to the last-good file.
func (b *Bucket) saveLastGood(ptr, body []byte) error {
	if b.LastGood == "" {
		return nil
	}
	raw, err := json.Marshal(lastGood{Pointer: ptr, Snapshot: body})
	if err != nil {
		return err
	}
	return writeAtomic(b.LastGood, raw)
}

// saveCheckpoint writes the sequence to the checkpoint file.
func (b *Bucket) saveCheckpoint(seq uint64) error {
	if b.Checkpoint == "" {
		return nil
	}
	return writeAtomic(b.Checkpoint, []byte(strconv.FormatUint(seq, 10)+"\n"))
}

// writeAtomic writes a file whole, then renames it into place.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	// The rename is durable once the directory is: without this, a power
	// loss can bring back the previous checkpoint, a lower floor.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// writeStatus writes this gateway's status object for golem-control's
// diagnostics; a failure is logged, never returned.
func (b *Bucket) writeStatus(ctx context.Context, ptr snapshot.Pointer) {
	if b.GatewayID == "" {
		return
	}
	st, _ := json.Marshal(map[string]any{"publishSeq": ptr.PublishSeq, "hash": ptr.Hash, "appliedAt": b.Holder.now().UTC()})
	if _, err := b.Bucket.Put(ctx, "status/"+b.Pool+"/"+b.GatewayID, st, ""); err != nil {
		b.log().Warn("status not written", "err", err)
	}
}

// Run polls at once and then at every interval until ctx ends, reporting
// a refusal or failure once until it changes.
func (b *Bucket) Run(ctx context.Context) {
	repeat(ctx, b.Interval, true, func() error { return b.Poll(ctx) }, func(err error) {
		b.log().Error("publication not applied; serving the previous one", "pool", b.Pool, "err", err)
	})
}

func (b *Bucket) log() *slog.Logger { return cmp.Or(b.Log, slog.Default()) }
