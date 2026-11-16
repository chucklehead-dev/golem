// Package compat runs the official SDKs (Python and TypeScript, Anthropic
// and OpenAI) against the gateway, with a stub provider behind it: wire
// compatibility without provider credentials (contracts.md), and the
// SDKs' retry rules by status code (the roadmap's SDK retry spike).
//
// It needs python3 with test/compat/clients/requirements.txt and node with
// test/compat/clients/package.json installed; set GOLEM_COMPAT_PYTHON and
// GOLEM_COMPAT_NODE_MODULES to use them (CI does). Otherwise it skips.
package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

// testKey is the dev key the SDKs authenticate with: Anthropic's send it as
// x-api-key, OpenAI's as Authorization: Bearer.
var testKey = key.New(key.Dev)

// provider is a stub that answers as each API documents, checking golem's
// credential and the rewritten model.
func provider(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model         string
			Stream        bool
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("X-Api-Key") != "provider-key" && r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("the provider got no golem credential: %v", r.Header)
		}
		f := w.(http.Flusher)
		switch r.URL.Path {
		case "/v1/messages":
			if req.Model != "claude-sonnet-4-5" {
				t.Errorf("anthropic model %q", req.Model)
			}
			if !req.Stream {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Hello from the stub"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":5}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, e := range []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}`,
				`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`event: ping` + "\n" + `data: {"type":"ping"}`,
				`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from "}}`,
				`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the stub"}}`,
				`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
				`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			} {
				io.WriteString(w, e+"\n\n")
				f.Flush()
			}
		case "/v1/chat/completions":
			if req.Model != "gpt-5" {
				t.Errorf("openai model %q", req.Model)
			}
			if !req.Stream {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from the stub"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`)
				return
			}
			usage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
			if !usage {
				t.Error("golem sent a stream without include_usage")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, d := range []string{"Hello from ", "the stub"} {
				fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}],"usage":null}`+"\n\n", d)
				f.Flush()
			}
			io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`+"\n\n")
			io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
}

// gateway compiles a standalone config pointing at the provider and serves
// both frontends, as golem-gateway --standalone does.
func gateway(t *testing.T, providerURL string) *httptest.Server {
	dir := t.TempDir()
	cfg := strings.ReplaceAll(`apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, env: KEY}
  deployments: [{name: sonnet, url: "URL/v1/messages", model: claude-sonnet-4-5}]
  network: {allowPrivateAddresses: true} # the stand-in provider is on loopback
---
apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: openai}
spec:
  dialect: openai.chat
  auth: {header: authorization, scheme: Bearer, env: KEY}
  deployments: [{name: gpt, url: "URL/v1/chat/completions", model: gpt-5}]
  network: {allowPrivateAddresses: true}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: anthropic, deployment: sonnet}]}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: gpt}
spec: {alias: gpt, pool: [{backend: openai, deployment: gpt}]}
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
  keys: [{key: KEY, principal: alice, scope: team-a}]
`, "URL", providerURL)
	cfg = strings.ReplaceAll(cfg, "KEY", testKey)
	os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(cfg), 0o600)
	h := &snapshot.Holder{Env: key.Dev}
	d := &snapshot.Dir{Path: dir, Holder: h, Secrets: snapshot.EnvSecrets{Getenv: func(string) string { return "provider-key" }}}
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	lim := limit.New(limit.NewMem())
	mux.Handle("POST /v1/messages", &proxy.Handler{Dialect: proxy.AnthropicMessages, Snapshots: h, Limiter: lim.Admitter(), MaxBody: 1 << 20})
	mux.Handle("POST /v1/chat/completions", &proxy.Handler{Dialect: proxy.OpenAIChat, Snapshots: h, Limiter: lim.Admitter(), MaxBody: 1 << 20})
	return httptest.NewServer(mux)
}

type client struct {
	name string
	cmd  func(kase, base string) *exec.Cmd
}

func clients(t *testing.T) []client {
	var cs []client
	if py := os.Getenv("GOLEM_COMPAT_PYTHON"); py != "" {
		cs = append(cs, client{"python", func(k, b string) *exec.Cmd {
			return exec.Command(py, "clients/sdk.py", k, b)
		}})
	}
	if nm := os.Getenv("GOLEM_COMPAT_NODE_MODULES"); nm != "" {
		cs = append(cs, client{"node", func(k, b string) *exec.Cmd {
			// Node resolves packages from the script's directory upwards: link
			// the installed node_modules beside a copy of the script.
			dir := t.TempDir()
			src, _ := os.ReadFile("clients/sdk.mjs")
			os.WriteFile(filepath.Join(dir, "sdk.mjs"), src, 0o600)
			os.Symlink(nm, filepath.Join(dir, "node_modules"))
			return exec.Command("node", filepath.Join(dir, "sdk.mjs"), k, b)
		}})
	}
	if len(cs) == 0 {
		t.Skip("no SDK clients: set GOLEM_COMPAT_PYTHON and/or GOLEM_COMPAT_NODE_MODULES")
	}
	return cs
}

