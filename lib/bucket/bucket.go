// Package bucket is the object store golem publishes through
// (control-plane.md §4, ADR 0006): gets with an entity tag, conditional
// gets (If-None-Match), and puts that may be conditional (If-Match, or
// If-None-Match: * to create only), as S3 provides. Dir is a directory's
// implementation, for tests and development; the S3 one is another.
package bucket

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
)

// Errors, as S3 reports them.
var (
	ErrNotFound     = errors.New("bucket: no such object")
	ErrNotModified  = errors.New("bucket: not modified") // a conditional get's object is unchanged
	ErrPrecondition = errors.New("bucket: precondition failed")
)

// Absent is the ifMatch that makes a put create the object only if none
// exists.
const Absent = "*"

// Bucket is an object store.
type Bucket interface {
	// Get returns an object and its entity tag; with ifNoneMatch set to the
	// tag the caller holds, ErrNotModified if it is unchanged.
	Get(ctx context.Context, key, ifNoneMatch string) (body []byte, etag string, err error)
	// Put writes an object: unconditionally with ifMatch "", only if it does
	// not exist with Absent, else only if its tag is ifMatch
	// (ErrPrecondition otherwise). It returns the new tag.
	Put(ctx context.Context, key string, body []byte, ifMatch string) (etag string, err error)
}

// Dir is a Bucket in a directory, opened with os.Root so a key cannot
// leave it. Its conditional puts are atomic within one process only: it
// is for tests and a single development publisher, not shared storage.
type Dir struct {
	root *os.Root
	mu   sync.Mutex
}

// OpenDir opens (creating if needed) a directory as a bucket.
func OpenDir(dir string) (*Dir, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Dir{root: r}, nil
}

// Close closes the directory.
func (d *Dir) Close() error { return d.root.Close() }

// etag is Dir's entity tag: a quoted digest of the contents, so equal
// bodies have equal tags, as S3's do for a single-part put.
func etag(b []byte) string {
	h := sha256.Sum256(b)
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

// checkKey refuses a key that is not a clean relative slash path.
func checkKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || path.Clean(key) != key || strings.Contains(key, "\\") {
		return fmt.Errorf("bucket: invalid key %q", key)
	}
	return nil
}

// Get implements Bucket.Get. With ErrNotModified it returns the current tag.
func (d *Dir) Get(_ context.Context, key, ifNoneMatch string) ([]byte, string, error) {
	if err := checkKey(key); err != nil {
		return nil, "", err
	}
	b, err := d.root.ReadFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	t := etag(b)
	if ifNoneMatch != "" && ifNoneMatch == t {
		return nil, t, ErrNotModified
	}
	return b, t, nil
}

// Put implements Bucket.Put. Its mutex makes the condition's check and the
// write one step, within this process.
func (d *Dir) Put(_ context.Context, key string, body []byte, ifMatch string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(key, ifMatch); err != nil {
		return "", err
	}
	if err := d.replace(key, body); err != nil {
		return "", err
	}
	return etag(body), nil
}

// check tests a put's condition against the object as it is now.
func (d *Dir) check(key, ifMatch string) error {
	if ifMatch == "" {
		return nil
	}
	cur, err := d.root.ReadFile(key)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if ifMatch != Absent {
			return ErrPrecondition
		}
	case err != nil:
		return err
	case ifMatch == Absent || etag(cur) != ifMatch:
		return ErrPrecondition
	}
	return nil
}

// replace writes body whole to a temporary file, then renames it over the
// old object: a reader sees one or the other, never a torn object.
func (d *Dir) replace(key string, body []byte) error {
	if dir := path.Dir(key); dir != "." {
		if err := d.root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := key + ".tmp-" + rand.Text()
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = d.root.Rename(tmp, key)
	}
	if err != nil {
		d.root.Remove(tmp)
	}
	return err
}
