// Command golem-gateway is the data plane. It runs a pool's gateways from
// the pool's signed snapshots in object storage (gateway/snapshot.Bucket),
// with the site's Valkey for the limiter and provider credentials from
// mounted Secrets:
//
//	golem-gateway -pool internal -bucket s3://golem-state/golem/ -keys /etc/golem/keys \
//	  -checkpoint /var/lib/golem/checkpoint -valkey valkey:6379 -valkey-tls -valkey-user golem-gateway -site eu-west-1 \
//	  -secrets /var/run/golem/secrets -listen :8080 -accept-unreleased
//
// or, for a developer's own machine, standalone (ADR 0023):
//
//	golem-gateway --standalone ./config
//
// Standalone mode reads the same CRD YAML as the GitOps repository,
// recompiles it on change, takes provider credentials from the environment
// (a Backend's spec.auth.env), and listens on localhost only. Pool mode
// is behind the release gate until golem#7's deployment evidence is in.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/valkey-io/valkey-go"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/bucket/s3bucket"
	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/sign"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// options are the command line.
type options struct {
	standalone, listen, tenant, audience, jwks, telem, localKeyFile, zoneName, denylist string
	maxBody                                                                             int64
	drainFor, unready                                                                   time.Duration
	// pool mode
	pool, bucket, keys, checkpoint, gatewayID, valkeyAddr, site, secrets string
	replicaAcks                                                          int
	acceptUnreleased                                                     bool
	maxSkew, maxLease                                                    time.Duration
	// the site's Valkey: TLS (with a CA file's roots, else the system's)
	// and an AUTH user; the password is $VALKEY_PASSWORD's, never a flag
	valkeyTLS            bool
	valkeyCA, valkeyUser string
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("golem-gateway", flag.ContinueOnError)
	fs.StringVar(&o.standalone, "standalone", "", "run from the CRD YAML in this directory, on this machine only (ADR 0023)")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "address to serve on")
	fs.Int64Var(&o.maxBody, "max-body", 32<<20, "largest request body accepted, in bytes")
	fs.StringVar(&o.tenant, "entra-tenant", "", "accept Entra tokens from this tenant (directory) ID; empty: golem keys only")
	fs.DurationVar(&o.drainFor, "drain", 30*time.Second, "how long shutdown waits for calls in flight, streams included")
	fs.DurationVar(&o.unready, "unready", 0, "how long /readyz fails before shutdown stops taking connections (in Kubernetes, longer than the endpoints take to follow)")
	fs.StringVar(&o.telem, "telemetry", "stdout", "stdout: ledger rows as JSON lines on stdout; off; or a collector's OTLP/HTTP host:port (TLS unless a literal loopback address: 127.0.0.1:4318, [::1]:4318)")
	fs.StringVar(&o.localKeyFile, "local-key", "", "standalone: the file holding the local key (generated on first use); default <user config dir>/golem/standalone-key")
	fs.StringVar(&o.zoneName, "period-zone", "UTC", "the budget periods' time zone (an IANA name): a period is a calendar month there")
	fs.StringVar(&o.jwks, "entra-jwks", "", "the tenant's signing key set URL; default Entra's public cloud's, https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys")
	fs.StringVar(&o.audience, "entra-audience", "api://golem", "golem API's audiences in Entra tokens, comma-separated (its App ID URI and client ID)")
	fs.StringVar(&o.denylist, "denylist", "", "the site's emergency denylist file (a mounted ConfigMap), re-read as it changes; empty: none")
	fs.StringVar(&o.pool, "pool", "", "pool mode: the gateway pool whose snapshots this gateway serves")
	fs.StringVar(&o.bucket, "bucket", "", "pool mode: the state bucket, s3://<bucket>/<prefix> (or a directory, for tests)")
	fs.StringVar(&o.keys, "keys", "", "pool mode: a directory of the publishing keys this gateway trusts (*.pub, PEM; a KMS key's with a Key-ID header)")
	fs.StringVar(&o.checkpoint, "checkpoint", "", "pool mode: a file on this gateway's own disk for the last publication applied (its floor across restarts), with last-good.json beside it (the publication itself, served at a restart while the bucket is unreachable); empty: the trusted checkpoint alone")
	fs.StringVar(&o.gatewayID, "id", "", "pool mode: this gateway's name, for its status object (default the host name)")
	fs.StringVar(&o.valkeyAddr, "valkey", "", "pool mode: the site's Valkey (host:port) for the limiter")
	fs.IntVar(&o.replicaAcks, "valkey-replica-acks", 0, "pool mode: replicas each spend add waits for (WAIT), so a failover cannot lose it")
	fs.BoolVar(&o.valkeyTLS, "valkey-tls", false, "pool mode: connect to Valkey over TLS (ElastiCache's in-transit encryption), verifying its certificate against the system's roots for the address's host name")
	fs.StringVar(&o.valkeyCA, "valkey-ca", "", "pool mode, with -valkey-tls: a PEM file of the CA certificates to verify Valkey's certificate against, in place of the system's roots")
	fs.StringVar(&o.valkeyUser, "valkey-user", "", "pool mode: the Valkey user to AUTH as (an ACL user; ElastiCache RBAC); its password in $VALKEY_PASSWORD. Empty with the password set: AUTH as the default user")
	fs.StringVar(&o.site, "site", "", "pool mode: this gateway's site (its Valkey's keys and ledger rows)")
	fs.DurationVar(&o.maxSkew, "clock-skew", 2*time.Minute, "pool mode: how far the publisher's clock may run ahead of this gateway's: a pointer issued further in its future is refused")
	fs.DurationVar(&o.maxLease, "max-lease", 4*time.Hour, "pool mode: the longest authorization lease the pool's publisher grants (golem-control's -lease): a snapshot whose lease runs further ahead, plus -clock-skew, is refused")
	fs.StringVar(&o.secrets, "secrets", "/var/run/golem/secrets", "pool mode: where provider Secrets are mounted, one directory per Secret")
	fs.BoolVar(&o.acceptUnreleased, "accept-unreleased", false, "pool mode: run although golem#7's deployment evidence is not in (test sites only)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	return o, releaseGate(o)
}

