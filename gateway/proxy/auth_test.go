package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

// standaloneHandler compiles a real LocalIdentity config (through the same
// path as golem-gateway --standalone) over an in-memory provider.
func standaloneHandler(t testing.TB, identity string, s http.RoundTripper) (*Handler, *limit.Limiter) {
	t.Helper()
	dir := t.TempDir()
	cfg := `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, env: K}
  deployments:
  - {name: sonnet, url: "https://provider.example/v1/messages", model: claude-sonnet}
  - {name: haiku, url: "https://provider.example/v1/messages", model: claude-haiku}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: anthropic, deployment: sonnet}]}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: haiku}
spec: {alias: haiku, pool: [{backend: anthropic, deployment: haiku}]}
---
apiVersion: golem.dev/v1alpha1
kind: PriceList
metadata: {name: p}
spec:
  currency: USD
  prices:
  - {model: claude-sonnet, inputPerMTok: 3000000000, outputPerMTok: 15000000000}
  - {model: claude-haiku, inputPerMTok: 1000000, outputPerMTok: 5000000}
---
` + identity
	os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(cfg), 0o600)
	lim := limit.New(limit.NewMem())
	h := &gsnap.Holder{Env: key.Dev}
	d := &gsnap.Dir{Path: dir, Holder: h, Secrets: gsnap.EnvSecrets{Getenv: func(string) string { return "provider-key" }}}
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
	c, _ := h.Current()
	c.Routes.Lookup([]byte("claude")).Pick().Transport = s
	c.Routes.Lookup([]byte("haiku")).Pick().Transport = s
	return &Handler{Dialect: AnthropicMessages, Snapshots: h, Limiter: lim, MaxBody: 1 << 20}, lim
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	var e struct{ Error struct{ Type string } }
	json.Unmarshal(w.Body.Bytes(), &e)
	return e.Error.Type
}

func send(h http.Handler, apiKey, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	if apiKey != "" {
		r.Header.Set("X-Api-Key", apiKey)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthAndLimits(t *testing.T) {
	alice, bob := key.New(key.Dev), key.New(key.Dev)
	s := &stub{events: anthropicStream(1)} // 2048 input tokens, 4 output
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes:
  - {name: team-a, models: [claude, haiku], budgetMicros: 100000000, inFlight: {principal: 1}}
  keys:
  - {key: `+alice+`, principal: alice, scope: team-a}
  - {key: `+bob+`, principal: bob, scope: team-a, models: [haiku]}
`, s)
	stream := `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`

	if w := send(h, "", stream); w.Code != 401 || errorCode(t, w) != "auth.missing" {
		t.Fatalf("no key: %d %s", w.Code, w.Body)
	}
	if w := send(h, key.New(key.Live), stream); w.Code != 401 || errorCode(t, w) != "auth.key_wrong_installation" {
		t.Fatalf("a live key standalone: %d %s", w.Code, w.Body)
	}
	if w := send(h, bob, stream); w.Code != 403 || errorCode(t, w) != "authz.model_not_allowed" {
		t.Fatalf("bob's key is narrowed to haiku: %d %s", w.Code, w.Body)
	}
	s.events = anthropicStream(1)
	if w := send(h, alice, stream); w.Code != 200 {
		t.Fatalf("alice: %d %s", w.Code, w.Body)
	}
	// Each call is 2,048 input tokens at 3,000,000,000 micro-units per
	// million (an exaggerated price) plus 4 output tokens: 6,204,000
	// micro-units. The budget of 100,000,000 admits calls while known spend
	// is below it: the 17th still is (at 99,264,000 spent), the 18th not.
	for range 16 {
		s.events = anthropicStream(1)
		send(h, alice, stream)
	}
	if w := send(h, alice, stream); w.Code != 402 || errorCode(t, w) != "budget.pool_exhausted" {
		t.Fatalf("after spending the budget: %d %s", w.Code, w.Body)
	}
}

func TestConcurrencyAndCancel(t *testing.T) {
	alice := key.New(key.Dev)
	release := make(chan struct{})
	slow := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		pr, pw := ioPipe()
		go func() {
			pw.Write(anthropicStream(0)[0])
			select {
			case <-release:
			case <-r.Context().Done():
			}
			pw.Close()
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
	})
	h, lim := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes:
  - {name: team-a, models: [claude, haiku], inFlight: {principal: 1}}
  keys:
  - {key: `+alice+`, principal: alice, scope: team-a}
`, slow)
	h.ProviderTail = 100 * time.Millisecond
	gw := httptest.NewServer(h)
	defer gw.Close()
	body := `{"model":"haiku","max_tokens":10,"stream":true,"messages":[]}`

	// The first call streams; the second is refused while it does.
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL, strings.NewReader(body))
	req.Header.Set("X-Api-Key", alice)
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	resp.Body.Read(buf)
	if w := send(h, alice, body); w.Code != 429 || errorCode(t, w) != "limit.concurrency" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("a second call in flight: %d %s", w.Code, w.Body)
	}
	// The caller goes away: the call is cancelled, charged what was seen,
	// and its slot freed once the upstream connection has closed.
	cancel()
	for range 200 {
		if lim.Known(t.Context(), "team-a") > 0 {
			break
		}
		syncWait()
	}
	if got := lim.Known(t.Context(), "team-a"); got == 0 {
		t.Fatal("the cancelled call's usage so far was not charged")
	}
	close(release)
	// Its slot is held for the provider's tail (golem#4), then freed.
	if w := send(h, alice, body); w.Code != 429 {
		t.Fatalf("during the provider's tail the slot is held: %d %s", w.Code, w.Body)
	}
	time.Sleep(h.ProviderTail + 50*time.Millisecond) // the tail's timer: real time
	if w := send(h, alice, body); w.Code == 429 {
		t.Fatalf("after the provider's tail the slot is free: %d %s", w.Code, w.Body)
	}
}
