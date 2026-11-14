package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	orgv1 "github.com/chucklehead-dev/golem/gen/golem/org/v1"
)

// org is golem.org.v1.OrgService.
type org struct{ s *service.Service }

func (o org) CreateScope(ctx context.Context, r *connect.Request[orgv1.CreateScopeRequest]) (*connect.Response[orgv1.CreateScopeResponse], error) {
	m := r.Msg
	err := o.s.CreateScope(ctx, caller(ctx), options(m.ValidateOnly), m.Id, policyToState(m.Policy))
	return reply(&orgv1.CreateScopeResponse{}, err)
}

func (o org) AddMember(ctx context.Context, r *connect.Request[orgv1.AddMemberRequest]) (*connect.Response[orgv1.AddMemberResponse], error) {
	m, mem := r.Msg, r.Msg.GetMember()
	err := o.s.AddMember(ctx, caller(ctx), options(m.ValidateOnly), m.Scope,
		service.Member{ID: mem.GetId(), Kind: mem.GetKind(), Entra: mem.GetEntra()})
	return reply(&orgv1.AddMemberResponse{}, err)
}

func (o org) RemoveMember(ctx context.Context, r *connect.Request[orgv1.RemoveMemberRequest]) (*connect.Response[orgv1.RemoveMemberResponse], error) {
	m := r.Msg
	err := o.s.RemoveMember(ctx, caller(ctx), options(m.ValidateOnly), m.Scope, m.Principal)
	return reply(&orgv1.RemoveMemberResponse{}, err)
}

func (o org) BindManager(ctx context.Context, r *connect.Request[orgv1.BindManagerRequest]) (*connect.Response[orgv1.BindManagerResponse], error) {
	m := r.Msg
	err := o.s.BindManager(ctx, caller(ctx), options(m.ValidateOnly), m.Scope, m.Principal)
	return reply(&orgv1.BindManagerResponse{}, err)
}

func (o org) Whoami(ctx context.Context, _ *connect.Request[orgv1.WhoamiRequest]) (*connect.Response[orgv1.WhoamiResponse], error) {
	me, err := o.s.Whoami(ctx, caller(ctx))
	return reply(&orgv1.WhoamiResponse{Principal: me.Principal, Admin: me.Admin, Kind: me.Kind,
		Scopes: me.Scopes, Manages: me.Manages}, err)
}

func (o org) ListScopes(ctx context.Context, _ *connect.Request[orgv1.ListScopesRequest]) (*connect.Response[orgv1.ListScopesResponse], error) {
	ids, err := o.s.ListScopes(ctx, caller(ctx))
	return reply(&orgv1.ListScopesResponse{Ids: ids}, err)
}

func (o org) GetScope(ctx context.Context, r *connect.Request[orgv1.GetScopeRequest]) (*connect.Response[orgv1.GetScopeResponse], error) {
	v, err := o.s.GetScope(ctx, caller(ctx), r.Msg.Id)
	return reply(&orgv1.GetScopeResponse{Id: v.ID, Policy: policyFromState(v.Policy),
		Members: v.Members, Managers: v.Managers}, err)
}

// policyToState is a scope's policy as the state stores it; a request
// with no policy is the zero policy.
func policyToState(p *orgv1.ScopePolicy) state.Scope {
	return state.Scope{
		Models:                 p.GetModels(),
		BudgetMicros:           p.GetBudgetMicros(),
		DeclaredWorstCaseCheck: p.GetDeclaredWorstCaseCheck(),
		PrincipalInFlight:      int(p.GetPrincipalInFlight()),
		ScopeInFlight:          int(p.GetScopeInFlight()),
		StorageAllowed:         p.GetStorageAllowed(),
		MaxOutputTokens:        p.GetMaxOutputTokens(),
		MaxInputBytes:          p.GetMaxInputBytes(),
		Features:               p.GetFeatures(),
	}
}

// policyFromState is the stored policy as the API shows it.
func policyFromState(p state.Scope) *orgv1.ScopePolicy {
	return &orgv1.ScopePolicy{
		Models:                 p.Models,
		BudgetMicros:           p.BudgetMicros,
		DeclaredWorstCaseCheck: p.DeclaredWorstCaseCheck,
		PrincipalInFlight:      int32(p.PrincipalInFlight),
		ScopeInFlight:          int32(p.ScopeInFlight),
		StorageAllowed:         p.StorageAllowed,
		MaxOutputTokens:        p.MaxOutputTokens,
		MaxInputBytes:          p.MaxInputBytes,
		Features:               p.Features,
	}
}