func main() { os.Exit(run(context.Background(), os.Args[1:])) }

// run is golem-gateway, until ctx ends or SIGTERM: its exit status. It
// builds the gateway's parts in dependency order, serves, and on the way
// out stops them in the order that loses no accounting (lifecycle).
func run(ctx context.Context, args []string) int {
	o, err := parse(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golem-gateway:", err)
		return 2
	}
	// One period zone for the counters and the ledger rows: a call near
	// midnight on the last of the month belongs to one month in both.
	zone, err := time.LoadLocation(o.zoneName)
	if err != nil {
		return failed("-period-zone: %v", err)
	}
	site, pool := "local", "standalone"
	if o.standalone == "" {
		site, pool = o.site, o.pool
	}
	tel, meter, shutdownTelemetry, err := setupTelemetry(o.telem, site, pool)
	if err != nil {
		return failed("%v", err)
	}
	defer shutdownTelemetry() // last: it delivers the rows the calls wrote
	if tel != nil {
		tel.Ledger.Period = zone
	}
	entra := newEntra(o.tenant, o.audience, o.jwks)

	sig, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The background loops live through the drain (renewals keep calls'
	// slots), and end after it. Every goroutine is owned here.
	bg, stopBg := context.WithCancel(context.Background())
	defer stopBg() // also on a failed start
	var wg sync.WaitGroup

	limiter, connectStore, closeStore, err := newLimiter(o, zone)
	if err != nil {
		return failed("%v", err)
	}
	defer closeStore()
	if connectStore != nil {
		wg.Go(func() { connectStore(bg) })
	}
	calls := &proxy.InFlight{}
	holder := newHolder(entra, limiter, calls)
	if o.denylist != "" {
		// Read before the source loads: a gateway started during an
		// incident must not admit what the list refuses, even once, and the
		// last-good publication a pool source applies at its start is
		// compiled with it.
		deny := newDenyFile(o.denylist, calls)
		if err := deny.Load(); err != nil {
			return failed("-denylist: %v", err)
		}
		holder.Deny = deny
		wg.Go(func() { deny.Run(bg) })
	}
	src, keyPath, err := startSource(bg, o, holder)
	if err != nil {
		return failed("%v", err)
	}
	if meter != nil {
		// Read at each collection: nothing on the request path.
		state := telemetry.State{Holder: holder, Limiter: limiter, Deny: holder.Deny}
		state.Source, _ = src.(*snapshot.Bucket)
		if err := telemetry.ObserveState(meter, state); err != nil {
			return failed("telemetry: %v", err)
		}
	}
	wg.Go(func() { src.Run(bg) })
	wg.Go(func() { limiter.Run(bg) })
	if entra != nil {
		wg.Go(func() { entra.Run(bg) })
	}

	lc := &lifecycle{unready: o.unready, drain: o.drainFor, cut: 10 * time.Second}
	// One view of the deployments' health and the routes' retry budgets for
	// every dialect's calls, as they share deployments, kept by the active
	// checks of those that have one (a local box).
	health := &route.Health{}
	checker := &route.Checker{Health: health}
	wg.Go(func() {
		checker.Run(bg, func() []*route.Target {
			if c := holder.Applied(); c != nil {
				return c.Routes.Targets()
			}
			return nil
		})
	})
	srv := newServer(o.listen, newMux(o, lc, holder, limiter, tel, calls, health))
	lc.srv = srv
	drained := make(chan struct{})
	wg.Go(func() {
		<-sig.Done()
		lc.shutdown()
		close(drained)
	})
	announce(o, holder, keyPath)
	code := 0
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) { // returns as the shutdown begins
		slog.Error("golem-gateway", "err", err)
		code = 1
	}
	stop() // a failed listen starts the shutdown too
	<-drained
	stopBg()
	wg.Wait()
	fctx, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
	limiter.Finish(fctx)
	fcancel()
	return code
}

