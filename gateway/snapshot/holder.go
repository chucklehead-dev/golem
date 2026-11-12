// Package snapshot holds the gateway's current configuration: a compiled
// lib/snapshot behind an atomic pointer, taken once per request and used to
// its end (gateway.md §1, §6). It enforces the two rules the publication
// model checks: a snapshot at or below the floor is never applied, and an
// expired authorization lease serves nothing new (model/publication.md).
package snapshot

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Compiled is a snapshot ready for the request path.
type Compiled struct {
	Snapshot *snapshot.Snapshot
	Hash     string
	Routes   *route.Table
}

// Errors the request path reports as 503s.
var (
	ErrNotReady = errors.New("snapshot: no configuration yet")
	ErrExpired  = errors.New("snapshot: the authorization lease has expired; the gateway serves no new requests until a renewed snapshot arrives")
)

// Secrets resolves a backend's credential reference to its value.
type Secrets interface {
	// Secret returns the value of a "name/key" reference or of an
	// environment variable (standalone).
	Secret(ref, env string) (string, error)
}

// Holder is the gateway's current snapshot.
type Holder struct {
	cur atomic.Pointer[Compiled]
	// Now is the clock (tests set it).
	Now func() time.Time

	mu    sync.Mutex
	floor uint64
	// transports by deployment identity, kept across snapshots so
	// connections and their state survive a swap (gateway.md §6).
	transports map[string]*http.Transport
}

// Current returns the snapshot a request should use.
func (h *Holder) Current() (*Compiled, error) {
	c := h.cur.Load()
	if c == nil {
		return nil, ErrNotReady
	}
	if v := c.Snapshot.ValidUntil; !v.IsZero() && !h.now().Before(v) {
		return c, ErrExpired
	}
	return c, nil
}

// Floor is the highest publication applied (or trusted from a checkpoint).
func (h *Holder) Floor() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.floor
}

// RaiseFloor sets the floor from a trusted checkpoint before any snapshot is
// applied (a restart's persisted checkpoint, the publisher's record).
func (h *Holder) RaiseFloor(seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.floor = max(h.floor, seq)
}

// Apply compiles s and swaps it in. A snapshot at or below the floor, one
// whose lease has already expired, or one that does not compile is refused
// and the previous snapshot kept.
func (h *Holder) Apply(s *snapshot.Snapshot, hash string, secrets Secrets) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// No exemption for the first snapshot after a start: the floor may come
	// from a trusted checkpoint, and a replay below it is exactly what it
	// is there to refuse (model/publication.md, the trusted floor).
	if s.PublishSeq <= h.floor {
		return fmt.Errorf("snapshot %d is not newer than %d: refused (a replay or a stale publisher)", s.PublishSeq, h.floor)
	}
	if !s.ValidUntil.IsZero() && !h.now().Before(s.ValidUntil) {
		return fmt.Errorf("snapshot %d expired at %s: refused", s.PublishSeq, s.ValidUntil)
	}
	c, err := h.compile(s, hash, secrets)
	if err != nil {
		return err
	}
	h.floor = s.PublishSeq
	h.cur.Store(c)
	return nil
}

func (h *Holder) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Holder) compile(s *snapshot.Snapshot, hash string, secrets Secrets) (*Compiled, error) {
	if h.transports == nil {
		h.transports = map[string]*http.Transport{}
	}
	type key struct{ backend, deployment string }
	targets := map[key]*route.Target{}
	used := map[string]bool{}
	for _, b := range s.Backends {
		secret, err := secrets.Secret(b.Auth.SecretRef, b.Auth.Env)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", b.Name, err)
		}
		v := secret
		if b.Auth.Scheme != "" {
			v = b.Auth.Scheme + " " + secret
		}
		header := http.Header{http.CanonicalHeaderKey(b.Auth.Header): {v}}
		for _, d := range b.Deployments {
			u, err := url.Parse(d.URL)
			if err != nil {
				return nil, fmt.Errorf("backend %s deployment %s: %w", b.Name, d.Name, err)
			}
			id := b.Name + "/" + d.Name + " " + u.Scheme + "://" + u.Host
			t := h.transports[id]
			if t == nil {
				t = http.DefaultTransport.(*http.Transport).Clone()
				t.MaxIdleConnsPerHost = 256
				h.transports[id] = t
			}
			used[id] = true
			targets[key{b.Name, d.Name}] = &route.Target{Backend: b.Name, Deployment: d.Name, Model: d.Model,
				URL: u, Transport: t, Header: header}
		}
	}
	routes := make([]*route.Route, 0, len(s.Routes))
	for _, r := range s.Routes {
		rt := &route.Route{Alias: r.Alias, Dialect: r.Dialect}
		for _, m := range r.Pool {
			t := targets[key{m.Backend, m.Deployment}]
			if t == nil {
				return nil, fmt.Errorf("route %s: no deployment %s/%s in the snapshot", r.Alias, m.Backend, m.Deployment)
			}
			rt.Pool = append(rt.Pool, t)
		}
		if len(rt.Pool) == 0 {
			return nil, fmt.Errorf("route %s: empty pool", r.Alias)
		}
		routes = append(routes, rt)
	}
	// Transports of deployments that left the config are closed once no
	// longer referenced by the snapshot being replaced: requests still
	// using it finish on their open connections.
	for id, t := range h.transports {
		if !used[id] {
			t.CloseIdleConnections()
			delete(h.transports, id)
		}
	}
	return &Compiled{Snapshot: s, Hash: hash, Routes: route.NewTable(routes...)}, nil
}

// Static is a Holder with one compiled snapshot and no lease: for tests.
func Static(c *Compiled) *Holder {
	if c.Snapshot == nil {
		c.Snapshot = &snapshot.Snapshot{Schema: snapshot.Schema}
	}
	h := &Holder{}
	h.cur.Store(c)
	return h
}

// EnvSecrets resolves standalone credentials from the environment, and
// refuses Secret references (there is no cluster).
type EnvSecrets struct{ Getenv func(string) string }

func (e EnvSecrets) Secret(ref, env string) (string, error) {
	if env == "" {
		return "", fmt.Errorf("secretRef %s: standalone mode reads credentials from environment variables (spec.auth.env)", ref)
	}
	v := e.Getenv(env)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is not set", env)
	}
	return strings.TrimSpace(v), nil
}
