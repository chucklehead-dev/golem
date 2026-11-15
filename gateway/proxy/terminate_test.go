package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
)

// terminateBound is how long a test waits for a withdrawn call to end:
// the sweep is synchronous with the apply, so this is slack for a loaded
// machine, not the mechanism's latency (which each test logs).
const terminateBound = 5 * time.Second

// tgw is a gateway whose snapshots are applied from a directory (as
// standalone's), each apply and each denylist change sweeping its calls in
// flight, as golem-gateway wires them; every call's ledger row, ticket end
// and result recorded.
type tgw struct {
	t       *testing.T
	d       Dialect
	dir     string
	src     *gsnap.Dir
	deny    *auth.DenyFile
	calls   *InFlight
	srv     *httptest.Server
	rows    lockedBuffer
	ends    ends
	results chan Result
	// provider: each call's upstream request is announced, and the
	// provider answers when told (release) or when its context ends.
	upstream chan *http.Request
	release  chan struct{}
	whole    bool // the provider answers a whole response, not a stream
	// headersFirst: the whole response's headers and its first bytes at
	// once, the rest when released.
	headersFirst bool
	keyA         string
	keyB         string
}

func (g *tgw) config(aliceRevoked bool) string {
	be := `{name: anthropic}
spec:
  dialect: anthropic.messages`
	if g.d == OpenAIChat {
		be = `{name: openai}
spec:
  dialect: openai.chat`
	}
	backend := "anthropic"
	if g.d == OpenAIChat {
		backend = "openai"
	}
	revoked := ""
	if aliceRevoked {
		revoked = ", revoked: true"
	}
	return `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: ` + be + `
  auth: {header: x-api-key, env: K}
  deployments:
  - {name: m, url: "https://provider.example/v1", model: provider-model}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: ` + backend + `, deployment: m}]}
---
apiVersion: golem.dev/v1alpha1
kind: PriceList
metadata: {name: p}
spec:
  currency: USD
  prices:
  - {model: provider-model, inputPerMTok: 1000000, outputPerMTok: 5000000}
---
apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], inFlight: {principal: 1}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys:
  - {key: ` + g.keyA + `, principal: alice, scope: team-a` + revoked + `}
  - {key: ` + g.keyB + `, principal: bob, scope: team-a}
`
}

// apply writes the config and applies it, as the directory source does.
func (g *tgw) apply(aliceRevoked bool) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, "golem.yaml"), []byte(g.config(aliceRevoked)), 0o600); err != nil {
		g.t.Fatal(err)
	}
	if err := g.src.Load(); err != nil {
		g.t.Fatal(err)
	}
	c, _ := g.src.Holder.Current()
	c.Routes.Lookup([]byte("claude")).Pool[0].Transport = roundTrip(g.provide)
}

// denyAlice puts alice on the emergency denylist.
func (g *tgw) denyAlice() {
	g.t.Helper()
	if err := os.WriteFile(g.deny.Path, []byte("principal alice\n"), 0o600); err != nil {
		g.t.Fatal(err)
	}
	if err := g.deny.Load(); err != nil {
		g.t.Fatal(err)
	}
}

func newTGW(t *testing.T, d Dialect, whole bool) *tgw {
	g := &tgw{t: t, d: d, dir: t.TempDir(), calls: &InFlight{}, results: make(chan Result, 8),
		upstream: make(chan *http.Request, 8), release: make(chan struct{}), whole: whole,
		keyA: key.New(key.Dev), keyB: key.New(key.Dev)}
	g.ends.m = map[string]string{}
	h := &gsnap.Holder{Env: key.Dev}
	h.OnApply = func(*gsnap.Compiled) { g.calls.Sweep() }
	g.calls.Keys = func() *auth.Keys {
		if c := h.Applied(); c != nil {
			return c.Auth
		}
		return nil
	}
	// The denylist beside the config's directory, not in it.
	g.deny = &auth.DenyFile{Path: filepath.Join(t.TempDir(), "denylist"), OnApply: func(*auth.Denylist) { g.calls.Sweep() }}
	os.WriteFile(g.deny.Path, nil, 0o600)
	if err := g.deny.Load(); err != nil {
		t.Fatal(err)
	}
	h.Deny = g.deny
	g.src = &gsnap.Dir{Path: g.dir, Holder: h, Secrets: gsnap.EnvSecrets{Getenv: func(string) string { return "provider-key" }}}
	g.apply(false)
	lim := limit.New(limit.NewMem())
	tel, err := telemetry.New(telemetry.Stdout(&g.rows), "local", "standalone")
	if err != nil {
		t.Fatal(err)
	}
	hd := &Handler{Dialect: d, Snapshots: h, Limiter: recAdmitter{lim.Admitter(), &g.ends}, MaxBody: 1 << 20,
		Telemetry: tel, InFlight: g.calls, ProviderTail: time.Minute,
		record: func(_ *http.Request, r Result) { g.results <- r }}
	g.srv = httptest.NewServer(hd)
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
		g.srv.Close()
	})
	return g
}

