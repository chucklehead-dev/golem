package proxy

import (
	"context"
	"encoding/json"
	"io"
	"maps"
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
	"github.com/chucklehead-dev/golem/lib/period"
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
	c.Routes.Lookup([]byte("claude")).Pool[0].Transport = s
	c.Routes.Lookup([]byte("haiku")).Pool[0].Transport = s
	return &Handler{Dialect: AnthropicMessages, Snapshots: h, Limiter: lim.Admitter(), Meters: lim, MaxBody: 1 << 20}, lim
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
	h, lim := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude, haiku], budgetMicros: 100000000, budgetMode: enforce, inFlight: {principal: 1}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
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
	// Known spend is the snapshot's (ADR 0031): the budget of 100,000,000
	// admits calls while it is below, and refuses them once a snapshot
	// says it is reached.
	now := period.Of(time.Now(), time.UTC)
	lim.ApplySnapshot(t.Context(), "team-a", limit.ScopeSpend{Period: now, Spend: 99_999_999})
	s.events = anthropicStream(1)
	if w := send(h, alice, stream); w.Code != 200 {
		t.Fatalf("below the budget: %d %s", w.Code, w.Body)
	}
	lim.ApplySnapshot(t.Context(), "team-a", limit.ScopeSpend{Period: now, Spend: 100_000_000})
	if w := send(h, alice, stream); w.Code != 402 || errorCode(t, w) != "budget.pool_exhausted" {
		t.Fatalf("after spending the budget: %d %s", w.Code, w.Body)
	}
}

func TestConcurrencyAndCancel(t *testing.T) {
	alice := key.New(key.Dev)
	release := make(chan struct{})
	slow := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		pr, pw := io.Pipe()
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
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude, haiku], inFlight: {principal: 1}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
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
	// The caller goes away: the call is cancelled, and its slot freed once
	// the upstream connection has closed.
	cancel()
	close(release)
	// Its slot is held for the provider's tail, then freed.
	if w := send(h, alice, body); w.Code != 429 {
		t.Fatalf("during the provider's tail the slot is held: %d %s", w.Code, w.Body)
	}
	time.Sleep(h.ProviderTail) // the tail's timer: real time
	var w *httptest.ResponseRecorder
	for range 100 { // and the handler's seeing the cancellation over a real connection
		if w = send(h, alice, body); w.Code != 429 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w.Code == 429 {
		t.Fatalf("after the provider's tail the slot is free: %d %s", w.Code, w.Body)
	}
}

// TestRepeatedCredential: a valid key followed by a second Authorization
// header is refused before authentication, not judged by its first copy;
// the key alone is admitted.
func TestRepeatedCredential(t *testing.T) {
	alice := key.New(key.Dev)
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], inFlight: {principal: 1}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys:
  - {key: `+alice+`, principal: alice, scope: team-a}
`, &stub{events: anthropicStream(1)})
	body := `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`
	for _, tc := range []struct {
		name   string
		header http.Header
		code   int
		err    string
	}{
		{"one key", http.Header{"Authorization": {"Bearer " + alice}}, 200, ""},
		{"the key, then another", http.Header{"Authorization": {"Bearer " + alice, "Bearer glm_dev_other"}}, 400, "request.invalid"},
		{"two keys joined", http.Header{"X-Api-Key": {alice + ", glm_dev_other"}}, 400, "request.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			maps.Copy(r.Header, tc.header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.code || (tc.err != "" && errorCode(t, w) != tc.err) {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body, tc.code, tc.err)
			}
		})
	}
}
