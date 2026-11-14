package snapshot

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/sign"
)

func keyPair(t *testing.T, id string) (sign.Ed25519, sign.Ed25519Keys) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return sign.Ed25519{ID: id, Key: priv}, sign.Ed25519Keys{id: pub}
}

// reseal re-encodes an envelope with its parts changed.
func reseal(t *testing.T, b []byte, change func(*signed)) []byte {
	t.Helper()
	var s signed
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	change(&s)
	out, _ := json.Marshal(s)
	return out
}

// TestPointer: only a pointer signed by a trusted key opens, and only as
// signed: a changed payload, an untrusted key, or the key's signature of
// the same bytes outside a pointer's context is refused.
func TestPointer(t *testing.T) {
	signer, keys := keyPair(t, "k1")
	p := Pointer{Pool: "internal", PublishSeq: 3, Epoch: 2, Source: 9, IssuedAt: time.Unix(0, 0).UTC(), Hash: "ab"}
	b, err := SignPointer(t.Context(), p, signer)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := OpenPointer(b, keys); err != nil || got != p {
		t.Fatalf("open %+v %v, want %+v", got, err, p)
	}
	other, _ := keyPair(t, "k2")
	forged, _ := SignPointer(t.Context(), p, other)
	later := p
	later.PublishSeq = 99
	laterPayload, _ := json.Marshal(later)
	bare, _ := signer.Sign(t.Context(), laterPayload) // the right key, no pointer context
	for name, bad := range map[string][]byte{
		"an untrusted key":                forged,
		"a changed payload":               reseal(t, b, func(s *signed) { s.Payload = laterPayload }),
		"the key relabelled":              reseal(t, forged, func(s *signed) { s.Key = "k1" }),
		"a signature outside the context": reseal(t, b, func(s *signed) { s.Payload, s.Sig = laterPayload, bare }),
	} {
		if _, err := OpenPointer(bad, keys); !errors.Is(err, sign.ErrBadSignature) {
			t.Errorf("%s: %v, want ErrBadSignature", name, err)
		}
	}
}
