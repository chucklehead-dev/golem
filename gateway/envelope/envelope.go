// Package envelope is what the request path tells an envelope about a call
// beyond its HTTP request. An envelope carries a dialect's request to a
// provider. The request path gives it the attempt's attribution, which it
// may pass to the provider for the provider's own records. The package is
// a leaf. The proxy sets the attribution and envelopes read it. ADR 0001.
package envelope

import (
	"context"
	"errors"
)

// ErrNotSent is what an envelope's error wraps when the request never left
// the gateway (its body could not be read or rewritten, or it could not be
// signed): the provider cannot have seen it, so the request path charges
// nothing for it and may fail over (gateway/proxy's reachedProvider).
var ErrNotSent = errors.New("envelope: the request was not sent")

// NotSent is err marked as a request that never left the gateway: its
// message is err's, and errors.Is finds both err and ErrNotSent. NotSent
// of nil is nil.
func NotSent(err error) error {
	if err == nil {
		return nil
	}
	return notSent{err}
}

type notSent struct{ err error }

func (e notSent) Error() string   { return e.err.Error() }
func (e notSent) Unwrap() []error { return []error{e.err, ErrNotSent} }

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
