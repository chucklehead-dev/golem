package s3bucket

import (
	"crypto/ed25519"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/lib/bucket/buckettest"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// against returns a Bucket on the real S3 client, speaking HTTP to fake.
func against(t *testing.T, fake *fakeS3) *Bucket {
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, Region: "us-east-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "secret", ""),
		// Plain HTTP to the fake: no streaming trailer checksums.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Bucket{Client: c, Name: "golem-test", Prefix: "golem/"}
}

// TestS3Conformance: the S3 adapter, through the real SDK, keeps the
// conditional semantics the publisher relies on.
func TestS3Conformance(t *testing.T) {
	buckettest.Run(t, against(t, &fakeS3{objects: map[string][]byte{}}))
}

// TestS3Publish: a publication through S3, read back by its pointer.
func TestS3Publish(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	b := against(t, fake)
	pk, sk, _ := ed25519.GenerateKey(nil)
	p := &publish.Publisher{Bucket: b, Signer: sign.Ed25519{ID: "k", Key: sk}, Pool: "internal"}
	for range 2 {
		if _, err := p.Publish(t.Context(), 1, 1, func(seq uint64) (*snapshot.Snapshot, error) {
			return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, ValidUntil: time.Now().Add(time.Hour).UTC()}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, err := b.Get(t.Context(), snapshot.PointerKey("internal"), "")
	if err != nil {
		t.Fatal(err)
	}
	ptr, err := snapshot.OpenPointer(raw, sign.Ed25519Keys{"k": pk})
	if err != nil || ptr.PublishSeq != 2 {
		t.Fatalf("pointer %+v %v", ptr, err)
	}
	if _, ok := fake.objects["golem-test/golem/"+snapshot.ObjectKey("internal", ptr.Hash)]; !ok {
		t.Fatalf("objects %v: the snapshot under the bucket and prefix", keys(fake.objects))
	}
}

// TestMaxObject: an object over the bound is refused, not buffered.
func TestMaxObject(t *testing.T) {
	b := against(t, &fakeS3{objects: map[string][]byte{"golem-test/golem/big": make([]byte, 100)}})
	b.MaxObject = 99
	if _, _, err := b.Get(t.Context(), "big", ""); err == nil {
		t.Fatal("an object over MaxObject was read")
	}
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
