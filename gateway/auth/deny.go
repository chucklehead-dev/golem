package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/key"
)

// Denylist is a site's emergency denylist (control-plane.md §5, ADR 0041
// §1): credentials and callers refused at this site's gateways whatever the
// snapshot says, for when a revocation cannot wait for the control plane or
// the bucket. A site admin edits it (a ConfigMap mounted as a file) and
// clears it once the control plane has the revocation.
//
// One entry a line, # comments:
//
//	key <hex SHA-256 of the key>
//	key-prefix <a key's display prefix, at least 16 characters>
//	principal <golem principal ID>
//	entra <a user's object ID or a service's app ID>
type Denylist struct {
	keys       map[[32]byte]bool
	prefixes   []string
	principals map[string]bool
	entra      map[string]bool
}

// minPrefix keeps a prefix entry from denying many keys by mistake: a key's
// display prefix is longer than its fixed part.
const minPrefix = 16

// ParseDenylist reads a denylist; any line it cannot read fails it whole,
// so a typo is noticed rather than an entry silently skipped.
func ParseDenylist(b []byte) (*Denylist, error) {
	d := &Denylist{keys: map[[32]byte]bool{}, principals: map[string]bool{}, entra: map[string]bool{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		kind, v, ok := strings.Cut(line, " ")
		v = strings.TrimSpace(v)
		if !ok || v == "" || strings.ContainsAny(v, " \t") {
			return nil, fmt.Errorf("denylist line %d: want <kind> <value>", n)
		}
		switch kind {
		case "key":
			h, err := hex.DecodeString(v)
			if err != nil || len(h) != sha256.Size {
				return nil, fmt.Errorf("denylist line %d: a key is its hex SHA-256 (golemctl keys list shows it)", n)
			}
			d.keys[[32]byte(h)] = true
		case "key-prefix":
			if len(v) < minPrefix || !strings.HasPrefix(v, "glm_") {
				return nil, fmt.Errorf("denylist line %d: a key prefix is a key's display prefix, glm_…, at least %d characters", n, minPrefix)
			}
			d.prefixes = append(d.prefixes, v)
		case "principal":
			d.principals[v] = true
		case "entra":
			d.entra[v] = true
		default:
			return nil, fmt.Errorf("denylist line %d: unknown kind %q (key, key-prefix, principal, entra)", n, kind)
		}
	}
	return d, sc.Err()
}

// Len is the number of entries.
func (d *Denylist) Len() int {
	if d == nil {
		return 0
	}
	return len(d.keys) + len(d.prefixes) + len(d.principals) + len(d.entra)
}

// denies reports whether a request authenticated as p, by cred (a key, or
// "" for a token) and entraID (the token's, or ""), is denied.
func (d *Denylist) denies(p *Principal, cred, entraID string) bool {
	if d == nil {
		return false
	}
	if cred != "" {
		if len(d.keys) > 0 && d.keys[key.Hash(cred)] {
			return true
		}
		for _, pre := range d.prefixes {
			if strings.HasPrefix(cred, pre) {
				return true
			}
		}
	}
	return d.principals[p.ID] || entraID != "" && d.entra[entraID]
}

// denied is the refusal of a caller the denylist names (read-only).
var denied = &Refusal{contract.AuthDenied, "this credential or caller is on the site's emergency denylist: ask the platform team"}

// DenyFile is a denylist file, re-read when it changes (a ConfigMap's
// mount is replaced, not edited, so its content is what is compared).
type DenyFile struct {
	Path     string
	Interval time.Duration // default 5 seconds
	Log      *slog.Logger

	cur atomic.Pointer[Denylist]
	sum [32]byte
}

// Load reads the file once: an error leaves the previous denylist in force
// (a bad edit must not clear it).
func (f *DenyFile) Load() error {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return fmt.Errorf("denylist %s: %w", f.Path, err)
	}
	sum := sha256.Sum256(b)
	if sum == f.sum && f.cur.Load() != nil {
		return nil
	}
	d, err := ParseDenylist(b)
	if err != nil {
		return fmt.Errorf("denylist %s, not applied (the previous one stays): %w", f.Path, err)
	}
	f.cur.Store(d)
	f.sum = sum
	if f.Log != nil {
		f.Log.Warn("emergency denylist applied", "path", f.Path, "entries", d.Len())
	}
	return nil
}

// Current is the denylist in force (nil: none).
func (f *DenyFile) Current() *Denylist {
	if f == nil {
		return nil
	}
	return f.cur.Load()
}

// Run re-reads the file every Interval until ctx ends.
func (f *DenyFile) Run(ctx context.Context) {
	interval := f.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Like the snapshot sources' loop: a failure is logged when it is
	// first seen or changes, not at every tick.
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		switch err := f.Load(); {
		case err == nil:
			last = ""
		case err.Error() != last:
			if f.Log != nil {
				f.Log.Error("emergency denylist", "err", err)
			}
			last = err.Error()
		}
	}
}
