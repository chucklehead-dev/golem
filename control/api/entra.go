package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/gateway/auth"
)

// EntraAuth authenticates callers by an Entra access token for golem's
// API (ADR 0009): the token's user or service is the principal with that
// Entra ID in the state document; the golem.admin app role makes it an
// admin. A caller with no principal yet (an admin bootstrapping, or a
// user before their first membership) is "entra:<id>" in the audit, with
// no rights but an admin's.
type EntraAuth struct {
	Entra *auth.Entra
	State *state.Store
}

func (a EntraAuth) Authenticate(_ context.Context, h http.Header) (service.Caller, error) {
	tok, ok := bearer(h)
	if !ok {
		return service.Caller{}, errors.New("an Entra access token is required (Authorization: Bearer); golemctl login gets one")
	}
	id, no := a.Entra.Verify(tok)
	if no != nil {
		return service.Caller{}, errors.New(no.Msg)
	}
	c := service.Caller{Principal: "entra:" + id.ID, Admin: id.Admin}
	for pid, p := range a.State.Current().Principals {
		if p.Entra == id.ID && (p.Kind == "service") == id.Service {
			c.Principal = pid
			break
		}
	}
	return c, nil
}
