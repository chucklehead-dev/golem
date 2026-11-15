// Package envelope is what the request path tells an envelope about a
// call beyond its HTTP request (ADR 0001: an envelope carries a dialect's
// request to a provider): the attempt's attribution, which an envelope
// may pass to the provider for its own records (Bedrock's request
// metadata). It is a leaf: the proxy sets it, envelopes read it.
package envelope

import "context"

// Attribution names an attempt as golem's ledger does: its IDs, and who
// made it. Nothing in it is a secret (the key is its display ID).
type Attribution struct {
	RequestID, AttemptID string
	Principal, Scope     string
	Key                  string // the key's display ID; empty for an Entra token
}

type attributionKey struct{}

// WithAttribution is ctx carrying a.
func WithAttribution(ctx context.Context, a *Attribution) context.Context {
	return context.WithValue(ctx, attributionKey{}, a)
}

// AttributionOf is the attribution ctx carries, or nil.
func AttributionOf(ctx context.Context) *Attribution {
	a, _ := ctx.Value(attributionKey{}).(*Attribution)
	return a
}
