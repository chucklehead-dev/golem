package api

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/chucklehead-dev/golem/control/service"
	keysv1 "github.com/chucklehead-dev/golem/gen/golem/keys/v1"
)

// keys is golem.keys.v1.KeyService.
type keys struct{ *Server }

func (k keys) MintKey(ctx context.Context, r *connect.Request[keysv1.MintKeyRequest]) (*connect.Response[keysv1.MintKeyResponse], error) {
	m := r.Msg
	req := service.KeyRequest{Owner: m.Owner, Scope: m.Scope, Models: m.Models, CapMicros: m.CapMicros}
	if m.Expires != nil {
		req.Expires = m.Expires.AsTime()
	}
	minted, err := k.Service.MintKey(ctx, caller(ctx), options(m.ValidateOnly), req)
	res, err := reply(k.Server, &keysv1.MintKeyResponse{Key: minted.Key, Hash: minted.Hash, Display: minted.Display}, err)
	if err == nil {
		res.Header().Set("Cache-Control", "no-store") // the key is in it
	}
	return res, err
}

func (k keys) RevokeKey(ctx context.Context, r *connect.Request[keysv1.RevokeKeyRequest]) (*connect.Response[keysv1.RevokeKeyResponse], error) {
	m := r.Msg
	err := k.Service.RevokeKey(ctx, caller(ctx), options(m.ValidateOnly), m.Hash)
	return reply(k.Server, &keysv1.RevokeKeyResponse{}, err)
}

func (k keys) ListKeys(ctx context.Context, r *connect.Request[keysv1.ListKeysRequest]) (*connect.Response[keysv1.ListKeysResponse], error) {
	vs, err := k.Service.ListKeys(ctx, caller(ctx), r.Msg.Scope)
	res := &keysv1.ListKeysResponse{}
	for _, v := range vs {
		res.Keys = append(res.Keys, keyFromState(v))
	}
	return reply(k.Server, res, err)
}

func (k keys) SetKeyCap(ctx context.Context, r *connect.Request[keysv1.SetKeyCapRequest]) (*connect.Response[keysv1.SetKeyCapResponse], error) {
	m := r.Msg
	err := k.Service.SetKeyCap(ctx, caller(ctx), options(m.ValidateOnly), m.Hash, m.CapMicros)
	return reply(k.Server, &keysv1.SetKeyCapResponse{}, err)
}

// keyFromState is a key's record as the API shows it: never the key.
func keyFromState(v service.KeyView) *keysv1.Key {
	pk := &keysv1.Key{Hash: v.Hash, Display: v.Display, Owner: v.Owner, Scope: v.Scope, Models: v.Models, CapMicros: v.CapMicros}
	if !v.ExpiresAt.IsZero() {
		pk.Expires = timestamppb.New(v.ExpiresAt)
	}
	if !v.RevokedAt.IsZero() {
		pk.Revoked = timestamppb.New(v.RevokedAt)
	}
	return pk
}
