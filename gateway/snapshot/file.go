package snapshot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
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

	seq  uint64
	last [32]byte
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
	sum, err := contentHash(fsys)
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
	if err := d.Holder.Apply(s, snapshot.Hash(b), d.Secrets); err != nil {
		return err
	}
	d.seq++
	d.last = sum
	d.log().Info("config applied", "dir", d.Path, "routes", len(s.Routes), "hash", snapshot.Hash(b)[:12])
	return nil
}

// Run reloads until ctx ends.
func (d *Dir) Run(ctx context.Context) {
	every := d.Interval
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := d.Load(); err != nil {
			if err.Error() != lastErr { // report a problem once, not every second
				d.log().Error("config not applied; serving the previous one", "dir", d.Path, "err", err)
			}
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
	}
}

func (d *Dir) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// contentHash hashes every YAML file's name and contents: a cheap test of
// whether anything changed.
func contentHash(fsys fs.FS) ([32]byte, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := path.Ext(p); e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, err
}

// FileSecrets resolves secretRef "name/key" to the file key in the
// directory a Secret name is mounted at, under Root (production: each
// Secret mounted at /var/run/golem/secrets/<name>). Env credentials are
// refused: they are standalone's only.
type FileSecrets struct{ Root *os.Root }

func (f FileSecrets) Secret(ref, env string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("environment credential %s: only standalone mode reads the environment", env)
	}
	name, key, _ := strings.Cut(ref, "/")
	b, err := f.Root.ReadFile(name + "/" + key)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w (is the Secret mounted?)", ref, err)
	}
	return strings.TrimSpace(string(b)), nil
}
