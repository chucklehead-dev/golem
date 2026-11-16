// golem-control publishes the config's snapshots to the bucket while it
// leads. The config comes from a git checkout and the entitlements from the
// state document (control-plane.md). Limitations: every pool gets the same
// snapshot; there are no pool selectors and no CRD watch.
//
//	golem-control -config ./config -bucket ./bucket -signing-key file:publishing.key -pools internal
//	golem-control -config ./config -bucket s3://golem-state/golem/ -signing-key kms:arn:aws:kms:... -pools internal,edge
//	golem-control keygen ./keys   (a development Ed25519 key pair)
//	golem-control pubkey kms:arn:aws:kms:...   (the publishing key's public half, for gateways' -keys)
//	golem-control restore -bucket s3://golem-state/golem/ -from old.json -actor alice [-apply]   (an older document's entitlements, never widening)
//	golem-control bootstrap -bucket s3://golem-state/golem/ -keys ./keys -pools internal,edge -actor alice [-adopt]   (a deleted document started over, or an older one adopted)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/chucklehead-dev/golem/control/api"
	"github.com/chucklehead-dev/golem/control/audit"
	"github.com/chucklehead-dev/golem/control/controller"
	"github.com/chucklehead-dev/golem/control/entrasync"
	"github.com/chucklehead-dev/golem/control/metrics"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/internal/flagusage"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/bucket/s3bucket"
	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/otlp"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/sign/kmssign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "golem-control: %v\n", err)
		os.Exit(2)
	}
}

// dispatch runs a subcommand (keygen, pubkey, restore, bootstrap), or golem-control
// itself given anything else, flags included.
func dispatch(args []string) error {
	ctx := context.Background()
	if len(args) == 0 {
		return run(args)
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return errors.New("usage: golem-control keygen <dir>")
		}
		return keygen(args[1])
	case "pubkey":
		if len(args) != 2 {
			return errors.New("usage: golem-control pubkey file:<key> | kms:<arn>")
		}
		return pubkey(ctx, os.Stdout, args[1])
	case "restore":
		return restore(ctx, os.Stdout, args[1:])
	case "bootstrap":
		return bootstrap(ctx, os.Stdout, args[1:])
	}
	return run(args)
}

// installed is what shows golem-control has run on a bucket publishing
// pools (a comma-separated list): each pool's checkpoint and pointer. The
// bucket interface cannot list, so these are looked for by name
// (state.Store.Installed).
func installed(pools string) []string {
	var keys []string
	for _, p := range list(pools) {
		keys = append(keys, snapshot.CheckpointKey(p), snapshot.PointerKey(p))
	}
	return keys
}

// bootstrap creates the state document deliberately where golem-control
// has run before (state.Store.Bootstrap), each pool's record at what the
// bucket proves was published (its checkpoint and pointer, verified with
// -keys), so publication goes on above what gateways trust. With -adopt it
// instead accepts the document that is there, an older version put back,
// raising its records the same way. Stop every golem-control replica
// first: one still running refuses the new document as having gone back.
func bootstrap(ctx context.Context, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	where := fs.String("bucket", "", "the state bucket: a directory, or s3://bucket/prefix/")
	keysDir := fs.String("keys", "", "the publishing keys' public halves (*.pub), as gateways' -keys: the checkpoints and pointers are verified with them")
	pools := fs.String("pools", "internal", "the gateway pools golem-control publishes, comma-separated")
	actor := fs.String("actor", "", "who bootstraps, for the audit event (required)")
	adopt := fs.Bool("adopt", false, "accept the document that is there (an older version put back) instead of creating one")
	outboxMax := fs.Int("audit-outbox-max", state.DefaultMaxOutbox, "the audit outbox's bound, as golem-control's -audit-outbox-max")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *where == "" || *keysDir == "" || *actor == "" || fs.NArg() > 0 {
		return errors.New("usage: golem-control bootstrap -bucket <dir|s3://...> -keys <dir> -pools <a,b> -actor <who> [-adopt]")
	}
	keys, err := sign.LoadKeys(*keysDir)
	if err != nil {
		return fmt.Errorf("-keys: %w", err)
	}
	b, err := openBucket(ctx, *where)
	if err != nil {
		return err
	}
	st := &state.Store{Bucket: b, MaxOutbox: *outboxMax}
	floors := map[string]uint64{}
	for _, p := range list(*pools) {
		f, err := (&publish.Publisher{State: st, Bucket: b, Keys: keys, Pool: p}).Floor(ctx)
		if err != nil {
			return fmt.Errorf("pool %s (nothing written): %w", p, err)
		}
		floors[p] = f
	}
	d, err := st.Bootstrap(ctx, floors, *actor, *adopt)
	if err != nil {
		return fmt.Errorf("bootstrap refused: %w", err)
	}
	verb := "created"
	if *adopt {
		verb = "adopted"
	}
	fmt.Fprintf(w, "%s %s at state version %d\n", state.DocumentKey, verb, d.StateVersion)
	for _, p := range slices.Sorted(maps.Keys(floors)) {
		fmt.Fprintf(w, "  pool %s: publication record %d (the bucket proves %d)\n", p, d.Publications[p].Seq, floors[p])
	}
	if !*adopt {
		fmt.Fprintln(w, "the document holds no principals, scopes or keys: every caller is refused until they are created again")
	} else {
		fmt.Fprintln(w, "publication resumes from this document: a key revoked after its version and not revoked again works again (the audit archive lists those revocations)")
	}
	return nil
}

