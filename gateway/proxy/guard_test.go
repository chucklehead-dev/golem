package proxy

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/guard"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestInvalidKeyFlood: a source sending invalid keys is refused with
// limit.source and a Retry-After once past its failures, without its
// credential checked or its body read, while a caller at another address
// with a valid key is served.
func TestInvalidKeyFlood(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	good := key.New(key.Live)
	ents := snapshot.Entitlements{
		Scopes:    []snapshot.Scope{{ID: "team-a", Models: []string{"claude"}}},
		KeyOwners: []snapshot.Owner{{ID: "alice", Kind: "user", Scopes: []string{"team-a"}}},
		Keys:      []snapshot.Key{{Hash: hashHex(good), Principal: "alice", PrincipalKind: "user", Scope: "team-a"}},
	}
	h := handler(AnthropicMessages, &stub{events: anthropicStream(1)}, nil)
	c, _ := h.Snapshots.Current()
	c.Auth = auth.Compile(ents, key.Live, func() time.Time { return now }, nil)
	h.Snapshots = gsnap.Static(c)
	h.Guard = &guard.Guard{FailRate: 1, FailBurst: 5, Now: func() time.Time { return now }}

	call := func(addr, k string, body *countingReader) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/messages", body)
		r.RemoteAddr = addr + ":1234"
		r.Header.Set("X-Api-Key", k)
		r.Header.Set("Anthropic-Version", "2023-06-01")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	bad := key.New(key.Live)
	for i := range 5 {
		if w := call("6.6.6.6", bad, &countingReader{r: bytes.NewReader(smallRequest)}); w.Code != 401 {
			t.Fatalf("failure %d: status %d, want 401 within the burst", i, w.Code)
		}
	}
	body := &countingReader{r: bytes.NewReader(smallRequest)}
	w := call("6.6.6.6", good, body)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "limit.source") || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("past the failures: %d %s (Retry-After %q), want 429 limit.source", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if body.n != 0 {
		t.Fatalf("read %d bytes of a refused source's body", body.n)
	}
	if w := call("10.1.1.1", good, &countingReader{r: bytes.NewReader(smallRequest)}); w.Code != 200 {
		t.Fatalf("another address with a valid key: %d %s", w.Code, w.Body)
	}
}

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