// newLimiter is the limiter over its store: in memory standalone (ADR
// 0023), the site's Valkey in pool mode. A Valkey unreachable at the start
// does not stop the gateway: the limiter starts in its outage mode
// (waivers, increments held) and connect, run in the background, dials
// until it answers; Run's Recover then brings the limiter back. connect is
// nil when there is nothing to dial; closeStore closes the store's client.
func newLimiter(o options, zone *time.Location) (limiter *limit.Limiter, connect func(context.Context), closeStore func(), err error) {
	var store limit.Store = limit.NewMem()
	closeStore = func() {}
	var down error
	if o.standalone == "" {
		opt, err := valkeyOption(o, os.Getenv)
		if err != nil {
			return nil, nil, nil, err
		}
		v, err := limit.DialValkey(o.site, func() (valkey.Client, error) { return valkey.NewClient(opt) })
		if err != nil {
			down = fmt.Errorf("-valkey %s: %w", o.valkeyAddr, err)
			slog.Error("golem-gateway: the site's Valkey is unreachable; starting with the limiter in its outage mode (calls admitted on local counts, recorded as waivers) and retrying the connection",
				"valkey", o.valkeyAddr, "err", err)
		}
		v.ReplicaAcks = o.replicaAcks
		store, connect, closeStore = v, v.Connect, v.Close
	}
	limiter = limit.New(store)
	limiter.Location = zone
	if down != nil {
		limiter.MarkDown(down)
	}
	return limiter, connect, closeStore, nil
}

// valkeyOption is the site's Valkey client's options: its address, TLS and
// AUTH from the flags, the password from getenv's VALKEY_PASSWORD (a flag
// would show it in the process list and the pod spec).
func valkeyOption(o options, getenv func(string) string) (valkey.ClientOption, error) {
	opt := valkey.ClientOption{InitAddress: []string{o.valkeyAddr}, DisableCache: true,
		Username: o.valkeyUser, Password: getenv("VALKEY_PASSWORD")}
	if o.valkeyCA != "" && !o.valkeyTLS {
		return opt, errors.New("-valkey-ca is the CA for -valkey-tls: add -valkey-tls, or drop -valkey-ca")
	}
	if o.valkeyUser != "" && opt.Password == "" {
		return opt, fmt.Errorf("-valkey-user %s needs its password in $VALKEY_PASSWORD", o.valkeyUser)
	}
	if !o.valkeyTLS {
		return opt, nil
	}
	// No ServerName: tls.Dialer takes each dial's host, so a cluster's
	// nodes, discovered at run time, are each verified by their own name.
	opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if o.valkeyCA != "" {
		pem, err := os.ReadFile(o.valkeyCA)
		if err != nil {
			return opt, fmt.Errorf("-valkey-ca: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return opt, fmt.Errorf("-valkey-ca %s holds no PEM certificate: give the CA's certificate, PEM-encoded", o.valkeyCA)
		}
		opt.TLSConfig.RootCAs = roots
	}
	return opt, nil
}

// newHolder is the snapshot holder of a live installation (startSource
// makes it a dev one standalone), handing each snapshot's known spend to
// the limiter as it is applied (ADR 0048).
// It also ends the calls in flight the snapshot withdraws (a key revoked,
// a principal gone from the key's scope or from golem), as soon as it is
// in force: calls is the gateway's registry of them.
func newHolder(v *entra.Verifier, limiter *limit.Limiter, calls *proxy.InFlight) *snapshot.Holder {
	h := &snapshot.Holder{Env: key.Live, Entra: v, AWS: awsCredentials()}
	h.OnApply = func(c *snapshot.Compiled) {
		for _, s := range c.Snapshot.Entitlements.Scopes {
			limiter.ApplySnapshot(context.Background(), s.ID, limit.Known{Period: s.Period, Spend: s.KnownSpend, Since: s.Since})
		}
		sweep(calls, "snapshot applied", "publish_seq", c.Snapshot.PublishSeq)
	}
	calls.Keys = func() *auth.Keys {
		if c := h.Applied(); c != nil { // its lease expired or not: expiry ends no call
			return c.Auth
		}
		return nil
	}
	return h
}

// awsCredentials signs bedrock deployments' requests: the AWS SDK's
// default chain (the pod's role through IRSA or Pod Identity, else AWS_*
// environment credentials), cached and refreshed before they expire. None
// is fetched until a bedrock call is made; nil if the chain cannot be set
// up (a bedrock call then fails with what to configure).
func awsCredentials() aws.CredentialsProvider {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		slog.Warn("no AWS credentials for bedrock deployments", "err", err)
		return nil
	}
	return cfg.Credentials
}

