package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const (
	tenant = "11111111-2222-3333-4444-555555555555"
	apiID  = "api://golem"
	alice  = "aaaaaaaa-0000-0000-0000-000000000001" // a user's oid
	bot    = "bbbbbbbb-0000-0000-0000-000000000002" // a service's app ID
)

var (
	signingKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _   = rsa.GenerateKey(rand.Reader, 2048)
	now           = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
)

func sign(t *testing.T, k *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k}, (&jose.SignerOptions{}).WithHeader("kid", kid).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(s).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func userClaims() map[string]any {
	return map[string]any{
		"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0", "aud": apiID, "tid": tenant, "ver": "2.0",
		"iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"oid": alice, "scp": "golem.access", "azp": "golemctl-client-id",
	}
}

func serviceClaims() map[string]any {
	c := userClaims()
	delete(c, "scp")
	delete(c, "oid")
	c["roles"] = []string{"golem.service"}
	c["azp"] = bot
	c["idtyp"] = "app"
	return c
}

func with(c map[string]any, kv ...any) map[string]any {
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(c, kv[i].(string))
		} else {
			c[kv[i].(string)] = kv[i+1]
		}
	}
	return c
}

func entraKeys() (*Keys, *entra.Verifier) {
	e := &entra.Verifier{Tenant: tenant, Audiences: []string{apiID, "golem-api-client-id"}, Now: func() time.Time { return now }}
	e.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &signingKey.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	ents := snapshot.Entitlements{
		Scopes: []snapshot.Scope{
			{ID: "team-a", Models: []string{"claude"}, PrincipalInFlight: 2},
			{ID: "team-b", Models: []string{"gpt"}, PrincipalInFlight: 2},
		},
		Users:    []snapshot.Member{{ID: "alice", Entra: alice, Scopes: []string{"team-a", "team-b"}, Default: "team-a"}},
		Services: []snapshot.Member{{ID: "support-bot", Entra: bot, Scopes: []string{"team-b"}, Default: "team-b"}},
	}
	return Compile(ents, key.Live, func() time.Time { return now }, e), e
}

func bearer(tok, scope string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + tok}}
	if scope != "" {
		h.Set("Golem-Scope", scope)
	}
	return h
}

func TestEntra(t *testing.T) {
	k, _ := entraKeys()
	for _, tc := range []struct {
		name  string
		token string
		scope string
		want  string // principal@scope, or the refusal's code
	}{
		{"a user, default scope", sign(t, signingKey, "k1", userClaims()), "", "alice@team-a"},
		{"a user, another of its scopes", sign(t, signingKey, "k1", userClaims()), "team-b", "alice@team-b"},
		{"a user, a scope it is not in", sign(t, signingKey, "k1", userClaims()), "team-c", contract.AuthzScopeNotMember.Name},
		{"a service", sign(t, signingKey, "k1", serviceClaims()), "", "support-bot@team-b"},
		{"the other audience form", sign(t, signingKey, "k1", with(userClaims(), "aud", "golem-api-client-id")), "", "alice@team-a"},
		{"expired", sign(t, signingKey, "k1", with(userClaims(), "exp", now.Add(-2*time.Minute).Unix())), "", contract.AuthTokenExpired.Name},
		{"expired within the leeway", sign(t, signingKey, "k1", with(userClaims(), "exp", now.Add(-30*time.Second).Unix())), "", "alice@team-a"},
		{"no expiry", sign(t, signingKey, "k1", with(userClaims(), "exp", nil)), "", contract.AuthTokenInvalid.Name},
		{"not yet valid", sign(t, signingKey, "k1", with(userClaims(), "nbf", now.Add(10*time.Minute).Unix())), "", contract.AuthTokenInvalid.Name},
		{"another tenant", sign(t, signingKey, "k1", with(userClaims(), "iss", "https://login.microsoftonline.com/other/v2.0", "tid", "other")), "", contract.AuthTokenInvalid.Name},
		{"tid not the issuer's", sign(t, signingKey, "k1", with(userClaims(), "tid", "other")), "", contract.AuthTokenInvalid.Name},
		{"another API", sign(t, signingKey, "k1", with(userClaims(), "aud", "api://something-else")), "", contract.AuthTokenInvalid.Name},
		{"a v1 token", sign(t, signingKey, "k1", with(userClaims(), "ver", "1.0")), "", contract.AuthTokenInvalid.Name},
		{"neither scope nor role", sign(t, signingKey, "k1", with(userClaims(), "scp", "User.Read")), "", contract.AuthTokenInvalid.Name},
		{"the control API's scope", sign(t, signingKey, "k1", with(userClaims(), "scp", "golem.manage")), "", contract.AuthTokenInvalid.Name},
		{"a service token without idtyp", sign(t, signingKey, "k1", with(serviceClaims(), "idtyp", nil)), "", contract.AuthTokenInvalid.Name},
		{"a user token claiming to be an app", sign(t, signingKey, "k1", with(userClaims(), "idtyp", "app")), "", contract.AuthTokenInvalid.Name},
		{"a user token naming a service's ID", sign(t, signingKey, "k1", with(userClaims(), "oid", bot)), "", contract.AuthzScopeNotMember.Name},
		{"an unknown user", sign(t, signingKey, "k1", with(userClaims(), "oid", "cccccccc-0000-0000-0000-000000000003")), "", contract.AuthzScopeNotMember.Name},
		{"signed by another key under a known kid", sign(t, otherKey, "k1", userClaims()), "", contract.AuthTokenInvalid.Name},
		{"an unknown kid", sign(t, otherKey, "k9", userClaims()), "", contract.AuthTokenInvalid.Name},
		{"tampered payload", tamper(sign(t, signingKey, "k1", userClaims())), "", contract.AuthTokenInvalid.Name},
		{"alg none", unsigned(userClaims()), "", contract.AuthTokenInvalid.Name},
		{"HS256 keyed with the public key", hmacWithPublicKey(t, userClaims()), "", contract.AuthTokenInvalid.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, r := k.Authenticate(bearer(tc.token, tc.scope))
			got := ""
			if p != nil {
				got = p.ID + "@" + p.Scope
			} else {
				got = r.Code.Name
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s (%v)", got, tc.want, r)
			}
		})
	}
	// Rights are the scope's, from the snapshot.
	p, _ := k.Authenticate(bearer(sign(t, signingKey, "k1", userClaims()), "team-b"))
	if !p.Allows([]byte("gpt")) || p.Allows([]byte("claude")) {
		t.Fatal("alice in team-b may call team-b's models only")
	}
}

