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
	"syscall"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

func main() {
	standalone := flag.String("standalone", "", "run from the CRD YAML in this directory, on this machine only (ADR 0023)")
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve on")
	maxBody := flag.Int64("max-body", 32<<20, "largest request body accepted, in bytes")
	tenant := flag.String("entra-tenant", "", "accept Entra tokens from this tenant (directory) ID; empty: golem keys only")
	audience := flag.String("entra-audience", "api://golem", "golem API's audiences in Entra tokens, comma-separated (its App ID URI and client ID)")
	flag.Parse()
	if *standalone == "" {
		fail("only --standalone <dir> is built so far; the bucket source comes with the publisher (roadmap M1)")
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil || !isLoopback(host) {
		fail("standalone mode listens on a loopback address only (got %q): it is not for shared use (ADR 0023)", *listen)
	}

	// Standalone: dev keys, the in-memory limiter (ADR 0023).
	limiter := limit.New(limit.NewMem())
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
		mux.Handle(pattern, localOnly(d, &proxy.Handler{Dialect: d, Snapshots: holder, Limiter: limiter, MaxBody: *maxBody}))
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
		// No WriteTimeout: streams are long (gateway.md §3 step 1).
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go dir.Run(ctx)
	go renew(ctx, limiter)
	if entra != nil {
		go entra.Run(ctx)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdown) // drains streams in flight, up to the deadline
	}()
	fmt.Fprintf(os.Stderr, "golem-gateway standalone: %s on http://%s — for this machine only, not for shared use\n", *standalone, *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("golem-gateway", "err", err)
		os.Exit(1)
	}
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

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "golem-gateway: "+format+"\n", args...)
	os.Exit(2)
}
