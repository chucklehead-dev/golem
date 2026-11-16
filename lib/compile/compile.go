// Package compile is the snapshot compiler: config objects in, a validated
// snapshot out, or every problem with its object and field path. One
// validation path (ADR 0005): golemctl config validate runs it offline,
// golem-control on the live set, the standalone gateway on its directory.
package compile

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Dialects the gateway serves.
var Dialects = []string{"anthropic.messages", "openai.chat", "openai.responses"} // read-only

// Options are the compile's context.
type Options struct {
	// Standalone allows env credentials and plain-HTTP endpoints on any
	// host, and produces a snapshot with no lease (ADR 0023).
	Standalone bool
	PublishSeq uint64        // the publication's sequence number
	Now        time.Time     // the snapshot's IssuedAt, and its lease's start
	Lease      time.Duration // zero: no lease (standalone only)
	// PeriodZone is the budget periods' time zone the snapshot states
	// (golem-control's -period-zone); "": none stated.
	PeriodZone string
	// ConfigRevision is the config's git commit, the snapshot's
	// Source.ConfigRevision ("": not from a checkout). A renewal keeps the
	// renewed publication's instead: its config part is that one's.
	ConfigRevision snapshot.Revision
	// Entitlements gives a published snapshot's entitlements (the state
	// document's, control/state), told the route aliases that exist: its
	// scopes are IDs (and known spend), which the compile joins with the
	// config's ScopePolicies by name. nil: none, a snapshot that authorizes
	// no one, and the ScopePolicies are only checked (golemctl config
	// validate, which has no state).
	Entitlements func(aliases []string) snapshot.Entitlements
	// Unmatched, if set, is told of each scope of Entitlements' that no
	// ScopePolicy names (published as DefaultScope: it can call nothing),
	// and of each ScopePolicy whose scope the state has not (left out). The
	// publication goes on: the state and the config change apart, so a
	// mismatch is reported, not fatal to every pool's publication (and with
	// it every gateway's authorization). nil: not reported.
	Unmatched func(Problem)
}

// Problem is one validation failure.
type Problem struct {
	Object string // Kind/name, and where it was read
	Path   string // e.g. spec.pool[0].backend
	Msg    string
}

// String is the problem as golemctl prints it: object, path, message.
func (p Problem) String() string { return p.Object + ": " + p.Path + ": " + p.Msg }

// Problems is every failure of one compile.
type Problems []Problem

// Error lists the problems, one per line.
func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return fmt.Sprintf("%d problem(s) in the config:\n  %s", len(ps), strings.Join(lines, "\n  "))
}

// add records a Problem; it is every pass's adder, so the problems come
// out in the order the passes run.
func (ps *Problems) add(obj, path, msg string, args ...any) {
	*ps = append(*ps, Problem{obj, path, fmt.Sprintf(msg, args...)})
}

// adder records a Problem: the object, the field path, and a message
// formatted as by fmt.Sprintf.
type adder func(obj, path, msg string, args ...any)

// Compile validates objs and builds a snapshot, sorted so that equal configs
// give equal bytes. Problems are reported in the order of the objects'
// kinds (backends, routes, scope policies, the local identity, prices)
// and, within a kind, of the objects.
func Compile(objs v1.Objects, o Options) (*snapshot.Snapshot, error) {
	var ps Problems
	add := ps.add
	if !o.Standalone && o.Lease <= 0 {
		add("compile", "options", "a published snapshot needs an authorization lease (ADR 0030)")
	}
	backends, cat := compileBackends(objs.Backends, o.Standalone, add)
	routes, aliases := compileRoutes(objs.Routes, cat, add)
	policies := compilePolicies(objs.Policies, aliases, add)
	// entitlements also refuses a LocalIdentity outside standalone mode, so
	// it runs even when the state document's entitlements replace its own.
	ents := entitlements(objs, o, policies, add)
	if !o.Standalone && o.Entitlements != nil {
		ents = o.Entitlements(slices.Sorted(maps.Keys(aliases)))
		ents.Scopes = join(ents.Scopes, policies, o.unmatched)
	}
	currency, prices := priceList(objs, add)
	checkPriced(policies, routes, cat, prices, add)
	if len(ps) > 0 {
		return nil, ps
	}

	slices.SortFunc(backends, func(x, y snapshot.Backend) int { return cmp.Compare(x.Name, y.Name) })
	slices.SortFunc(routes, func(x, y snapshot.Route) int { return cmp.Compare(x.Alias, y.Alias) })
	return o.publication(snapshot.Snapshot{Backends: backends, Routes: routes, Entitlements: ents, Currency: currency, Prices: prices}), nil
}

