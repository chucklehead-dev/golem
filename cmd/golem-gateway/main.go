// Command golem-gateway is the data plane. In the spike it serves the fast
// path from routes given on the command line; snapshots, authentication,
// limits and telemetry replace the flags as they are built.
//
//	golem-gateway -listen :8080 \
//	  -route claude=claude-sonnet-4-5@https://api.anthropic.com/v1/messages
//
// A route's provider key comes from GOLEM_UPSTREAM_KEY (the spike's only
// secret); Anthropic routes send it as x-api-key, others as a bearer token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chucklehead-dev/golem/gateway/proxy"
)

type routes map[string]*proxy.Route

func (r routes) String() string { return fmt.Sprint(len(r), " routes") }

// Set parses alias=model@url.
func (r routes) Set(s string) error {
	alias, rest, ok := strings.Cut(s, "=")
	model, raw, ok2 := strings.Cut(rest, "@")
	if !ok || !ok2 || alias == "" || model == "" {
		return errors.New("a route is alias=model@url, e.g. claude=claude-sonnet-4-5@https://api.anthropic.com/v1/messages")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("route %s: %q is not an absolute URL", alias, raw)
	}
	h := http.Header{}
	if key := os.Getenv("GOLEM_UPSTREAM_KEY"); key != "" {
		if strings.HasSuffix(u.Host, "anthropic.com") {
			h.Set("X-Api-Key", key)
		} else {
			h.Set("Authorization", "Bearer "+key)
		}
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 256
	r[alias] = &proxy.Route{Model: model, URL: u, Transport: t, Header: h}
	return nil
}

func main() {
	rs := routes{}
	listen := flag.String("listen", ":8080", "address to serve on")
	maxBody := flag.Int64("max-body", 32<<20, "largest request body accepted, in bytes")
	flag.Var(rs, "route", "alias=model@url (repeatable)")
	flag.Parse()
	if len(rs) == 0 {
		fmt.Fprintln(os.Stderr, "golem-gateway: no routes; give at least one -route alias=model@url")
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", &proxy.Handler{Dialect: proxy.AnthropicMessages, Routes: rs, MaxBody: *maxBody})
	mux.Handle("POST /v1/chat/completions", &proxy.Handler{Dialect: proxy.OpenAIChat, Routes: rs, MaxBody: *maxBody})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

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
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdown) // drains streams in flight, up to the deadline
	}()
	slog.Info("golem-gateway serving", "listen", *listen, "routes", len(rs))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("golem-gateway", "err", err)
		os.Exit(1)
	}
}
