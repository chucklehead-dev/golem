package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestSDKsSeeTermination: a stream the gateway ends because its key was
// revoked mid-way surfaces in each SDK as an error, after the text that
// came before it, carrying golem's code: never as a short, whole answer.
func TestSDKsSeeTermination(t *testing.T) {
	cs := clients(t)
	arrived := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == "/v1/messages" {
			io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"provider-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}`+"\n\n")
			io.WriteString(w, "event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
			io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`+"\n\n")
		} else {
			io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"provider-model","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}],"usage":null}`+"\n\n")
		}
		w.(http.Flusher).Flush()
		arrived <- struct{}{}
		<-r.Context().Done() // the rest never comes: the gateway ends the call
	}))
	defer up.Close()

	dir := t.TempDir()
	k := key.New(key.Dev)
	write := func(revoked bool) {
		rv := ""
		if revoked {
			rv = ", revoked: true"
		}
		cfg := strings.ReplaceAll(`apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, env: K}
  deployments: [{name: m, url: "URL/v1/messages", model: provider-model}]
  network: {allowPrivateAddresses: true}
---
apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: openai}
spec:
  dialect: openai.chat
  auth: {header: authorization, scheme: Bearer, env: K}
  deployments: [{name: m, url: "URL/v1/chat/completions", model: provider-model}]
  network: {allowPrivateAddresses: true}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: anthropic, deployment: m}]}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: gpt}
spec: {alias: gpt, pool: [{backend: openai, deployment: m}]}
---
apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude, gpt]}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: KEY, principal: alice, scope: team-a`+rv+`}]
`, "URL", up.URL)
		os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(strings.ReplaceAll(cfg, "KEY", k)), 0o600)
	}
	write(false)
	calls := &proxy.InFlight{}
	h := &snapshot.Holder{Env: key.Dev, OnApply: func(*snapshot.Compiled) { calls.Sweep() }}
	calls.Keys = func() *auth.Keys {
		if c := h.Applied(); c != nil {
			return c.Auth
		}
		return nil
	}
	d := &snapshot.Dir{Path: dir, Holder: h, Secrets: snapshot.EnvSecrets{Getenv: func(string) string { return "provider-key" }}}
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
	lim := limit.New(limit.NewMem())
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", &proxy.Handler{Dialect: proxy.AnthropicMessages, Snapshots: h, Limiter: lim.Admitter(), MaxBody: 1 << 20, InFlight: calls})
	mux.Handle("POST /v1/chat/completions", &proxy.Handler{Dialect: proxy.OpenAIChat, Snapshots: h, Limiter: lim.Admitter(), MaxBody: 1 << 20, InFlight: calls})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	var report strings.Builder
	for _, c := range cs {
		for _, kase := range []string{"anthropic_stream_terminated", "openai_stream_terminated"} {
			t.Run(c.name+"/"+kase, func(t *testing.T) {
				write(false)
				if err := d.Load(); err != nil {
					t.Fatal(err)
				}
				cmd := c.cmd(kase, gw.URL)
				cmd.Env = append(os.Environ(), "ANTHROPIC_LOG=", "OPENAI_LOG=", "GOLEM_KEY="+k)
				type result struct {
					out []byte
					err error
				}
				done := make(chan result, 1)
				go func() { out, err := cmd.Output(); done <- result{out, err} }()
				select {
				case <-arrived:
				case r := <-done:
					t.Fatalf("the client ended before its stream began: %v %s", r.err, r.out)
				case <-time.After(30 * time.Second):
					t.Fatal("the client's stream did not reach the provider")
				}
				write(true)
				if err := d.Load(); err != nil {
					t.Fatal(err)
				}
				var r result
				select {
				case r = <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("the client did not see the termination")
				}
				var got map[string]any
				if err := json.Unmarshal(r.out, &got); err != nil {
					t.Fatalf("%v: the client printed %q", r.err, r.out)
				}
				fmt.Fprintf(&report, "  %s/%s: %v\n", c.name, kase, got)
				msg, _ := got["message"].(string)
				if got["text"] != "Hello " || got["error_class"] == nil || !strings.Contains(msg, "revoked") {
					t.Fatalf("the client saw %v; want the text before the end, then an error naming the revocation", got)
				}
				if got["code"] != "auth.key_revoked" {
					t.Fatalf("the client saw %v; want golem's code auth.key_revoked on the error", got)
				}
			})
		}
	}
	t.Logf("what each SDK raises on a terminated stream:\n%s", report.String())
}
