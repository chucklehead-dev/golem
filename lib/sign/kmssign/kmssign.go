// Package kmssign signs with an AWS KMS asymmetric key (ECC_NIST_P256,
// ECDSA_SHA_256): golem-control's publishing key never leaves KMS
// (control-plane.md §4, §5). Gateways verify with sign.ECDSAKeys and the
// key's public key (PublicKey), never calling KMS.
package kmssign

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/chucklehead-dev/golem/lib/sign"
)

// API is the part of the KMS client a signer uses.
type API interface {
	Sign(ctx context.Context, in *kms.SignInput, opts ...func(*kms.Options)) (*kms.SignOutput, error)
}

// Signer signs with one KMS key. Key is the key's ARN: it names the key in
// what is signed, so gateways must trust the same string.
type Signer struct {
	Client API
	Key    string
}

func (s Signer) KeyID() string { return s.Key }

// Sign signs SHA-256(msg) as a digest: KMS caps a raw message at 4 KB, and
// the digest is what ECDSA_SHA_256 signs either way.
func (s Signer) Sign(ctx context.Context, msg []byte) ([]byte, error) {
	d := sha256.Sum256(msg)
	out, err := s.Client.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.Key),
		Message:          d[:],
		MessageType:      types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("kms sign with %s: %w", s.Key, err)
	}
	return out.Signature, nil
}

// PublicKeyAPI is the part of the KMS client PublicKey uses.
type PublicKeyAPI interface {
	GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, opts ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
}

// PublicKey fetches a key's public half, to give gateways (golemctl, at
// bootstrap): it refuses a key that is not ECC_NIST_P256 for signing.
func PublicKey(ctx context.Context, c PublicKeyAPI, keyID string) (*ecdsa.PublicKey, error) {
	out, err := c.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("kms public key %s: %w", keyID, err)
	}
	if out.KeySpec != types.KeySpecEccNistP256 || out.KeyUsage != types.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("kms key %s is %s for %s; the publishing key must be ECC_NIST_P256 for SIGN_VERIFY", keyID, out.KeySpec, out.KeyUsage)
	}
	return sign.ParseECDSAPublicKey(out.PublicKey)
}

var _ sign.Signer = Signer{}
