package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// team-a's ScopePolicy, as golem-control would read it from the config.
	models := func(scope string) ([]string, bool) { return map[string][]string{"team-a": {"claude"}}[scope], true }
	srv := httptest.NewServer((&Server{Service: &service.Service{State: st, ScopeModels: models}, Auth: headerAuth{}}).Handler())
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
		ValidateOnly: true})); err != nil || len(st.Current().Scopes) != 0 {
		t.Fatalf("validate only: %v, scopes %v", err, st.Current().Scopes)
	}
	if _, err := org("root").CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: "team-a"})); err != nil {
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
	got, err := org("alice").GetScope(ctx, connect.NewRequest(&orgv1.GetScopeRequest{Id: "team-a"}))
	if err != nil || got.Msg.Id != "team-a" || strings.Join(got.Msg.Members, ",") != "alice" {
		t.Fatalf("get: %v %+v", err, got)
	}
	// eve can see nothing of team-a: not even that it exists.
	if _, err := keys("eve").MintKey(ctx, connect.NewRequest(&keysv1.MintKeyRequest{Owner: "alice", Scope: "team-a"})); code(err) != connect.CodeNotFound {
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
		strings.NewReader(`{"id":"team-b"}`))
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

// readFlag is a body that records whether it was read.
type readFlag struct {
	io.Reader
	read bool
}

func (r *readFlag) Read(p []byte) (int, error) { r.read = true; return r.Reader.Read(p) }

// TestLimits: an unauthenticated call is refused before its body is read;
// an authenticated one over the size limit is refused; an internal error
// is answered without its text, which is logged.
func TestLimits(t *testing.T) {
	_, st := serve(t)
	var logged strings.Builder
	s := &Server{Service: &service.Service{State: st}, Auth: headerAuth{}, MaxRequest: 1024,
		Log: slog.New(slog.NewTextHandler(&logged, nil))}
	h := s.Handler()
	call := func(who string, body io.Reader) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", orgv1connect.OrgServiceCreateScopeProcedure, body)
		r.Header.Set("Content-Type", "application/json")
		if who != "" {
			r.Header.Set("X-Caller", who)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	big := `{"id":"` + strings.Repeat("a", 4096) + `"}`
	body := &readFlag{Reader: strings.NewReader(big)}
	if w := call("", body); w.Code != http.StatusUnauthorized || body.read {
		t.Fatalf("unauthenticated: %d, body read %v; want 401 unread", w.Code, body.read)
	}
	if w := call("root", strings.NewReader(big)); !strings.Contains(w.Body.String(), "resource_exhausted") {
		t.Fatalf("oversized: %d %s", w.Code, w.Body)
	}

	s.Service.State = &state.Store{Bucket: failing{}}
	w := call("root", strings.NewReader(`{"id":"team-b"}`))
	if !strings.Contains(w.Body.String(), `"internal"`) || strings.Contains(w.Body.String(), "s3://secret-bucket") {
		t.Fatalf("internal error: %s", w.Body)
	}
	if !strings.Contains(logged.String(), "s3://secret-bucket") {
		t.Fatalf("the error was not logged: %q", logged.String())
	}
	if c := connect.CodeOf(s.connectError(fmt.Errorf("x: %w", context.DeadlineExceeded))); c != connect.CodeDeadlineExceeded {
		t.Fatalf("a deadline: %v", c)
	}
}

// failing is a bucket whose every call fails with an error naming it.
type failing struct{}

func (failing) Get(context.Context, string, string) ([]byte, string, error) {
	return nil, "", errors.New("GET s3://secret-bucket/state/document.json: access denied")
}

func (failing) Put(context.Context, string, []byte, string) (string, error) {
	return "", errors.New("PUT s3://secret-bucket/state/document.json: access denied")
}

// TestOutboxFullCode: a grant refused by the audit outbox's bound is
// ResourceExhausted, with the store's message (the cause and the fix); a
// revocation in the reserve lands.
func TestOutboxFullCode(t *testing.T) {
	srv, st := serve(t)
	ctx := t.Context()
	org := orgv1connect.NewOrgServiceClient(srv.Client(), srv.URL, as("root"))
	keys := keysv1connect.NewKeyServiceClient(srv.Client(), srv.URL, as("alice"))
	if _, err := org.CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: "team-a"})); err != nil {
		t.Fatal(err)
	}
	if _, err := org.AddMember(ctx, connect.NewRequest(&orgv1.AddMemberRequest{Scope: "team-a",
		Member: &orgv1.Member{Id: "alice", Kind: "user", Entra: "oid-alice"}})); err != nil {
		t.Fatal(err)
	}
	k, err := keys.MintKey(ctx, connect.NewRequest(&keysv1.MintKeyRequest{Owner: "alice", Scope: "team-a"}))
	if err != nil {
		t.Fatal(err)
	}
	st.MaxOutbox = 10
	if _, err := st.Update(ctx, func(d *state.Document) error {
		for len(d.Audit) < state.Reserve(10) {
			d.Audit = append(d.Audit, state.AuditEvent{Actor: "root", Action: "key.revoke", Target: "none"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = keys.MintKey(ctx, connect.NewRequest(&keysv1.MintKeyRequest{Owner: "alice", Scope: "team-a"}))
	if code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "-audit-archive") {
		t.Fatalf("a mint past the reserve: %v, want ResourceExhausted naming the fix", err)
	}
	if _, err := keys.RevokeKey(ctx, connect.NewRequest(&keysv1.RevokeKeyRequest{Hash: k.Msg.Hash})); err != nil {
		t.Fatalf("a revocation past the reserve: %v", err)
	}
}
