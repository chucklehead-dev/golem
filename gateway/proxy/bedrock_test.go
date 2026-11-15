package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/chucklehead-dev/golem/gateway/canon"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
)

// bedrockStub is Bedrock's runtime for the tests: it checks each request's
// SigV4 signature by signing it again, then answers with an event stream,
// or with a throttling exception as its first event for a region told to
// throttle.
type bedrockStub struct {
	creds    aws.Credentials
	throttle string // a region whose first call is throttled
	mu       sync.Mutex
	regions  []string // each request's signed region, in order
	paths    []string
	bad      []string     // what failed the signature check
	rows     lockedBuffer // the gateway's ledger rows
}

func (s *bedrockStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	auth := r.Header.Get("Authorization")
	// Credential=AKID/<date>/<region>/bedrock/aws4_request
	scope := strings.Split(strings.SplitN(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), ",", 2)[0], "/")
	if len(scope) != 5 || scope[3] != "bedrock" {
		s.fail(w, "no SigV4 credential scope: "+auth)
		return
	}
	region := scope[2]
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		s.fail(w, "no X-Amz-Date")
		return
	}
	// As AWS does: only the headers the signature names are signed (a
	// transport adds others, Accept-Encoding, after signing).
	signed := map[string]bool{}
	if i := strings.Index(auth, "SignedHeaders="); i >= 0 {
		for h := range strings.SplitSeq(strings.SplitN(auth[i+len("SignedHeaders="):], ",", 2)[0], ";") {
			signed[http.CanonicalHeaderKey(h)] = true
		}
	}
	again := r.Clone(context.Background())
	again.URL.Scheme, again.URL.Host = "http", r.Host
	for h := range again.Header {
		if !signed[h] || h == "X-Amz-Date" || h == "X-Amz-Security-Token" {
			again.Header.Del(h)
		}
	}
	sum := sha256.Sum256(body)
	v4.NewSigner().SignHTTP(context.Background(), s.creds, again, hex.EncodeToString(sum[:]), "bedrock", region, at)
	if again.Header.Get("Authorization") != auth {
		s.fail(w, "the signature does not match the request")
		return
	}
	s.mu.Lock()
	first := !strings.Contains(strings.Join(s.regions, " "), region)
	s.regions, s.paths = append(s.regions, region), append(s.paths, r.URL.EscapedPath())
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	enc := eventstream.NewEncoder()
	if region == s.throttle && first {
		var m eventstream.Message
		m.Headers.Set(":message-type", eventstream.StringValue("exception"))
		m.Headers.Set(":exception-type", eventstream.StringValue("throttlingException"))
		m.Payload = []byte(`{"message":"Too many requests, please wait before trying again."}`)
		enc.Encode(w, m)
		return
	}
	for _, ev := range bedrockEvents {
		var m eventstream.Message
		m.Headers.Set(":message-type", eventstream.StringValue("event"))
		m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
		m.Payload = []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(ev)) + `"}`)
		enc.Encode(w, m)
		w.(http.Flusher).Flush()
	}
}

