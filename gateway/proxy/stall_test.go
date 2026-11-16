package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// stallingWriter is a caller whose connection takes the writes it is
// given until one holds match, which blocks as a client that stopped
// reading does: until a write deadline set on it passes (the connection's
// net.Conn would then fail the write), or, with none, for ever (here
// until free closes).
type stallingWriter struct {
	*httptest.ResponseRecorder
	match []byte

	mu       sync.Mutex
	deadline time.Time
	set      chan struct{} // a deadline was set
	free     chan struct{}
}

func newStallingWriter(match []byte) *stallingWriter {
	return &stallingWriter{ResponseRecorder: httptest.NewRecorder(), match: match,
		set: make(chan struct{}, 1), free: make(chan struct{})}
}

func (s *stallingWriter) SetWriteDeadline(d time.Time) error {
	s.mu.Lock()
	s.deadline = d
	s.mu.Unlock()
	select {
	case s.set <- struct{}{}:
	default:
	}
	return nil
}

func (s *stallingWriter) Write(b []byte) (int, error) {
	if !bytes.Contains(b, s.match) {
		return s.ResponseRecorder.Write(b)
	}
	for {
		s.mu.Lock()
		d := s.deadline
		s.mu.Unlock()
		if !d.IsZero() {
			time.Sleep(time.Until(d))
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-s.set:
		case <-s.free:
			return s.ResponseRecorder.Write(b)
		}
	}
}

// TestCutStreamsErrorEventBounded: the error event that ends a stream its
// provider cut is a write to the caller like any other, bounded by the
// stall: a caller that stopped reading cannot hold the handler (and its
// slot) after the provider's cut.
func TestCutStreamsErrorEventBounded(t *testing.T) {
	cut := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(io.MultiReader(bytes.NewReader(anthropicStream(1)[0]), failing{}))}, nil
	})
	h := handler(AnthropicMessages, cut, nil)
	h.WriteStall = 100 * time.Millisecond
	w := newStallingWriter([]byte("upstream.stream_failed"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { recover() }() // the cut response's abort
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest)))
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(w.free)
		<-done
		t.Fatal("the cut stream's error event blocked past the stall: its write had no deadline")
	}
}
