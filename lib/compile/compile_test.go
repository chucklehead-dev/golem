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

// TestBedrockBackend: a bedrock backend compiles with no auth, each
// deployment's region its own or the backend's and its URL optional; it is
// refused with an auth, another dialect, no region or a malformed one, and
// an envelope golem does not have is named.
func TestBedrockBackend(t *testing.T) {
	backend := func(spec string) string {
		return `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: bedrock}
spec:
` + spec + `
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: bedrock, deployment: east}]}
`
	}
	ok := `  dialect: anthropic.messages
  envelope: bedrock
  aws: {region: us-east-2}
  deployments:
  - {name: east, model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}
  - {name: west, region: us-west-2, model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}`
	s, err := Compile(objects(t, map[string]string{"golem.yaml": backend(ok)}), published)
	if err != nil {
		t.Fatal(err)
	}
	b := s.Backends[0]
	if b.Envelope != "bedrock" || b.Auth != (snapshot.Auth{}) || b.Deployments[0].Region != "us-east-2" || b.Deployments[1].Region != "us-west-2" {
		t.Fatalf("%+v", b)
	}
	for _, c := range []struct{ name, spec, want string }{
		{"auth", strings.Replace(ok, "  envelope: bedrock", "  envelope: bedrock\n  auth: {header: x-api-key, secretRef: {name: a, key: b}}", 1), "spec.auth: a bedrock backend has no auth"},
		{"dialect", strings.Replace(ok, "anthropic.messages", "openai.chat", 1), "a bedrock backend speaks anthropic.messages"},
		{"no region", strings.Replace(ok, "  aws: {region: us-east-2}\n", "", 1), `spec.deployments[0].region: "": give the deployment's AWS region`},
		{"bad region", strings.Replace(ok, "us-west-2", "us-west-2.evil.example", 1), `spec.deployments[1].region: "us-west-2.evil.example"`},
		{"envelope", strings.Replace(ok, "envelope: bedrock", "envelope: vertex", 1), `spec.envelope: "vertex" is not an envelope`},
		{"health check", strings.Replace(ok, "{name: east,", "{name: east, healthCheck: {path: /ping},", 1), "spec.deployments[0].healthCheck: a bedrock deployment has no health check"},
	} {
		_, err := Compile(objects(t, map[string]string{"golem.yaml": backend(c.spec)}), published)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// TestLocalBox: a shared local box's deployment carries its per-principal
// ceiling and its health check (the interval defaulted) into the snapshot;
// a negative ceiling or interval, and a path that is not one, are refused.
func TestLocalBox(t *testing.T) {
	box := func(d string) string {
		return strings.Replace(good, "{name: sonnet, url: \"https://api.anthropic.com/v1/messages\", model: claude-sonnet-4-5}", "{name: sonnet, url: \"https://api.anthropic.com/v1/messages\", model: claude-sonnet-4-5, "+d+"}", 1)
	}
	s, err := Compile(objects(t, map[string]string{"golem.yaml": box("maxInFlightPerPrincipal: 2, healthCheck: {path: /v1/models}, quota: {requestsPerMinute: 50}, betas: [b2, b1, b2]")}), published)
	if err != nil {
		t.Fatal(err)
	}
	if d := s.Backends[0].Deployments[0]; d.MaxInFlightPerPrincipal != 2 || d.HealthPath != "/v1/models" || d.HealthIntervalMs != 10000 || d.QuotaRPM != 50 || !slices.Equal(d.Betas, []string{"b1", "b2"}) {
		t.Fatalf("%+v", d)
	}
	for _, c := range []struct{ name, d, want string }{
		{"ceiling", "maxInFlightPerPrincipal: -1", "spec.deployments[0].maxInFlightPerPrincipal: must not be negative"},
		{"interval", "healthCheck: {path: /health, intervalSeconds: -5}", "spec.deployments[0].healthCheck.intervalSeconds: must not be negative"},
		{"path", "healthCheck: {path: v1/models}", `spec.deployments[0].healthCheck.path: "v1/models": give a path`},
		{"query", "healthCheck: {path: \"/health?x=1\"}", `spec.deployments[0].healthCheck.path: "/health?x=1"`},
		{"quota", "quota: {requestsPerMinute: -1}", "spec.deployments[0].quota.requestsPerMinute: must not be negative"},
	} {
		_, err := Compile(objects(t, map[string]string{"golem.yaml": box(c.d)}), published)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
