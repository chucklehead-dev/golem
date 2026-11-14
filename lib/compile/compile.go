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
	"slices"
	"strings"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Dialects the gateway serves.
var Dialects = []string{"anthropic.messages", "openai.chat"} // read-only

// Options are the compile's context.
type Options struct {
	// Standalone allows env credentials and plain-HTTP endpoints on any
	// host, and produces a snapshot with no lease (ADR 0023).
	Standalone bool
	PublishSeq uint64        // the publication's sequence number
	Now        time.Time     // the snapshot's IssuedAt, and its lease's start
	Lease      time.Duration // zero: no lease (standalone only)
	// Entitlements gives a published snapshot's entitlements (the state
	// document's, control/state), told the route aliases that exist; nil:
	// none, a snapshot that authorizes no one.
	Entitlements func(aliases []string) snapshot.Entitlements
	// Unpriced, if set, is told of each alias that a scope from
	// Entitlements, one with a budget, can reach though no PriceList prices
	// a model it routes to; the scope is published without that alias
	// rather than the compile failing. The scope fails closed: its members
	// cannot call what it could not be charged for, and every other scope
	// and pool is published as usual. A problem in the state's entitlements
	// is not the config's, so it must not stop every pool's publication
	// (and with it every gateway's authorization). nil: such a scope is a
	// problem, as a LocalIdentity's always is.
	Unpriced func(Problem)
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

// adder records a Problem: the object, the field path, and a message
// formatted as by fmt.Sprintf. Every pass reports through one, so the
// problems come out in the order the passes run.
type adder func(obj, path, msg string, args ...any)

// Compile validates objs and builds a snapshot, sorted so that equal configs
// give equal bytes. Problems are reported in the order of the objects'
// kinds (backends, routes, entitlements, prices) and, within a kind, of
// the objects.
func Compile(objs v1.Objects, o Options) (*snapshot.Snapshot, error) {
	var ps Problems
	add := func(obj, path, msg string, args ...any) {
		ps = append(ps, Problem{obj, path, fmt.Sprintf(msg, args...)})
	}
	if !o.Standalone && o.Lease <= 0 {
		add("compile", "options", "a published snapshot needs an authorization lease (ADR 0030)")
	}
	backends, cat := compileBackends(objs.Backends, o.Standalone, add)
	routes, aliases := compileRoutes(objs.Routes, cat, add)
	// entitlements also refuses a LocalIdentity outside standalone mode, so
	// it runs even when the state document's entitlements replace its own.
	ents := entitlements(objs, o, aliases, add)
	fromState := !o.Standalone && o.Entitlements != nil
	if fromState {
		ents = o.Entitlements(slices.Sorted(maps.Keys(aliases)))
	}
	currency, prices := priceList(objs, add)
	if fromState && o.Unpriced != nil {
		ents.Scopes = withoutUnpriced(ents.Scopes, routes, cat, prices, o.Unpriced)
	} else {
		checkPriced(ents.Scopes, routes, cat, prices, add)
	}
	if len(ps) > 0 {
		return nil, ps
	}

	slices.SortFunc(backends, func(x, y snapshot.Backend) int { return cmp.Compare(x.Name, y.Name) })
	slices.SortFunc(routes, func(x, y snapshot.Route) int { return cmp.Compare(x.Alias, y.Alias) })
	s := &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: o.PublishSeq, IssuedAt: o.Now.UTC(),
		Backends: backends, Routes: routes, Entitlements: ents, Currency: currency, Prices: prices}
	if o.Lease > 0 {
		s.ValidUntil = o.Now.Add(o.Lease).UTC()
	}
	s.Source.ConfigDigest = digest(s.Backends, s.Routes)
	return s, nil
}

// Renew is a published snapshot rebuilt as a new publication: its config
// part (backends, routes, currency and prices, which Compile built from
// the config alone) kept as published, and o's sequence, time, lease and
// entitlements, checked for unpriced scopes as Compile checks them. A
// published snapshot's entitlements are the state's alone (Compile refuses
// a LocalIdentity outside standalone mode), so nothing of the config is
// lost: this is what golem-control renews from when no config in its
// directory has compiled since it started (a restart over a broken edit).
func Renew(prev *snapshot.Snapshot, o Options) (*snapshot.Snapshot, error) {
	var ps Problems
	add := func(obj, path, msg string, args ...any) {
		ps = append(ps, Problem{obj, path, fmt.Sprintf(msg, args...)})
	}
	if o.Standalone || o.Lease <= 0 {
		add("compile", "options", "a renewed publication needs an authorization lease (ADR 0030), and is never standalone")
	}
	if prev.Entitlements.Implicit || prev.ValidUntil.IsZero() {
		add("snapshot", "entitlements", "publication %d is a standalone snapshot, not one golem-control published", prev.PublishSeq)
	}
	cat := catalog{dialect: map[string]string{}, models: map[target][]string{}}
	for _, b := range prev.Backends {
		cat.add(b)
	}
	var aliases []string
	for _, r := range prev.Routes {
		aliases = append(aliases, r.Alias)
	}
	slices.Sort(aliases)
	var ents snapshot.Entitlements
	if o.Entitlements != nil {
		ents = o.Entitlements(aliases)
	}
	if o.Unpriced != nil {
		ents.Scopes = withoutUnpriced(ents.Scopes, prev.Routes, cat, prev.Prices, o.Unpriced)
	} else {
		checkPriced(ents.Scopes, prev.Routes, cat, prev.Prices, add)
	}
	if len(ps) > 0 {
		return nil, ps
	}
	s := &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: o.PublishSeq, IssuedAt: o.Now.UTC(), ValidUntil: o.Now.Add(o.Lease).UTC(),
		Backends: prev.Backends, Routes: prev.Routes, Entitlements: ents, Currency: prev.Currency, Prices: prev.Prices}
	s.Source.ConfigDigest = digest(s.Backends, s.Routes)
	return s, nil
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
			Auth: backendAuth(b.Spec.Auth, standalone, func(path, msg string) { add(obj, path, "%s", msg) })}
		sb.Deployments = deployments(obj, b.Spec.Deployments, standalone, add)
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