// Renew is a published snapshot rebuilt as a new publication: its config
// part (backends, routes, currency and prices, and its scopes' policies,
// which Compile built from the config) kept as published, and o's
// sequence, time, lease and entitlements, the state's scopes joined with
// those policies as Compile joins them with the ScopePolicies. A scope the
// state has created since is DefaultScope's (the snapshot keeps no
// policy it did not publish). This is what golem-control renews from when
// no config in its directory has compiled since it started (a restart
// over a broken edit).
func Renew(prev *snapshot.Snapshot, o Options) (*snapshot.Snapshot, error) {
	var ps Problems
	if o.Standalone || o.Lease <= 0 {
		ps.add("compile", "options", "a renewed publication needs an authorization lease (ADR 0030), and is never standalone")
	}
	if prev.Entitlements.Implicit || prev.ValidUntil.IsZero() {
		ps.add("snapshot", "entitlements", "publication %d is a standalone snapshot, not one golem-control published", prev.PublishSeq)
	}
	var ents snapshot.Entitlements
	if o.Entitlements != nil {
		var aliases []string
		for _, r := range prev.Routes {
			aliases = append(aliases, r.Alias)
		}
		slices.Sort(aliases)
		ents = o.Entitlements(aliases)
		ents.Scopes = join(ents.Scopes, policiesOf(prev.Entitlements.Scopes), o.unmatched)
	}
	if len(ps) > 0 {
		return nil, ps
	}
	s := o.publication(snapshot.Snapshot{Backends: prev.Backends, Routes: prev.Routes, Entitlements: ents,
		Currency: prev.Currency, Prices: prev.Prices})
	s.Source.ConfigRevision = prev.Source.ConfigRevision
	return s, nil
}

// publication is s as o's publication: its schema, sequence, time and
// lease (none if o has none), and its config's digest.
func (o Options) publication(s snapshot.Snapshot) *snapshot.Snapshot {
	s.Schema, s.PublishSeq, s.IssuedAt, s.PeriodZone = snapshot.Schema, o.PublishSeq, o.Now.UTC(), o.PeriodZone
	if o.Lease > 0 {
		s.ValidUntil = o.Now.Add(o.Lease).UTC()
	}
	s.Source.ConfigDigest, s.Source.ConfigRevision = digest(s.Backends, s.Routes), o.ConfigRevision
	return &s
}

// objectName names an object in a problem: its kind and name, and where it
// was read.
func objectName(kind, name, where string) string { return kind + "/" + name + " (" + where + ")" }

// target is one deployment of one backend, as a pool names it.
type target struct{ backend, deployment string }

// catalog is what routes may refer to: the backends compiled, each
// backend's first definition only.
type catalog struct {
	dialect map[string]string   // backend name to dialect
	models  map[target][]string // a deployment's provider models: one, unless its name is repeated
}

