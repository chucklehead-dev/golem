package compile

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const good = `
apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, secretRef: {name: anthropic, key: api-key}}
  deployments:
  - {name: sonnet, url: "https://api.anthropic.com/v1/messages", model: claude-sonnet-4-5}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec:
  alias: claude
  pool: [{backend: anthropic, deployment: sonnet}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: ignored}
`

func objects(t *testing.T, files map[string]string) v1.Objects {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, s := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(s)}
	}
	objs, err := v1.DecodeFS(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

var published = Options{PublishSeq: 7, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Lease: 4 * time.Hour}

func TestCompile(t *testing.T) {
	s, err := Compile(objects(t, map[string]string{"golem.yaml": good}), published)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Routes) != 1 || s.Routes[0].Dialect != "anthropic.messages" || s.Routes[0].Pool[0].Weight != 1 {
		t.Fatalf("routes %+v", s.Routes)
	}
	if s.Backends[0].Auth.SecretRef != "anthropic/api-key" || s.ValidUntil.Sub(s.IssuedAt) != 4*time.Hour {
		t.Fatalf("snapshot %+v", s)
	}
	b, _ := s.Encode()
	if strings.Contains(string(b), "sk-") {
		t.Fatal("a secret value in a snapshot")
	}
	back, err := snapshot.Decode(b)
	if err != nil || back.Source.ConfigDigest != s.Source.ConfigDigest {
		t.Fatalf("round trip: %v", err)
	}
}

func TestDeterministic(t *testing.T) {
	split := map[string]string{"a.yaml": strings.SplitN(good, "---", 2)[1], "b.yaml": strings.SplitN(good, "---", 2)[0]}
	x, _ := Compile(objects(t, map[string]string{"golem.yaml": good}), published)
	y, err := Compile(objects(t, split), published)
	if err != nil {
		t.Fatal(err)
	}
	bx, _ := x.Encode()
	by, _ := y.Encode()
	if string(bx) != string(by) {
		t.Fatalf("the same config in other files compiled differently:\n%s\n%s", bx, by)
	}
}

func TestProblems(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		opts       Options
		want       []string // substrings of the problems, in order
	}{
		{"missing reference", strings.Replace(good, "deployment: sonnet}", "deployment: opus}", 1), published,
			[]string{`ModelRoute/claude (golem.yaml#2): spec.pool[0].deployment: Backend "anthropic" has no deployment "opus"`}},
		{"unknown dialect", strings.Replace(good, "anthropic.messages", "anthropic.complete", 1), published,
			[]string{`spec.dialect: "anthropic.complete" is not a dialect`}},
		{"env outside standalone", strings.Replace(good, "secretRef: {name: anthropic, key: api-key}", "env: ANTHROPIC_API_KEY", 1), published,
			[]string{"spec.auth.env: environment credentials are for standalone mode only"}},
		{"plain http to a remote host", strings.Replace(good, "https://api.anthropic.com", "http://dgx-1.lan", 1), published,
			[]string{"spec.deployments[0].url: plain http is allowed only to a loopback address"}},
		{"credentials in the URL", strings.Replace(good, "https://api", "https://user:pw@api", 1), published,
			[]string{"credentials do not belong in a URL"}},
		{"no lease", good, Options{}, []string{"a published snapshot needs an authorization lease"}},
		{"duplicate alias", good + `---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude-again}
spec: {alias: claude, pool: [{backend: anthropic, deployment: sonnet}]}
`, published, []string{`spec.alias: alias "claude" is already routed by ModelRoute/claude`}},
		{"mixed dialects", good + `---
apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: openai}
spec:
  dialect: openai.chat
  auth: {header: authorization, scheme: Bearer, secretRef: {name: openai, key: api-key}}
  deployments: [{name: gpt, url: "https://api.openai.com/v1/chat/completions", model: gpt-5}]
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: mixed}
spec: {alias: mixed, pool: [{backend: anthropic, deployment: sonnet}, {backend: openai, deployment: gpt}]}
`, published, []string{"spec.pool[1].backend: Backend \"openai\" speaks openai.chat, the pool's first member anthropic.messages"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(objects(t, map[string]string{"golem.yaml": tc.yaml}), tc.opts)
			ps, ok := err.(Problems)
			if !ok {
				t.Fatalf("Compile error = %v, want Problems", err)
			}
			if len(ps) != len(tc.want) {
				t.Fatalf("problems:\n%v\nwant %d", ps, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(ps[i].String(), w) {
					t.Errorf("problem %q, want it to contain %q", ps[i], w)
				}
			}
		})
	}
}

