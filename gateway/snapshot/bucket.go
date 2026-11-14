package snapshot

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// GatewayID, if set, names the status object written after each apply
	// (status/<pool>/<id>): diagnostic only, never trusted (anyone with the
	// pool's credentials can overwrite it).
	GatewayID string
	Interval  time.Duration // default one second
	Log       *slog.Logger

	tag string // the pointer's entity tag last seen
}

// Start raises the holder's floor to the local checkpoint and the pool's
// trusted checkpoint, whichever is higher. A gateway with neither (an
// empty disk, the bucket unreadable) does not start: it could not tell a
// replayed pointer from the current one. A pool never published has no
// trusted checkpoint, and its floor is zero.
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
	return nil
}

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

func (b *Bucket) trustedCheckpoint(ctx context.Context) (uint64, error) {
	raw, _, err := b.Bucket.Get(ctx, snapshot.CheckpointKey(b.Pool), "")
	if errors.Is(err, bucket.ErrNotFound) {
		return 0, nil // never published
	}
	if err != nil {
		return 0, err
	}
	var cp snapshot.Checkpoint
	if err := jsonv2.Unmarshal(raw, &cp, jsonv2.RejectUnknownMembers(true)); err != nil {
		return 0, fmt.Errorf("checkpoint %s: %w", snapshot.CheckpointKey(b.Pool), err)
	}
	if cp.Pool != b.Pool {
		return 0, fmt.Errorf("checkpoint %s is pool %q's", snapshot.CheckpointKey(b.Pool), cp.Pool)
	}
	return cp.PublishSeq, nil
}

// Poll reads the pointer and applies a newer publication. An unchanged
// pointer costs one conditional get.
func (b *Bucket) Poll(ctx context.Context) error {
	raw, tag, err := b.Bucket.Get(ctx, snapshot.PointerKey(b.Pool), b.tag)
	if errors.Is(err, bucket.ErrNotModified) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pointer %s: %w", snapshot.PointerKey(b.Pool), err)
	}
	err = b.read(ctx, raw)
	// A pointer judged (applied, held already, or refused) is not read
	// again until it changes; one whose snapshot could not be fetched is.
	if _, transient := errors.AsType[fetchError](err); !transient {
		b.tag = tag
	}
	return err
}

// fetchError is a snapshot object the bucket did not return: retried at
// the next poll.
type fetchError struct{ error }

func (e fetchError) Unwrap() error { return e.error }

// read judges a pointer and applies the publication it names, unless the
// gateway holds it already.
func (b *Bucket) read(ctx context.Context, raw []byte) error {
	ptr, err := snapshot.OpenPointer(raw, b.Keys)
	if err != nil {
		return err
	}
	if ptr.Pool != b.Pool {
		return fmt.Errorf("pointer for pool %q in pool %q's place: refused", ptr.Pool, b.Pool)
	}
	if floor := b.Holder.Floor(); ptr.PublishSeq < floor {
		return fmt.Errorf("pointer %d is below the floor %d: refused (a replayed pointer, or a stale publisher)", ptr.PublishSeq, floor)
	}
	if c := b.Holder.cur.Load(); c != nil && ptr.PublishSeq == c.Snapshot.PublishSeq && ptr.Hash == c.Hash {
		return nil
	}
	body, _, err := b.Bucket.Get(ctx, snapshot.ObjectKey(b.Pool, ptr.Hash), "")
	if err != nil {
		return fetchError{fmt.Errorf("snapshot %s: %w", ptr.Hash, err)}
	}
	if got := snapshot.Hash(body); got != ptr.Hash {
		return fmt.Errorf("snapshot %s hashes to %s: refused (not the object the pointer signed)", ptr.Hash, got)
	}
	s, err := snapshot.Decode(body)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", ptr.Hash, err)
	}
	if s.PublishSeq != ptr.PublishSeq {
		return fmt.Errorf("snapshot %s is publication %d, its pointer says %d: refused", ptr.Hash, s.PublishSeq, ptr.PublishSeq)
	}
	if err := b.Holder.Apply(s, ptr.Hash, b.Secrets); err != nil {
		return err
	}
	if err := b.saveCheckpoint(ptr.PublishSeq); err != nil {
		// Applied but not persisted: a restart's floor is the older one.
		b.log().Error("snapshot checkpoint not written", "path", b.Checkpoint, "err", err)
	}
	b.writeStatus(ctx, ptr)
	b.log().Info("snapshot applied", "pool", b.Pool, "seq", ptr.PublishSeq, "hash", ptr.Hash[:12])
	return nil
}

// saveCheckpoint writes the sequence whole, then renames it into place.
func (b *Bucket) saveCheckpoint(seq uint64) error {
	if b.Checkpoint == "" {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(b.Checkpoint), ".checkpoint-*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(strconv.FormatUint(seq, 10) + "\n")
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), b.Checkpoint)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func (b *Bucket) writeStatus(ctx context.Context, ptr snapshot.Pointer) {
	if b.GatewayID == "" {
		return
	}
	st, _ := json.Marshal(map[string]any{"publishSeq": ptr.PublishSeq, "hash": ptr.Hash, "appliedAt": b.Holder.now().UTC()})
	if _, err := b.Bucket.Put(ctx, "status/"+b.Pool+"/"+b.GatewayID, st, ""); err != nil {
		b.log().Warn("status not written", "err", err)
	}
}

// Run polls until ctx ends, reporting a refusal or failure once until it
// changes.
func (b *Bucket) Run(ctx context.Context) {
	every := b.Interval
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var lastErr string
	for {
		if err := b.Poll(ctx); err != nil {
			if err.Error() != lastErr {
				b.log().Error("publication not applied; serving the previous one", "pool", b.Pool, "err", err)
			}
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Bucket) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}
