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
	Hash       string    `json:"hash"` // Hash of the snapshot object's bytes
}

// signed is a pointer as it is stored: the payload's bytes, and the
// signature over pointerContext followed by them.
type signed struct {
	Payload []byte `json:"payload"`
	Key     string `json:"key"`
	Sig     []byte `json:"sig"`
}

// pointerContext separates a pointer's signatures from anything else the
// publishing key may sign.
const pointerContext = "golem snapshot pointer v1\n"

// PointerKey is the pointer's object key in the bucket.
func PointerKey(pool string) string { return "snapshots/" + pool + "/current" }

// ObjectKey is a snapshot's object key: content-addressed, so writing it
// again is harmless.
func ObjectKey(pool, hash string) string { return "snapshots/" + pool + "/" + hash + ".json" }

// SignPointer encodes and signs p.
func SignPointer(ctx context.Context, p Pointer, s sign.Signer) ([]byte, error) {
	payload, err := jsonv2.Marshal(p)
	if err != nil {
		return nil, err
	}
	sig, err := s.Sign(ctx, append([]byte(pointerContext), payload...))
	if err != nil {
		return nil, err
	}
	return jsonv2.Marshal(signed{Payload: payload, Key: s.KeyID(), Sig: sig})
}

// OpenPointer verifies a stored pointer and decodes it. Only verified bytes
// are decoded, and strictly, as a snapshot is.
func OpenPointer(b []byte, v sign.Verifier) (Pointer, error) {
	var s signed
	if err := jsonv2.Unmarshal(b, &s, jsonv2.RejectUnknownMembers(true)); err != nil {
		return Pointer{}, fmt.Errorf("pointer: %w", err)
	}
	if err := v.Verify(s.Key, append([]byte(pointerContext), s.Payload...), s.Sig); err != nil {
		return Pointer{}, fmt.Errorf("pointer: %w", err)
	}
	var p Pointer
	if err := jsonv2.Unmarshal(s.Payload, &p, jsonv2.RejectUnknownMembers(true)); err != nil {
		return Pointer{}, fmt.Errorf("pointer: %w", err)
	}
	return p, nil
}