func TestStandalone(t *testing.T) {
	y := strings.Replace(strings.Replace(good, "secretRef: {name: anthropic, key: api-key}", "env: ANTHROPIC_API_KEY", 1),
		"https://api.anthropic.com", "http://studio.lan:8000", 1)
	s, err := Compile(objects(t, map[string]string{"golem.yaml": y}), Options{Standalone: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidUntil.IsZero() || s.Backends[0].Auth.Env != "ANTHROPIC_API_KEY" {
		t.Fatalf("standalone snapshot %+v", s)
	}
}

func TestDecodeStrict(t *testing.T) {
	_, err := v1.DecodeFS(fstest.MapFS{"x.yaml": {Data: []byte(strings.Replace(good, "alias: claude", "aliass: claude", 1))}})
	if err == nil || !strings.Contains(err.Error(), `x.yaml#2: ModelRoute`) || !strings.Contains(err.Error(), "aliass") {
		t.Fatalf("a misspelt field: %v; want an error naming the file, document and field", err)
	}
	_, err = v1.DecodeFS(fstest.MapFS{"x.yaml": {Data: []byte("apiVersion: golem.dev/v1alpha1\nkind: Hook\nmetadata: {name: h}\n")}})
	if err == nil || !strings.Contains(err.Error(), `kind "Hook"`) {
		t.Fatalf("an unsupported kind: %v", err)
	}
}

func TestRouteTimeouts(t *testing.T) {
	var bad []string
	report := func(field, msg string) { bad = append(bad, field) }
	if m, h, i := timeouts(nil, report); m != DefaultMaxDuration.Milliseconds() || h != m || i != DefaultIdle.Milliseconds() || bad != nil {
		t.Fatalf("defaults %d %d %d %v: headers default to the whole call", m, h, i, bad)
	}
	if m, h, i := timeouts(&v1.RouteTimeouts{MaxDuration: "2m", Idle: "90s"}, report); m != 120000 || h != 120000 || i != 90000 || bad != nil {
		t.Fatalf("set: %d %d %d %v", m, h, i, bad)
	}
	timeouts(&v1.RouteTimeouts{MaxDuration: "1m", Headers: "2m", Idle: "soon"}, report)
	if !slices.Equal(bad, []string{"idle", "headers"}) {
		t.Fatalf("refused %v: an unparsable idle, and headers past the whole call", bad)
	}
}

// TestRouteRetries: a route's attempts default to DefaultMaxAttempts, and
// one outside 1..MaxAttempts is refused, the default kept.
func TestRouteRetries(t *testing.T) {
	var bad []string
	report := func(msg string) { bad = append(bad, msg) }
	if n := maxAttempts(nil, report); n != DefaultMaxAttempts || bad != nil {
		t.Fatalf("default %d %v", n, bad)
	}
	if n := maxAttempts(&v1.RouteRetries{MaxAttempts: 1}, report); n != 1 || bad != nil {
		t.Fatalf("one attempt (no retries): %d %v", n, bad)
	}
	for _, v := range []int{-1, MaxAttempts + 1} {
		bad = nil
		if n := maxAttempts(&v1.RouteRetries{MaxAttempts: v}, report); n != DefaultMaxAttempts || len(bad) != 1 {
			t.Fatalf("%d: got %d %v, want refused", v, n, bad)
		}
	}
}