func (s *bedrockStub) fail(w http.ResponseWriter, why string) {
	s.mu.Lock()
	s.bad = append(s.bad, why)
	s.mu.Unlock()
	w.Header().Set("X-Amzn-Errortype", "UnrecognizedClientException")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"message":"The security token included in the request is invalid."}`))
}

var bedrockEvents = []string{
	`{"type":"message_start","message":{"id":"msg_b","type":"message","role":"assistant","content":[],"model":"claude","usage":{"input_tokens":120,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from Bedrock"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
	`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":120,"outputTokenCount":7,"invocationLatency":300,"firstByteLatency":100}}`,
}

// bedrockHandler compiles a real config with a bedrock backend in two
// regions, both served by the stub, the route's pool the east first.
func bedrockHandler(t *testing.T, stub *bedrockStub, credentials aws.CredentialsProvider) (*Handler, string, *Result) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	alice := key.New(key.Dev)
	cfg := `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: bedrock}
spec:
  dialect: anthropic.messages
  envelope: bedrock
  aws: {region: us-east-2}
  network: {allowPrivateAddresses: true}
  deployments:
  - {name: east, url: "` + srv.URL + `", model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}
  - {name: west, url: "` + srv.URL + `", region: us-west-2, model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, agent: true, pool: [{backend: bedrock, deployment: east}, {backend: bedrock, deployment: west}]}
---
apiVersion: golem.dev/v1alpha1
kind: PriceList
metadata: {name: p}
spec:
  currency: USD
  prices:
  - {model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", inputPerMTok: 3000000, outputPerMTok: 15000000}
---
apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude]}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: ` + alice + `, principal: alice, scope: team-a}]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "golem.yaml"), []byte(cfg), 0o600)
	h := &gsnap.Holder{Env: key.Dev, AWS: credentials}
	d := &gsnap.Dir{Path: dir, Holder: h, Secrets: gsnap.EnvSecrets{Getenv: os.Getenv}}
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	tel, _ := telemetry.New(telemetry.Stdout(&stub.rows), "local", "test")
	return &Handler{Dialect: AnthropicMessages, Snapshots: h, MaxBody: 1 << 20, Telemetry: tel,
		record: func(_ *http.Request, r Result) { *res = r }}, alice, res
}

var staticCreds = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "token"}, nil
})

// TestBedrockEndToEnd: Claude Code's kind of call, an Anthropic Messages
// stream with a beta, through the gateway to Claude on Bedrock: compiled
// from config (a bedrock backend, two regions), signed with SigV4 for each
// deployment's region (the stub checks every signature by signing again),
// the first region's throttling exception refused before the commit and
// failed over to the second (ADR 0051), and the caller given the Messages
// SSE, byte for byte what Bedrock's chunks carried, its usage read for the
// row. Without AWS credentials the call fails, saying what to configure.
func TestBedrockEndToEnd(t *testing.T) {
	stub := &bedrockStub{creds: aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "token"},
		throttle: "us-east-2"}
	h, alice, res := bedrockHandler(t, stub, staticCreds)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("X-Api-Key", alice)
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var want bytes.Buffer
	for _, ev := range bedrockEvents {
		typ := strings.SplitN(strings.TrimPrefix(ev, `{"type":"`), `"`, 2)[0]
		want.WriteString("event: " + typ + "\ndata: " + ev + "\n\n")
	}
	switch {
	case len(stub.bad) > 0:
		t.Fatalf("the stub refused a signature: %v", stub.bad)
	case w.Code != 200 || w.Body.String() != want.String():
		t.Fatalf("status %d, body\n%s\nwant\n%s", w.Code, w.Body, want.String())
	case strings.Join(stub.regions, " ") != "us-east-2 us-west-2":
		t.Fatalf("regions %v: the east throttled, the call failed over to the west, each signed for its own", stub.regions)
	case !strings.Contains(stub.paths[1], "/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke-with-response-stream"):
		t.Fatalf("path %s", stub.paths[1])
	}
	if want := (canon.Usage{Input: 120, Output: 7}); res.Usage != want || res.Target.Deployment != "west" {
		t.Fatalf("usage %+v from %s: want %+v from the west", res.Usage, res.Target.Deployment, want)
	}
	rows := rowsOf(t, stub.rows.snapshot())
	if len(rows) != 2 || rows[0]["envelope"] != "bedrock" || rows[1]["envelope"] != "bedrock" ||
		rows[0]["cost_micros"].(float64) != 0 || rows[1]["deployment"] != "bedrock/west" {
		t.Fatalf("rows %v: the throttled attempt free, the west's charged, both of the bedrock envelope", rows)
	}

	// No credentials: refused before anything is sent.
	stub2 := &bedrockStub{creds: stub.creds}
	h, alice, _ = bedrockHandler(t, stub2, nil)
	r = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":64,"stream":true,"messages":[]}`))
	r.Header.Set("X-Api-Key", alice)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || len(stub2.regions)+len(stub2.bad) != 0 {
		t.Fatalf("status %d, stub saw %v %v: without credentials nothing is sent", w.Code, stub2.regions, stub2.bad)
	}
}
