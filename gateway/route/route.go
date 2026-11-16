// Package route resolves an alias to the deployment that serves it. The
// table is built from a snapshot and replaced whole; nothing in it changes
// after it is built, so requests read it without locks (gateway.md §1).
// A call's attempts take the pool in order (failover order), each from a
// deployment Health admits (health.go); weights and affinity come later.
package route

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Target is one deployment as the request path uses it.
type Target struct {
	Backend, Deployment string
	Model               string // the provider's model name
	URL                 *url.URL
	Transport           http.RoundTripper
	Header              http.Header // the provider credential; never logged
	// MaxInFlight is this gateway's ceiling on the deployment's calls in
	// flight; 0: none.
	MaxInFlight int
	// MaxPerPrincipal is this gateway's ceiling on one principal's calls in
	// flight on the deployment (a shared local box); 0: none.
	MaxPerPrincipal int
	// HealthPath, if set, is checked every HealthInterval by a Checker
	// (lib/api's HealthCheck).
	HealthPath     string
	HealthInterval time.Duration
	// QuotaRPM is the site's share of the provider's quota, requests a
	// minute (lib/api's); 0: none.
	QuotaRPM int64
	// Betas are the anthropic-beta values the deployment accepts; nil: any.
	Betas map[string]bool
	// Envelope is how its requests reach the provider: "" or "bedrock"
	// (the ledger's envelope; the transport does the work).
	Envelope string
	// ID is "backend/deployment", set when the snapshot is compiled (Key
	// makes it if not), so the request path does not build it per call.
	ID string
}

// Accepts reports whether the deployment accepts every one of betas.
func (t *Target) Accepts(betas []string) bool {
	if t.Betas == nil {
		return true
	}
	for _, b := range betas {
		if !t.Betas[b] {
			return false
		}
	}
	return true
}

// Key is the deployment's identity across snapshots, "backend/deployment":
// what its health is kept by, and the ledger's deployment.
func (t *Target) Key() string {
	if t.ID != "" {
		return t.ID
	}
	return t.Backend + "/" + t.Deployment
}

// Route is an alias and its pool.
type Route struct {
	Alias   string
	Dialect string
	Pool    []*Target
	// A call's bounds on this route: the whole call, the wait for the
	// response's headers, and a provider's silence once it has begun.
	MaxDuration, Headers, Idle time.Duration
	// Agent: a coding-agent route (compatible features by default); Betas,
	// the anthropic-beta values restricted mode forwards.
	Agent bool
	Betas map[string]bool
	// MaxAttempts is the most attempts a call makes before it commits.
	MaxAttempts int
}

// Table is every route of one snapshot, by alias.
type Table struct{ byAlias map[string]*Route }

// NewTable builds a table; routes must have unique aliases and non-empty
// pools (lib/compile guarantees both).
func NewTable(routes ...*Route) *Table {
	t := &Table{byAlias: make(map[string]*Route, len(routes))}
	for _, r := range routes {
		t.byAlias[r.Alias] = r
	}
	return t
}

// Lookup finds an alias, without allocating.
func (t *Table) Lookup(alias []byte) *Route { return t.byAlias[string(alias)] }

// Aliases are the aliases of a dialect's routes, sorted.
func (t *Table) Aliases(dialect string) []string {
	var out []string
	for a, r := range t.byAlias {
		if r.Dialect == dialect {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

// Targets are the table's deployments, each once, sorted by Key.
func (t *Table) Targets() []*Target {
	seen := map[string]*Target{}
	for _, r := range t.byAlias {
		for _, d := range r.Pool {
			seen[d.Key()] = d
		}
	}
	return slices.SortedFunc(maps.Values(seen), func(a, b *Target) int { return strings.Compare(a.Key(), b.Key()) })
}

// Len is the number of routes.
func (t *Table) Len() int { return len(t.byAlias) }
