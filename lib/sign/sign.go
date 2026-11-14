// Package sign signs and verifies what golem-control publishes: the
// snapshot pointer, which binds a snapshot's hash (control-plane.md §4,
// ADR 0006, 0007). A Signer holds the publishing key; gateways hold only a
// Verifier, by key ID, so a key is rotated by publishing with a new ID
// while gateways trust both.
//
// Ed25519 is the implementation here (tests, dev, standalone publishing);
// the KMS signer is another implementation of the same interfaces.
package sign

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
)

// Signer signs messages with one key.
type Signer interface {
	// KeyID names the key, so a verifier knows which to check with.
	KeyID() string
	Sign(ctx context.Context, msg []byte) ([]byte, error)
}

// Verifier checks a signature made by the key named keyID.
type Verifier interface {
	Verify(keyID string, msg, sig []byte) error
}

// ErrBadSignature is a signature that does not verify, or a key the
// verifier does not trust.
var ErrBadSignature = errors.New("sign: the signature does not verify")

// Ed25519 is a Signer over an Ed25519 private key.
type Ed25519 struct {
	ID  string
	Key ed25519.PrivateKey
}

func (e Ed25519) KeyID() string { return e.ID }

func (e Ed25519) Sign(_ context.Context, msg []byte) ([]byte, error) {
	if len(e.Key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sign: key %q is not an Ed25519 private key", e.ID)
	}
	return ed25519.Sign(e.Key, msg), nil
}

// Ed25519Keys is a Verifier: the public keys it trusts, by ID.
type Ed25519Keys map[string]ed25519.PublicKey

func (k Ed25519Keys) Verify(keyID string, msg, sig []byte) error {
	pub, ok := k[keyID]
	if !ok {
		return fmt.Errorf("%w: key %q is not trusted by this gateway", ErrBadSignature, keyID)
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("%w (key %q)", ErrBadSignature, keyID)
	}
	return nil
}
