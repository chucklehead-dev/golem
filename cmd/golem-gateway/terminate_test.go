package main

import (
	"bytes"
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

// TestWiringEndsWithdrawnCalls: as golem-gateway wires them, a snapshot
// revoking a key ends that key's stream in flight with the dialect's error
// event, and a denylist entry another key's; a third key's stream runs on.
func TestWiringEndsWithdrawnCalls(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
			w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	ka, kb, kc := key.New(key.Dev), key.New(key.Dev), key.New(key.Dev)
	dir := t.TempDir()
	config := func(revoked bool) {
		rv := ""
		if revoked {
			rv = ", revoked: true"
		}
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
spec: {models: [claude]}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys:
  - {key: `+ka+`, principal: alice, scope: team-a`+rv+`}
  - {key: `+kb+`, principal: bob, scope: team-a}
  - {key: `+kc+`, principal: carol, scope: team-a}
`), 0o600)
	}
	config(false)

	lim := limit.New(limit.NewMem())
	calls := &proxy.InFlight{}
	holder := newHolder(nil, lim, calls)
	holder.Env = key.Dev
	denyPath := filepath.Join(t.TempDir(), "denylist")
	os.WriteFile(denyPath, nil, 0o600)
	deny := newDenyFile(denyPath, calls)
	if err := deny.Load(); err != nil {
		t.Fatal(err)
	}
	holder.Deny = deny
	src := &snapshot.Dir{Path: dir, Holder: holder, Secrets: snapshot.EnvSecrets{Getenv: func(string) string { return "provider-key" }}}
	if err := src.Load(); err != nil {
		t.Fatal(err)
	}
	o := options{standalone: dir, maxBody: 1 << 20}
	lc := &lifecycle{drain: time.Second, cut: time.Second}
	gw := httptest.NewServer(newMux(o, lc, holder, lim, nil, calls, &route.Health{}))
	defer gw.Close()

	stream := func(k string) *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", gw.URL+"/v1/messages",
			strings.NewReader(`{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", k)
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
		resp.Body.Read(make([]byte, 16)) // under way
		return resp
	}
	ended := func(resp *http.Response, code string) {
		t.Helper()
		done := make(chan []byte, 1)
		go func() { b, _ := io.ReadAll(resp.Body); done <- b }()
		select {
		case b := <-done:
			if !bytes.Contains(b, []byte("event: error\ndata: ")) || !bytes.Contains(b, []byte(`"type":"`+code+`"`)) {
				t.Fatalf("the stream ended with %q; want the error event with %s", b, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the withdrawn call's stream did not end")
		}
	}
	a, b, c := stream(ka), stream(kb), stream(kc)
	defer a.Body.Close()
	defer b.Body.Close()
	defer c.Body.Close()

	config(true)
	if err := src.Load(); err != nil {
		t.Fatal(err)
	}
	ended(a, "auth.key_revoked")
	os.WriteFile(denyPath, []byte("principal bob\n"), 0o600)
	if err := deny.Load(); err != nil {
		t.Fatal(err)
	}
	ended(b, "auth.denied")
	if n := calls.Len(); n != 1 {
		t.Fatalf("%d calls in flight, want carol's alone", n)
	}
	// Closed, not sent: an ended call's upstream handler may not yet have
	// seen its connection close, and must not take carol's release.
	close(release)
	rest, err := io.ReadAll(c.Body)
	if err != nil || !bytes.Contains(rest, []byte("message_stop")) || bytes.Contains(rest, []byte("event: error")) {
		t.Fatalf("carol's stream: %v %q", err, rest)
	}
}