func (c client) run(t *testing.T, kase, base string) map[string]any {
	t.Helper()
	cmd := c.cmd(kase, base)
	cmd.Env = append(os.Environ(), "ANTHROPIC_LOG=", "OPENAI_LOG=", "GOLEM_KEY="+testKey)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", c.name, kase, err, out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%s %s printed %q", c.name, kase, out)
	}
	return got
}

func TestSDKs(t *testing.T) {
	cs := clients(t)
	up := provider(t)
	defer up.Close()
	gw := gateway(t, up.URL)
	defer gw.Close()

	for _, c := range cs {
		for _, kase := range []string{"anthropic_create", "anthropic_stream", "openai_create", "openai_stream", "openai_stream_usage"} {
			t.Run(c.name+"/"+kase, func(t *testing.T) {
				got := c.run(t, kase, gw.URL)
				if got["text"] != "Hello from the stub" {
					t.Fatalf("the client saw %v", got)
				}
				if in, ok := got["input"]; ok && (in != 12.0 || got["output"] != 5.0) {
					t.Fatalf("usage %v", got)
				}
			})
		}
		t.Run(c.name+"/wrong_key", func(t *testing.T) {
			cmd := c.cmd("anthropic_unknown_model", gw.URL)
			cmd.Env = append(os.Environ(), "GOLEM_KEY="+key.New(key.Dev))
			out, _ := cmd.Output()
			if !strings.Contains(string(out), "auth.key_unknown") || !strings.Contains(string(out), "401") {
				t.Fatalf("an unknown key: the client saw %s", out)
			}
		})
		for _, kase := range []string{"anthropic_unknown_model", "openai_unknown_model"} {
			t.Run(c.name+"/"+kase, func(t *testing.T) {
				got := c.run(t, kase, gw.URL)
				msg, _ := got["message"].(string)
				if got["status"] != 404.0 || !strings.Contains(msg, "ask the scope's managers") {
					t.Fatalf("the client saw %v; want a 404 whose message says what to do", got)
				}
				if kase == "openai_unknown_model" && got["code"] != "route.unknown_model" {
					t.Fatalf("the OpenAI error code %v, want golem's", got["code"])
				}
			})
		}
	}
}

// TestRetryRules records how many attempts each SDK makes per status, with
// two retries allowed, and checks the rules contracts.md relies on: a
// refusal golem means as final (400, 401, 402, 403, 404) is not retried;
// 429 and 503 are.
func TestRetryRules(t *testing.T) {
	cs := clients(t)
	var hits atomic.Int64
	status := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "10") // honoured by both SDKs: keeps the test fast
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(status)
		io.WriteString(w, `{"type":"error","error":{"type":"x","message":"m"}}`)
	}))
	defer srv.Close()

	final := map[int]bool{400: true, 401: true, 402: true, 403: true, 404: true, 413: true, 422: true}
	retried := map[int]bool{408: true, 409: true, 429: true, 500: true, 502: true, 503: true, 504: true}
	var report strings.Builder
	for _, c := range cs {
		for _, sdk := range []string{"anthropic", "openai"} {
			fmt.Fprintf(&report, "%s/%s:", c.name, sdk)
			for _, code := range []int{400, 401, 402, 403, 404, 408, 409, 413, 422, 429, 500, 502, 503, 504} {
				status = code
				hits.Store(0)
				got := c.run(t, "retries_"+sdk, srv.URL)
				n := hits.Load()
				fmt.Fprintf(&report, " %d×%d", code, n)
				if got["status"] != float64(code) {
					t.Errorf("%s/%s %d: the client saw %v", c.name, sdk, code, got)
				}
				if final[code] && n != 1 {
					t.Errorf("%s/%s retried a %d (%d attempts): golem's final refusals must not use it", c.name, sdk, code, n)
				}
				if retried[code] && n != 3 {
					t.Errorf("%s/%s made %d attempts on a %d; contracts.md §1 assumes it retries", c.name, sdk, n, code)
				}
			}
			report.WriteString("\n")
		}
	}
	t.Logf("attempts per status (two retries allowed):\n%s", report.String())
}
