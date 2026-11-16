package entrasync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestFederatedTokenSingleFlight: callers while a token is being fetched
// wait for that one fetch, not on a lock held across it: one with a short
// deadline gives up at it, and the others share the one request.
func TestFederatedTokenSingleFlight(t *testing.T) {
	var asked atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		<-release // Entra slow to answer
		json.NewEncoder(w).Encode(map[string]any{"access_token": "graph-tok", "expires_in": 3600})
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("sa-token\n"), 0o600)
	f := &FederatedToken{Tenant: "t1", ClientID: "app", TokenFile: file, Authority: srv.URL}

	first := make(chan error, 1)
	go func() { _, err := f.Token(t.Context()); first <- err }()
	for deadline := time.Now().Add(5 * time.Second); asked.Load() == 0; time.Sleep(5 * time.Millisecond) { // the first fetch under way: another goroutine's
		if time.Now().After(deadline) {
			t.Fatal("the first fetch never reached Entra")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	short := make(chan error, 1)
	go func() { _, err := f.Token(ctx); short <- err }()
	select {
	case err := <-short:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller with a short deadline: %v, want its deadline", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a caller waited past its deadline on another's fetch: the lock is held across the request")
	}
	second := make(chan string, 1)
	go func() { tok, _ := f.Token(t.Context()); second <- tok }()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if tok := <-second; tok != "graph-tok" || asked.Load() != 1 {
		t.Fatalf("the waiting caller: %q, %d requests; want the one fetch's token", tok, asked.Load())
	}
}
