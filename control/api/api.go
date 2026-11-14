// Package api is golem-control's transport (control-plane.md §2,
// architecture.md §3): the Connect services generated from proto/golem,
// served on net/http, so each method is callable as gRPC, gRPC-Web or
// plain HTTP/JSON. It authenticates the caller, converts messages and maps
// errors; the rules are control/service's, and this package never writes
// the store (EntraAuth only reads it, to find a token's principal).
package api

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/gen/golem/keys/v1/keysv1connect"
	"github.com/chucklehead-dev/golem/gen/golem/org/v1/orgv1connect"
)

// Authenticator says who a request is from.
type Authenticator interface {
	Authenticate(ctx context.Context, h http.Header) (service.Caller, error)
}

// Server serves the org and key APIs.
type Server struct {
	Service *service.Service
	Auth    Authenticator
}

type callerKey struct{}

// Handler is the APIs' routes, every call authenticated first. Every
// method is unary, so a unary interceptor covers them all; a streaming
// method would need a streaming one too.
func (s *Server) Handler() http.Handler {
	authn := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			c, err := s.Auth.Authenticate(ctx, req.Header())
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			return next(context.WithValue(ctx, callerKey{}, c), req)
		}
	}))
	mux := http.NewServeMux()
	mux.Handle(orgv1connect.NewOrgServiceHandler(org{s.Service}, authn))
	mux.Handle(keysv1connect.NewKeyServiceHandler(keys{s.Service}, authn))
	return mux
}

// caller is the request's caller, as the interceptor authenticated them.
func caller(ctx context.Context) service.Caller {
	c, _ := ctx.Value(callerKey{}).(service.Caller)
	return c
}

func options(validateOnly bool) service.Options { return service.Options{ValidateOnly: validateOnly} }

// reply is a method's response: msg, or err as its Connect error.
func reply[T any](msg *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(msg), nil
}

// connectError maps the service's and store's errors to Connect codes; its
// message keeps the field path and what to do.
func connectError(err error) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, service.ErrPermission):
		code = connect.CodePermissionDenied
	case errors.Is(err, service.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, service.ErrInvalid):
		code = connect.CodeInvalidArgument
	case errors.Is(err, state.ErrInvariant):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, state.ErrContended):
		code = connect.CodeAborted
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeCanceled
	}
	return connect.NewError(code, err)
}
