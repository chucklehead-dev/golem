// Package api is golem-control's transport (control-plane.md §2,
// architecture.md §3): the Connect services generated from proto/golem,
// served on net/http, so each method is callable as gRPC, gRPC-Web or
// plain HTTP/JSON. It authenticates the caller, converts messages and maps
// errors; the rules are control/service's, and this package never writes
// the store (EntraAuth only reads it, to find a token's principal).
package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
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
	// MaxRequest bounds a request's message (default 1 MiB): the API's
	// messages are small, and an oversized one is refused unread.
	MaxRequest int
	Log        *slog.Logger
}

type callerKey struct{}

// Handler is the APIs' routes. Every call is authenticated from its
// headers before its body is read, so an unauthenticated caller costs a
// header check, never a decode; a message over MaxRequest is refused.
func (s *Server) Handler() http.Handler {
	limit := connect.WithReadMaxBytes(cmp.Or(s.MaxRequest, 1<<20))
	mux := http.NewServeMux()
	mux.Handle(orgv1connect.NewOrgServiceHandler(org{s}, limit))
	mux.Handle(keysv1connect.NewKeyServiceHandler(keys{s}, limit))
	errs := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.Auth.Authenticate(r.Context(), r.Header)
		if err != nil {
			errs.Write(w, r, connect.NewError(connect.CodeUnauthenticated, err))
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

// caller is the request's caller, as the interceptor authenticated them.
func caller(ctx context.Context) service.Caller {
	c, _ := ctx.Value(callerKey{}).(service.Caller)
	return c
}

func options(validateOnly bool) service.Options { return service.Options{ValidateOnly: validateOnly} }

// reply is a method's response: msg, or err as its Connect error.
func reply[T any](s *Server, msg *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, s.connectError(err)
	}
	return connect.NewResponse(msg), nil
}

// connectError maps the service's and store's errors to Connect codes; its
// message keeps the field path and what to do. Any other error (the
// bucket's, S3's) is logged and answered with a generic message: its text
// names buckets, keys and endpoints a caller has no need of.
func (s *Server) connectError(err error) error {
	var code connect.Code
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
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	default:
		id := rand.Text()[:12]
		cmp.Or(s.Log, slog.Default()).Error("control API: internal error", "id", id, "err", err)
		return connect.NewError(connect.CodeInternal, fmt.Errorf("internal error %s: see golem-control's log", id))
	}
	return connect.NewError(code, err)
}
