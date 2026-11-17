package snapshot

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

type fixed map[string]string

func (f fixed) Secret(ref, env string) (string, error) { return f[ref+env], nil }

func snap(seq uint64, until time.Time) *snapshot.Snapshot {
	return &snapshot.Snapshot{Schema: 1, PublishSeq: seq, ValidUntil: until,
		Backends: []snapshot.Backend{{Name: "a", Dialect: "anthropic.messages",
			Auth:        snapshot.Auth{Header: "x-api-key", SecretRef: "a/key"},
			Deployments: []snapshot.Deployment{{Name: "d", URL: "https://p.example/v1/messages", Model: "m"}}}},
		Routes: []snapshot.Route{{Alias: "claude", Dialect: "anthropic.messages", Pool: []snapshot.Target{{Backend: "a", Deployment: "d", Weight: 1}}}}}
}

//quint:checks publication.revocationBounded, publication.noExpiredServe, publication.noRollback, publication.noRollbackBelowCheckpoint
func TestHolder(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	h := &Holder{Now: func() time.Time { return now }}
	if _, err := h.Current(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("before any snapshot: %v", err)
	}
	if err := h.Apply(snap(5, now.Add(time.Hour)), "h5", fixed{"a/key": "sk"}); err != nil {
		t.Fatal(err)
	}
	c, err := h.Current()
	if err != nil || c.Routes.Lookup([]byte("claude")).Pool[0].Header.Get("X-Api-Key") != "sk" {
		t.Fatalf("current %v %v", c, err)
	}
	tr := c.Routes.Lookup([]byte("claude")).Pool[0].Transport
	// A backend without a provider account is recorded by its name.
	if b := c.Snapshot.Backends[0]; b.ProviderAccount != "" || c.Accounts[b.Name] != b.Name {
		t.Fatalf("accounts %v for backend %q: want its name", c.Accounts, b.Name)
	}

	// The floor: a replay or a stale publisher's snapshot is refused, and
	// so is the applied one again.
	for _, seq := range []uint64{5, 4} {
		if err := h.Apply(snap(seq, now.Add(2*time.Hour)), "old", fixed{}); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("snapshot %d with 5 applied: %v", seq, err)
		}
	}
	// An already expired snapshot is refused.
	if err := h.Apply(snap(6, now), "exp", fixed{}); err == nil {
		t.Fatal("applied an expired snapshot")
	}
	// A newer one is applied, and keeps the deployment's transport.
	if err := h.Apply(snap(6, now.Add(time.Hour)), "h6", fixed{"a/key": "sk2"}); err != nil {
		t.Fatal(err)
	}
	c, _ = h.Current()
	if c.Hash != "h6" || c.Routes.Lookup([]byte("claude")).Pool[0].Transport != tr {
		t.Fatal("the transport did not survive the swap")
	}
	// Past the lease, nothing new is served.
	now = now.Add(time.Hour)
	if _, err := h.Current(); !errors.Is(err, ErrExpired) {
		t.Fatalf("after the lease: %v", err)
	}
}

// TestRaiseFloor: after a restart whose floor comes from a trusted
// checkpoint, a replayed older snapshot is refused even though the gateway
// holds nothing yet.
//
//quint:checks publication.noRollbackBelowCheckpoint
func TestRaiseFloor(t *testing.T) {
	h := &Holder{}
	h.RaiseFloor(10)
	if err := h.Apply(snap(3, time.Now().Add(time.Hour)), "x", fixed{"a/key": "k"}); err == nil {
		t.Fatal("a replay below the trusted floor was applied on an empty gateway")
	}
	if _, err := h.Current(); !errors.Is(err, ErrNotReady) || h.Floor() != 10 {
		t.Fatalf("after the refused replay: %v, floor %d", err, h.Floor())
	}
	// The checkpoint itself is the current publication: a restarted gateway
	// must be able to apply it (found by the publication model's connect test).
	if err := h.Apply(snap(10, time.Now().Add(time.Hour)), "y", fixed{"a/key": "k"}); err != nil {
		t.Fatal(err)
	}
}