// restore brings an older state document's entitlements back without
// widening anyone's rights (state.Restore): a dry run by default, which
// prints what would be withheld and rolled back; written with -apply. The
// backup is a document file, an old version taken from the bucket's
// history or its Object Lock replica (control-plane.md).
func restore(ctx context.Context, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	where := fs.String("bucket", "", "the state bucket: a directory, or s3://bucket/prefix/")
	from := fs.String("from", "", "the backup: a state document file")
	actor := fs.String("actor", "", "who restores, for the audit event (required)")
	apply := fs.Bool("apply", false, "write the restore; without it, only show what it would do")
	outboxMax := fs.Int("audit-outbox-max", state.DefaultMaxOutbox, "the audit outbox's bound, as golem-control's -audit-outbox-max: a restore is restrictive, and lands until the bound")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *where == "" || *from == "" || *actor == "" || fs.NArg() > 0 {
		return errors.New("usage: golem-control restore -bucket <dir|s3://...> -from <document.json> -actor <who> [-apply]")
	}
	raw, err := os.ReadFile(*from)
	if err != nil {
		return err
	}
	backup, err := state.Decode(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", *from, err)
	}
	b, err := openBucket(ctx, *where)
	if err != nil {
		return err
	}
	st := &state.Store{Bucket: b, MaxOutbox: *outboxMax}
	report, err := st.Restore(ctx, backup, *actor, !*apply)
	if err != nil {
		return fmt.Errorf("restore refused (nothing written): %w", err)
	}
	verb := "would be"
	if *apply {
		verb = "were"
	}
	fmt.Fprintf(w, "restoring state version %d: %d right(s) in the backup %s withheld, %d change(s) since it %s rolled back\n",
		backup.StateVersion, len(report.Withheld), verb, len(report.Removed), verb)
	for _, l := range report.Withheld {
		fmt.Fprintln(w, "  withheld:", l, "(re-grant it as its own change if it is wanted)")
	}
	for _, l := range report.Removed {
		fmt.Fprintln(w, "  rolled back:", l)
	}
	if !*apply {
		fmt.Fprintln(w, "nothing written: run again with -apply")
	}
	return nil
}

// options are golem-control's flags.
type options struct {
	config, bucket, signingKey, pools, id string
	configLink, configFetched             string
	maxConfigAge                          time.Duration
	trustedKeys                           string
	authLease                             time.Duration
	spendStaleAfter                       time.Duration
	listen, probes                        string
	drain                                 time.Duration
	archive, telemetry                    string
	outboxMax                             int
	tenant, audience                      string
	syncClient                            string
	syncEvery                             time.Duration
	syncMax                               int
	clickhouse, clickhouseUser            string
	allowance                             time.Duration
	periodZone                            string
}