// newDenyFile is the site's emergency denylist at path: each change it
// puts in force ends the calls in flight it now denies.
func newDenyFile(path string, calls *proxy.InFlight) *auth.DenyFile {
	return &auth.DenyFile{Path: path, Log: slog.Default(),
		OnApply: func(d *auth.Denylist) { sweep(calls, "denylist applied", "entries", d.Len()) }}
}

// sweep ends the calls in flight the current keys no longer honour, and
// logs how many it ended, with why it ran.
func sweep(calls *proxy.InFlight, why string, args ...any) {
	if n := calls.Sweep(); n > 0 {
		slog.Warn("golem-gateway: ended calls in flight whose authorization was withdrawn", append([]any{"after", why, "ended", n}, args...)...)
	}
}

// startSource is the holder's snapshot source, loaded once before the
// gateway serves: standalone's directory, with the dev installation and
// its local key (in the file keyPath), or the pool's bucket.
func startSource(ctx context.Context, o options, holder *snapshot.Holder) (src interface{ Run(context.Context) }, keyPath string, err error) {
	if o.standalone == "" {
		b, err := poolSource(ctx, o, holder)
		if err != nil {
			return nil, "", err
		}
		return b, "", nil
	}
	holder.Env = key.Dev
	if holder.LocalKey, keyPath, err = loadLocalKey(o.localKeyFile); err != nil {
		return nil, "", fmt.Errorf("the local key: %w", err)
	}
	dir := &snapshot.Dir{Path: o.standalone, Holder: holder, Secrets: snapshot.EnvSecrets{Getenv: os.Getenv}}
	if err := dir.Load(); err != nil {
		return nil, "", err
	}
	return dir, keyPath, nil
}

// newMux is the gateway's routes: the two dialects' APIs, each call
// counted by lc (and, standalone, behind localOnly), and registered in
// calls while in flight; and the probes.
func newMux(o options, lc *lifecycle, holder *snapshot.Holder, limiter *limit.Limiter, tel *telemetry.Telemetry, calls *proxy.InFlight, health *route.Health) *http.ServeMux {
	mux := http.NewServeMux()
	for pattern, d := range map[string]proxy.Dialect{"POST /v1/messages": proxy.AnthropicMessages, "POST /v1/chat/completions": proxy.OpenAIChat,
		"POST /v1/responses": proxy.OpenAIResponses} {
		var h http.Handler = &proxy.Handler{Dialect: d, Snapshots: holder, Limiter: limiter.Admitter(), MaxBody: o.maxBody, Telemetry: tel, InFlight: calls, Health: health}
		if o.standalone != "" {
			h = localOnly(d, h)
		}
		mux.Handle(pattern, lc.track(d, h))
	}
	// The models list, either dialect's shape (ServeModels picks it): the
	// Messages handler serves it, with the same snapshots and identity.
	var models http.Handler = http.HandlerFunc((&proxy.Handler{Dialect: proxy.AnthropicMessages, Snapshots: holder}).ServeModels)
	if o.standalone != "" {
		models = loopbackOnly(proxy.AnthropicMessages, models)
	}
	for _, pattern := range []string{"GET /v1/models", "GET /v1/models/{id}"} {
		mux.Handle(pattern, lc.track(proxy.AnthropicMessages, models))
	}
	// A stored response's retrieval, deletion, cancellation or input items:
	// M1 stores none (security-and-correctness.md §4).
	mux.HandleFunc("/v1/responses/{rest...}", func(w http.ResponseWriter, _ *http.Request) {
		proxy.Refuse(w, proxy.OpenAIResponses, contract.AuthzProviderStorageForbidden,
			"golem serves stateless responses only: no response is stored to retrieve, delete or cancel")
	})
	// Anthropic's token count: free, no admission, no ledger row.
	var count http.Handler = http.HandlerFunc((&proxy.Handler{Dialect: proxy.AnthropicMessages, Snapshots: holder, MaxBody: o.maxBody}).ServeCountTokens)
	if o.standalone != "" {
		count = localOnly(proxy.AnthropicMessages, count)
	}
	mux.Handle("POST /v1/messages/count_tokens", lc.track(proxy.AnthropicMessages, count))
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	// Ready once a snapshot is applied and its authorization holds.
	mux.HandleFunc("GET /readyz", lc.readyz(func() error { _, err := holder.Current(); return err }))
	return mux
}

