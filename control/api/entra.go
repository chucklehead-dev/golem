package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/entra"
)

// EntraAuth authenticates callers by an Entra access token for golem's
// API (ADR 0009): the token's user or service is the principal with that
// Entra ID in the state document; the golem.admin app role makes it an
// admin. A caller with no principal yet (an admin bootstrapping, or a
// user before their first membership) is "entra:<id>" in the audit, with
// no rights but an admin's.
type EntraAuth struct {
	Entra *entra.Verifier
	State *state.Store
}

// Authenticate verifies the request's bearer token and finds its
// principal in the document as last read: the store refreshes it in the
// background, so a principal just added elsewhere is found once it has.
func (a EntraAuth) Authenticate(_ context.Context, h http.Header) (service.Caller, error) {
	tok, ok := bearer(h)
	if !ok {
		return service.Caller{}, errors.New("an Entra access token is required (Authorization: Bearer); golemctl login gets one")
	}
	id, refused := a.Entra.Verify(tok)
	if refused != nil {
		return service.Caller{}, refused
	}
	c := service.Caller{Principal: "entra:" + id.ID, Admin: id.Admin}
	// The store keeps Entra IDs unique, so at most one principal matches.
	for pid, p := range a.State.Current().Principals {
		if p.Entra == id.ID && (p.Kind == "service") == id.Service {
			c.Principal = pid
			break
		}
	}
	return c, nil
}

// bearer is an Authorization header's token, if it has exactly one
// Authorization header and a non-empty token in it: a second header is not
// a credential to choose from.
func bearer(h http.Header) (string, bool) {
	if len(h.Values("Authorization")) != 1 {
		return "", false
	}
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	return tok, ok && tok != ""
}