// flagGroups are golem-control's flags as -help prints them.
var flagGroups = []flagusage.Group{ // read-only
	{Title: "Publishing", Flags: []string{"config", "config-link", "config-fetched", "max-config-age", "bucket", "pools",
		"signing-key", "trusted-keys", "lease", "period-zone", "id", "probes", "telemetry"}},
	{Title: "The control API", Flags: []string{"listen", "drain", "entra-tenant", "entra-audience"}},
	{Title: "The audit", Flags: []string{"audit-archive", "audit-outbox-max"}},
	{Title: "Entra group sync", Flags: []string{"entra-sync-client-id", "entra-sync-every", "entra-sync-max-removals"}},
	{Title: "Known spend", Flags: []string{"clickhouse", "clickhouse-user", "spend-allowance", "spend-stale-after"},
		Env: []flagusage.Env{{Name: "CLICKHOUSE_PASSWORD", Usage: "the password of -clickhouse-user; never a flag, which the process list would show"}}},
}

// parseFlags reads golem-control's flags and checks those that need each
// other. -help, or a flag it does not know, exits with the usage.
func parseFlags(args []string) (options, error) {
	return parseFlagsInto(flag.NewFlagSet("golem-control", flag.ExitOnError), args)
}

// parseFlagsInto is parseFlags with fs, so a test can read its usage.
func parseFlagsInto(fs *flag.FlagSet, args []string) (options, error) {
	var o options
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: golem-control -config <dir> -bucket <dir|s3://...> -signing-key <file:...|kms:...> [flags]")
		fmt.Fprintln(fs.Output(), "       golem-control keygen | pubkey | restore | bootstrap ...")
		flagusage.Print(fs.Output(), fs, flagGroups)
	}
	fs.StringVar(&o.config, "config", "", "the config YAML's directory")
	fs.StringVar(&o.configLink, "config-link", "", "git-sync's --link, which -config is within; its target's commit is published as each snapshot's configRevision; empty: no revision")
	fs.StringVar(&o.configFetched, "config-fetched", "", "a file whose modification time is the checkout's last fetch, such as git-sync's <root>/.git/FETCH_HEAD; empty: the checkout's age is not bounded")
	fs.DurationVar(&o.maxConfigAge, "max-config-age", 30*time.Minute, "with -config-fetched, how old the checkout may be before leases stop being renewed; changes are still published; 0: no bound (see docs/operations.md)")
	fs.StringVar(&o.bucket, "bucket", "", "the state bucket: a directory, or s3://<bucket>/<prefix>")
	fs.StringVar(&o.signingKey, "signing-key", "", "the publishing key: file:<an Ed25519 PKCS#8 PEM> or kms:<a KMS key ARN, ECC_NIST_P256>; every gateway's -keys must hold its public half")
	fs.StringVar(&o.trustedKeys, "trusted-keys", "", "during a key rotation: a directory of the retiring keys' public halves (*.pub), still trusted until re-signed; empty: the signing key's own only")
	fs.StringVar(&o.pools, "pools", "internal", "the gateway pools to publish to, comma-separated")
	fs.StringVar(&o.id, "id", "", "this replica's name (default the host name)")
	fs.DurationVar(&o.authLease, "lease", 4*time.Hour, "the snapshots' authorization lease: four hours for internal pools, five minutes for the edge; at most every gateway's -max-lease, or they refuse every snapshot")
	fs.StringVar(&o.listen, "listen", "", "serve the control API (Connect: gRPC, gRPC-Web, HTTP/JSON) here; empty: no API")
	fs.StringVar(&o.probes, "probes", "", "serve GET /livez and /readyz here, ready while this replica holds the fence; empty: none")
	fs.DurationVar(&o.drain, "drain", 10*time.Second, "at shutdown, how long the control API's calls in flight are given to finish")
	fs.StringVar(&o.archive, "audit-archive", "", "the audit archive, an Object Lock bucket in a separate account: a directory, or s3://<bucket>/<prefix>; empty: the outbox is not shipped")
	fs.IntVar(&o.outboxMax, "audit-outbox-max", state.DefaultMaxOutbox, "the audit outbox's bound, in events; past 80% of it, changes that grant access are refused (see docs/operations.md)")
	fs.StringVar(&o.telemetry, "telemetry", "off", "off, or a collector's OTLP/HTTP host:port for metrics and the audit's log records, TLS unless a literal loopback address. Off by default: golem-control writes no ledger, unlike the gateway")
	fs.StringVar(&o.tenant, "entra-tenant", "", "the Entra tenant whose tokens the API takes; required with -listen")
	fs.StringVar(&o.audience, "entra-audience", "api://golem-control", "the control API registration's audiences in Entra tokens, comma-separated; never the gateway's")
	fs.StringVar(&o.syncClient, "entra-sync-client-id", "", "the jobs app registration's client ID, to sync the bound Entra groups' members by workload identity; empty: no sync")
	fs.DurationVar(&o.syncEvery, "entra-sync-every", 5*time.Minute, "how often the group sync runs")
	fs.IntVar(&o.syncMax, "entra-sync-max-removals", 50, "the most memberships of still-active users one sync may end; past it, only offboarding is written")
	fs.StringVar(&o.clickhouse, "clickhouse", "", "the ledger's ClickHouse HTTP interface (https://host:8443), for known spend; its password in $CLICKHOUSE_PASSWORD; empty: snapshots carry no known spend")
	fs.StringVar(&o.clickhouseUser, "clickhouse-user", "golem_control", "the ClickHouse user known spend is read as, read-only on golem.spend")
	fs.DurationVar(&o.spendStaleAfter, "spend-stale-after", spend.DefaultStaleAfter, "a known-spend result whose boundary is older than this is reported stale; it stays published")
	fs.DurationVar(&o.allowance, "spend-allowance", 5*time.Minute, "how long after a call ends its ledger row is surely in ClickHouse; known spend's boundary is this far back")
	fs.StringVar(&o.periodZone, "period-zone", "UTC", "the budget periods' time zone, an IANA name, stated in every snapshot; every gateway's -period-zone must be the same")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.config == "" || o.bucket == "" || o.signingKey == "" {
		return o, errors.New("-config, -bucket and -signing-key are required")
	}
	if o.maxConfigAge < 0 {
		return o, fmt.Errorf("-max-config-age %s: not negative (0 for no bound)", o.maxConfigAge)
	}
	if o.outboxMax < 10 {
		return o, fmt.Errorf("-audit-outbox-max %d: at least 10, so its last 20%% is kept for restrictive changes (default %d)", o.outboxMax, state.DefaultMaxOutbox)
	}
	return o, nil
}

