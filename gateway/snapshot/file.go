package snapshot

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/compile"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Dir is the standalone source (ADR 0023): the CRD YAML in a directory,
// compiled in-process by lib/compile and re-applied when the files change.
// The directory is opened with os.Root, so a symlink cannot lead the
// gateway outside it.
type Dir struct {
	Path     string
	Holder   *Holder
	Secrets  Secrets
	Log      *slog.Logger
	Interval time.Duration // default one second

	seq  uint64   // the last publication number given; each applied config takes the next
	last [32]byte // the content hash of the files last applied
}

// Load reads the directory once and applies it if it changed. A config that
// does not compile is reported and the previous one kept.
func (d *Dir) Load() error {
	root, err := os.OpenRoot(d.Path)
	if err != nil {
		return fmt.Errorf("config directory: %w", err)
	}
	defer root.Close()
	fsys := root.FS()
	sum, err := v1.ContentHash(fsys)
	if err != nil {
		return err
	}
	if sum == d.last && d.Holder.cur.Load() != nil {
		return nil
	}
	objs, err := v1.DecodeFS(fsys)
	if err != nil {
		return err
	}
	s, err := compile.Compile(objs, compile.Options{Standalone: true, PublishSeq: d.seq + 1, Now: d.Holder.now()})
	if err != nil {
		return err
	}
	b, err := s.Encode()
	if err != nil {
		return err
	}
	hash := snapshot.Hash(b)
	if err := d.Holder.Apply(s, hash, d.Secrets); err != nil {
		return err
	}
	d.seq++
	d.last = sum
	d.log().Info("config applied", "dir", d.Path, "routes", len(s.Routes), "hash", hash[:12])
	return nil
}

// Run reloads at every interval until ctx ends; the first load is the
// caller's (Load), so the first reload waits an interval.
func (d *Dir) Run(ctx context.Context) {
	repeat(ctx, d.Interval, false, d.Load, func(err error) {
		d.log().Error("config not applied; serving the previous one", "dir", d.Path, "err", err)
	})
}

func (d *Dir) log() *slog.Logger { return cmp.Or(d.Log, slog.Default()) }
