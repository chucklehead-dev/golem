package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestAPIDrains: at shutdown the control API's call in flight finishes and
// its answer reaches the caller before serveAPI returns, after which the
// lease is given up; a call that outlasts the drain is cut.
func TestAPIDrains(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // a slow call: a MintKey whose write has landed
		io.WriteString(w, "the key")
	})
	srv := &http.Server{Handler: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	serving := make(chan struct{}) // Serve returned: Shutdown has begun
	done := make(chan error, 1)
	go func() {
		done <- serveAPI(ctx, srv, func() error { defer close(serving); return srv.Serve(ln) }, time.Minute)
	}()
	answer := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			answer <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		answer <- string(b)
	}()
	<-entered
	cancel() // SIGTERM
	<-serving
	// Serve has returned, as it does the moment Shutdown begins; serveAPI
	// must not, while the call is in flight. (A bounded wait for something
	// that must not happen: the drain itself is not timed.)
	select {
	case err := <-done:
		t.Fatalf("serveAPI returned (%v) with a call in flight: the lease would be released and the call cut", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("serveAPI after the drain: %v", err)
	}
	select {
	case a := <-answer:
		if a != "the key" {
			t.Fatalf("the call in flight: %q, want its answer", a)
		}
	default:
		t.Fatal("serveAPI returned before the call in flight was answered")
	}

	t.Run("a call outlasting the drain is cut", func(t *testing.T) {
		entered, never := make(chan struct{}), make(chan struct{})
		defer close(never)
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-never:
			case <-r.Context().Done(): // cut by Close
			}
		})}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- serveAPI(ctx, srv, func() error { return srv.Serve(ln) }, 10*time.Millisecond) }()
		failed := make(chan error, 1)
		go func() {
			resp, err := http.Get("http://" + ln.Addr().String() + "/")
			if err == nil {
				resp.Body.Close()
			}
			failed <- err
		}()
		<-entered
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("serveAPI after a drain that ran out: %v", err)
		}
		if err := <-failed; err == nil {
			t.Fatal("the call that outlasted the drain was answered")
		}
	})
}