// run is golem-control: set up, then run until SIGTERM or a component
// fails. Shutdown is in order: the publishing loop stops (no publication
// left half-done) while the API drains (its calls in flight finish, up to
// -drain), then the audit's log records are flushed, then the metrics'
// last collection is exported. golem-control runs as one replica (ADR
// 0053): it claims the state document's fence as it starts, and exits if
// a successor claims over it.
// Errors are returned, never an exit from a goroutine, so every deferred
// step runs.
func run(args []string) error {
	o, err := parseFlags(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b, err := openBucket(ctx, o.bucket)
	if err != nil {
		return fmt.Errorf("-bucket: %w", err)
	}
	signer, err := openSigner(ctx, o.signingKey)
	if err != nil {
		return fmt.Errorf("-signing-key: %w", err)
	}
	replica := o.id
	if replica == "" {
		if replica, err = os.Hostname(); err != nil {
			return fmt.Errorf("-id: %w", err)
		}
	}
	st := &state.Store{Bucket: b, Installed: installed(o.pools), MaxOutbox: o.outboxMax}
	if err := st.Load(ctx); err != nil {
		return fmt.Errorf("the state document: %w", err)
	}
	c, closeAudit, err := newController(ctx, o, st, b, signer, replica)
	if err != nil {
		return err
	}
	// The one leadership step (ADR 0053): this replica claims the fence at
	// one above the document's epoch, so a predecessor still running is
	// refused at its next fenced write.
	if err := c.Claim(ctx); err != nil {
		closeAudit()
		return err
	}
	closeMetrics, err := openMetrics(ctx, o.telemetry, replica, c)
	if err != nil {
		closeAudit()
		return err
	}
	defer func() { closeAudit(); closeMetrics() }() // the audit's records, then the metrics' last collection
	slog.Info("golem-control: running", "replica", replica, "pools", o.pools, "signing key", signer.KeyID())
	// The settings that must agree with every gateway's, in one line to
	// compare with theirs (docs/operations.md).
	slog.Info("golem-control: settings shared with the gateways", "lease", o.authLease, "period-zone", o.periodZone,
		"signing key ID", signer.KeyID(), "trusted-keys", o.trustedKeys)
	// Built before any goroutine starts: a failure here returns with
	// nothing left running.
	var srv *http.Server
	var refreshKeys func(context.Context)
	if o.listen != "" {
		if srv, refreshKeys, err = apiServer(o.listen, o.tenant, o.audience, "", &service.Service{State: st, ScopeModels: c.ScopeModels}); err != nil {
			return fmt.Errorf("-listen: %w", err)
		}
	}
	// A component's failure stops the rest; the first is the cause.
	ctx, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	var wg sync.WaitGroup
	wg.Go(func() { st.Run(ctx) })
	if o.probes != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
		mux.HandleFunc("GET /readyz", readyz(c))
		probes := &http.Server{Addr: o.probes, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		defer context.AfterFunc(ctx, func() { probes.Close() })()
		wg.Go(func() {
			if err := probes.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fail(fmt.Errorf("-probes: %w", err))
			}
		})
	}
	if srv != nil {
		wg.Go(func() { refreshKeys(ctx) })
		// Drained when the context ends; serveAPI returns only once the
		// calls in flight have finished (or -drain passed), so the wait
		// below follows the drain.
		wg.Go(func() {
			if err := serveAPI(ctx, srv, srv.ListenAndServe, o.drain); err != nil {
				fail(fmt.Errorf("the control API: %w", err)) // read after wg.Wait
			}
		})
		slog.Info("golem-control: the control API is listening", "addr", o.listen)
	}
	// Until SIGTERM, another component cancels, or a successor claims the
	// fence over this replica (controller.ErrDeposed): it then exits.
	if err := c.Run(ctx); err != nil {
		fail(err)
	}
	wg.Wait() // the state's loop, and the API drained
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	slog.Info("golem-control: stopped")
	return nil
}

