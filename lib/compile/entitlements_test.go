package compile

import (
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/key"
)

var devKey = key.New(key.Dev)

func identity(extra string) string {
	return good + `---
apiVersion: golem.dev/v1alpha1
kind: PriceList
metadata: {name: default}
spec:
  currency: USD
  prices: [{model: claude-sonnet-4-5, inputPerMTok: 3000000, outputPerMTok: 15000000}]
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes:
  - {name: team-a, models: [claude], budgetMicros: 5000000, inFlight: {principal: 2}}
  keys:
  - {key: ` + devKey + `, principal: alice, scope: team-a` + extra + `}
`
}

var standalone = Options{Standalone: true, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func TestEntitlements(t *testing.T) {
	s, err := Compile(objects(t, map[string]string{"golem.yaml": identity("")}), standalone)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Entitlements
	if e.Implicit || len(e.Scopes) != 1 || e.Scopes[0].PrincipalInFlight != 2 || e.Scopes[0].ScopeInFlight != DefaultScopeInFlight {
		t.Fatalf("scopes %+v", e.Scopes)
	}
	h := key.Hash(devKey)
	if len(e.Keys) != 1 || e.Keys[0].Principal != "alice" || e.Keys[0].Display != key.Display(devKey) || e.Keys[0].Hash == "" || h == [32]byte{} {
		t.Fatalf("keys %+v", e.Keys)
	}
	b, _ := s.Encode()
	if strings.Contains(string(b), devKey) {
		t.Fatal("a key in a snapshot: only its hash may be")
	}
	if len(s.Prices) != 1 || s.Currency != "USD" {
		t.Fatalf("prices %+v %s", s.Prices, s.Currency)
	}
}

func TestImplicit(t *testing.T) {
	s, _ := Compile(objects(t, map[string]string{"golem.yaml": strings.Replace(good, "secretRef: {name: anthropic, key: api-key}", "env: K", 1)}), standalone)
	if !s.Entitlements.Implicit {
		t.Fatal("standalone without a LocalIdentity is the implicit local principal (ADR 0023)")
	}
}

func TestEntitlementProblems(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		opts       Options
		want       string
	}{
		{"live key in a file", strings.Replace(identity(""), devKey, key.New(key.Live), 1), standalone, "takes only glm_dev_ keys"},
		{"mistyped key", strings.Replace(identity(""), devKey, devKey[:len(devKey)-1]+"x", 1), standalone, "checksum"},
		{"key wider than its scope", identity(", models: [claude, gpt]"), standalone, `scope "team-a" does not allow "gpt"`},
		{"unknown scope", strings.Replace(identity(""), "scope: team-a", "scope: team-b", 1), standalone, `no scope named "team-b"`},
		{"unknown alias", strings.Replace(identity(""), "models: [claude]", "models: [claude, opus]", 1), standalone, `no ModelRoute has alias "opus"`},
		{"unpriced budget", strings.Replace(identity(""), "model: claude-sonnet-4-5,", "model: other,", 1), standalone, "no PriceList prices"},
		{"outside standalone", identity(""), published, "for standalone mode only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y := strings.Replace(tc.yaml, "secretRef: {name: anthropic, key: api-key}", "env: K", 1)
			if !tc.opts.Standalone {
				y = tc.yaml
			}
			_, err := Compile(objects(t, map[string]string{"golem.yaml": y}), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v; want a problem containing %q", err, tc.want)
			}
		})
	}
}