func TestDir(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: local}
spec:
  dialect: openai.chat
  auth: {header: authorization, scheme: Bearer, env: LOCAL_KEY}
  deployments: [{name: qwen, url: "http://127.0.0.1:8000/v1/chat/completions", model: qwen3}]
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: local}
spec: {alias: local, pool: [{backend: local, deployment: qwen}]}
`
	write(cfg)
	h := &Holder{}
	d := &Dir{Path: dir, Holder: h, Secrets: EnvSecrets{Getenv: func(string) string { return "k" }}}
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
	c, _ := h.Current()
	if got := c.Routes.Lookup([]byte("local")).Pool[0].Header.Get("Authorization"); got != "Bearer k" {
		t.Fatalf("auth header %q", got)
	}
	// A broken edit keeps the previous config.
	write(strings.Replace(cfg, "deployment: qwen}", "deployment: llama}", 1))
	if err := d.Load(); err == nil || !strings.Contains(err.Error(), `has no deployment "llama"`) {
		t.Fatalf("broken config: %v", err)
	}
	if c2, _ := h.Current(); c2 != c {
		t.Fatal("a broken config replaced the working one")
	}
	// A fixed edit applies.
	write(strings.Replace(cfg, "alias: local", "alias: qwen", 1))
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
	if c3, _ := h.Current(); c3.Routes.Lookup([]byte("qwen")) == nil {
		t.Fatal("the fixed config was not applied")
	}
}

// TestUpstreamTransport: the settings the pool depends on under load, so a
// change to them is a decision, not an accident.
func TestUpstreamTransport(t *testing.T) {
	tr := upstreamTransport(false, nil)
	switch {
	case tr.Proxy != nil:
		t.Error("a proxy from the environment: egress is the mesh's (ADR 0004)")
	case tr.MaxIdleConns != 0 || tr.MaxIdleConnsPerHost < 256:
		t.Errorf("idle connections capped at %d in all (%d per host): concurrent streams would re-handshake", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	case tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout == 0:
		t.Error("no HTTP/2 pings: a dead connection under a long stream goes unnoticed")
	case tr.ResponseHeaderTimeout != 0:
		t.Error("a response-header timeout cuts long non-streamed answers; it is the route's to set")
	}
}

// TestCheckDestination: where golem dials.
func TestCheckDestination(t *testing.T) {
	for _, tc := range []struct {
		addr         string
		allowPrivate bool
		ok           bool
	}{
		{"203.0.113.7:443", false, true},
		{"[2001:db8::1]:443", false, true},
		{"169.254.169.254:80", true, false}, // metadata, even granted
		{"[fd00:ec2::254]:80", true, false},
		{"100.100.100.200:80", true, false},
		{"[fe80::1]:443", true, false},
		{"0.0.0.0:443", true, false},
		{"127.0.0.1:8000", false, false},
		{"127.0.0.1:8000", true, true},
		{"10.1.2.3:443", false, false},
		{"10.1.2.3:443", true, true},
		{"100.64.1.1:443", false, false},
		{"[::ffff:192.168.1.1]:443", false, false}, // IPv4-mapped
		{"[fc00::1]:443", false, false},
	} {
		if err := checkDestination(tc.addr, tc.allowPrivate); (err == nil) != tc.ok {
			t.Errorf("%s (allowPrivate %v): %v, want ok %v", tc.addr, tc.allowPrivate, err, tc.ok)
		}
	}
}

// TestDialRefused: the check runs on the connection itself.
func TestDialRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	if _, err := upstreamTransport(false, nil).RoundTrip(req); err == nil || !strings.Contains(err.Error(), "refusing to dial") {
		t.Fatalf("%v: a loopback server without the grant", err)
	}
	if resp, err := upstreamTransport(true, nil).RoundTrip(req); err != nil {
		t.Fatalf("%v: with the grant", err)
	} else {
		resp.Body.Close()
	}
}

// TestDialReresolved: a deployment's name that re-resolves to a forbidden
// address between connections is refused at the next dial, not trusted
// for having resolved well once. The name first points at the
// provider (a loopback server, granted), then at cloud metadata.
func TestDialReresolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	var answer atomic.Value // the A record the name resolves to now
	answer.Store([4]byte{127, 0, 0, 1})
	dns := fakeDNS(t, func() [4]byte { return answer.Load().([4]byte) })
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", dns)
	}}
	tr := upstreamTransport(true, resolver)
	get := func() error {
		req, _ := http.NewRequest("GET", "http://provider.golem-test:"+port+"/", nil)
		resp, err := tr.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(); err != nil {
		t.Fatalf("%v: the name at the provider, granted", err)
	}
	answer.Store([4]byte{100, 100, 100, 200}) // cloud metadata (Alibaba's): never dialled
	tr.CloseIdleConnections()                 // the next request dials again
	if err := get(); err == nil || !strings.Contains(err.Error(), "refusing to dial") {
		t.Fatalf("%v: the name re-resolved to metadata must be refused at the dial", err)
	}
	answer.Store([4]byte{169, 254, 169, 254}) // link-local metadata (AWS, GCP, Azure)
	if err := get(); err == nil || !strings.Contains(err.Error(), "refusing to dial") {
		t.Fatalf("%v: the name re-resolved to link-local must be refused at the dial", err)
	}
}

// fakeDNS answers every A query with a(), on a local UDP port it returns.
func fakeDNS(t *testing.T, a func() [4]byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() { //qenv:ok a test's DNS responder, not simulated code
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			b.EnableCompression()
			b.StartQuestions()
			b.Question(q)
			b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 0}, dnsmessage.AResource{A: a()})
			}
			if msg, err := b.Finish(); err == nil {
				pc.WriteTo(msg, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}