// announce says what the gateway serves: standalone, on stderr for the
// developer, with where its local key is; in pool mode, in the log.
func announce(o options, holder *snapshot.Holder, keyPath string) {
	if o.standalone == "" {
		slog.Info("golem-gateway: serving", "pool", o.pool, "site", o.site, "listen", o.listen)
		return
	}
	fmt.Fprintf(os.Stderr, "golem-gateway standalone: %s on http://%s — for this machine only, not for shared use\n", o.standalone, o.listen)
	if c, err := holder.Current(); err == nil && c.Snapshot.Entitlements.Implicit {
		fmt.Fprintf(os.Stderr, "  its local key is in %s: export ANTHROPIC_API_KEY=$(cat %s) OPENAI_API_KEY=$(cat %s)\n", keyPath, keyPath, keyPath)
	}
}

// poolSource is the pool's bucket source, its floor raised (Start) before
// the gateway serves: a gateway that cannot tell a replayed pointer from
// the current one does not start.
func poolSource(ctx context.Context, o options, holder *snapshot.Holder) (*snapshot.Bucket, error) {
	b, err := s3bucket.Open(ctx, o.bucket)
	if err != nil {
		return nil, fmt.Errorf("-bucket: %w", err)
	}
	keys, err := sign.LoadKeys(o.keys)
	if err != nil {
		return nil, fmt.Errorf("-keys: %w", err)
	}
	root, err := os.OpenRoot(o.secrets)
	if err != nil {
		return nil, fmt.Errorf("-secrets: %w", err)
	}
	id := o.gatewayID
	if id == "" {
		if id, err = os.Hostname(); err != nil {
			return nil, fmt.Errorf("-id: %w", err)
		}
	}
	src := &snapshot.Bucket{Bucket: b, Keys: keys, Pool: o.pool, Holder: holder, Secrets: snapshot.FileSecrets{Root: root},
		Checkpoint: o.checkpoint, LastGood: lastGoodPath(o.checkpoint), GatewayID: id, MaxSkew: o.maxSkew, MaxLease: o.maxLease, Log: slog.Default()}
	if err := src.Start(ctx); err != nil {
		return nil, fmt.Errorf("pool %s: %w", o.pool, err)
	}
	if err := src.Poll(ctx); err != nil {
		// Not fatal: the gateway serves its last-good publication if it
		// had one (Start), is not ready until one applies, and polls on.
		slog.Error("golem-gateway: the pool's publication was not read", "pool", o.pool, "err", err)
	}
	return src, nil
}

// newEntra is the verifier of tenant's Entra tokens; nil (golem keys only)
// without a tenant. Its signing keys are fetched once here, but a failure
// does not stop the gateway: golem keys work meanwhile, Entra tokens are
// refused as not yet checkable (gateway.not_ready, retryable) and the
// verifier's Run retries the fetch with backoff.
func newEntra(tenant, audiences, jwks string) *entra.Verifier {
	if tenant == "" {
		return nil
	}
	var auds []string
	for a := range strings.SplitSeq(audiences, ",") {
		if a = strings.TrimSpace(a); a != "" {
			auds = append(auds, a)
		}
	}
	e := &entra.Verifier{Tenant: tenant, Audiences: auds, JWKS: jwks}
	if err := e.Refresh(context.Background()); err != nil {
		slog.Error("golem-gateway: Entra's signing keys not fetched; starting without them (Entra tokens refused until a retry fetches them, golem keys served)", "tenant", tenant, "err", err)
	}
	return e
}

