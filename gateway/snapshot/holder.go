// Package snapshot holds the gateway's current configuration: a compiled
// lib/snapshot behind an atomic pointer, taken once per request and used to
// its end (gateway.md §1, §6). It enforces the rules the publication model
// checks (model/publication.md, and its connect test here): a snapshot is
// applied only if it is at least the floor and newer than the one applied,
// and an expired authorization lease serves nothing new.
//
// The floor is inclusive: it is the lowest publication the gateway may
// accept, such as the publisher's trusted checkpoint after a restart with an
// empty disk. That checkpoint is the current publication, which the
// restarted gateway must be able to apply.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Compiled is a snapshot ready for the request path.
type Compiled struct {
	Snapshot *snapshot.Snapshot
	Hash     string
	Routes   *route.Table
	// Auth authenticates against the snapshot's entitlements; nil (tests)
	// lets everyone call everything.
	Auth   *auth.Keys
	Prices map[string]snapshot.Price // by the provider's model name
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
	// Env is the key prefix this gateway accepts: Live, or Dev standalone.
	Env key.Env
	// Entra verifies Entra tokens; nil: the gateway takes golem keys only.
	Entra *auth.Entra
	// LocalKey is the standalone gateway's generated key, the one credential
	// its implicit principal takes (golem#5).
	LocalKey string
	// OnApply, if set, is called with each snapshot applied (the limiter
	// takes its known spend).
	OnApply func(*Compiled)

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

// Floor is the lowest publication the gateway may apply: the last one it
// applied, or a trusted checkpoint.
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

// Apply compiles s and swaps it in. A snapshot below the floor, one not
// newer than the snapshot applied, one whose lease has already expired, or
// one that does not compile is refused and the previous snapshot kept.
func (h *Holder) Apply(s *snapshot.Snapshot, hash string, secrets Secrets) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// No exemption for the first snapshot after a start: the floor may come
	// from a trusted checkpoint, and a replay below it is exactly what it
	// is there to refuse (model/publication.md, the trusted floor).
	if s.PublishSeq < h.floor {
		return fmt.Errorf("snapshot %d is below the floor %d: refused (a replay or a stale publisher)", s.PublishSeq, h.floor)
	}
	if c := h.cur.Load(); c != nil && s.PublishSeq <= c.Snapshot.PublishSeq {
		return fmt.Errorf("snapshot %d is not newer than the applied %d: refused", s.PublishSeq, c.Snapshot.PublishSeq)
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
	if h.OnApply != nil {
		h.OnApply(c)
	}
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
	type depKey struct{ backend, deployment string }
	targets := map[depKey]*route.Target{}
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
			id := fmt.Sprint(b.Name, "/", d.Name, " ", u.Scheme, "://", u.Host, " private:", b.AllowPrivate)
			t := h.transports[id]
			if t == nil {
				t = upstreamTransport(b.AllowPrivate)
				h.transports[id] = t
			}
			used[id] = true
			targets[depKey{b.Name, d.Name}] = &route.Target{Backend: b.Name, Deployment: d.Name, Model: d.Model,
				URL: u, Transport: t, Header: header}
		}
	}
	routes := make([]*route.Route, 0, len(s.Routes))
	for _, r := range s.Routes {
		rt := &route.Route{Alias: r.Alias, Dialect: r.Dialect, MaxDuration: time.Duration(r.MaxDurationMs) * time.Millisecond,
			Headers: time.Duration(r.HeadersMs) * time.Millisecond, Idle: time.Duration(r.IdleMs) * time.Millisecond,
			Agent: r.Agent, Betas: map[string]bool{}}
		for _, b := range r.ApprovedBetas {
			rt.Betas[b] = true
		}
		for _, m := range r.Pool {
			t := targets[depKey{m.Backend, m.Deployment}]
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
	env := h.Env
	if env == "" {
		env = key.Live
	}
	prices := map[string]snapshot.Price{}
	for _, p := range s.Prices {
		prices[p.Model] = p
	}
	return &Compiled{Snapshot: s, Hash: hash, Routes: route.NewTable(routes...),
		Auth: h.compileAuth(s.Entitlements, env), Prices: prices}, nil
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

// upstreamTransport is a deployment's connection pool, every setting chosen
// rather than inherited from http.DefaultTransport:
//   - no proxy: egress goes through the mesh (ADR 0004), and a stray
//     HTTPS_PROXY in a pod must not redirect calls carrying provider keys;
//   - idle connections bounded per host only: DefaultTransport's
//     MaxIdleConns of 100 is a total over all hosts, so above 100
//     concurrent streams connections were closed and re-handshaken;
//   - HTTP/2 pings on a connection silent for 30 seconds, so a dead one is
//     found under a long stream rather than at the next request;
//   - no response-header timeout here: a non-streamed answer's headers come
//     when the model has finished, which for a long answer is minutes; that
//     bound is the route's (ModelRoute spec.timeouts: headers, maxDuration).
func upstreamTransport(allowPrivate bool) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			return checkDestination(address, allowPrivate)
		}}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         d.DialContext,
		ForceAttemptHTTP2:   true,
		HTTP2:               &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second},
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// compileAuth indexes the snapshot's entitlements, with the standalone
// gateway's local key for an implicit principal.
func (h *Holder) compileAuth(e snapshot.Entitlements, env key.Env) *auth.Keys {
	k := auth.Compile(e, env, h.now, h.Entra)
	if h.LocalKey != "" {
		k.RequireLocalKey(h.LocalKey)
	}
	return k
}

// metadata are cloud instance-metadata addresses outside link-local
// ranges (Alibaba's); link-local ones (AWS, GCP, Azure: 169.254.169.254,
// fd00:ec2::254 aside) are refused as link-local.
var metadata = []netip.Prefix{netip.MustParsePrefix("100.100.100.200/32"), netip.MustParsePrefix("fd00:ec2::254/128")} // read-only

// cgnat is shared address space (RFC 6598), private in effect.
var cgnat = netip.MustParsePrefix("100.64.0.0/10") // read-only

// checkDestination refuses to connect where a provider cannot be (golem#5):
// the address a deployment's name resolved to, checked as the connection
// is made, so a name that resolves somewhere else later is caught too.
// Cloud metadata, link-local, unspecified and multicast addresses are
// refused always; loopback and private ones unless the backend allows
// them.
func checkDestination(address string, allowPrivate bool) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("golem: refusing to dial %q: not an address", address)
	}
	ip := ap.Addr().Unmap()
	for _, p := range metadata {
		if p.Contains(ip) {
			return fmt.Errorf("golem: refusing to dial %s: a cloud metadata address", ip)
		}
	}
	switch {
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("golem: refusing to dial %s: link-local, unspecified or multicast", ip)
	case !allowPrivate && (ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)):
		return fmt.Errorf("golem: refusing to dial %s: a loopback or private address, which this backend is not granted (spec.network.allowPrivateAddresses)", ip)
	}
	return nil
}
