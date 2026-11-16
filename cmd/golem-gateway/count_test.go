package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestCountTokensWiring: as golem-gateway wires it, a token count takes
// from the scope's rate limits (the limiter's meters) and is judged by
// the calls' shared health: a deployment the calls ejected takes no count.
func TestCountTokensWiring(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer up.Close()
	ka, kb := key.New(key.Dev), key.New(key.Dev)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(`apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, env: K}
  network: {allowPrivateAddresses: true}
  deployments: [{name: m, url: "`+up.URL+`/v1/messages", model: provider-model}]
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: anthropic, deployment: m}]}
---
apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], rateLimits: {member: {requestsPerMinute: 1}}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+ka+`, principal: alice, scope: team-a}, {key: `+kb+`, principal: bob, scope: team-a}]
`), 0o600)
	lim := limit.New(limit.NewMem())
	calls := &proxy.InFlight{}
	holder := newHolder(nil, lim, calls)
	holder.Env = key.Dev
	src := &snapshot.Dir{Path: dir, Holder: holder, Secrets: snapshot.EnvSecrets{Getenv: func(string) string { return "provider-key" }}}
	if err := src.Load(); err != nil {
		t.Fatal(err)
	}
	health := &route.Health{EjectAfter: 1}
	gw := httptest.NewServer(newMux(options{standalone: dir, maxBody: 1 << 20}, &lifecycle{drain: time.Second, cut: time.Second}, holder, lim, nil, calls, health))
	defer gw.Close()
	count := func(k string) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", gw.URL+"/v1/messages/count_tokens",
			strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", k)
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, b := count(ka); code != 200 {
		t.Fatalf("alice's count: %d %s", code, b)
	}
	if code, b := count(ka); code != 429 || !strings.Contains(b, "limit.rate") {
		t.Fatalf("alice's second count, past her rate: %d %s; want limit.rate", code, b)
	}
	c, _ := holder.Current()
	l, _ := health.Acquire(c.Routes.Lookup([]byte("claude")).Pool[0], "x")
	l.Fail(0) // the calls ejected it
	if code, b := count(kb); code != 503 || !strings.Contains(b, "upstream.unavailable") {
		t.Fatalf("a count on a deployment the calls ejected: %d %s; want upstream.unavailable", code, b)
	}
}

// TestReadingFlags: the bound on request bodies being read at once is
// on by default (512 MiB, 8 a principal), and 0 turns either off.
func TestReadingFlags(t *testing.T) {
	o, err := parse([]string{"-standalone", t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r := newReading(o); r.MaxBytes != 512<<20 || r.PerPrincipal != 8 {
		t.Fatalf("defaults %d bytes, %d a principal", r.MaxBytes, r.PerPrincipal)
	}
	o, _ = parse([]string{"-standalone", t.TempDir(), "-max-reading=0", "-max-reading-per-principal=0"})
	if r := newReading(o); r.MaxBytes >= 0 || r.PerPrincipal >= 0 {
		t.Fatalf("0: %d bytes, %d a principal; want no bound (negative)", r.MaxBytes, r.PerPrincipal)
	}
}
