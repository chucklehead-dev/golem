package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/api"
	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// fakeEntra is Entra's device code and token endpoints: the code is
// pending twice, then issued for the control API; a refresh token gets a
// new access token for the resource its scope names (tok-gw for the
// gateway's).
type fakeEntra struct {
	polls   atomic.Int32
	access  atomic.Value // the access token it issues
	refresh atomic.Int32
}

func (f *fakeEntra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/devicecode"):
		if !strings.Contains(r.Form.Get("scope"), "api://golem-control/golem.manage") || !strings.Contains(r.Form.Get("scope"), "offline_access") {
			http.Error(w, `{"error":"invalid_scope"}`, 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD", "interval": 1,
			"expires_in": 60, "message": "To sign in, open https://microsoft.com/devicelogin and enter ABCD."})
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") && r.Form.Get("grant_type") == "refresh_token":
		f.refresh.Add(1)
		access := f.access.Load()
		if strings.Contains(r.Form.Get("scope"), "api://golem/golem.access") {
			access = "tok-gw"
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "expires_in": 3600})
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		if f.polls.Add(1) <= 2 {
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": f.access.Load(), "refresh_token": "rt", "expires_in": 3600})
	default:
		http.NotFound(w, r)
	}
}

// tokenAuth takes "Bearer tok-<principal>"; root is an admin.
type tokenAuth struct{}

func (tokenAuth) Authenticate(_ context.Context, h http.Header) (service.Caller, error) {
	p, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer tok-")
	if !ok {
		return service.Caller{}, errors.New("no token")
	}
	return service.Caller{Principal: p, Admin: p == "root"}, nil
}

func TestGolemctl(t *testing.T) {
	pollUnit = time.Millisecond
	home := t.TempDir()
	t.Setenv("GOLEMCTL_HOME", home)
	entra := &fakeEntra{}
	entra.access.Store("tok-root")
	idp := httptest.NewServer(entra)
	defer idp.Close()
	b, _ := bucket.OpenDir(t.TempDir())
	defer b.Close()
	st := &state.Store{Bucket: b}
	ctl := httptest.NewServer((&api.Server{Service: &service.Service{State: st}, Auth: tokenAuth{}}).Handler())
	defer ctl.Close()

	golemctl := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(t.Context(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := golemctl("login", "--tenant", "t1", "--client-id", "golemctl", "--api", ctl.URL, "--authority", idp.URL); code != 0 || !strings.Contains(errOut, "enter ABCD") {
		t.Fatalf("login: %d %s", code, errOut)
	}
	if fi, err := os.Stat(filepath.Join(home, "golemctl.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the settings file: %v %v", fi, err)
	}
	for _, args := range [][]string{
		{"scopes", "create", "team-a"},
		{"members", "add", "team-a", "alice", "--kind", "user", "--entra", "oid-a"},
		{"managers", "add", "team-a", "alice"},
		{"groups", "bind", "team-a", "group-1"},
	} {
		if code, out, errOut := golemctl(args...); code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, errOut)
		}
	}
	if code, out, _ := golemctl("scopes", "create", "team-b", "--dry-run"); code != 0 || !strings.Contains(out, "dry run") || len(st.Current().Scopes) != 1 {
		t.Fatalf("a dry run: %d %q, scopes %v", code, out, st.Current().Scopes)
	}
	code, out, errOut := golemctl("keys", "create", "--scope", "team-a", "--owner", "alice", "--models", "claude", "--expires", "30d")
	key := regexp.MustCompile(`glm_live_\S+`).FindString(out)
	hash := regexp.MustCompile(`[0-9a-f]{64}`).FindString(out)
	if code != 0 || key == "" || hash == "" {
		t.Fatalf("keys create: %d %q %s", code, out, errOut)
	}
	k := st.Current().Keys[hash]
	if k.Owner != "alice" || k.ExpiresAt.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("the key's record %+v", k)
	}
	if code, _, errOut := golemctl("keys", "revoke", hash); code != 0 || st.Current().Keys[hash].RevokedAt.IsZero() {
		t.Fatalf("keys revoke: %d %s", code, errOut)
	}

	t.Run("reads", func(t *testing.T) {
		code, out, errOut := golemctl("whoami")
		if code != 0 || !strings.Contains(out, "root") || !strings.Contains(out, "admin      true") {
			t.Fatalf("whoami: %d %q %s", code, out, errOut)
		}
		if code, out, _ := golemctl("scopes", "list"); code != 0 || strings.TrimSpace(out) != "team-a" {
			t.Fatalf("scopes list: %d %q", code, out)
		}
		if code, out, _ := golemctl("scopes", "get", "team-a"); code != 0 || !strings.Contains(out, "alice") || !strings.Contains(out, "ScopePolicy/team-a in the config repository") || !strings.Contains(out, "group-1") {
			t.Fatalf("scopes get: %d %q", code, out)
		}
		code, out, _ = golemctl("keys", "list", "--scope", "team-a")
		if code != 0 || !strings.Contains(out, "alice") || !strings.Contains(out, "revoked") || strings.Contains(out, key) || !strings.Contains(out, hash[:12]) {
			t.Fatalf("keys list: %d %q", code, out)
		}
	})
	t.Run("a denied caller is told why", func(t *testing.T) {
		s, _ := loadSettings()
		s.Tokens["control"] = token{Access: "tok-eve", Expires: time.Now().Add(time.Hour)}
		s.save()
		if code, _, errOut := golemctl("scopes", "create", "team-c"); code != 1 || !strings.Contains(errOut, "permission_denied") {
			t.Fatalf("%d %s", code, errOut)
		}
	})
	t.Run("the gateway's token is its own", func(t *testing.T) {
		// golemctl token redeems the refresh token for the gateway, and
		// leaves the control API's token as it was.
		if code, out, _ := golemctl("token"); code != 0 || strings.TrimSpace(out) != "tok-gw" || entra.refresh.Load() != 1 {
			t.Fatalf("token: %d %q, refreshes %d", code, out, entra.refresh.Load())
		}
		if s, _ := loadSettings(); s.Tokens["gateway"].Access != "tok-gw" || s.Tokens["control"].Access != "tok-eve" || s.Refresh != "rt" {
			t.Fatalf("saved %+v", s)
		}
	})
	t.Run("an expired token is refreshed", func(t *testing.T) {
		s, _ := loadSettings()
		s.Tokens["control"] = token{Access: "stale", Expires: time.Now().Add(-time.Hour)}
		s.save()
		entra.access.Store("tok-root")
		if code, out, errOut := golemctl("whoami"); code != 0 || !strings.Contains(out, "root") || entra.refresh.Load() != 2 {
			t.Fatalf("whoami: %d %q %s, refreshes %d", code, out, errOut, entra.refresh.Load())
		}
		if s, _ := loadSettings(); s.Tokens["control"].Access != "tok-root" {
			t.Fatalf("saved %+v", s.Tokens)
		}
	})
	t.Run("usage", func(t *testing.T) {
		for _, args := range [][]string{{"keys", "create"}, {"members", "add", "team-a"}, {"frob", "it"}, {"keys", "create", "--scope", "a", "--expires", "soon"}} {
			if code, _, _ := golemctl(args...); code != 2 {
				t.Errorf("%v: exit %d, want 2", args, code)
			}
		}
	})
}
