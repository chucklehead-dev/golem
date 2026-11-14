package sign

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// ECDSAKeys is a Verifier of ECDSA P-256 signatures over SHA-256 (ASN.1
// DER, as KMS's ECDSA_SHA_256 produces), by key ID: the KMS key's ARN.
// Gateways verify with the public key alone, so a poll never calls KMS.
type ECDSAKeys map[string]*ecdsa.PublicKey

// Verify implements Verifier.
func (k ECDSAKeys) Verify(keyID string, msg, sig []byte) error {
	pub, ok := k[keyID]
	if !ok {
		return untrusted(keyID)
	}
	d := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(pub, d[:], sig) {
		return fmt.Errorf("%w (key %q)", ErrBadSignature, keyID)
	}
	return nil
}

// ParseECDSAPublicKey reads a P-256 public key as KMS's GetPublicKey gives
// it (DER SubjectPublicKeyInfo) or PEM-encoded ("PUBLIC KEY").
func ParseECDSAPublicKey(b []byte) (*ecdsa.PublicKey, error) {
	if blk, _ := pem.Decode(b); blk != nil {
		if blk.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("sign: a PEM %q block, want PUBLIC KEY", blk.Type)
		}
		b = blk.Bytes
	}
	k, err := x509.ParsePKIXPublicKey(b)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || !isP256(pub) {
		return nil, errors.New("sign: not an ECDSA P-256 public key (the KMS key spec must be ECC_NIST_P256)")
	}
	return pub, nil
}

// isP256 reports whether pub is on the one curve golem verifies ECDSA on
// (KMS's ECC_NIST_P256).
func isP256(pub *ecdsa.PublicKey) bool { return pub.Curve.Params().Name == "P-256" }
