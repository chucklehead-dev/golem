// Package snapshot holds the gateway's current configuration, a compiled
// lib/snapshot behind an atomic pointer. A request takes it once and uses
// it to its end. The package enforces the rules model/publication.md
// checks, and a connect test here checks them against the model. A
// snapshot is applied only if it is at least the floor and newer than the
// one applied. An expired authorization lease serves nothing new.
//
// The floor is inclusive. It is the lowest publication the gateway may
// accept, such as the publisher's trusted checkpoint after a restart with
// an empty disk. That checkpoint is the current publication, which the
// restarted gateway must be able to apply.
//
// Package variables marked read-only are never written after init.
package snapshot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/envelope/bedrock"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Compiled is a snapshot ready for the request path.
type Compiled struct {
	Snapshot *snapshot.Snapshot
	Hash     string
	Routes   *route.Table
	// Auth authenticates against the snapshot's entitlements. Nil, as in
	// tests, lets everyone call everything.
	Auth   *auth.Keys
	Prices map[string]snapshot.Price // by the provider's model name
	// Accounts are the backends' provider accounts by backend name, the
	// ledger's provider_account. A backend without one uses its name.
	Accounts map[string]string
}

// Errors the request path reports as 503s.
var (
	ErrNotReady = errors.New("snapshot: no configuration yet")
	ErrExpired  = errors.New("snapshot: the authorization lease has expired; the gateway serves no new requests until a renewed snapshot arrives")
)

// Holder is the gateway's current snapshot.
type Holder struct {
	cur atomic.Pointer[Compiled]
	// Now is the clock. Tests set it.
	Now func() time.Time
	// Env is the key environment this gateway accepts, Live, or Dev for
	// standalone.
	Env key.Env
	// Entra verifies Entra tokens. Nil means the gateway takes golem keys
	// only.
	Entra *entra.Verifier
	// LocalKey is the standalone gateway's generated key, the one credential
	// its implicit principal takes.
	LocalKey string
	// OnApply, if set, is called with each snapshot applied. The limiter
	// takes its known spend this way.
	OnApply func(*Compiled)
	// Deny, if set, is the site's emergency denylist, applied over every
	// snapshot's entitlements (control-plane.md).
	Deny *auth.DenyFile
	// AWS signs bedrock deployments' requests with the gateway's role,
	// through the AWS SDK's provider chain. Without it, a bedrock
	// deployment's calls fail saying what to configure.
	AWS aws.CredentialsProvider

	// applying serializes Apply, its OnApply included, so the callbacks see
	// snapshots in the order they were applied.
	applying sync.Mutex
	// mu guards the floor and the transports only. It is not held across
	// OnApply, so Floor and RaiseFloor do not wait on a sweep.
	mu    sync.Mutex
	floor uint64
	// transports are kept by deployment identity across snapshots, so
	// connections and their state survive a swap.
	transports map[transportKey]*http.Transport
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

// Applied is the snapshot applied, whether or not its lease holds, or nil
// before the first. Metrics read its age and the lease remaining.
func (h *Holder) Applied() *Compiled { return h.cur.Load() }

// Floor is the lowest publication the gateway may apply. It is the last
// one applied, or a trusted checkpoint.
func (h *Holder) Floor() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.floor
}

// RaiseFloor raises the floor to a trusted checkpoint before any snapshot
// is applied. The checkpoint is a restart's persisted one or the
// publisher's record.
func (h *Holder) RaiseFloor(seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.floor = max(h.floor, seq)
}

// Apply compiles a snapshot and swaps it in. It refuses a snapshot below
// the floor, one not newer than the snapshot applied, one whose lease has
// expired, and one that does not compile. A refusal keeps the previous
// snapshot.
func (h *Holder) Apply(s *snapshot.Snapshot, hash string, secrets Secrets) error {
	h.applying.Lock()
	defer h.applying.Unlock()
	c, err := h.swap(s, hash, secrets)
	if err != nil {
		return err
	}
	if h.OnApply != nil {
		h.OnApply(c)
	}
	return nil
}