// tamper changes the payload's oid without re-signing.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(b, &c)
	c["oid"] = bot
	b, _ = json.Marshal(c)
	parts[1] = base64.RawURLEncoding.EncodeToString(b)
	return strings.Join(parts, ".")
}

func unsigned(c map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT", "kid": "k1"})
	p, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p) + "."
}

// hmacWithPublicKey is the algorithm-confusion attack: HS256 with the RSA
// public key's bytes as the shared secret.
func hmacWithPublicKey(t *testing.T, c map[string]any) string {
	t.Helper()
	pub, _ := json.Marshal(jose.JSONWebKey{Key: &signingKey.PublicKey})
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: pub}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(s).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNoEntraConfigured(t *testing.T) {
	k := Compile(snapshot.Entitlements{}, key.Live, time.Now, nil)
	if _, r := k.Authenticate(bearer(sign(t, signingKey, "k1", userClaims()), "")); r == nil || r.Code != contract.AuthTokenInvalid {
		t.Fatalf("%+v: a gateway without Entra configured refuses tokens", r)
	}
}

func TestRefresh(t *testing.T) {
	keys, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &otherKey.PublicKey, KeyID: "k2", Use: "sig"}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(keys) }))
	defer srv.Close()
	k, e := entraKeys()
	e.JWKS = srv.URL
	tok := sign(t, otherKey, "k2", userClaims())
	if _, r := k.Authenticate(bearer(tok, "")); r == nil {
		t.Fatal("a key not yet fetched was accepted")
	}
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p, r := k.Authenticate(bearer(tok, "")); r != nil || p.ID != "alice" {
		t.Fatalf("after the refresh: %+v %+v", p, r)
	}
}

// FuzzAuthenticate: no credential, however malformed, panics the chain or
// authenticates without a valid signature.
func FuzzAuthenticate(f *testing.F) {
	k, _ := entraKeys()
	f.Add("eyJhbGciOiJSUzI1NiJ9.e30.")
	f.Add("glm_live_" + strings.Repeat("0", 49))
	f.Add("eyJ.eyJ.eyJ")
	f.Fuzz(func(t *testing.T, cred string) {
		if p, _ := k.Authenticate(http.Header{"Authorization": {"Bearer " + cred}}); p != nil {
			t.Fatalf("%q authenticated as %s", cred, p.ID)
		}
	})
}

// TestStaleKeys: past MaxKeyAge without a refresh, tokens are refused, not
// trusted on a key set Entra may have revoked since.
func TestStaleKeys(t *testing.T) {
	k, e := entraKeys()
	clock := now
	e.Now = func() time.Time { return clock }
	tok := sign(t, signingKey, "k1", userClaims())
	if p, r := k.Authenticate(bearer(tok, "")); p == nil {
		t.Fatalf("fresh keys: %v", r)
	}
	clock = now.Add(25 * time.Hour)
	stale := sign(t, signingKey, "k1", with(userClaims(), "exp", clock.Add(time.Hour).Unix(), "nbf", clock.Unix(), "iat", clock.Unix()))
	if _, r := k.Authenticate(bearer(stale, "")); r == nil || r.Code != contract.AuthTokenInvalid {
		t.Fatalf("%+v: keys not refreshed for 25 hours must not be trusted", r)
	}
}