// compileBackends validates the Backends. Every backend with a usable name
// enters the catalog, problems or not, so that routes are checked against
// it rather than reported as dangling too.
func compileBackends(srcs []v1.Source[v1.Backend], standalone bool, add adder) ([]snapshot.Backend, catalog) {
	cat := catalog{dialect: map[string]string{}, models: map[target][]string{}}
	var out []snapshot.Backend
	for _, src := range srcs {
		b := src.Object
		obj := objectName("Backend", b.Metadata.Name, src.Where())
		if b.Metadata.Name == "" {
			add(obj, "metadata.name", "required")
			continue
		}
		if _, dup := cat.dialect[b.Metadata.Name]; dup {
			add(obj, "metadata.name", "another Backend has this name")
			continue
		}
		if !slices.Contains(Dialects, b.Spec.Dialect) {
			add(obj, "spec.dialect", "%q is not a dialect golem serves (%s)", b.Spec.Dialect, strings.Join(Dialects, ", "))
		}
		sb := snapshot.Backend{Name: b.Metadata.Name, Dialect: b.Spec.Dialect, AllowPrivate: b.Spec.Network.AllowPrivateAddresses,
			Envelope: b.Spec.Envelope, ProviderAccount: cmp.Or(b.Spec.ProviderAccount, b.Metadata.Name)}
		switch b.Spec.Envelope {
		case "":
			sb.Auth = backendAuth(obj, b.Spec.Auth, standalone, add)
			sb.Deployments = deployments(obj, b.Spec.Deployments, nil, standalone, add)
		case EnvelopeBedrock:
			sb.Deployments = bedrockBackend(obj, b.Spec, standalone, add)
		default:
			add(obj, "spec.envelope", "%q is not an envelope golem has (none, or %q)", b.Spec.Envelope, EnvelopeBedrock)
		}
		out = append(out, sb)
		cat.add(sb)
	}
	return out, cat
}

// add enters a compiled backend in the catalog.
func (cat catalog) add(b snapshot.Backend) {
	cat.dialect[b.Name] = b.Dialect
	for _, d := range b.Deployments {
		t := target{b.Name, d.Name}
		cat.models[t] = append(cat.models[t], d.Model)
	}
}

// EnvelopeBedrock is Claude on Amazon Bedrock's envelope.
const EnvelopeBedrock = "bedrock"

// bedrockBackend validates a bedrock Backend: the Messages dialect, no
// auth (its requests are signed by the gateway's AWS role), and its
// deployments, each in a region, its own or the backend's.
func bedrockBackend(obj string, spec v1.BackendSpec, standalone bool, add adder) []snapshot.Deployment {
	if spec.Dialect != "anthropic.messages" {
		add(obj, "spec.dialect", "a bedrock backend speaks anthropic.messages (Claude's Messages API on Bedrock)")
	}
	if spec.Auth != (v1.Auth{}) {
		add(obj, "spec.auth", "a bedrock backend has no auth: the gateway signs its requests with its AWS role (IRSA or Pod Identity)")
	}
	return deployments(obj, spec.Deployments, cmp.Or(spec.AWS, &v1.AWS{}), standalone, add)
}

