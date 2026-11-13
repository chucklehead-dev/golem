// Package route resolves an alias to the deployment that serves it. The
// table is built from a snapshot and replaced whole; nothing in it changes
// after it is built, so requests read it without locks (gateway.md §1).
// Strategies, health and affinity come later; for now a route picks its
// first deployment.
package route

import (
	"net/http"
	"net/url"
	"time"
)

// Target is one deployment as the request path uses it.
type Target struct {
	Backend, Deployment string
	Model               string // the provider's model name
	URL                 *url.URL
	Transport           http.RoundTripper
	Header              http.Header // the provider credential; never logged
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
}

// Pick chooses the deployment for one request.
func (r *Route) Pick() *Target { return r.Pool[0] }

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

// Len is the number of routes.
func (t *Table) Len() int { return len(t.byAlias) }
