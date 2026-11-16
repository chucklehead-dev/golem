package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartInterruptible: a pool-mode gateway whose bucket accepts
// requests and never answers stops on its context (SIGTERM's) while it is
// still starting, rather than once its start's own deadline has passed.
func TestStartInterruptible(t *testing.T) {
	var asked atomic.Int64
	release := make(chan struct{})
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select { // accepted, never answered
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(s3.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits for the handlers
	t.Setenv("AWS_ENDPOINT_URL_S3", s3.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))

	f := newPoolFixture(t) // its keys and secrets; the bucket is the silent server
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-pool", "internal", "-bucket", "s3://golem-test/golem", "-keys", filepath.Join(f.root, "keys"),
			"-valkey", freeAddr(t), "-site", "test", "-secrets", filepath.Join(f.root, "secrets"),
			"-listen", freeAddr(t), "-telemetry", "off", "-drain", "1s", "-unready", "0", "-accept-unreleased"})
	}()
	for deadline := time.Now().Add(10 * time.Second); asked.Load() == 0; time.Sleep(10 * time.Millisecond) { // another goroutine's start
		if time.Now().After(deadline) {
			t.Fatal("the gateway never asked the bucket for its checkpoint")
		}
	}
	cancel()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("a start ended by its signal exited 0, as if it had served")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway's start, hung on the bucket, did not stop on its context")
	}
}