// swap is Apply's checks, compilation and swap, under mu.
func (h *Holder) swap(s *snapshot.Snapshot, hash string, secrets Secrets) (*Compiled, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// No exemption for the first snapshot after a start: the floor may come
	// from a trusted checkpoint, and a replay below it is exactly what it
	// is there to refuse (model/publication.md, the trusted floor).
	if s.PublishSeq < h.floor {
		return nil, fmt.Errorf("snapshot %d is below the floor %d: refused (a replay or a stale publisher)", s.PublishSeq, h.floor)
	}
	if c := h.cur.Load(); c != nil && s.PublishSeq <= c.Snapshot.PublishSeq {
		return nil, fmt.Errorf("snapshot %d is not newer than the applied %d: refused", s.PublishSeq, c.Snapshot.PublishSeq)
	}
	if !s.ValidUntil.IsZero() && !h.now().Before(s.ValidUntil) {
		return nil, fmt.Errorf("snapshot %d expired at %s: refused", s.PublishSeq, s.ValidUntil)
	}
	c, err := h.compile(s, hash, secrets)
	if err != nil {
		// Not the publication's fault alone: a secret it names may be
		// mounted later. The bucket source retries it (applyError).
		return nil, applyError{err}
	}
	h.floor = s.PublishSeq
	h.cur.Store(c)
	return c, nil
}

func (h *Holder) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// compile builds s's request-path form: its deployments as targets, its
// routes over them, its entitlements and its prices.
func (h *Holder) compile(s *snapshot.Snapshot, hash string, secrets Secrets) (*Compiled, error) {
	targets, used, err := h.targets(s.Backends, secrets)
	if err != nil {
		return nil, err
	}
	routes := make([]*route.Route, 0, len(s.Routes))
	for _, r := range s.Routes {
		rt, err := compileRoute(r, targets)
		if err != nil {
			return nil, err
		}
		routes = append(routes, rt)
	}
	// Transports of deployments that left the config are dropped, and
	// their idle connections closed; requests still on the snapshot being
	// replaced finish on the connections they hold.
	for id, t := range h.transports {
		if !used[id] {
			t.CloseIdleConnections()
			delete(h.transports, id)
		}
	}
	prices := map[string]snapshot.Price{}
	for _, p := range s.Prices {
		prices[p.Model] = p
	}
	accounts := make(map[string]string, len(s.Backends))
	for _, b := range s.Backends {
		accounts[b.Name] = cmp.Or(b.ProviderAccount, b.Name)
	}
	return &Compiled{Snapshot: s, Hash: hash, Routes: route.NewTable(routes...),
		Auth: h.compileAuth(s.Entitlements), Prices: prices, Accounts: accounts}, nil
}

// targetKey names a deployment in a snapshot.
type targetKey struct{ backend, deployment string }

// transportKey is a deployment's identity for its connection pool: a
// deployment keeps its pool across snapshots while its name, scheme, host
// and network grant stay the same.
type transportKey struct {
	backend, deployment, scheme, host string
	allowPrivate                      bool
}

// targets resolves each backend's credential and makes a target of each of
// its deployments, on the deployment's kept transport or a new one; used
// are the transports the targets take.
func (h *Holder) targets(backends []snapshot.Backend, secrets Secrets) (targets map[targetKey]*route.Target, used map[transportKey]bool, err error) {
	if h.transports == nil {
		h.transports = map[transportKey]*http.Transport{}
	}
	targets, used = map[targetKey]*route.Target{}, map[transportKey]bool{}
	for _, b := range backends {
		var header http.Header // the provider credential; a bedrock backend signs instead
		if b.Envelope == "" {
			secret, err := secrets.Secret(b.Auth.SecretRef, b.Auth.Env)
			if err != nil {
				return nil, nil, fmt.Errorf("backend %s: %w", b.Name, err)
			}
			v := secret
			if b.Auth.Scheme != "" {
				v = b.Auth.Scheme + " " + secret
			}
			header = http.Header{http.CanonicalHeaderKey(b.Auth.Header): {v}}
		}
		for _, d := range b.Deployments {
			raw := d.URL
			if b.Envelope == "bedrock" && raw == "" {
				raw = bedrock.EndpointFor(d.Region).String()
			}
			u, err := url.Parse(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("backend %s deployment %s: %w", b.Name, d.Name, err)
			}
			id := transportKey{b.Name, d.Name, u.Scheme, u.Host, b.AllowPrivate}
			t := h.transports[id]
			if t == nil {
				t = upstreamTransport(b.AllowPrivate, nil)
				h.transports[id] = t
			}
			used[id] = true
			var rt http.RoundTripper = t
			if b.Envelope == "bedrock" {
				rt = &bedrock.Transport{Base: t, Endpoint: u, Region: d.Region, Model: d.Model, Credentials: h.AWS}
			}
			targets[targetKey{b.Name, d.Name}] = &route.Target{Backend: b.Name, Deployment: d.Name, Model: d.Model,
				URL: u, Transport: rt, Header: header, MaxInFlight: d.MaxInFlight, MaxPerPrincipal: d.MaxInFlightPerPrincipal, Envelope: b.Envelope,
				HealthPath: d.HealthPath, HealthInterval: time.Duration(d.HealthIntervalMs) * time.Millisecond,
				Betas: setOf(d.Betas),
				ID:    b.Name + "/" + d.Name}
		}
	}
	return targets, used, nil
}

