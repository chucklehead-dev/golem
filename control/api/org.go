package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/chucklehead-dev/golem/control/service"
	orgv1 "github.com/chucklehead-dev/golem/gen/golem/org/v1"
)

// org is golem.org.v1.OrgService.
type org struct{ *Server }

func (o org) CreateScope(ctx context.Context, r *connect.Request[orgv1.CreateScopeRequest]) (*connect.Response[orgv1.CreateScopeResponse], error) {
	m := r.Msg
	err := o.Service.CreateScope(ctx, caller(ctx), options(m.ValidateOnly), m.Id)
	return reply(o.Server, &orgv1.CreateScopeResponse{}, err)
}

func (o org) AddMember(ctx context.Context, r *connect.Request[orgv1.AddMemberRequest]) (*connect.Response[orgv1.AddMemberResponse], error) {
	m, mem := r.Msg, r.Msg.GetMember()
	err := o.Service.AddMember(ctx, caller(ctx), options(m.ValidateOnly), m.Scope,
		service.Member{ID: mem.GetId(), Kind: mem.GetKind(), Entra: mem.GetEntra()})
	return reply(o.Server, &orgv1.AddMemberResponse{}, err)
}

func (o org) RemoveMember(ctx context.Context, r *connect.Request[orgv1.RemoveMemberRequest]) (*connect.Response[orgv1.RemoveMemberResponse], error) {
	m := r.Msg
	err := o.Service.RemoveMember(ctx, caller(ctx), options(m.ValidateOnly), m.Scope, m.Principal)
	return reply(o.Server, &orgv1.RemoveMemberResponse{}, err)
}

func (o org) BindManager(ctx context.Context, r *connect.Request[orgv1.BindManagerRequest]) (*connect.Response[orgv1.BindManagerResponse], error) {
	m := r.Msg
	err := o.Service.BindManager(ctx, caller(ctx), options(m.ValidateOnly), m.Scope, m.Principal)
	return reply(o.Server, &orgv1.BindManagerResponse{}, err)
}

func (o org) BindGroup(ctx context.Context, r *connect.Request[orgv1.BindGroupRequest]) (*connect.Response[orgv1.BindGroupResponse], error) {
	m := r.Msg
	err := o.Service.BindGroup(ctx, caller(ctx), options(m.ValidateOnly), m.Scope, m.Group)
	return reply(o.Server, &orgv1.BindGroupResponse{}, err)
}

func (o org) UnbindGroup(ctx context.Context, r *connect.Request[orgv1.UnbindGroupRequest]) (*connect.Response[orgv1.UnbindGroupResponse], error) {
	m := r.Msg
	err := o.Service.UnbindGroup(ctx, caller(ctx), options(m.ValidateOnly), m.Scope, m.Group)
	return reply(o.Server, &orgv1.UnbindGroupResponse{}, err)
}

func (o org) Whoami(ctx context.Context, _ *connect.Request[orgv1.WhoamiRequest]) (*connect.Response[orgv1.WhoamiResponse], error) {
	me, err := o.Service.Whoami(ctx, caller(ctx))
	return reply(o.Server, &orgv1.WhoamiResponse{Principal: me.Principal, Admin: me.Admin, Kind: me.Kind,
		Scopes: me.Scopes, Manages: me.Manages}, err)
}

func (o org) ListScopes(ctx context.Context, _ *connect.Request[orgv1.ListScopesRequest]) (*connect.Response[orgv1.ListScopesResponse], error) {
	ids, err := o.Service.ListScopes(ctx, caller(ctx))
	return reply(o.Server, &orgv1.ListScopesResponse{Ids: ids}, err)
}

func (o org) GetScope(ctx context.Context, r *connect.Request[orgv1.GetScopeRequest]) (*connect.Response[orgv1.GetScopeResponse], error) {
	v, err := o.Service.GetScope(ctx, caller(ctx), r.Msg.Id)
	return reply(o.Server, &orgv1.GetScopeResponse{Id: v.ID, Members: v.Members, Managers: v.Managers, Groups: v.Groups}, err)
}