// newController wires the publishing loop: a publisher per
// pool over st, and the jobs the flags ask for (audit shipping, known
// spend, the group sync). closeAudit flushes the audit's log records; it
// is a no-op without them, and has run already when err is set.
func newController(ctx context.Context, o options, st *state.Store, b bucket.Bucket, signer sign.Signer, replica string) (c *controller.Controller, closeAudit func(), err error) {
	closeAudit = func() {}
	defer func() {
		if err != nil {
			closeAudit()
		}
	}()
	// The publishing key's own public half is always trusted; -trusted-keys
	// adds the retiring ones during a rotation (docs/operations.md).
	var trusted sign.Verifier
	if o.trustedKeys != "" {
		k, err := sign.LoadKeys(o.trustedKeys)
		if err != nil {
			return nil, closeAudit, fmt.Errorf("-trusted-keys: %w", err)
		}
		trusted = k
	}
	// Checked whether or not known spend uses it: every snapshot states it,
	// and gateways of another zone refuse the snapshot.
	zone, err := time.LoadLocation(o.periodZone)
	if err != nil {
		return nil, closeAudit, fmt.Errorf("-period-zone: %w", err)
	}
	c = &controller.Controller{Dir: o.config, Lease: o.authLease, PeriodZone: o.periodZone,
		State: st, ID: replica, Publishers: map[string]*publish.Publisher{}}
	if o.configLink != "" || o.configFetched != "" {
		c.Checkout = &controller.Checkout{Link: o.configLink, Fetched: o.configFetched}
		c.MaxConfigAge = o.maxConfigAge
	} else {
		slog.Warn("golem-control: no -config-fetched: the config checkout's age is not bounded, and a stalled git-sync's last checkout is renewed for as long as golem-control runs")
	}
	for _, p := range list(o.pools) {
		c.Publishers[p] = &publish.Publisher{State: st, Bucket: b, Signer: signer, Keys: trusted, Pool: p}
	}
	if o.archive != "" {
		s, flush, err := openAudit(ctx, st, o.archive, o.telemetry, replica)
		if err != nil {
			return nil, closeAudit, err
		}
		c.Audit, closeAudit = s, flush
	} else {
		slog.Warn("golem-control: no -audit-archive: the audit outbox is not shipped; past 80% of -audit-outbox-max changes that grant access are refused, and at it every audited change", "audit-outbox-max", st.OutboxMax())
	}
	if o.clickhouse != "" {
		c.Spend = &spend.Job{Ledger: spend.ClickHouse{URL: o.clickhouse, User: o.clickhouseUser, Password: os.Getenv("CLICKHOUSE_PASSWORD")},
			Allowance: o.allowance, Location: zone, StaleAfter: o.spendStaleAfter}
		if err := c.Spend.Validate(); err != nil {
			return nil, closeAudit, err
		}
	} else {
		slog.Warn("golem-control: no -clickhouse: snapshots carry no known spend, so no budget sees any spend")
	}
	if o.syncClient != "" {
		if o.tenant == "" {
			return nil, closeAudit, errors.New("-entra-sync-client-id needs -entra-tenant")
		}
		tok := &entrasync.FederatedToken{Tenant: o.tenant, ClientID: o.syncClient}
		c.Sync = &entrasync.Sync{State: st, Directory: &entrasync.Graph{Token: tok.Token}, MaxRemovals: o.syncMax}
		c.SyncEvery = o.syncEvery
	}
	return c, closeAudit, nil
}