// regionPattern is an AWS region's name (us-east-2, eu-central-1,
// us-gov-west-1): it becomes part of a host name.
var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`) // read-only

// deployments validates a Backend's spec.deployments and compiles them,
// sorted by name. Each is compiled, problems or not, so that the catalog
// knows it. aws is a bedrock backend's (nil for any other): a bedrock
// deployment needs a region, has no health check (Bedrock's own failures
// eject it), and its URL is optional (the region's Bedrock runtime by
// default).
func deployments(obj string, ds []v1.Deployment, aws *v1.AWS, standalone bool, add adder) []snapshot.Deployment {
	bedrock := aws != nil
	if len(ds) == 0 {
		add(obj, "spec.deployments", "a Backend needs at least one deployment")
	}
	var out []snapshot.Deployment
	names := map[string]bool{}
	for i, d := range ds {
		at := fmt.Sprintf("spec.deployments[%d]", i)
		switch {
		case d.Name == "":
			add(obj, at+".name", "required")
		case names[d.Name]:
			add(obj, at+".name", "another deployment of this Backend is named %q", d.Name)
		}
		names[d.Name] = true
		sd := snapshot.Deployment{Name: d.Name, URL: d.URL, Model: d.Model, MaxInFlight: max(d.MaxInFlight, 0),
			MaxInFlightPerPrincipal: max(d.MaxInFlightPerPrincipal, 0), Betas: betasOf(d.Betas)}
		// The problems in the order they always came: a bedrock
		// deployment's model, health check and region before its URL.
		if bedrock {
			if d.Model == "" {
				add(obj, at+".model", "required: the Bedrock model ID, inference profile ID or ARN")
			}
			if d.HealthCheck != nil {
				add(obj, at+".healthCheck", "a bedrock deployment has no health check: Bedrock's own failures eject it")
			}
			sd.Region = cmp.Or(d.Region, aws.Region)
			if !regionPattern.MatchString(sd.Region) {
				add(obj, at+".region", "%q: give the deployment's AWS region (here or in spec.aws.region), e.g. us-east-2", sd.Region)
			}
		} else if d.Model == "" {
			add(obj, at+".model", "required: the provider's name for the model")
		}
		if !bedrock || d.URL != "" {
			if msg := checkURL(d.URL, standalone); msg != "" {
				add(obj, at+".url", "%s", msg)
			}
		}
		if d.MaxInFlight < 0 {
			add(obj, at+".maxInFlight", "must not be negative (0: no ceiling)")
		}
		if d.MaxInFlightPerPrincipal < 0 {
			add(obj, at+".maxInFlightPerPrincipal", "must not be negative (0: no ceiling)")
		}
		if !bedrock {
			sd.HealthPath, sd.HealthIntervalMs = healthCheck(obj, at, d.HealthCheck, add)
		}
		out = append(out, sd)
	}
	slices.SortFunc(out, func(x, y snapshot.Deployment) int { return cmp.Compare(x.Name, y.Name) })
	return out
}

// betasOf is a deployment's accepted betas, sorted, each once; nil for
// any.
func betasOf(bs []string) []string {
	if len(bs) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(bs)))
}

// requestsPerMinute validates the Rate at path at (a scope's rate limit),
// giving its requests a minute (0 for none).
func requestsPerMinute(obj, at string, r *v1.Rate, add adder) int64 {
	if r == nil {
		return 0
	}
	if r.RequestsPerMinute < 0 {
		add(obj, at+".requestsPerMinute", "must not be negative (0: no limit)")
	}
	return max(r.RequestsPerMinute, 0)
}

// healthCheck validates a deployment's active check, giving its path and
// interval ("", 0 for none).
func healthCheck(obj, at string, hc *v1.HealthCheck, add adder) (string, int64) {
	if hc == nil {
		return "", 0
	}
	if !strings.HasPrefix(hc.Path, "/") || strings.ContainsAny(hc.Path, "?# ") {
		add(obj, at+".healthCheck.path", "%q: give a path on the deployment's host, e.g. /v1/models", hc.Path)
	}
	if hc.IntervalSeconds < 0 {
		add(obj, at+".healthCheck.intervalSeconds", "must not be negative (0: the default, 10)")
	}
	return hc.Path, int64(cmp.Or(max(hc.IntervalSeconds, 0), 10)) * 1000
}

// backendAuth validates a Backend's spec.auth and compiles it: the secret
// as a reference, never a value (ADR 0005). It reports at most one problem.
func backendAuth(obj string, a v1.Auth, standalone bool, add adder) snapshot.Auth {
	sa := snapshot.Auth{Header: strings.ToLower(a.Header), Scheme: a.Scheme, Env: a.Env}
	switch {
	case a.Header == "":
		add(obj, "spec.auth.header", "required: the header that carries the provider credential")
	case (a.SecretRef == nil) == (a.Env == ""):
		add(obj, "spec.auth", "give exactly one of secretRef and env")
	case a.Env != "" && !standalone:
		add(obj, "spec.auth.env", "environment credentials are for standalone mode only; use secretRef (ADR 0023)")
	case a.SecretRef != nil && (a.SecretRef.Name == "" || a.SecretRef.Key == ""):
		add(obj, "spec.auth.secretRef", "needs name and key")
	case a.SecretRef != nil:
		sa.SecretRef = a.SecretRef.Name + "/" + a.SecretRef.Key
	}
	return sa
}

// compileRoutes validates the ModelRoutes against the catalog. It returns
// the routes and their aliases, each with the ModelRoute that routes it.
func compileRoutes(srcs []v1.Source[v1.ModelRoute], cat catalog, add adder) ([]snapshot.Route, map[string]string) {
	aliases := map[string]string{}
	var routes []snapshot.Route
	for _, src := range srcs {
		r := src.Object
		obj := objectName("ModelRoute", r.Metadata.Name, src.Where())
		if r.Spec.Alias == "" {
			add(obj, "spec.alias", "required: the model name callers send")
			continue
		}
		if other, dup := aliases[r.Spec.Alias]; dup {
			add(obj, "spec.alias", "alias %q is already routed by %s", r.Spec.Alias, other)
			continue
		}
		aliases[r.Spec.Alias] = "ModelRoute/" + r.Metadata.Name
		if len(r.Spec.Pool) == 0 {
			add(obj, "spec.pool", "a route needs at least one deployment")
		}
		sr := snapshot.Route{Alias: r.Spec.Alias, Agent: r.Spec.Agent, ApprovedBetas: slices.Clone(r.Spec.ApprovedBetas)}
		sr.MaxDurationMs, sr.HeadersMs, sr.IdleMs = timeouts(r.Spec.Timeouts, func(field, msg string) { add(obj, "spec.timeouts."+field, "%s", msg) })
		sr.MaxAttempts = maxAttempts(r.Spec.Retries, func(msg string) { add(obj, "spec.retries.maxAttempts", "%s", msg) })
		sr.Dialect, sr.Pool = pool(obj, r.Spec.Pool, cat, add)
		routes = append(routes, sr)
	}
	return routes, aliases
}

// pool validates a route's spec.pool against the catalog and compiles it,
// with the pool's dialect, its first member's. A member that names no
// catalogued deployment is left out.
func pool(obj string, members []v1.PoolMember, cat catalog, add adder) (dialect string, out []snapshot.Target) {
	for i, m := range members {
		at := fmt.Sprintf("spec.pool[%d]", i)
		d, ok := cat.dialect[m.Backend]
		switch {
		case !ok:
			add(obj, at+".backend", "no Backend named %q", m.Backend)
			continue
		case cat.models[target{m.Backend, m.Deployment}] == nil:
			add(obj, at+".deployment", "Backend %q has no deployment %q", m.Backend, m.Deployment)
			continue
		case m.Weight != nil && *m.Weight < 0:
			add(obj, at+".weight", "must not be negative")
		case m.Weight != nil && *m.Weight == 0:
			add(obj, at+".weight", "weight 0 does not take %s/%s out of the pool: weights are not used in M1 (the gateway tries the pool in order); remove the member from spec.pool to stop routing to it, or leave a failing deployment to the gateway's ejection",
				m.Backend, m.Deployment)
		}
		// One dialect per route: conversion is explicit, never implied by a
		// mixed pool (ADR 0033).
		if dialect == "" {
			dialect = d
		} else if d != dialect {
			add(obj, at+".backend", "Backend %q speaks %s, the pool's first member %s: a route's pool has one dialect (ADR 0033)",
				m.Backend, d, dialect)
		}
		weight := 1
		if m.Weight != nil {
			weight = *m.Weight
		}
		out = append(out, snapshot.Target{Backend: m.Backend, Deployment: m.Deployment, Weight: weight})
	}
	return dialect, out
}

// unmatched reports p to o.Unmatched, if set.
func (o Options) unmatched(p Problem) {
	if o.Unmatched != nil {
		o.Unmatched(p)
	}
}

// checkPriced refuses a ScopePolicy with a budget that can reach an
// unpriced model: a scope with a budget is charged by price, so every
// model it can reach needs one, or its budget could never be spent
// (contracts.md §4). Both are config, so this is the config's problem,
// found by golemctl config validate before it is merged.
func checkPriced(policies map[string]compiledPolicy, routes []snapshot.Route, cat catalog, prices []snapshot.Price, add adder) {
	priced := map[string]bool{}
	for _, p := range prices {
		priced[p.Model] = true
	}
	modelsOf := map[string][]string{} // alias to provider models
	for _, r := range routes {
		for _, m := range r.Pool {
			modelsOf[r.Alias] = append(modelsOf[r.Alias], cat.models[target{m.Backend, m.Deployment}]...)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(policies)) {
		p := policies[id]
		if p.scope.BudgetMicros == 0 {
			continue
		}
		for _, a := range p.scope.Models {
			for _, m := range modelsOf[a] {
				if !priced[m] {
					add(p.obj, "spec.models", "alias %q reaches model %q, which no PriceList prices: the scope's budget could not be charged; price the model in a PriceList, or remove the alias", a, m)
				}
			}
		}
	}
}

// digest names the accepted config: equal configs, equal digests.
func digest(b []snapshot.Backend, r []snapshot.Route) string {
	j, _ := json.Marshal(struct {
		B []snapshot.Backend
		R []snapshot.Route
	}{b, r})
	h := sha256.Sum256(j)
	return hex.EncodeToString(h[:])
}

// checkURL says why a deployment's URL is refused, or "" if it is not:
// https everywhere, plain http only to a loopback address (a TLS proxy in
// front of a local model server) or in standalone mode.
func checkURL(raw string, standalone bool) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Sprintf("%q is not an absolute http(s) URL", raw)
	}
	if u.User != nil {
		return "credentials do not belong in a URL; use spec.auth"
	}
	if u.Scheme == "http" && !standalone {
		if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
			return "plain http is allowed only to a loopback address; put the model server behind TLS (ADR 0017)"
		}
	}
	return ""
}

// The default call bounds: half an hour for a whole call (a long agent
// turn), five minutes of a provider's silence (a reasoning pause is
// seconds to a minute or two).
const (
	DefaultMaxDuration = 30 * time.Minute
	DefaultIdle        = 5 * time.Minute
)

// DefaultMaxAttempts is a route's attempts per call unless it says: one
// failover, to the pool's next deployment where it has one (ADR 0051: a
// call never retries a deployment it tried). MaxAttempts is
// the most a route may ask for (each attempt is a provider's round trip
// before the caller sees a byte).
const (
	DefaultMaxAttempts = 2
	MaxAttempts        = 5
)

// maxAttempts reads a route's attempts per call, with the default.
func maxAttempts(r *v1.RouteRetries, bad func(msg string)) int {
	if r == nil || r.MaxAttempts == 0 {
		return DefaultMaxAttempts
	}
	if r.MaxAttempts < 1 || r.MaxAttempts > MaxAttempts {
		bad(fmt.Sprintf("%d: give 1 (no retries) to %d", r.MaxAttempts, MaxAttempts))
		return DefaultMaxAttempts
	}
	return r.MaxAttempts
}

// timeouts reads a route's call bounds, with the defaults, in milliseconds.
func timeouts(t *v1.RouteTimeouts, bad func(field, msg string)) (maxMs, headersMs, idleMs int64) {
	var spec v1.RouteTimeouts
	if t != nil {
		spec = *t
	}
	parse := func(field, v string, def time.Duration) time.Duration {
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad(field, fmt.Sprintf("%q is not a positive duration (e.g. \"30m\", \"90s\")", v))
			return def
		}
		return d
	}
	maxD := parse("maxDuration", spec.MaxDuration, DefaultMaxDuration)
	headers := parse("headers", spec.Headers, maxD)
	idle := parse("idle", spec.Idle, min(DefaultIdle, maxD))
	if headers > maxD {
		bad("headers", "longer than maxDuration, which bounds the whole call")
	}
	if idle > maxD {
		bad("idle", "longer than maxDuration, which bounds the whole call")
	}
	return maxD.Milliseconds(), headers.Milliseconds(), idle.Milliseconds()
}
