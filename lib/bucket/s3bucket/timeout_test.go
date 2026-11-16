package s3bucket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestResponseHeaderTimeout: Open's client gives up on a server that
// accepts a request and never answers, though the caller's context has no
// deadline: a poll or a state write is not held for good.
func TestResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // accepted, never answered
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits for the handlers
	c := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, Region: "us-east-2",
		Credentials:                credentials.NewStaticCredentialsProvider("AKIDTEST", "secret", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 httpClient(50 * time.Millisecond),
		RetryMaxAttempts:           1,
	})
	b := &Bucket{Client: c, Name: "golem-test"}
	done := make(chan error, 1)
	go func() { _, _, err := b.Get(context.Background(), "snapshots/internal/current", ""); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a get the server never answered succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the get waited on a server that never answers: no response-header timeout")
	}
}