// newServer is the API's server, its limits set for long streams over
// HTTP/1.1 and h2c.
func newServer(addr string, h http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // h2c behind the mesh (gateway.md §3 step 1)
	return &http.Server{
		Addr:              addr,
		Handler:           h,
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
}

// setupTelemetry starts the gateway's telemetry: ledger rows on stdout, none,
// or everything over OTLP to a collector. meter is the meter provider for
// the gateway's state gauges (no-op on stdout; nil when off). The returned
// func flushes it.
func setupTelemetry(where, site, pool string) (_ *telemetry.Telemetry, meter metric.MeterProvider, _ func(), _ error) {
	var p telemetry.Providers
	shutdown := func(context.Context) error { return nil }
	switch where {
	case "off":
		return nil, nil, func() {}, nil
	case "stdout":
		p = telemetry.Stdout(os.Stdout)
	default:
		res := resource.NewSchemaless(attribute.String("service.name", "golem-gateway"), attribute.String("golem.site", site), attribute.String("golem.pool", pool))
		var err error
		if p, shutdown, err = telemetry.OTLP(context.Background(), where, res); err != nil {
			return nil, nil, nil, fmt.Errorf("telemetry to %s: %w", where, err)
		}
	}
	tel, err := telemetry.New(p, site, pool)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("telemetry: %w", err)
	}
	return tel, p.Meter, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			slog.Warn("golem-gateway: telemetry not flushed", "err", err)
		}
	}, nil
}

// releaseGate is the production gate (golem#5, #7). Standalone listens on
// loopback only. Pool mode needs every one of its inputs, and runs only
// with -accept-unreleased until golem#7's deployment evidence is in:
// provider-account spending limits, OTLP TLS against the real collector,
// and the mTLS owner of each hop.
func releaseGate(o options) error {
	switch {
	case o.standalone != "" && o.pool != "":
		return errors.New("-standalone or -pool, not both")
	case o.standalone != "":
		if host, _, err := net.SplitHostPort(o.listen); err != nil || !isLoopback(host) {
			return fmt.Errorf("standalone mode listens on a loopback address only (got %q): it is not for shared use (ADR 0023)", o.listen)
		}
		return nil
	case o.pool == "":
		return errors.New("-standalone <dir>, or -pool <name> with its bucket, keys, Valkey and site")
	}
	var missing []string
	for name, v := range map[string]string{"-bucket": o.bucket, "-keys": o.keys, "-valkey": o.valkeyAddr, "-site": o.site} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("pool mode needs %s", strings.Join(missing, ", "))
	}
	if o.maxLease <= 0 || o.maxSkew < 0 {
		return fmt.Errorf("-max-lease %s and -clock-skew %s: the lease must be positive and the skew not negative", o.maxLease, o.maxSkew)
	}
	if !o.acceptUnreleased {
		return errors.New("pool mode is not released: golem#7's deployment evidence (provider spending limits, OTLP TLS to the real collector, each hop's mTLS owner) is not in; -accept-unreleased runs it on a test site")
	}
	return nil
}

// loadLocalKey reads standalone's local key, or makes one: a dev key in a
// file only its user may read, kept across restarts so the developer's
// tools keep working (ADR 0023; golem#5).
func loadLocalKey(path string) (k, at string, err error) {
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", "", err
		}
		path = filepath.Join(dir, "golem", "standalone-key")
	}
	if b, err := os.ReadFile(path); err == nil {
		k := strings.TrimSpace(string(b))
		if env, err := key.Parse(k); err == nil && env == key.Dev {
			return k, path, nil
		}
		return "", "", fmt.Errorf("%s holds no glm_dev_ key: remove it, and a new one is made", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}
	k = key.New(key.Dev)
	if err := os.WriteFile(path, []byte(k+"\n"), 0o600); err != nil {
		return "", "", err
	}
	return k, path, nil
}

// failed reports a start-up failure: exit status 1.
func failed(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "golem-gateway: "+format+"\n", args...)
	return 1
}

// lastGoodPath is the last-good publication's file, beside the checkpoint;
// none without one.
func lastGoodPath(checkpoint string) string {
	if checkpoint == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(checkpoint), "last-good.json")
}
