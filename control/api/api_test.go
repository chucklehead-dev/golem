package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	keysv1 "github.com/chucklehead-dev/golem/gen/golem/keys/v1"
	"github.com/chucklehead-dev/golem/gen/golem/keys/v1/keysv1connect"
	orgv1 "github.com/chucklehead-dev/golem/gen/golem/org/v1"
	"github.com/chucklehead-dev/golem/gen/golem/org/v1/orgv1connect"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// headerAuth is a test authenticator: the caller is the X-Caller header,
// an admin if it is "root".
type headerAuth struct{}

func (headerAuth) Authenticate(_ context.Context, h http.Header) (service.Caller, error) {
	p := h.Get("X-Caller")
	if p == "" {
		return service.Caller{}, errors.New("no caller")
	}
	return service.Caller{Principal: p, Admin: p == "root"}, nil
}

func as(who string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			if who != "" {
				r.Header().Set("X-Caller", who)
			}
			return next(ctx, r)
		}
	}))
}

func serve(t *testing.T) (*httptest.Server, *state.Store) {
	t.Helper()
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	st := &state.Store{Bucket: b}
	srv := httptest.NewServer((&Server{Service: &service.Service{State: st}, Auth: headerAuth{}}).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func code(err error) connect.Code { return connect.CodeOf(err) }

// TestAPI: the org and key services over Connect, as golemctl would call
// them: authenticated, authorized by the service, errors as Connect codes.
func TestAPI(t *testing.T) {
	srv, st := serve(t)
	ctx := t.Context()
	org := func(who string) orgv1connect.OrgServiceClient {
		return orgv1connect.NewOrgServiceClient(srv.Client(), srv.URL, as(who))
	}
	keys := func(who string) keysv1connect.KeyServiceClient {
		return keysv1connect.NewKeyServiceClient(srv.Client(), srv.URL, as(who))
	}
	if _, err := org("").CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: "team-a"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("no caller: %v", err)
	}
	if _, err := org("alice").CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: "team-a"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a non-admin creating a scope: %v", err)
	}
	if _, err := org("root").CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: "team-a",
		Policy: &orgv1.ScopePolicy{Models: []string{"claude"}}, ValidateOnly: true})); err != nil || len(st.Current().Scopes) != 0 {
		t.Fatalf("validate only: %v, scopes %v", err, st.Current().Scopes)
	}
	if _, err := org("root").CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: "team-a",
		Policy: &orgv1.ScopePolicy{Models: []string{"claude"}}})); err != nil {
		t.Fatal(err)
	}
	if _, err := org("root").AddMember(ctx, connect.NewRequest(&orgv1.AddMemberRequest{Scope: "team-a",
		Member: &orgv1.Member{Id: "alice", Kind: "user", Entra: "oid-alice"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := keys("alice").MintKey(ctx, connect.NewRequest(&keysv1.MintKeyRequest{Owner: "alice", Scope: "team-a",
		Models: []string{"opus"}})); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "key.models[0]") {
		t.Fatalf("a model outside the scope: %v", err)
	}
	if _, err := keys("eve").MintKey(ctx, connect.NewRequest(&keysv1.MintKeyRequest{Owner: "alice", Scope: "team-a"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("eve minting alice's key: %v", err)
	}
	res, err := keys("alice").MintKey(ctx, connect.NewRequest(&keysv1.MintKeyRequest{Owner: "alice", Scope: "team-a"}))
	if err != nil || !strings.HasPrefix(res.Msg.Key, "glm_live_") || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("mint: %v %+v %v", err, res, res.Header())
	}
	if k := st.Current().Keys[res.Msg.Hash]; k.Owner != "alice" {
		t.Fatalf("the key's record: %+v", k)
	}
	if _, err := keys("alice").RevokeKey(ctx, connect.NewRequest(&keysv1.RevokeKeyRequest{Hash: res.Msg.Hash})); err != nil {
		t.Fatal(err)
	}
	if _, err := keys("alice").RevokeKey(ctx, connect.NewRequest(&keysv1.RevokeKeyRequest{Hash: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("an unknown key: %v", err)
	}
}

// TestPlainJSON: the API answers plain HTTP/JSON (architecture.md §3:
// curl works).
func TestPlainJSON(t *testing.T) {
	srv, _ := serve(t)
	req, _ := http.NewRequest("POST", srv.URL+"/golem.org.v1.OrgService/CreateScope",
		strings.NewReader(`{"id":"team-b","policy":{"models":["claude"],"budgetMicros":"5000000"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caller", "root")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "{}" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/golem.org.v1.OrgService/CreateScope", strings.NewReader(`{"id":"team-c"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caller", "alice")
	resp, _ = srv.Client().Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(body), "permission_denied") {
		t.Fatalf("a non-admin, as JSON: %d %s", resp.StatusCode, body)
	}
}
