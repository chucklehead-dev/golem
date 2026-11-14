// golem-control publishes the config's snapshots to the bucket while it
// leads (control-plane.md §4; this slice: config CRDs from a directory,
// every pool the same snapshot, entitlements from the state document).
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
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/chucklehead-dev/golem/control/api"
	"github.com/chucklehead-dev/golem/control/audit"
	"github.com/chucklehead-dev/golem/control/controller"
	"github.com/chucklehead-dev/golem/control/entrasync"
	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/control/state"
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
		fail("%v", err)
	}
}

// dispatch runs a subcommand (keygen, pubkey, restore, bootstrap), or golem-control
// itself given anything else, flags included.
func dispatch(args []string) error {
	ctx := context.Background()
	if len(args) == 0 {
		return run()
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
	return run()
}

// installed is what shows golem-control has run on a bucket publishing
// pools (a comma-separated list): the lease object, and each pool's
// checkpoint and pointer. The bucket interface cannot list, so these are
// looked for by name (state.Store.Installed).
func installed(pools string) []string {
	keys := []string{lead.LeaseKey}
	for p := range strings.SplitSeq(pools, ",") {
		if p = strings.TrimSpace(p); p != "" {
			keys = append(keys, snapshot.CheckpointKey(p), snapshot.PointerKey(p))
		}
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
	st := &state.Store{Bucket: b}
	floors := map[string]uint64{}
	for p := range strings.SplitSeq(*pools, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
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
// history or its Object Lock replica (control-plane.md §3).
func restore(ctx context.Context, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	where := fs.String("bucket", "", "the state bucket: a directory, or s3://bucket/prefix/")
	from := fs.String("from", "", "the backup: a state document file")
	actor := fs.String("actor", "", "who restores, for the audit event (required)")
	apply := fs.Bool("apply", false, "write the restore; without it, only show what it would do")
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
	st := &state.Store{Bucket: b}
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
	trustedKeys                           string
	authLease                             time.Duration
	listen                                string
	archive, telemetry                    string
	tenant, audience                      string
	syncClient                            string
	syncEvery                             time.Duration
	syncMax                               int
	clickhouse, clickhouseUser            string
	allowance                             time.Duration
	periodZone                            string
}

// parseFlags reads golem-control's flags and checks those that need each
// other.
func parseFlags() (options, error) {
	var o options
	flag.StringVar(&o.config, "config", "", "the config CRDs' directory")
	flag.StringVar(&o.bucket, "bucket", "", "the bucket: a directory, or s3://<bucket>/<prefix>")
	flag.StringVar(&o.signingKey, "signing-key", "", "the publishing key: file:<an Ed25519 PKCS#8 PEM> or kms:<a KMS key ARN, ECC_NIST_P256>")
	flag.StringVar(&o.trustedKeys, "trusted-keys", "", "a directory of retiring publishing keys' public halves (*.pub, as a gateway's -keys): checkpoints and pointers they signed are trusted while the signer is rotated, and re-signed with -signing-key at the next publication; empty: the signing key's own only")
	flag.StringVar(&o.pools, "pools", "internal", "the gateway pools to publish to, comma-separated")
	flag.StringVar(&o.id, "id", "", "this replica's name (default: the host name)")
	flag.DurationVar(&o.authLease, "lease", 4*time.Hour, "the snapshots' authorization lease (ADR 0032: four hours internal, five minutes edge)")
	flag.StringVar(&o.listen, "listen", "", "serve the control API (Connect: gRPC, gRPC-Web, HTTP/JSON) here; empty: no API")
	flag.StringVar(&o.archive, "audit-archive", "", "the audit archive (an Object Lock bucket in a separate account): a directory, or s3://<bucket>/<prefix>; empty: the outbox is not shipped")
	flag.StringVar(&o.telemetry, "telemetry", "off", "off, or a collector's OTLP/HTTP host:port, for the audit's log records (TLS unless a literal loopback address)")
	flag.StringVar(&o.tenant, "entra-tenant", "", "the Entra tenant whose tokens the API takes (required with -listen)")
	flag.StringVar(&o.audience, "entra-audience", "api://golem-control", "the control API registration's audiences in Entra tokens, comma-separated (ADR 0046: not the gateway's)")
	flag.StringVar(&o.syncClient, "entra-sync-client-id", "", "the jobs registration's client ID (entra.md #4): sync bound Entra groups' members by workload identity; empty: no sync")
	flag.DurationVar(&o.syncEvery, "entra-sync-every", 5*time.Minute, "how often the group sync runs")
	flag.IntVar(&o.syncMax, "entra-sync-max-removals", 50, "the most memberships one sync may end; a step over it changes nothing (raise it for one run when intended)")
	flag.StringVar(&o.clickhouse, "clickhouse", "", "the ledger's ClickHouse HTTP interface (https://host:8443) for known spend (ADR 0048); its password in $CLICKHOUSE_PASSWORD; empty: snapshots carry none, and gateways count their site alone")
	flag.StringVar(&o.clickhouseUser, "clickhouse-user", "golem_control", "the ClickHouse user known spend is read as (read-only on golem.spend)")
	flag.DurationVar(&o.allowance, "spend-allowance", 5*time.Minute, "how long after a call's end its ledger row is surely in ClickHouse: known spend's boundary is this far back")
	flag.StringVar(&o.periodZone, "period-zone", "UTC", "the budget periods' time zone (an IANA name): the gateways' -period-zone")
	flag.Parse()
	if o.config == "" || o.bucket == "" || o.signingKey == "" {
		return o, errors.New("-config, -bucket and -signing-key are required")
	}
	if o.archive == "" && o.telemetry != "off" {
		return o, errors.New("-telemetry carries the audit's log records, which need -audit-archive: set both, or neither")
	}
	return o, nil
}

// run is golem-control: set up, then run until SIGTERM or a component
// fails. Shutdown is in order: the publishing loop stops (no publication
// left half-done), the lease is given up so another replica publishes at
// once rather than after the lease lapses, the API drains, and the audit's
// log records are flushed. Errors are returned, never an exit from a
// goroutine, so every deferred step runs.
func run() error {
	o, err := parseFlags()
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
	st := &state.Store{Bucket: b, Installed: installed(o.pools)}
	if err := st.Load(ctx); err != nil {
		return fmt.Errorf("the state document: %w", err)
	}
	c, closeAudit, err := newController(ctx, o, st, b, signer, replica)
	if err != nil {
		return err
	}
	defer closeAudit()
	slog.Info("golem-control: running", "replica", replica, "pools", o.pools, "signing key", signer.KeyID())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var failed error // a component's failure, which stops the rest
	wg.Go(func() { st.Run(ctx) })
	if o.listen != "" {
		srv, err := apiServer(ctx, o.listen, o.tenant, o.audience, &service.Service{State: st, Priced: c.CheckScope})
		if err != nil {
			return fmt.Errorf("-listen: %w", err)
		}
		wg.Go(func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed = fmt.Errorf("the control API: %w", err) // read after wg.Wait
				cancel()
			}
		})
		slog.Info("golem-control: the control API is listening", "addr", o.listen)
		// Drained when the context ends, so ListenAndServe returns and the
		// wait below ends.
		context.AfterFunc(ctx, func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.Shutdown(sctx)
		})
	}
	c.Run(ctx) // until SIGTERM, or another component cancels
	wg.Wait()
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	if err := c.Elector.Release(rctx); err != nil {
		slog.Warn("golem-control: the lease was not given up; another replica takes it when it lapses", "err", err)
	}
	if failed != nil {
		return failed
	}
	slog.Info("golem-control: stopped")
	return nil
}

// newController wires the publishing loop: an elector and a publisher per
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
	c = &controller.Controller{Dir: o.config, Lease: o.authLease,
		Elector: &lead.Elector{State: st, ID: replica}, Publishers: map[string]*publish.Publisher{}}
	for _, p := range strings.Split(o.pools, ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Publishers[p] = &publish.Publisher{State: st, Bucket: b, Signer: signer, Keys: trusted, Pool: p}
		}
	}
	if o.archive != "" {
		if c.Audit, closeAudit, err = openAudit(ctx, st, o.archive, o.telemetry); err != nil {
			closeAudit = func() {}
			return nil, closeAudit, err
		}
	} else {
		slog.Warn("golem-control: no -audit-archive: the audit outbox is not shipped, and grows in the state document")
	}
	if o.clickhouse != "" {
		zone, err := time.LoadLocation(o.periodZone)
		if err != nil {
			return nil, closeAudit, fmt.Errorf("-period-zone: %w", err)
		}
		c.Spend = &spend.Job{Ledger: spend.ClickHouse{URL: o.clickhouse, User: o.clickhouseUser, Password: os.Getenv("CLICKHOUSE_PASSWORD")},
			Allowance: o.allowance, Location: zone}
	} else {
		slog.Warn("golem-control: no -clickhouse: snapshots carry no known spend, and each site's gateways budget against their own counter alone")
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
// mint keys.
func apiServer(ctx context.Context, listen, tenant, audience string, svc *service.Service) (*http.Server, error) {
	if tenant == "" {
		return nil, errors.New("the control API needs -entra-tenant: it authenticates every caller")
	}
	e := &entra.Verifier{Tenant: tenant, Audiences: audiences(audience), Scope: entra.ScopeManage}
	if err := e.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("Entra's signing keys: %w", err)
	}
	go e.Run(ctx) //qenv:ok the key refresh loop, ended by ctx
	h := (&api.Server{Service: svc, Auth: api.EntraAuth{Entra: e, State: svc.State}}).Handler()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC behind the mesh, which ends TLS
	// The API's calls are small and quick: a slow sender or reader is
	// cut off rather than held.
	return &http.Server{Addr: listen, Handler: h, Protocols: &protocols, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: time.Minute,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}, nil
}

// audiences splits a comma-separated list, dropping spaces and empty
// entries ("api://a, api://b" is two audiences, not one with a space).
func audiences(list string) []string {
	var out []string
	for a := range strings.SplitSeq(list, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// openAudit is the audit shipper over the archive, emitting log records to
// the collector at telem unless it is "off". shutdown flushes the log
// records, waiting at most five seconds.
func openAudit(ctx context.Context, st *state.Store, archive, telem string) (_ *audit.Shipper, shutdown func(), _ error) {
	ab, err := openBucket(ctx, archive)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-archive: %w", err)
	}
	s := &audit.Shipper{State: st, Archive: ab}
	if telem == "off" {
		return s, func() {}, nil
	}
	lp, stop, err := otlp.LoggerProvider(ctx, telem, resource.NewSchemaless(attribute.String("service.name", "golem-control")))
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

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "golem-control: "+format+"\n", args...)
	os.Exit(2)
}