// apiServer serves the control API, authenticated by Entra tokens: no
// API without a tenant, since an unauthenticated one would let anyone
// mint keys. jwks is Entra's key set URL (empty: the tenant's).
//
// Entra's signing keys are fetched in the background, not before the API
// starts: a replica started during an Entra outage serves all the same,
// refusing tokens as unavailable until the keys arrive (as the gateway's
// verifier does), and the publisher runs regardless. A replica that
// refused to start would stop publication, and every gateway's lease
// would run out with it.
//
// refreshKeys is the verifier's key refresh loop, for the caller to run
// (and wait for) until ctx ends: it fetches at once, retrying until Entra
// answers.
func apiServer(listen, tenant, audience, jwks string, svc *service.Service) (_ *http.Server, refreshKeys func(context.Context), _ error) {
	if tenant == "" {
		return nil, nil, errors.New("the control API needs -entra-tenant: it authenticates every caller")
	}
	e := &entra.Verifier{Tenant: tenant, Audiences: list(audience), Scope: entra.ScopeManage, JWKS: jwks}
	h := (&api.Server{Service: svc, Auth: api.EntraAuth{Entra: e, State: svc.State}}).Handler()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC behind the mesh, which ends TLS
	// The API's calls are small and quick: a slow sender or reader is
	// cut off rather than held.
	return &http.Server{Addr: listen, Handler: h, Protocols: &protocols, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: time.Minute,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}, e.Run, nil
}

// readyz is golem-control's readiness: ready while it leads, which it does
// once it has loaded the state and claimed the fence, and until a
// successor claims over it (it then exits).
func readyz(c *controller.Controller) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if _, leads := c.Fence(); !leads {
			http.Error(w, "not leading: the fence is not claimed, or another replica has claimed over this one", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	}
}

