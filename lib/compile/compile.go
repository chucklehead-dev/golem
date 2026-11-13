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
	PublishSeq uint64
	Now        time.Time
	Lease      time.Duration // zero: no lease (standalone only)
}

// Problem is one validation failure.
type Problem struct {
	Object string // Kind/name, and where it was read
	Path   string // e.g. spec.pool[0].backend
	Msg    string
}

func (p Problem) String() string { return p.Object + ": " + p.Path + ": " + p.Msg }

// Problems is every failure of one compile.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return fmt.Sprintf("%d problem(s) in the config:\n  %s", len(ps), strings.Join(lines, "\n  "))
}

// Compile validates objs and builds a snapshot, sorted so that equal configs
// give equal bytes.
func Compile(objs v1.Objects, o Options) (*snapshot.Snapshot, error) {
	var ps Problems
	add := func(obj, path, msg string, args ...any) {
		ps = append(ps, Problem{obj, path, fmt.Sprintf(msg, args...)})
	}
	if !o.Standalone && o.Lease <= 0 {
		add("compile", "options", "a published snapshot needs an authorization lease (ADR 0030)")
	}

	dialectOf := map[string]string{} // backend name to dialect
	deployments := map[string]map[string]bool{}
	var out []snapshot.Backend
	for _, src := range objs.Backends {
		b := src.Object
		obj := "Backend/" + b.Metadata.Name + " (" + src.Where() + ")"
		if b.Metadata.Name == "" {
			add(obj, "metadata.name", "required")
			continue
		}
		if _, dup := dialectOf[b.Metadata.Name]; dup {
			add(obj, "metadata.name", "another Backend has this name")
			continue
		}
		if !slices.Contains(Dialects, b.Spec.Dialect) {
			add(obj, "spec.dialect", "%q is not a dialect golem serves (%s)", b.Spec.Dialect, strings.Join(Dialects, ", "))
		}
		a := b.Spec.Auth
		sb := snapshot.Backend{Name: b.Metadata.Name, Dialect: b.Spec.Dialect, AllowPrivate: b.Spec.Network.AllowPrivateAddresses,
			Auth: snapshot.Auth{Header: strings.ToLower(a.Header), Scheme: a.Scheme, Env: a.Env}}
		switch {
		case a.Header == "":
			add(obj, "spec.auth.header", "required: the header that carries the provider credential")
		case (a.SecretRef == nil) == (a.Env == ""):
			add(obj, "spec.auth", "give exactly one of secretRef and env")
		case a.Env != "" && !o.Standalone:
			add(obj, "spec.auth.env", "environment credentials are for standalone mode only; use secretRef (ADR 0023)")
		case a.SecretRef != nil && (a.SecretRef.Name == "" || a.SecretRef.Key == ""):
			add(obj, "spec.auth.secretRef", "needs name and key")
		case a.SecretRef != nil:
			sb.Auth.SecretRef = a.SecretRef.Name + "/" + a.SecretRef.Key
		}
		if len(b.Spec.Deployments) == 0 {
			add(obj, "spec.deployments", "a Backend needs at least one deployment")
		}
		names := map[string]bool{}
		for i, d := range b.Spec.Deployments {
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
			if msg := checkURL(d.URL, o.Standalone); msg != "" {
				add(obj, at+".url", "%s", msg)
			}
			sb.Deployments = append(sb.Deployments, snapshot.Deployment{Name: d.Name, URL: d.URL, Model: d.Model})
		}
		slices.SortFunc(sb.Deployments, func(x, y snapshot.Deployment) int { return cmp.Compare(x.Name, y.Name) })
		out = append(out, sb)
		dialectOf[sb.Name] = sb.Dialect
		deployments[sb.Name] = names
	}

	aliases := map[string]string{}
	var routes []snapshot.Route
	for _, src := range objs.Routes {
		r := src.Object
		obj := "ModelRoute/" + r.Metadata.Name + " (" + src.Where() + ")"
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
		for i, m := range r.Spec.Pool {
			at := fmt.Sprintf("spec.pool[%d]", i)
			dialect, ok := dialectOf[m.Backend]
			switch {
			case !ok:
				add(obj, at+".backend", "no Backend named %q", m.Backend)
				continue
			case !deployments[m.Backend][m.Deployment]:
				add(obj, at+".deployment", "Backend %q has no deployment %q", m.Backend, m.Deployment)
				continue
			case m.Weight < 0:
				add(obj, at+".weight", "must not be negative")
			}
			// One dialect per route: conversion is explicit, never implied by a
			// mixed pool (ADR 0033).
			if sr.Dialect == "" {
				sr.Dialect = dialect
			} else if dialect != sr.Dialect {
				add(obj, at+".backend", "Backend %q speaks %s, the pool's first member %s: a route's pool has one dialect (ADR 0033)",
					m.Backend, dialect, sr.Dialect)
			}
			w := m.Weight
			if w == 0 {
				w = 1
			}
			sr.Pool = append(sr.Pool, snapshot.Target{Backend: m.Backend, Deployment: m.Deployment, Weight: w})
		}
		routes = append(routes, sr)
	}
	ents := entitlements(objs, o, aliases, add)
	currency, prices := priceList(objs, add)
	// A scope with a budget is charged by price: every model it can reach
	// needs one, or its budget could never be spent (contracts.md §4).
	priced := map[string]bool{}
	for _, p := range prices {
		priced[p.Model] = true
	}
	modelsOf := map[string][]string{} // alias -> provider models
	for _, r := range routes {
		for _, m := range r.Pool {
			for _, b := range out {
				for _, d := range b.Deployments {
					if b.Name == m.Backend && d.Name == m.Deployment {
						modelsOf[r.Alias] = append(modelsOf[r.Alias], d.Model)
					}
				}
			}
		}
	}
	for _, sc := range ents.Scopes {
		if sc.BudgetMicros == 0 {
			continue
		}
		for _, a := range sc.Models {
			for _, m := range modelsOf[a] {
				if !priced[m] {
					add("scope "+sc.ID, "models", "alias %q reaches model %q, which no PriceList prices: the scope's budget could not be charged", a, m)
				}
			}
		}
	}
	if len(ps) > 0 {
		return nil, ps
	}

	slices.SortFunc(out, func(x, y snapshot.Backend) int { return cmp.Compare(x.Name, y.Name) })
	slices.SortFunc(routes, func(x, y snapshot.Route) int { return cmp.Compare(x.Alias, y.Alias) })
	s := &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: o.PublishSeq, IssuedAt: o.Now.UTC(),
		Backends: out, Routes: routes, Entitlements: ents, Currency: currency, Prices: prices}
	if o.Lease > 0 {
		s.ValidUntil = o.Now.Add(o.Lease).UTC()
	}
	s.Source.ConfigDigest = digest(s.Backends, s.Routes)
	return s, nil
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

// checkURL: https everywhere, plain http only to a loopback address (a TLS
// proxy in front of a local model server) or in standalone mode.
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
