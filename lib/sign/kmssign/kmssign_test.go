package kmssign

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const arn = "arn:aws:kms:us-east-2:111122223333:key/0000-test"

// fakeKMS signs as KMS does for an ECC_NIST_P256 key: ASN.1 DER over the
// digest it is given, refusing anything but a digest under ECDSA_SHA_256.
type fakeKMS struct {
	key  *ecdsa.PrivateKey
	spec types.KeySpec
}

func (f fakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	if aws.ToString(in.KeyId) != arn || in.MessageType != types.MessageTypeDigest ||
		in.SigningAlgorithm != types.SigningAlgorithmSpecEcdsaSha256 || len(in.Message) != 32 {
		return nil, errors.New("ValidationException")
	}
	sig, err := ecdsa.SignASN1(rand.Reader, f.key, in.Message)
	return &kms.SignOutput{Signature: sig}, err
}

func (f fakeKMS) GetPublicKey(_ context.Context, in *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	return &kms.GetPublicKeyOutput{PublicKey: der, KeySpec: f.spec, KeyUsage: types.KeyUsageTypeSignVerify}, err
}

// TestKMSPointer: a pointer signed by the KMS key opens with its public
// key alone (the gateways' view); with another key, or tampered, it does
// not.
func TestKMSPointer(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := fakeKMS{key: k, spec: types.KeySpecEccNistP256}
	pub, err := PublicKey(t.Context(), f, arn)
	if err != nil {
		t.Fatal(err)
	}
	keys := sign.ECDSAKeys{arn: pub}
	b, err := snapshot.SignPointer(t.Context(), snapshot.Pointer{Pool: "internal", PublishSeq: 4, Hash: "ab"}, Signer{Client: f, Key: arn})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := snapshot.OpenPointer(b, keys); err != nil || p.PublishSeq != 4 {
		t.Fatalf("open %+v %v", p, err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := snapshot.OpenPointer(b, sign.ECDSAKeys{arn: &other.PublicKey}); !errors.Is(err, sign.ErrBadSignature) {
		t.Fatalf("another key: %v", err)
	}
	if _, err := snapshot.OpenPointer(b, sign.ECDSAKeys{"arn:other": pub}); !errors.Is(err, sign.ErrBadSignature) {
		t.Fatalf("an untrusted key ID: %v", err)
	}
}

// TestPublicKeySpec: only an ECC_NIST_P256 signing key is accepted.
func TestPublicKeySpec(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := PublicKey(t.Context(), fakeKMS{key: k, spec: types.KeySpecRsa2048}, arn); err == nil || !strings.Contains(err.Error(), "ECC_NIST_P256") {
		t.Fatalf("an RSA key: %v", err)
	}
	edPub, _, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKIXPublicKey(edPub)
	if _, err := sign.ParseECDSAPublicKey(der); err == nil {
		t.Fatal("an Ed25519 key parsed as ECDSA")
	}
}