// provide is the provider: a stream's first event at once and the rest
// when released, or a whole response's headers only when released.
func (g *tgw) provide(r *http.Request) (*http.Response, error) {
	r.Body.Close()
	g.upstream <- r
	first, rest := g.events()
	if g.whole && g.headersFirst {
		first, rest = g.wholeBody()[:8], g.wholeBody()[8:]
	} else if g.whole {
		select {
		case <-g.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(bytes.NewReader(g.wholeBody()))}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		pw.Write(first)
		select {
		case <-g.release:
			pw.Write(rest)
			pw.Close()
		case <-r.Context().Done():
			pw.CloseWithError(r.Context().Err()) // as a transport's body read fails
		}
	}()
	ct := "text/event-stream"
	if g.whole {
		ct = "application/json"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: pr}, nil
}

func (g *tgw) events() (first, rest []byte) {
	if g.d == OpenAIChat {
		return []byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n"),
			[]byte(`data: {"choices":[{"delta":{"content":" there"}}],"usage":null}` + "\n\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n")
	}
	ev := anthropicStream(2)
	return ev[0], bytes.Join(ev[1:], nil)
}

func (g *tgw) wholeBody() []byte {
	if g.d == OpenAIChat {
		return []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}
	return []byte(`{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
}

// call starts a call with k, and returns its response once its first
// event (a stream's) or its upstream request (a whole response's) is
// under way.
func (g *tgw) call(k string, stream bool) *http.Response {
	g.t.Helper()
	body := `{"model":"claude","max_tokens":10,"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"messages":[]}`
	req, _ := http.NewRequestWithContext(g.t.Context(), "POST", g.srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+k)
	type got struct {
		resp *http.Response
		err  error
	}
	done := make(chan got, 1)
	go func() {
		resp, err := g.srv.Client().Do(req)
		done <- got{resp, err}
	}()
	select {
	case <-g.upstream:
	case <-time.After(terminateBound):
		g.t.Fatal("the call did not reach the provider")
	}
	if !stream {
		return &http.Response{Body: &lateBody{done: func() (*http.Response, error) {
			select {
			case r := <-done:
				return r.resp, r.err
			case <-time.After(terminateBound):
				g.t.Fatalf("the call was not answered within %s", terminateBound)
				return nil, nil
			}
		}}}
	}
	r := <-done
	if r.err != nil {
		g.t.Fatal(r.err)
	}
	if r.resp.StatusCode != 200 {
		b, _ := io.ReadAll(r.resp.Body)
		g.t.Fatalf("%d %s", r.resp.StatusCode, b)
	}
	return r.resp
}

// lateBody stands for a whole response still to come.
type lateBody struct {
	done func() (*http.Response, error)
}

func (*lateBody) Read([]byte) (int, error) { return 0, io.EOF }
func (*lateBody) Close() error             { return nil }

// readAllBounded reads a body to its end or its error, within the bound.
func readAllBounded(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	type got struct {
		b   []byte
		err error
	}
	ch := make(chan got, 1)
	go func() { b, err := io.ReadAll(r); ch <- got{b, err} }()
	select {
	case g := <-ch:
		return g.b, g.err
	case <-time.After(terminateBound):
		t.Fatalf("the response did not end within %s", terminateBound)
		return nil, nil
	}
}

// result is the next call's result, its ledger row and its ticket's end.
func (g *tgw) result() (Result, map[string]any, string) {
	g.t.Helper()
	var r Result
	select {
	case r = <-g.results:
	case <-time.After(terminateBound):
		g.t.Fatal("no result")
	}
	row, end := g.rowOf(r.RequestID.String())
	return r, row, end
}

// rowOf is a call's ledger row and its ticket's end.
func (g *tgw) rowOf(id string) (map[string]any, string) {
	g.t.Helper()
	for _, row := range splitRows(g.t, g.rows.snapshot()) {
		if row["request_id"] == id {
			return row, g.ends.of(id)
		}
	}
	g.t.Fatalf("no ledger row for %s", id)
	return nil, ""
}

// errorEventCode is the code of the dialect's stream error event in b.
func errorEventCode(t *testing.T, d Dialect, b []byte) string {
	t.Helper()
	for ev := range strings.SplitSeq(string(b), "\n\n") {
		data, ok := strings.CutPrefix(ev, "event: error\ndata: ")
		if d == OpenAIChat {
			data, ok = strings.CutPrefix(ev, "data: ")
			ok = ok && strings.HasPrefix(data, `{"error"`)
		}
		if !ok {
			continue
		}
		var e struct {
			Type  string
			Error struct{ Type, Code, Message string }
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("an error event that is not JSON: %q", data)
		}
		if d == OpenAIChat {
			return e.Error.Code
		}
		if e.Type != "error" {
			t.Fatalf("an Anthropic error event of type %q", e.Type)
		}
		return e.Error.Type
	}
	return ""
}

// TestTerminateStream: a stream in flight for a key ends with its
// dialect's error event once a snapshot revoking the key is applied, or
// once the denylist names its principal; its row says terminated, it is
// charged what was produced and its slot is held for the provider's tail;
// another key's stream goes on to its end.
func TestTerminateStream(t *testing.T) {
	for _, d := range []Dialect{AnthropicMessages, OpenAIChat} {
		for _, tc := range []struct {
			name     string
			withdraw func(*tgw)
			code     string
		}{
			{"revoked", func(g *tgw) { g.apply(true) }, "auth.key_revoked"},
			{"denied", (*tgw).denyAlice, "auth.denied"},
		} {
			t.Run(d.Name()+"/"+tc.name, func(t *testing.T) {
				g := newTGW(t, d, false)
				alice := g.call(g.keyA, true)
				defer alice.Body.Close()
				bob := g.call(g.keyB, true)
				defer bob.Body.Close()
				first := make([]byte, 16)
				if _, err := io.ReadAtLeast(alice.Body, first, 1); err != nil {
					t.Fatal(err)
				}
				if n := g.calls.Len(); n != 2 {
					t.Fatalf("%d calls in flight, want 2", n)
				}

				at := time.Now()
				tc.withdraw(g)
				rest, err := readAllBounded(t, alice.Body)
				t.Logf("from the withdrawal to the stream's end: %s", time.Since(at))
				if err == nil {
					t.Fatal("the terminated stream ended cleanly: a client could take it for whole")
				}
				if got := errorEventCode(t, d, rest); got != tc.code {
					t.Fatalf("the stream ended with %q (%q), want its dialect's error event with %s", got, rest, tc.code)
				}
				res, row, end := g.result()
				if !res.Terminated || res.CallerGone {
					t.Fatalf("result %+v: want terminated, not a caller gone", res)
				}
				if row["outcome"] != "terminated" || row["error_code"] != tc.code {
					t.Fatalf("row %v: want outcome terminated, code %s", row, tc.code)
				}
				if c, _ := row["cost_micros"].(float64); c <= 0 {
					t.Fatalf("row %v: a terminated call is charged what was produced", row)
				}
				if end != "cancel, held 1m0s" {
					t.Fatalf("ticket ended by %q: want a cancel holding the slot for the provider's tail", end)
				}

				// bob's stream was untouched: it runs to its end.
				close(g.release)
				body, err := readAllBounded(t, bob.Body)
				if err != nil || errorEventCode(t, d, body) != "" {
					t.Fatalf("the other key's stream: %v %q", err, body)
				}
				if _, row, end := g.result(); row["outcome"] != "completed" || end != "complete" {
					t.Fatalf("the other key's call: row %v, ticket %q", row, end)
				}
			})
		}
	}
}

// TestTerminateWhole: a whole (non-streamed) call still waiting for its
// provider's headers is answered with the dialect's error once its key
// is revoked, and its row says terminated, its slot held for the tail.
func TestTerminateWhole(t *testing.T) {
	for _, d := range []Dialect{AnthropicMessages, OpenAIChat} {
		t.Run(d.Name(), func(t *testing.T) {
			g := newTGW(t, d, true)
			alice := g.call(g.keyA, false).Body.(*lateBody)
			at := time.Now()
			g.apply(true)
			resp, err := alice.done()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("from the revocation to the answer: %s", time.Since(at))
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var e struct {
				Error struct{ Type, Code string }
			}
			json.Unmarshal(b, &e)
			code := e.Error.Type
			if d == OpenAIChat {
				code = e.Error.Code
			}
			if resp.StatusCode != 401 || code != "auth.key_revoked" {
				t.Fatalf("%d %s: want 401 auth.key_revoked in the dialect's shape", resp.StatusCode, b)
			}
			row, end := g.rowOf(resp.Header.Get("Golem-Request-Id"))
			if row["outcome"] != "terminated" || row["error_code"] != "auth.key_revoked" {
				t.Fatalf("row %v", row)
			}
			if c, _ := row["cost_micros"].(float64); c <= 0 {
				t.Fatalf("row %v: the provider had the request: its input is charged", row)
			}
			if end != "cancel, held 1m0s" {
				t.Fatalf("ticket ended by %q: want a cancel holding the slot for the tail", end)
			}
		})
	}
	// Once the provider's headers are forwarded, the call can only be
	// cut: the caller never gets a whole body.
	for _, d := range []Dialect{AnthropicMessages, OpenAIChat} {
		t.Run(d.Name()+"/headers sent", func(t *testing.T) {
			g := newTGW(t, d, true)
			g.headersFirst = true
			alice := g.call(g.keyA, false).Body.(*lateBody)
			g.apply(true)
			r, err := alice.done()
			if err == nil {
				var b []byte
				b, err = readAllBounded(t, r.Body)
				r.Body.Close()
				if err == nil {
					t.Fatalf("a cut whole response ended cleanly: %d %s", r.StatusCode, b)
				}
			}
			res, row, end := g.result()
			if !res.Terminated || row["outcome"] != "terminated" || end != "cancel, held 1m0s" {
				t.Fatalf("result %+v, row %v, ticket %q", res, row, end)
			}
		})
	}
}

// TestLeaseExpiryEndsNoCall: a snapshot whose lease has run out stops new
// requests only: a sweep over its keys (as a denylist reload makes) ends
// nothing honoured.
func TestLeaseExpiryEndsNoCall(t *testing.T) {
	g := newTGW(t, AnthropicMessages, false)
	alice := g.call(g.keyA, true)
	defer alice.Body.Close()
	c := g.src.Holder.Applied()
	c.Snapshot.ValidUntil = time.Now().Add(-time.Second) // expired: no new request
	if _, err := g.src.Holder.Current(); err == nil {
		t.Fatal("the lease did not expire")
	}
	if n := g.calls.Sweep(); n != 0 {
		t.Fatalf("a sweep after the lease expired ended %d calls", n)
	}
	close(g.release)
	if body, err := readAllBounded(t, alice.Body); err != nil || errorEventCode(t, AnthropicMessages, body) != "" {
		t.Fatalf("the call in flight: %v %q", err, body)
	}
}

// TestTerminateRace: a call that authenticated before a sweep and
// registered after the sweep passed its shard checks itself, and does
// not reach the provider.
func TestTerminateRace(t *testing.T) {
	g := newTGW(t, AnthropicMessages, false)
	k := g.calls.Keys()
	var grant auth.Grant
	if _, no := k.AuthenticateGrant(http.Header{"X-Api-Key": {g.keyA}}, &grant); no != nil {
		t.Fatal(no.Msg)
	}
	gen := g.calls.generation()
	g.apply(true) // the sweep: nothing registered yet
	var c inFlightCall
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	if tm := g.calls.add(&c, &grant, cancel, 3, gen); tm == nil || tm.code.Name != "auth.key_revoked" || terminatedBy(ctx) != tm {
		t.Fatalf("a call registered after the sweep: %v", tm)
	}
	g.calls.remove(&c)
	var d inFlightCall
	if tm := g.calls.add(&d, &grant, cancel, 3, g.calls.generation()); tm != nil {
		t.Fatal("a call registered with the current generation is not rechecked")
	}
	g.calls.remove(&d)
}

// lockedBuffer is a buffer the handlers' telemetry writes concurrently.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) snapshot() *bytes.Buffer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.NewBuffer(bytes.Clone(l.b.Bytes()))
}

// ends records each ticket's end by call ID: "complete", or "cancel" with
// the hold's length.
type ends struct {
	mu sync.Mutex
	m  map[string]string
}

func (e *ends) of(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.m[id]
}

type recAdmitter struct {
	admission.Admitter
	e *ends
}

func (a recAdmitter) Admit(ctx context.Context, c admission.Call) (admission.Ticket, error) {
	t, err := a.Admitter.Admit(ctx, c)
	if t == nil {
		return t, err
	}
	return &recTicket{t, c.ID, a.e}, err
}

type recTicket struct {
	admission.Ticket
	id string
	e  *ends
}

func (t *recTicket) Complete(ctx context.Context, cost int64) {
	t.e.mu.Lock()
	t.e.m[t.id] = "complete"
	t.e.mu.Unlock()
	t.Ticket.Complete(ctx, cost)
}

func (t *recTicket) Cancel(ctx context.Context, seen int64, until time.Time) {
	t.e.mu.Lock()
	t.e.m[t.id] = "cancel, held " + time.Until(until).Round(time.Second).String()
	t.e.mu.Unlock()
	t.Ticket.Cancel(ctx, seen, until)
}

// BenchmarkInFlight is the registry's share of a call: its add and its
// remove, from every core at once.
func BenchmarkInFlight(b *testing.B) {
	f := &InFlight{}
	var g auth.Grant
	cancel := context.CancelCauseFunc(func(error) {})
	var pick atomic.Uint32
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var c inFlightCall
		p := byte(pick.Add(1))
		for pb.Next() {
			f.add(&c, &g, cancel, p, f.generation())
			f.remove(&c)
			p += 7
		}
	})
}
