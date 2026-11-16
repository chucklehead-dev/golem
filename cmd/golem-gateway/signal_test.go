package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/key"
)

// TestSecondSignal: a second SIGTERM during the drain cuts it short but
// does not kill the gateway: the call cut still accounts, the telemetry
// still flushes, and run returns. Signals reach the whole test binary, so
// this test must not run in parallel with another.
func TestSecondSignal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a stream longer than any drain
	}))
	defer up.Close()
	dir := t.TempDir()
	k := key.New(key.Dev)
	os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(`apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, env: GOLEM_TEST_PROVIDER_KEY}
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
  - {key: `+k+`, principal: alice, scope: team-a}
`), 0o600)
	t.Setenv("GOLEM_TEST_PROVIDER_KEY", "provider-key")
	listen := freeAddr(t)
	done := make(chan int, 1)
	go func() {
		done <- run(t.Context(), []string{"-standalone", dir, "-listen", listen, "-local-key", filepath.Join(t.TempDir(), "key"),
			"-telemetry", "off", "-drain", "30s"})
	}()
	base := "http://" + listen
	for ready := false; !ready; {
		if r, err := http.Get(base + "/readyz"); err == nil {
			r.Body.Close()
			ready = r.StatusCode == 200
		}
		select {
		case code := <-done:
			t.Fatalf("the gateway exited %d while starting", code)
		case <-time.After(25 * time.Millisecond): // the gateway starting: another goroutine's server
		}
	}
	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", k)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	resp.Body.Read(make([]byte, 16)) // under way: the drain waits for it

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond) // the drain begun: another goroutine's
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a second signal did not cut the 30 s drain short")
	}
}