// serveAPI runs serve (srv's ListenAndServe, or Serve on a listener) until
// ctx ends, then shuts srv down and returns only once that has: the calls
// in flight have finished, or drain has passed and they are cut (logged,
// with what to do). Serve returns ErrServerClosed as soon as Shutdown
// starts, so waiting on serve alone would exit the process while calls
// were in flight: a MintKey whose write landed, cut before the key reached
// its caller. It returns serve's error if serving fails before ctx ends.
func serveAPI(ctx context.Context, srv *http.Server, serve func() error, drain time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- serve() }() //qenv:ok ended by Shutdown below, and waited for
	select {
	case err := <-served:
		return err // not ErrServerClosed: nothing has shut it down
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		slog.Warn("golem-control: the control API's calls in flight did not finish within -drain and were cut; their callers see an error and may retry (raise -drain if calls take longer)",
			"drain", drain, "err", err)
		srv.Close()
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// list splits a comma-separated flag (pools, audiences), dropping spaces
// and empty entries ("api://a, api://b" is two audiences, not one with a
// space).
func list(s string) []string {
	var out []string
	for a := range strings.SplitSeq(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// resourceOf is golem-control's telemetry resource: the service, and the
// replica (each replica reports its own leadership and publications).
func resourceOf(replica string) *resource.Resource {
	return resource.NewSchemaless(attribute.String("service.name", "golem-control"), attribute.String("service.instance.id", replica))
}

// openMetrics exports golem-control's metrics (control/metrics) to the
// collector at telem, unless it is "off". shutdown exports a last
// collection, waiting at most five seconds.
func openMetrics(ctx context.Context, telem, replica string, c *controller.Controller) (shutdown func(), _ error) {
	if telem == "off" {
		return func() {}, nil
	}
	mp, stop, err := otlp.MeterProvider(ctx, telem, resourceOf(replica))
	if err != nil {
		return nil, fmt.Errorf("-telemetry: %w", err)
	}
	if err := metrics.Register(mp, c); err != nil {
		return nil, errors.Join(fmt.Errorf("metrics: %w", err), stop(ctx))
	}
	if err := registerConfigAge(mp, c); err != nil {
		return nil, errors.Join(fmt.Errorf("metrics: %w", err), stop(ctx))
	}
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stop(sctx); err != nil {
			slog.Warn("golem-control: the last metrics were not exported", "err", err)
		}
	}, nil
}

// ConfigAge is the config checkout's age instrument's name.
const ConfigAge = "golem.control.config.age"

// registerConfigAge registers golem.control.config.age: how long ago the
// config checkout was last fetched (controller.ConfigAge), in seconds;
// nothing while it is not known (no -config-fetched, or unreadable).
// Past -max-config-age leases are not renewed (operations.md, Alerts).
func registerConfigAge(mp metric.MeterProvider, c *controller.Controller) error {
	_, err := mp.Meter("golem/control").Float64ObservableGauge(ConfigAge, metric.WithUnit("s"),
		metric.WithDescription("How long ago the config checkout was last fetched (git-sync's FETCH_HEAD); past -max-config-age leases are not renewed"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			if age, ok := c.ConfigAge(); ok {
				o.Observe(age.Seconds())
			}
			return nil
		}))
	return err
}

// openAudit is the audit shipper over the archive, emitting log records to
// the collector at telem unless it is "off". shutdown flushes the log
// records, waiting at most five seconds.
func openAudit(ctx context.Context, st *state.Store, archive, telem, replica string) (_ *audit.Shipper, shutdown func(), _ error) {
	ab, err := openBucket(ctx, archive)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-archive: %w", err)
	}
	s := &audit.Shipper{State: st, Archive: ab}
	if telem == "off" {
		return s, func() {}, nil
	}
	lp, stop, err := otlp.LoggerProvider(ctx, telem, resourceOf(replica))
	if err != nil {
		return nil, nil, fmt.Errorf("-telemetry: %w", err)
	}
	s.Logger = lp.Logger("golem-control")
	return s, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(sctx)
	}, nil
}

// pubkey writes the publishing key's public half as a gateway's -keys
// directory takes it: a PEM PUBLIC KEY whose Key-ID header names the key as
// it signs (a KMS key's ARN; a file key's name).
func pubkey(ctx context.Context, w io.Writer, spec string) error {
	signer, err := openSigner(ctx, spec)
	if err != nil {
		return err
	}
	var pub any
	switch s := signer.(type) {
	case sign.Ed25519:
		pub = s.Key.Public()
	case kmssign.Signer:
		pub = s.Public
	default:
		return fmt.Errorf("no public key for a %T", signer)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	return pem.Encode(w, &pem.Block{Type: "PUBLIC KEY", Headers: map[string]string{"Key-ID": signer.KeyID()}, Bytes: der})
}

// openBucket is a directory, or an S3 bucket (s3bucket.Open).
func openBucket(ctx context.Context, where string) (bucket.Bucket, error) {
	return s3bucket.Open(ctx, where)
}

func openSigner(ctx context.Context, spec string) (sign.Signer, error) {
	switch kind, ref, _ := strings.Cut(spec, ":"); kind {
	case "file":
		return loadKey(ref)
	case "kms":
		awsCfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, err
		}
		c := kms.NewFromConfig(awsCfg)
		pub, err := kmssign.PublicKey(ctx, c, ref) // the key exists and is the right kind
		if err != nil {
			return nil, err
		}
		return kmssign.Signer{Client: c, Key: ref, Public: pub}, nil
	default:
		return nil, fmt.Errorf("%q: want file:<path> or kms:<arn>", spec)
	}
}

// loadKey reads an Ed25519 private key (PKCS#8 PEM); its key ID is the
// file's name without extension.
func loadKey(path string) (sign.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: not a PEM PRIVATE KEY", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return sign.Ed25519{ID: id, Key: priv}, nil
}

// keygen writes publishing.key (private, 0600) and publishing.pub (the
// gateways' verifying key) into dir.
func keygen(dir string) error {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	p8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "publishing.key")
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w (keygen never overwrites a key)", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: p8}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "publishing.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}), 0o644)
}