// deployments validates a Backend's spec.deployments and compiles them,
// sorted by name. Each is compiled, problems or not, so that the catalog
// knows it.
func deployments(obj string, ds []v1.Deployment, standalone bool, add adder) []snapshot.Deployment {
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
		if d.Model == "" {
			add(obj, at+".model", "required: the provider's name for the model")
		}
		if msg := checkURL(d.URL, standalone); msg != "" {
			add(obj, at+".url", "%s", msg)
		}
		out = append(out, snapshot.Deployment{Name: d.Name, URL: d.URL, Model: d.Model})
	}
	slices.SortFunc(out, func(x, y snapshot.Deployment) int { return cmp.Compare(x.Name, y.Name) })
	return out
}

// backendAuth validates a Backend's spec.auth and compiles it: the secret
// as a reference, never a value (ADR 0005). It reports at most one problem.
func backendAuth(a v1.Auth, standalone bool, bad func(path, msg string)) snapshot.Auth {
	sa := snapshot.Auth{Header: strings.ToLower(a.Header), Scheme: a.Scheme, Env: a.Env}
	switch {
	case a.Header == "":
		bad("spec.auth.header", "required: the header that carries the provider credential")
	case (a.SecretRef == nil) == (a.Env == ""):
		bad("spec.auth", "give exactly one of secretRef and env")
	case a.Env != "" && !standalone:
		bad("spec.auth.env", "environment credentials are for standalone mode only; use secretRef (ADR 0023)")
	case a.SecretRef != nil && (a.SecretRef.Name == "" || a.SecretRef.Key == ""):
		bad("spec.auth.secretRef", "needs name and key")
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
		case m.Weight < 0:
			add(obj, at+".weight", "must not be negative")
		}
		// One dialect per route: conversion is explicit, never implied by a
		// mixed pool (ADR 0033).
		if dialect == "" {
			dialect = d
		} else if d != dialect {
			add(obj, at+".backend", "Backend %q speaks %s, the pool's first member %s: a route's pool has one dialect (ADR 0033)",
				m.Backend, d, dialect)
		}
		out = append(out, snapshot.Target{Backend: m.Backend, Deployment: m.Deployment, Weight: cmp.Or(m.Weight, 1)})
	}
	return dialect, out
}

// checkPriced refuses a scope with a budget that can reach an unpriced
// model: a scope with a budget is charged by price, so every model it can
// reach needs one, or its budget could never be spent (contracts.md §4).
func checkPriced(scopes []snapshot.Scope, routes []snapshot.Route, cat catalog, prices []snapshot.Price, add adder) {
	unpriced(scopes, routes, cat, prices, func(sc snapshot.Scope, alias, model string) {
		add("scope "+sc.ID, "models", "%s", unpricedMsg(alias, model))
	})
}

func unpricedMsg(alias, model string) string {
	return fmt.Sprintf("alias %q reaches model %q, which no PriceList prices: the scope's budget could not be charged", alias, model)
}

// withoutUnpriced is scopes with each alias that checkPriced would refuse
// taken out of its scope's models, report told of each once. A key that
// narrows its scope to such an alias is left as it is: the gateway
// intersects a key's models with its scope's, so it can call nothing.
func withoutUnpriced(scopes []snapshot.Scope, routes []snapshot.Route, cat catalog, prices []snapshot.Price, report func(Problem)) []snapshot.Scope {
	drop := map[string]map[string]bool{} // scope to aliases
	unpriced(scopes, routes, cat, prices, func(sc snapshot.Scope, alias, model string) {
		if drop[sc.ID] == nil {
			drop[sc.ID] = map[string]bool{}
		}
		if !drop[sc.ID][alias] {
			report(Problem{Object: "scope " + sc.ID, Path: "models", Msg: unpricedMsg(alias, model)})
		}
		drop[sc.ID][alias] = true
	})
	if len(drop) == 0 {
		return scopes
	}
	out := slices.Clone(scopes)
	for i, sc := range out {
		if d := drop[sc.ID]; d != nil {
			out[i].Models = slices.DeleteFunc(slices.Clone(sc.Models), func(a string) bool { return d[a] })
		}
	}
	return out
}

// unpriced calls bad with each alias of a scope with a budget that reaches
// a model no PriceList prices, and that model.
func unpriced(scopes []snapshot.Scope, routes []snapshot.Route, cat catalog, prices []snapshot.Price, bad func(sc snapshot.Scope, alias, model string)) {
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
	for _, sc := range scopes {
		if sc.BudgetMicros == 0 {
			continue
		}
		for _, a := range sc.Models {
			for _, m := range modelsOf[a] {
				if !priced[m] {
					bad(sc, a, m)
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
