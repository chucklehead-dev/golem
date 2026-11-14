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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/chucklehead-dev/golem/control/controller"
	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
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
	slog.Info("golem-control: running", "replica", replica, "pools", *pools, "signing key", signer.KeyID())
	go st.Run(ctx) //qenv:ok the state refresh loop, ended by ctx
	c.Run(ctx)
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