// compileRoute builds a route over the snapshot's targets.
func compileRoute(r snapshot.Route, targets map[targetKey]*route.Target) (*route.Route, error) {
	rt := &route.Route{Alias: r.Alias, Dialect: r.Dialect, MaxDuration: time.Duration(r.MaxDurationMs) * time.Millisecond,
		Headers: time.Duration(r.HeadersMs) * time.Millisecond, Idle: time.Duration(r.IdleMs) * time.Millisecond,
		Agent: r.Agent, Betas: map[string]bool{}, MaxAttempts: r.MaxAttempts}
	for _, b := range r.ApprovedBetas {
		rt.Betas[b] = true
	}
	for _, m := range r.Pool {
		t := targets[targetKey{m.Backend, m.Deployment}]
		if t == nil {
			return nil, fmt.Errorf("route %s: no deployment %s/%s in the snapshot", r.Alias, m.Backend, m.Deployment)
		}
		rt.Pool = append(rt.Pool, t)
	}
	if len(rt.Pool) == 0 {
		return nil, fmt.Errorf("route %s: empty pool", r.Alias)
	}
	return rt, nil
}

// compileAuth indexes the snapshot's entitlements for this gateway's
// installation (Live unless Env says otherwise), with the standalone
// gateway's local key for an implicit principal.
func (h *Holder) compileAuth(e snapshot.Entitlements) *auth.Keys {
	k := auth.Compile(e, cmp.Or(h.Env, key.Live), h.now, h.Entra)
	k.Deny(h.Deny)
	if h.LocalKey != "" {
		k.RequireLocalKey(h.LocalKey)
	}
	return k
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

// upstreamTransport is a deployment's connection pool. Every setting is
// chosen rather than inherited from http.DefaultTransport:
//   - No proxy. Egress goes through the mesh, and a stray HTTPS_PROXY in a
//     pod must not redirect calls carrying provider keys.
//   - Idle connections are bounded per host only. DefaultTransport's limit
//     of 100 is a total over all hosts, which closed connections above 100
//     concurrent streams.
//   - HTTP/2 pings a connection silent for 30 seconds, so a dead one is
//     found under a long stream rather than at the next request.
//   - No response-header timeout here. A whole answer's headers come when
//     the model has finished, minutes for a long answer. The route's
//     timeouts bound that wait.
//   - Every dial's resolved address is checked (checkDestination). A name
//     that re-resolves somewhere forbidden is refused at its next
//     connection, and an open connection stays on the address checked.
//
// The resolver is nil, meaning the system's, except in tests. ADR 0004.
func upstreamTransport(allowPrivate bool, resolver *net.Resolver) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Resolver: resolver,
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

// metadata are cloud instance-metadata addresses outside link-local
// ranges (Alibaba's); link-local ones (AWS, GCP, Azure: 169.254.169.254,
// fd00:ec2::254 aside) are refused as link-local.
var metadata = []netip.Prefix{netip.MustParsePrefix("100.100.100.200/32"), netip.MustParsePrefix("fd00:ec2::254/128")} // read-only

// cgnat is shared address space (RFC 6598), private in effect.
var cgnat = netip.MustParsePrefix("100.64.0.0/10") // read-only

// checkDestination refuses to connect where a provider cannot be:
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

// setOf is a list as a set; nil for none.
func setOf(xs []string) map[string]bool {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
