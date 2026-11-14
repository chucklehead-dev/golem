package sign

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keys is a Verifier over keys of both kinds golem signs with: Ed25519
// (development, a file key) and ECDSA P-256 (KMS), by key ID.
type Keys struct {
	Ed25519 Ed25519Keys
	ECDSA   ECDSAKeys
}

// Verify implements Verifier, with whichever kind holds keyID.
func (k Keys) Verify(keyID string, msg, sig []byte) error {
	if _, ok := k.Ed25519[keyID]; ok {
		return k.Ed25519.Verify(keyID, msg, sig)
	}
	if _, ok := k.ECDSA[keyID]; ok {
		return k.ECDSA.Verify(keyID, msg, sig)
	}
	return untrusted(keyID)
}

// LoadKeys reads the publishing keys a gateway trusts from dir: each
// *.pub file a PEM PUBLIC KEY (golem-control keygen's, or a KMS key's
// public half), its key ID the PEM's Key-ID header if it has one (a KMS
// key's ARN), else the file's name without .pub. A key is rotated by adding
// the new one beside the old, then removing the old.
//
// A key ID given by two files is refused, naming both: one would silently
// replace the other, or, an Ed25519 and an ECDSA key under one ID, Verify
// would try only the Ed25519 one, so a misplaced file could shadow a
// trusted key.
func LoadKeys(dir string) (Keys, error) {
	k := Keys{Ed25519: Ed25519Keys{}, ECDSA: ECDSAKeys{}}
	from := map[string]string{} // key ID to the file that gave it
	paths, err := filepath.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return k, err
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return k, err
		}
		blk, _ := pem.Decode(raw)
		if blk == nil || blk.Type != "PUBLIC KEY" {
			return k, fmt.Errorf("%s: not a PEM PUBLIC KEY", p)
		}
		id := blk.Headers["Key-ID"]
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(p), ".pub")
		}
		if other, dup := from[id]; dup {
			return k, fmt.Errorf("%s and %s both give key ID %q: remove the misplaced one, or give each its own Key-ID header or file name", other, p, id)
		}
		from[id] = p
		pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return k, fmt.Errorf("%s: %w", p, err)
		}
		switch pub := pub.(type) {
		case ed25519.PublicKey:
			k.Ed25519[id] = pub
		case *ecdsa.PublicKey:
			if !isP256(pub) {
				return k, fmt.Errorf("%s: an ECDSA key not on P-256", p)
			}
			k.ECDSA[id] = pub
		default:
			return k, fmt.Errorf("%s: a %T, want Ed25519 or ECDSA P-256", p, pub)
		}
	}
	if len(k.Ed25519)+len(k.ECDSA) == 0 {
		return k, errors.New("no publishing keys (*.pub) in " + dir + ": a gateway trusts no snapshot without one")
	}
	return k, nil
}
