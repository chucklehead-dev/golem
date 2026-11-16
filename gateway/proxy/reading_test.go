package proxy

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/key"
)

// readingHandler is a standalone handler for alice and bob in a scope
// whose requests are at most maxInput bytes (0: no ceiling).
func readingHandler(t *testing.T, maxInput string) (*Handler, string, string) {
	t.Helper()
	alice, bob := key.New(key.Dev), key.New(key.Dev)
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], maxInputBytes: `+maxInput+`}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+alice+`, principal: alice, scope: team-a}, {key: `+bob+`, principal: bob, scope: team-a}]
`, &stub{events: anthropicStream(1)})
	return h, alice, bob
}

// stalled is a body that sends its first part, then nothing until done
// is closed: an upload held open.
type stalled struct {
	first []byte
	sent  chan struct{} // closed once the first part is read
	done  chan struct{}
	rest  io.Reader
}

func newStalled(first, rest string) *stalled {
	return &stalled{first: []byte(first), sent: make(chan struct{}), done: make(chan struct{}), rest: strings.NewReader(rest)}
}

func (s *stalled) Read(p []byte) (int, error) {
	if len(s.first) > 0 {
		n := copy(p, s.first)
		if s.first = s.first[n:]; len(s.first) == 0 {
			close(s.sent)
		}
		return n, nil
	}
	<-s.done
	return s.rest.Read(p)
}

// counted is a body that counts the bytes read of it.
type counted struct {
	r io.Reader
	n int
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// serveBody serves body as k's Messages call.
func serveBody(h *Handler, k string, body io.Reader, length int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/messages", body)
	r.ContentLength = length
	r.Header.Set("X-Api-Key", k)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const readingBody = `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`

// TestReadingClaimOverCeiling: a body whose claimed length is past the
// scope's input ceiling is refused before a byte of it is read.
func TestReadingClaimOverCeiling(t *testing.T) {
	h, alice, _ := readingHandler(t, "100")
	body := &counted{r: strings.NewReader(readingBody)} // under the ceiling, claiming more
	w := serveBody(h, alice, body, 31<<20)
	if w.Code != 400 || errorCode(t, w) != "request.over_ceiling" || body.n != 0 {
		t.Fatalf("%d %s (%d bytes read): want request.over_ceiling, the body unread", w.Code, w.Body, body.n)
	}
}

// TestReadingBounds: the gateway's bound on request-body bytes being read
// at once, and each principal's on its bodies, refuse limit.concurrency
// with a Retry-After of a second while an upload holds them, and free them
// when its reading ends.
func TestReadingBounds(t *testing.T) {
	t.Run("the gateway's bytes", func(t *testing.T) {
		h, alice, bob := readingHandler(t, "0")
		h.Reading = &Reading{MaxBytes: 100}
		held := newStalled(strings.Repeat(" ", 90), readingBody)
		done := make(chan int)
		go func() { done <- serveBody(h, alice, held, -1).Code }()
		<-held.sent
		w := serveBody(h, bob, strings.NewReader(readingBody), int64(len(readingBody)))
		if w.Code != 429 || errorCode(t, w) != "limit.concurrency" || w.Header().Get("Retry-After") != "1" ||
			!strings.Contains(w.Body.String(), "reading too many request bodies") {
			t.Fatalf("%d %s (Retry-After %q): want limit.concurrency, the gateway's bound", w.Code, w.Body, w.Header().Get("Retry-After"))
		}
		close(held.done)
		if code := <-done; code != 429 { // its own bytes past the bound too
			t.Fatalf("the held upload: %d", code)
		}
		if n := h.Reading.Bytes(); n != 0 {
			t.Fatalf("%d bytes still counted as being read", n)
		}
		if w := serveBody(h, bob, strings.NewReader(readingBody), int64(len(readingBody))); w.Code != 200 {
			t.Fatalf("%d %s: once the upload ended, a body fits", w.Code, w.Body)
		}
	})
	t.Run("a principal's bodies", func(t *testing.T) {
		h, alice, bob := readingHandler(t, "0")
		h.Reading = &Reading{PerPrincipal: 1}
		held := newStalled(`{"model"`, `:"claude","max_tokens":10,"stream":true,"messages":[]}`)
		done := make(chan int)
		go func() { done <- serveBody(h, alice, held, -1).Code }()
		<-held.sent
		w := serveBody(h, alice, strings.NewReader(readingBody), int64(len(readingBody)))
		if w.Code != 429 || errorCode(t, w) != "limit.concurrency" || !strings.Contains(w.Body.String(), "your request bodies") {
			t.Fatalf("%d %s: want limit.concurrency, alice's own bound", w.Code, w.Body)
		}
		if w := serveBody(h, bob, strings.NewReader(readingBody), int64(len(readingBody))); w.Code != 200 {
			t.Fatalf("%d %s: bob is not held to alice's uploads", w.Code, w.Body)
		}
		close(held.done)
		select {
		case code := <-done:
			if code != 200 {
				t.Fatalf("alice's held upload: %d", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the held upload did not end")
		}
		if w := serveBody(h, alice, strings.NewReader(readingBody), int64(len(readingBody))); w.Code != 200 {
			t.Fatalf("%d %s: once her upload ended, alice's next is read", w.Code, w.Body)
		}
	})
}
