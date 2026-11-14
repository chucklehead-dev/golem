// Package api is golem-control's transport (control-plane.md §2,
// architecture.md §3): the Connect services generated from proto/golem,
// served on net/http, so each method is callable as gRPC, gRPC-Web or
// plain HTTP/JSON. It authenticates the caller and maps errors; the rules
// are control/service's, and this package never touches the store.
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	keysv1 "github.com/chucklehead-dev/golem/gen/golem/keys/v1"
	"github.com/chucklehead-dev/golem/gen/golem/keys/v1/keysv1connect"
	orgv1 "github.com/chucklehead-dev/golem/gen/golem/org/v1"
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

// Handler is the APIs' routes, every call authenticated first.
func (s *Server) Handler() http.Handler {
	authn := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			c, err := s.Auth.Authenticate(ctx, req.Header())
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			return next(context.WithValue(ctx, callerKey{}, c), req)
		}
	})
	mux := http.NewServeMux()
	mux.Handle(orgv1connect.NewOrgServiceHandler(org{s}, connect.WithInterceptors(authn)))
	mux.Handle(keysv1connect.NewKeyServiceHandler(keys{s}, connect.WithInterceptors(authn)))
	return mux
}

func caller(ctx context.Context) service.Caller {
	c, _ := ctx.Value(callerKey{}).(service.Caller)
	return c
}

