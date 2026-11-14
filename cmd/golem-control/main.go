// golem-control publishes the config's snapshots to the bucket while it
// leads (control-plane.md §4; this slice: config CRDs from a directory,
// every pool the same snapshot, entitlements from the state document).
//
//	golem-control -config ./config -bucket ./bucket -signing-key file:publishing.key -pools internal
//	golem-control -config ./config -bucket s3://golem-state/golem/ -signing-key kms:arn:aws:kms:... -pools internal,edge
//	golem-control keygen ./keys   (a development Ed25519 key pair)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/chucklehead-dev/golem/control/api"
	"github.com/chucklehead-dev/golem/control/audit"
	"github.com/chucklehead-dev/golem/control/controller"
	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/bucket/s3bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/sign/kmssign"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if len(os.Args) != 3 {
			fail("usage: golem-control keygen <dir>")
		}
		if err := keygen(os.Args[2]); err != nil {
			fail("%v", err)
		}
		return
	}
	cfg := flag.String("config", "", "the config CRDs' directory")
	bkt := flag.String("bucket", "", "the bucket: a directory, or s3://<bucket>/<prefix>")
	key := flag.String("signing-key", "", "the publishing key: file:<an Ed25519 PKCS#8 PEM> or kms:<a KMS key ARN, ECC_NIST_P256>")
	pools := flag.String("pools", "internal", "the gateway pools to publish to, comma-separated")
	id := flag.String("id", "", "this replica's name (default: the host name)")
	authLease := flag.Duration("lease", 4*time.Hour, "the snapshots' authorization lease (ADR 0032: four hours internal, five minutes edge)")
	listen := flag.String("listen", "", "serve the control API (Connect: gRPC, gRPC-Web, HTTP/JSON) here; empty: no API")
	archive := flag.String("audit-archive", "", "the audit archive (an Object Lock bucket in a separate account): a directory, or s3://<bucket>/<prefix>; empty: the outbox is not shipped")
	telem := flag.String("telemetry", "off", "off, or a collector's OTLP/HTTP host:port, for the audit's log records (TLS unless a literal loopback address)")
	tenant := flag.String("entra-tenant", "", "the Entra tenant whose tokens the API takes (required with -listen)")
	audience := flag.String("entra-audience", "api://golem-control", "the control API registration's audiences in Entra tokens, comma-separated (ADR 0046: not the gateway's)")
	flag.Parse()
	if *cfg == "" || *bkt == "" || *key == "" {
		fail("-config, -bucket and -signing-key are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b, err := openBucket(ctx, *bkt)
	if err != nil {
		fail("-bucket: %v", err)
	}
	signer, err := openSigner(ctx, *key)
	if err != nil {
		fail("-signing-key: %v", err)
	}
	replica := *id
	if replica == "" {
		if replica, err = os.Hostname(); err != nil {
			fail("-id: %v", err)
		}
	}
	st := &state.Store{Bucket: b}
	if err := st.Load(ctx); err != nil {
		fail("the state document: %v", err)
	}
	c := &controller.Controller{Dir: *cfg, Lease: *authLease,
		Elector: &lead.Elector{State: st, ID: replica}, Publishers: map[string]*publish.Publisher{}}
	for _, p := range strings.Split(*pools, ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Publishers[p] = &publish.Publisher{State: st, Bucket: b, Signer: signer, Pool: p}
		}
	}
	if *archive != "" {
		shipper, shutdown, err := openAudit(ctx, st, *archive, *telem)
		if err != nil {
			fail("%v", err)
		}
		defer shutdown()
		c.Audit = shipper
	} else {
		slog.Warn("golem-control: no -audit-archive: the audit outbox is not shipped, and grows in the state document")
	}
	slog.Info("golem-control: running", "replica", replica, "pools", *pools, "signing key", signer.KeyID())
	go st.Run(ctx) //qenv:ok the state refresh loop, ended by ctx
	if *listen != "" {
		srv, err := apiServer(ctx, *listen, *tenant, *audience, st)
		if err != nil {
			fail("-listen: %v", err)
		}
		go func() { //qenv:ok the API server, shut down below
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fail("the control API: %v", err)
			}
		}()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.Shutdown(sctx)
		}()
		slog.Info("golem-control: the control API is listening", "addr", *listen)
	}
	c.Run(ctx)
}

// apiServer serves the control API, authenticated by Entra tokens: no
// API without a tenant, since an unauthenticated one would let anyone
// mint keys.
func apiServer(ctx context.Context, listen, tenant, audience string, st *state.Store) (*http.Server, error) {
	if tenant == "" {
		return nil, errors.New("the control API needs -entra-tenant: it authenticates every caller")
	}
	e := &auth.Entra{Tenant: tenant, Audiences: audiences(audience), Scope: auth.ScopeManage}
	if err := e.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("Entra's signing keys: %w", err)
	}
	go e.Run(ctx) //qenv:ok the key refresh loop, ended by ctx
	h := (&api.Server{Service: &service.Service{State: st}, Auth: api.EntraAuth{Entra: e, State: st}}).Handler()
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
	p, stop, err := telemetry.OTLP(ctx, telem, resource.NewSchemaless(attribute.String("service.name", "golem-control")))
	if err != nil {
		return nil, nil, fmt.Errorf("-telemetry: %w", err)
	}
	s.Logger = p.Logger.Logger("golem-control")
	return s, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(sctx)
	}, nil
}

// openBucket is a directory, or an S3 bucket with the default AWS
// credential chain (workload identity in the cluster).
func openBucket(ctx context.Context, where string) (bucket.Bucket, error) {
	rest, ok := strings.CutPrefix(where, "s3://")
	if !ok {
		return bucket.OpenDir(where)
	}
	name, prefix, _ := strings.Cut(rest, "/")
	if name == "" {
		return nil, errors.New("s3://<bucket>/<prefix>: no bucket")
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &s3bucket.Bucket{Client: s3.NewFromConfig(awsCfg), Name: name, Prefix: prefix}, nil
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
		if _, err := kmssign.PublicKey(ctx, c, ref); err != nil { // the key exists and is the right kind
			return nil, err
		}
		return kmssign.Signer{Client: c, Key: ref}, nil
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
