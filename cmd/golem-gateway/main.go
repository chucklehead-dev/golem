// Command golem-gateway is the data plane. It runs from a pool's signed
// snapshots in object storage (not yet built) or, for a developer's own
// machine, standalone (ADR 0023):
//
//	golem-gateway --standalone ./config
//
// Standalone mode reads the same CRD YAML as the GitOps repository,
// recompiles it on change, takes provider credentials from the environment
// (a Backend's spec.auth.env), and listens on localhost only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

func main() {
	standalone := flag.String("standalone", "", "run from the CRD YAML in this directory, on this machine only (ADR 0023)")
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve on")
	maxBody := flag.Int64("max-body", 32<<20, "largest request body accepted, in bytes")
	tenant := flag.String("entra-tenant", "", "accept Entra tokens from this tenant (directory) ID; empty: golem keys only")
	drainFor := flag.Duration("drain", 30*time.Second, "how long shutdown waits for calls in flight, streams included")
	telem := flag.String("telemetry", "stdout", "stdout: ledger rows as JSON lines on stdout; off; or a collector's OTLP/HTTP host:port")
	audience := flag.String("entra-audience", "api://golem", "golem API's audiences in Entra tokens, comma-separated (its App ID URI and client ID)")
	flag.Parse()
	if *standalone == "" {
		fail("only --standalone <dir> is built so far; the bucket source comes with the publisher (roadmap M1)")
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil || !isLoopback(host) {
		fail("standalone mode listens on a loopback address only (got %q): it is not for shared use (ADR 0023)", *listen)
	}

	// Standalone: dev keys, the in-memory limiter (ADR 0023).
	tel, shutdownTelemetry := setupTelemetry(*telem)
	defer shutdownTelemetry()
	limiter := limit.New(limit.NewMem())
	limiter.OnNumbered = func(_ string, e limit.Event, seq int64) { tel.Numbered(e.ID, seq) }
	var entra *auth.Entra
	if *tenant != "" {
		entra = &auth.Entra{Tenant: *tenant, Audiences: strings.Split(*audience, ",")}
		if err := entra.Refresh(context.Background()); err != nil {
			fail("Entra's signing keys: %v", err)
		}
	}
	holder := &snapshot.Holder{Env: key.Dev, Entra: entra, OnApply: func(c *snapshot.Compiled) {
		for _, s := range c.Snapshot.Entitlements.Scopes {
			limiter.ApplySnapshot(context.Background(), s.ID, limit.Known{Period: s.Period, Spend: s.KnownSpend, Watermark: s.Watermark})
		}
	}}
	dir := &snapshot.Dir{Path: *standalone, Holder: holder, Secrets: snapshot.EnvSecrets{Getenv: os.Getenv}}
	if err := dir.Load(); err != nil {
		fail("%v", err)
	}

	mux := http.NewServeMux()
	for pattern, d := range map[string]proxy.Dialect{"POST /v1/messages": proxy.AnthropicMessages, "POST /v1/chat/completions": proxy.OpenAIChat} {
		mux.Handle(pattern, localOnly(d, &proxy.Handler{Dialect: d, Snapshots: holder, Limiter: limiter, MaxBody: *maxBody, Telemetry: tel}))
	}
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := holder.Current(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // h2c behind the mesh (gateway.md §3 step 1)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		Protocols:         &protocols,
		ReadHeaderTimeout: 10 * time.Second,
		// A keep-alive connection idle this long is closed: idle ones held
		// for ever are memory and file descriptors under load.
		IdleTimeout: 2 * time.Minute,
		// Headers carry a key or an Entra token (a few KB): 64 KiB, not the
		// default 1 MiB a client could make every connection hold.
		MaxHeaderBytes: 64 << 10,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 256,              // per connection: an agent's parallel calls, not a flood
			SendPingTimeout:      30 * time.Second, // find dead connections under long streams
			PingTimeout:          15 * time.Second,
		},
		// No WriteTimeout: streams are long (gateway.md §3 step 1).
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Every goroutine is owned here and ends with ctx; main returns only
	// when all have, the server's drain included.
	var wg sync.WaitGroup
	wg.Go(func() { dir.Run(ctx) })
	wg.Go(func() { renew(ctx, limiter) })
	if entra != nil {
		wg.Go(func() { entra.Run(ctx) })
	}
	wg.Go(func() {
		<-ctx.Done()
		// Shutdown stops accepting and waits for calls in flight, streams
		// included, up to the deadline; ListenAndServe returns at once, so
		// main waits for this.
		drain, cancel := context.WithTimeout(context.Background(), *drainFor)
		defer cancel()
		if err := srv.Shutdown(drain); err != nil {
			slog.Warn("golem-gateway: calls still in flight at the drain deadline were cut", "err", err)
		}
	})
	fmt.Fprintf(os.Stderr, "golem-gateway standalone: %s on http://%s — for this machine only, not for shared use\n", *standalone, *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("golem-gateway", "err", err)
		stop() // end the other goroutines
		wg.Wait()
		os.Exit(1)
	}
	wg.Wait()
}

// renew keeps running calls' slots alive (ADR 0038: every 30 seconds) and,
// after an outage, replays held increments and re-acquires slots.
func renew(ctx context.Context, l *limit.Limiter) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if l.Down() {
			if err := l.Recover(ctx); err != nil {
				slog.Warn("limiter store still unreachable; admitting on local counts, recorded as waivers", "err", err)
			}
			continue
		}
		if err := l.Renew(ctx); err != nil {
			slog.Warn("slot renewal failed; admitting on local counts until the store answers", "err", err)
		}
	}
}

// setupTelemetry starts the gateway's telemetry: ledger rows on stdout, none,
// or everything over OTLP to a collector. The returned func flushes it.
func setupTelemetry(where string) (*telemetry.Telemetry, func()) {
	var p telemetry.Providers
	shutdown := func(context.Context) error { return nil }
	switch where {
	case "off":
		return nil, func() {}
	case "stdout":
		p = telemetry.Stdout(os.Stdout)
	default:
		res := resource.NewSchemaless(attribute.String("service.name", "golem-gateway"), attribute.String("golem.site", "local"))
		var err error
		if p, shutdown, err = telemetry.OTLP(context.Background(), where, res); err != nil {
			fail("telemetry to %s: %v", where, err)
		}
	}
	tel, err := telemetry.New(p, "local", "standalone")
	if err != nil {
		fail("telemetry: %v", err)
	}
	return tel, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			slog.Warn("golem-gateway: telemetry not flushed", "err", err)
		}
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "golem-gateway: "+format+"\n", args...)
	os.Exit(2)
}