// codeOf maps the service's and store's errors to Connect codes; its
// message keeps the field path and what to do.
func codeOf(err error) error {
	if err == nil {
		return nil
	}
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

type org struct{ s *Server }

func (o org) CreateScope(ctx context.Context, r *connect.Request[orgv1.CreateScopeRequest]) (*connect.Response[orgv1.CreateScopeResponse], error) {
	m, p := r.Msg, r.Msg.GetPolicy()
	err := o.s.Service.CreateScope(ctx, caller(ctx), service.Options{ValidateOnly: m.ValidateOnly}, m.Id, state.Scope{
		Models: p.GetModels(), BudgetMicros: p.GetBudgetMicros(), DeclaredWorstCaseCheck: p.GetDeclaredWorstCaseCheck(),
		PrincipalInFlight: int(p.GetPrincipalInFlight()), ScopeInFlight: int(p.GetScopeInFlight()),
		StorageAllowed: p.GetStorageAllowed(), MaxOutputTokens: p.GetMaxOutputTokens(), MaxInputBytes: p.GetMaxInputBytes(),
		Features: p.GetFeatures()})
	return connect.NewResponse(&orgv1.CreateScopeResponse{}), codeOf(err)
}

func (o org) AddMember(ctx context.Context, r *connect.Request[orgv1.AddMemberRequest]) (*connect.Response[orgv1.AddMemberResponse], error) {
	m := r.Msg.GetMember()
	err := o.s.Service.AddMember(ctx, caller(ctx), service.Options{ValidateOnly: r.Msg.ValidateOnly}, r.Msg.Scope,
		service.Member{ID: m.GetId(), Kind: m.GetKind(), Entra: m.GetEntra()})
	return connect.NewResponse(&orgv1.AddMemberResponse{}), codeOf(err)
}

func (o org) RemoveMember(ctx context.Context, r *connect.Request[orgv1.RemoveMemberRequest]) (*connect.Response[orgv1.RemoveMemberResponse], error) {
	err := o.s.Service.RemoveMember(ctx, caller(ctx), service.Options{ValidateOnly: r.Msg.ValidateOnly}, r.Msg.Scope, r.Msg.Principal)
	return connect.NewResponse(&orgv1.RemoveMemberResponse{}), codeOf(err)
}

func (o org) BindManager(ctx context.Context, r *connect.Request[orgv1.BindManagerRequest]) (*connect.Response[orgv1.BindManagerResponse], error) {
	err := o.s.Service.BindManager(ctx, caller(ctx), service.Options{ValidateOnly: r.Msg.ValidateOnly}, r.Msg.Scope, r.Msg.Principal)
	return connect.NewResponse(&orgv1.BindManagerResponse{}), codeOf(err)
}

type keys struct{ s *Server }

func (k keys) MintKey(ctx context.Context, r *connect.Request[keysv1.MintKeyRequest]) (*connect.Response[keysv1.MintKeyResponse], error) {
	m := r.Msg
	var expires time.Time
	if m.Expires != nil {
		expires = m.Expires.AsTime()
	}
	minted, err := k.s.Service.MintKey(ctx, caller(ctx), service.Options{ValidateOnly: m.ValidateOnly},
		service.KeyRequest{Owner: m.Owner, Scope: m.Scope, Models: m.Models, Expires: expires})
	if err != nil {
		return nil, codeOf(err)
	}
	res := connect.NewResponse(&keysv1.MintKeyResponse{Key: minted.Key, Hash: minted.Hash, Display: minted.Display})
	res.Header().Set("Cache-Control", "no-store") // the key is in it
	return res, nil
}

func (k keys) RevokeKey(ctx context.Context, r *connect.Request[keysv1.RevokeKeyRequest]) (*connect.Response[keysv1.RevokeKeyResponse], error) {
	err := k.s.Service.RevokeKey(ctx, caller(ctx), service.Options{ValidateOnly: r.Msg.ValidateOnly}, r.Msg.Hash)
	return connect.NewResponse(&keysv1.RevokeKeyResponse{}), codeOf(err)
}

// bearer is an Authorization header's token.
func bearer(h http.Header) (string, bool) {
	v := h.Get("Authorization")
	tok, ok := strings.CutPrefix(v, "Bearer ")
	return strings.TrimSpace(tok), ok && strings.TrimSpace(tok) != ""
}

func (o org) Whoami(ctx context.Context, _ *connect.Request[orgv1.WhoamiRequest]) (*connect.Response[orgv1.WhoamiResponse], error) {
	me, err := o.s.Service.Whoami(ctx, caller(ctx))
	if err != nil {
		return nil, codeOf(err)
	}
	return connect.NewResponse(&orgv1.WhoamiResponse{Principal: me.Principal, Admin: me.Admin, Kind: me.Kind,
		Scopes: me.Scopes, Manages: me.Manages}), nil
}

func (o org) ListScopes(ctx context.Context, _ *connect.Request[orgv1.ListScopesRequest]) (*connect.Response[orgv1.ListScopesResponse], error) {
	ids, err := o.s.Service.ListScopes(ctx, caller(ctx))
	if err != nil {
		return nil, codeOf(err)
	}
	return connect.NewResponse(&orgv1.ListScopesResponse{Ids: ids}), nil
}

func (o org) GetScope(ctx context.Context, r *connect.Request[orgv1.GetScopeRequest]) (*connect.Response[orgv1.GetScopeResponse], error) {
	v, err := o.s.Service.GetScope(ctx, caller(ctx), r.Msg.Id)
	if err != nil {
		return nil, codeOf(err)
	}
	p := v.Policy
	return connect.NewResponse(&orgv1.GetScopeResponse{Id: v.ID, Members: v.Members, Managers: v.Managers,
		Policy: &orgv1.ScopePolicy{Models: p.Models, BudgetMicros: p.BudgetMicros, DeclaredWorstCaseCheck: p.DeclaredWorstCaseCheck,
			PrincipalInFlight: int32(p.PrincipalInFlight), ScopeInFlight: int32(p.ScopeInFlight), StorageAllowed: p.StorageAllowed,
			MaxOutputTokens: p.MaxOutputTokens, MaxInputBytes: p.MaxInputBytes, Features: p.Features}}), nil
}

func (k keys) ListKeys(ctx context.Context, r *connect.Request[keysv1.ListKeysRequest]) (*connect.Response[keysv1.ListKeysResponse], error) {
	ks, err := k.s.Service.ListKeys(ctx, caller(ctx), r.Msg.Scope)
	if err != nil {
		return nil, codeOf(err)
	}
	res := &keysv1.ListKeysResponse{}
	for _, v := range ks {
		pk := &keysv1.Key{Hash: v.Hash, Display: v.Display, Owner: v.Owner, Scope: v.Scope, Models: v.Models}
		if !v.ExpiresAt.IsZero() {
			pk.Expires = timestamppb.New(v.ExpiresAt)
		}
		if !v.RevokedAt.IsZero() {
			pk.Revoked = timestamppb.New(v.RevokedAt)
		}
		res.Keys = append(res.Keys, pk)
	}
	return connect.NewResponse(res), nil
}
