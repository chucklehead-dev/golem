package snapshot

import (
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"time"

	"github.com/chucklehead-dev/golem/lib/sign"
)

// Pointer is a pool's current publication (control-plane.md §4): which
// snapshot, by its hash, at which sequence. Gateways poll it and trust
// only a signed one; the snapshot object it names is checked against its
// hash, so the snapshot needs no signature of its own.
type Pointer struct {
	Pool       string    `json:"pool"`
	PublishSeq uint64    `json:"publishSeq"`
	Epoch      uint64    `json:"epoch"`  // the publisher's leadership epoch
	Source     uint64    `json:"source"` // the state version it was built from
	IssuedAt   time.Time `json:"issuedAt"`
	// Expires is when this pointer is overdue for replacement: the
	// publisher republishes every minute, so a gateway that still reads it
	// after Expires is cut off from the publisher, or is being shown an old
	// pointer (TUF's timestamp role). It is a signal, not a refusal: the
	// snapshot's own ValidUntil is what stops a gateway serving (ADR 0030).
	Expires time.Time `json:"expires"`
	Hash    string    `json:"hash"` // Hash of the snapshot object's bytes
}

// Checkpoint is a pool's trusted floor: the last publication golem-control
// recorded, for a gateway that starts with no local checkpoint (the
// publication model's TRUSTED_FLOOR). It is signed, as the pointer is, and
// lives under a prefix only golem-control's role may write
// (control-plane.md §3, "Write access"). golem-control writes it after the
// record and before the pointer.
type Checkpoint struct {
	Pool       string `json:"pool"`
	PublishSeq uint64 `json:"publishSeq"`
}

// signed is an object as it is stored: the payload's bytes, and the
// signature over the object's context followed by them.
type signed struct {
	Payload []byte `json:"payload"`
	Key     string `json:"key"`
	Sig     []byte `json:"sig"`
}

// The contexts separate each kind's signatures from each other's and from
// anything else the publishing key may sign.
const (
	pointerContext    = "golem snapshot pointer v1\n"
	checkpointContext = "golem snapshot checkpoint v1\n"
)

// signedBytes is what a signature covers: the payload in its context.
func signedBytes(sigContext string, payload []byte) []byte {
	return append([]byte(sigContext), payload...)
}

// PointerKey is the pointer's object key in the bucket.
func PointerKey(pool string) string { return "snapshots/" + pool + "/current" }

// ObjectKey is a snapshot's object key: content-addressed, so writing it
// again is harmless.
func ObjectKey(pool, hash string) string { return "snapshots/" + pool + "/" + hash + ".json" }

// CheckpointKey is the checkpoint's object key.
func CheckpointKey(pool string) string { return "control/" + pool + "/checkpoint" }

// SignPointer encodes and signs p.
func SignPointer(ctx context.Context, p Pointer, s sign.Signer) ([]byte, error) {
	return seal(ctx, pointerContext, p, s)
}

// OpenPointer verifies a stored pointer and decodes it. Only verified bytes
// are decoded, and strictly, as a snapshot is.
func OpenPointer(b []byte, v sign.Verifier) (Pointer, error) {
	var p Pointer
	return p, open(b, pointerContext, v, &p, "pointer")
}

// SignCheckpoint encodes and signs c.
func SignCheckpoint(ctx context.Context, c Checkpoint, s sign.Signer) ([]byte, error) {
	return seal(ctx, checkpointContext, c, s)
}

// OpenCheckpoint verifies a stored checkpoint and decodes it.
func OpenCheckpoint(b []byte, v sign.Verifier) (Checkpoint, error) {
	var c Checkpoint
	return c, open(b, checkpointContext, v, &c, "checkpoint")
}

// seal encodes v and signs it in sigContext, as signed's envelope.
func seal(ctx context.Context, sigContext string, v any, s sign.Signer) ([]byte, error) {
	payload, err := jsonv2.Marshal(v)
	if err != nil {
		return nil, err
	}
	sig, err := s.Sign(ctx, signedBytes(sigContext, payload))
	if err != nil {
		return nil, err
	}
	return jsonv2.Marshal(signed{Payload: payload, Key: s.KeyID(), Sig: sig})
}

// open verifies an envelope in sigContext and only then decodes its payload
// into v, strictly. what names the object in errors.
func open(b []byte, sigContext string, ver sign.Verifier, v any, what string) error {
	var s signed
	if err := jsonv2.Unmarshal(b, &s, jsonv2.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := ver.Verify(s.Key, signedBytes(sigContext, s.Payload), s.Sig); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := jsonv2.Unmarshal(s.Payload, v, jsonv2.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}
