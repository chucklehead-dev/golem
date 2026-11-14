package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/compile"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestReleaseGate: standalone listens on loopback only; pool mode needs its
// inputs, and -accept-unreleased until golem#7's evidence is in.
func TestReleaseGate(t *testing.T) {
	full := options{pool: "internal", bucket: "s3://b/p", keys: "/k", valkeyAddr: "v:6379", site: "eu", listen: ":8080"}
	accepted := full
	accepted.acceptUnreleased = true
	missing := accepted
	missing.valkeyAddr = ""
	both := accepted
	both.standalone = "./config"
	for name, tc := range map[string]struct {
		o    options
		want string // "" ok, else a word of the refusal
	}{
		"nothing":                   {options{listen: "127.0.0.1:8080"}, "-standalone"},
		"standalone, loopback":      {options{standalone: "./config", listen: "127.0.0.1:8080"}, ""},
		"standalone, ::1":           {options{standalone: "./config", listen: "[::1]:8080"}, ""},
		"standalone, localhost":     {options{standalone: "./config", listen: "localhost:8080"}, ""},
		"standalone, every host":    {options{standalone: "./config", listen: "0.0.0.0:8080"}, "loopback"},
		"standalone, empty host":    {options{standalone: "./config", listen: ":8080"}, "loopback"},
		"standalone, a LAN address": {options{standalone: "./config", listen: "10.0.0.5:8080"}, "loopback"},
		"pool, unreleased":          {full, "golem#7"},
		"pool, accepted":            {accepted, ""},
		"pool, no Valkey":           {missing, "-valkey"},
		"both modes":                {both, "not both"},
	} {
		err := releaseGate(tc.o)
		if (err == nil) != (tc.want == "") || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// TestPoolMode: golem-control's publisher writes a pool's snapshot to a
// bucket, and a pool-mode gateway, over a real Valkey with its provider
// credential from a mounted Secret, serves a key's call from it, then stops
// on its context.
func TestPoolMode(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("X-Api-Key") != "sk-from-the-secret" {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	root := t.TempDir()
	dir := func(name string) string { p := filepath.Join(root, name); os.MkdirAll(p, 0o700); return p }
	bkt, err := bucket.OpenDir(dir("bucket"))
	if err != nil {
		t.Fatal(err)
	}
	defer bkt.Close()
	pub, priv, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	os.WriteFile(filepath.Join(dir("keys"), "publishing.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir("secrets/anthropic"), "api-key"), []byte("sk-from-the-secret\n"), 0o600)

	// The state: r1 leads; alice is a member of team-a with a key.
	alice := key.New(key.Live)
	h := sha256.Sum256([]byte(alice))
	st := &state.Store{Bucket: bkt}
	if _, err := st.Update(ctx, func(d *state.Document) error {
		d.Fence = &state.Fence{Holder: "r1", Epoch: 1}
		d.Scopes = map[string]state.Scope{"team-a": {Models: []string{"claude"}}}
		d.Principals = map[string]state.Principal{"alice": {Kind: "user", Scopes: []string{"team-a"}}}
		d.Keys = map[string]state.Key{hex.EncodeToString(h[:]): {Display: key.Display(alice), Owner: "alice", Scope: "team-a"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := dir("config")
	os.WriteFile(filepath.Join(cfg, "backend.yaml"), []byte(`apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, secretRef: {name: anthropic, key: api-key}}
  network: {allowPrivateAddresses: true}
  deployments:
  - {name: sonnet, url: "`+upstream.URL+`/v1/messages", model: m}
`), 0o600)
	os.WriteFile(filepath.Join(cfg, "route.yaml"), []byte(`apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: anthropic, deployment: sonnet}]}
`), 0o600)
	fsRoot, _ := os.OpenRoot(cfg)
	objs, err := v1.DecodeFS(fsRoot.FS())
	if err != nil {
		t.Fatal(err)
	}
	p := &publish.Publisher{State: st, Bucket: bkt, Signer: sign.Ed25519{ID: "publishing", Key: priv}, Pool: "internal"}
	doc := st.Current()
	if _, err := p.Publish(ctx, publish.Lease{Holder: "r1", Epoch: 1}, doc.StateVersion, func(seq uint64) (*snapshot.Snapshot, error) {
		return compile.Compile(objs, compile.Options{PublishSeq: seq, Now: time.Now().UTC(), Lease: time.Hour, Entitlements: doc.Entitlements})
	}); err != nil {
		t.Fatal(err)
	}

	listen := freeAddr(t)
	vk := startValkey(t)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-pool", "internal", "-bucket", filepath.Join(root, "bucket"), "-keys", filepath.Join(root, "keys"),
			"-checkpoint", filepath.Join(root, "checkpoint"), "-valkey", vk, "-site", "test", "-secrets", filepath.Join(root, "secrets"),
			"-listen", listen, "-telemetry", "off", "-drain", "1s", "-accept-unreleased"})
	}()
	base := "http://" + listen
	ready := false
	for range 200 {
		if r, err := http.Get(base + "/readyz"); err == nil {
			r.Body.Close()
			if ready = r.StatusCode == 200; ready {
				break
			}
		}
		time.Sleep(25 * time.Millisecond) // the gateway starting: another goroutine's server
	}
	if !ready {
		t.Fatal("the gateway never became ready")
	}
	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Api-Key", alice)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"text":"hi"`) {
		t.Fatalf("the call: %d %s", resp.StatusCode, body)
	}
	// The call's spend is in the site's Valkey, not a memory store.
	c, err := valkeygo.NewClient(valkeygo.ClientOption{InitAddress: []string{vk}, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	keys, err := c.Do(ctx, c.B().Keys().Pattern("{golem:test}*").Build()).AsStrSlice()
	if err != nil || len(keys) == 0 {
		t.Fatalf("Valkey holds %v (%v): the limiter is not the site's", keys, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "checkpoint")); err != nil || strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("the local checkpoint: %q %v", b, err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway did not stop on its context")
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startValkey is a local valkey-server or redis-server for the test, or
// GOLEM_VALKEY_ADDR.
func startValkey(t *testing.T) string {
	if a := os.Getenv("GOLEM_VALKEY_ADDR"); a != "" {
		return a
	}
	bin, err := exec.LookPath("valkey-server")
	if err != nil {
		if bin, err = exec.LookPath("redis-server"); err != nil {
			t.Skip("no Valkey: set GOLEM_VALKEY_ADDR or install valkey-server")
		}
	}
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	cmd := exec.Command(bin, "--port", port, "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for range 100 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond) // the server starting: an external process
	}
	t.Fatal(fmt.Sprintf("Valkey at %s did not start", addr))
	return ""
}
