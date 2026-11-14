package api

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/gateway/auth"
)

const tenant = "11111111-2222-3333-4444-555555555555"

// TestEntraAuth: a verified token's user or service is the principal with
// that Entra ID; golem.admin makes it an admin; a token for no principal
// is "entra:<id>"; a missing or bad token is refused.
func TestEntraAuth(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	e := &auth.Entra{Tenant: tenant, Audiences: []string{"api://golem"}, Now: func() time.Time { return now }}
	e.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k}, (&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	token := func(extra map[string]any) string {
		c := map[string]any{"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0", "aud": "api://golem", "tid": tenant,
			"ver": "2.0", "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix()}
		for k, v := range extra {
			c[k] = v
		}
		raw, err := jwt.Signed(signer).Claims(c).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	_, st := serve(t)
	if _, err := st.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {}}
		d.Principals = map[string]state.Principal{
			"alice": {Kind: "user", Entra: "oid-alice", Scopes: []string{"team-a"}},
			"ci":    {Kind: "service", Entra: "oid-alice", Scopes: []string{"team-a"}}, // a service whose app ID equals a user's oid
		}
		return nil
	}); err == nil {
		t.Fatal("two principals with one Entra ID were accepted")
	}
	st.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {}}
		d.Principals = map[string]state.Principal{
			"alice": {Kind: "user", Entra: "oid-alice", Scopes: []string{"team-a"}},
			"ci":    {Kind: "service", Entra: "app-ci", Scopes: []string{"team-a"}},
		}
		return nil
	})
	a := EntraAuth{Entra: e, State: st}
	hdr := func(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }
	for name, tc := range map[string]struct {
		tok          string
		who          string
		admin, fails bool
	}{
		"a user":          {tok: token(map[string]any{"oid": "oid-alice", "scp": "golem.access"}), who: "alice"},
		"a user, admin":   {tok: token(map[string]any{"oid": "oid-alice", "scp": "golem.access", "roles": []string{"golem.admin"}}), who: "alice", admin: true},
		"a service":       {tok: token(map[string]any{"azp": "app-ci", "roles": []string{"golem.service"}}), who: "ci"},
		"no principal":    {tok: token(map[string]any{"oid": "oid-new", "scp": "golem.access", "roles": []string{"golem.admin"}}), who: "entra:oid-new", admin: true},
		"another tenant":  {tok: token(map[string]any{"oid": "oid-alice", "scp": "golem.access", "tid": "x"}), fails: true},
		"no access scope": {tok: token(map[string]any{"oid": "oid-alice"}), fails: true},
	} {
		c, err := a.Authenticate(t.Context(), hdr(tc.tok))
		if (err != nil) != tc.fails || (!tc.fails && (c.Principal != tc.who || c.Admin != tc.admin)) {
			t.Errorf("%s: %+v %v", name, c, err)
		}
	}
	if _, err := a.Authenticate(t.Context(), http.Header{}); err == nil {
		t.Error("no token accepted")
	}
}
