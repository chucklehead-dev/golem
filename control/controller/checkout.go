package controller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Checkout is the git checkout the config directory is read from, as
// git-sync keeps it (ADR 0054): a link to the current worktree, which is
// named by its commit's hash, and the repository's FETCH_HEAD, rewritten
// at every successful fetch (each sync period, whether or not the branch
// moved).
type Checkout struct {
	// Link is git-sync's --link (<root>/<link>): a symbolic link to the
	// worktree of the commit checked out, <root>/.worktrees/<hash>. The
	// config directory is Link or within it. "": no revision is known.
	Link string
	// Fetched is a file whose modification time is when the checkout was
	// last confirmed to be the branch's head: git-sync's
	// <root>/.git/FETCH_HEAD. "": the checkout's age is not known, and not
	// bounded.
	Fetched string
}

// ErrStale means the config checkout has not been fetched for longer than
// MaxConfigAge, or its age cannot be read. Leases are not renewed until it
// is fetched again.
var ErrStale = errors.New("the config checkout is stale")

// resolve is dir, which is k's Link or within it, as it is in the worktree
// the link points at now, and that worktree's commit. Reading the
// worktree's own path, not the link, keeps the config read and its
// revision one checkout when git-sync moves the link meanwhile. A nil k,
// or one with no Link, is dir itself with no revision.
func (k *Checkout) resolve(dir string) (string, snapshot.Revision, error) {
	if k == nil || k.Link == "" {
		return dir, "", nil
	}
	rel, err := filepath.Rel(k.Link, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%s is not within the checkout's link %s", dir, k.Link)
	}
	target, err := os.Readlink(k.Link)
	if err != nil {
		return "", "", fmt.Errorf("the checkout's link: %w", err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(k.Link), target)
	}
	return filepath.Join(target, rel), snapshot.Revision(filepath.Base(target)), nil
}

// fetchedAt is when the checkout was last fetched (Fetched's modification
// time); ok is false without Fetched.
func (k *Checkout) fetchedAt() (at time.Time, ok bool, err error) {
	if k == nil || k.Fetched == "" {
		return time.Time{}, false, nil
	}
	fi, err := os.Stat(k.Fetched)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("the checkout's last fetch: %w", err)
	}
	return fi.ModTime(), true, nil
}

// ConfigAge is how long ago the config checkout was last fetched, by the
// controller's clock; ok is false when it is not known (no Checkout or no
// Fetched, or Fetched unreadable). For a metric (golem.control.config.age).
func (c *Controller) ConfigAge() (age time.Duration, ok bool) {
	at, known, err := c.Checkout.fetchedAt()
	if !known || err != nil {
		return 0, false
	}
	return c.now().Sub(at), true
}

// staleErr is ErrStale, saying since when and what to do, while the
// checkout is older than MaxConfigAge or its age cannot be read; nil
// otherwise, or with no bound. The message names the fetch's time, not
// the age, so the loop logs it once until it changes.
func (c *Controller) staleErr() error {
	if c.MaxConfigAge <= 0 {
		return nil
	}
	at, known, err := c.Checkout.fetchedAt()
	switch {
	case !known:
		return nil
	case err != nil:
		return fmt.Errorf("%w: its age is unknown (%v), so leases are not renewed: the gateways' run out unless git-sync's checkout is readable again", ErrStale, err)
	case c.now().Sub(at) > c.MaxConfigAge:
		return fmt.Errorf("%w: last fetched at %s, longer ago than -max-config-age %s, so leases are not renewed (changes are still published, within the lease already granted) and the gateways' run out at their ValidUntil: fix git-sync (its log, the deploy key, the repository host)",
			ErrStale, at.UTC().Format(time.RFC3339), c.MaxConfigAge)
	}
	return nil
}
