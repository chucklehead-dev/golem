package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/chucklehead-dev/golem/gateway/envelope"
)

var creds = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "token"}, nil
})

var signedAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

type capture struct {
	req  *http.Request
	body []byte
	resp *http.Response
}

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.req = r
	c.body, _ = io.ReadAll(r.Body)
	if c.resp != nil {
		return c.resp, nil
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{}}`))}, nil
}

const profile = "arn:aws:bedrock:us-east-2:123456789012:inference-profile/us.anthropic.claude-sonnet"

func transport(base http.RoundTripper) *Transport {
	return &Transport{Base: base, Endpoint: EndpointFor("us-east-2"), Region: "us-east-2", Model: profile,
		Credentials: creds, Now: func() time.Time { return signedAt }}
}

// TestRewrite: the Messages body as Bedrock takes it: no model, no stream
// (the path's and the action's), anthropic_version set, the body's betas
// and then the header's as anthropic_beta; everything else as it was.
func TestRewrite(t *testing.T) {
	out, stream, err := rewrite([]byte(`{"model":"claude","max_tokens":10,"stream":true,"anthropic_beta":["a"],"messages":[{"role":"user","content":"hi"}]}`), []string{"b", "c"})
	if err != nil || !stream {
		t.Fatalf("stream %v, %v", stream, err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	switch {
	case got["model"] != nil || got["stream"] != nil:
		t.Fatalf("%s: model and stream must go", out)
	case got["anthropic_version"] != Version:
		t.Fatalf("%s: anthropic_version", out)
	case got["max_tokens"] != 10.0 || got["messages"] == nil:
		t.Fatalf("%s: the rest kept", out)
	}
	if b, _ := json.Marshal(got["anthropic_beta"]); string(b) != `["a","b","c"]` {
		t.Fatalf("anthropic_beta %s: the body's, then the header's", b)
	}
	if _, stream, _ := rewrite([]byte(`{"model":"m","messages":[]}`), nil); stream {
		t.Fatal("a request without stream: true streams")
	}
	if _, _, err := rewrite([]byte(`[1]`), nil); err == nil {
		t.Fatal("a body that is not an object was rewritten")
	}
}

// TestRequest: a streamed call goes to InvokeModelWithResponseStream with
// the model (an inference profile's ARN) one escaped path segment, no
// anthropic-* header, and a SigV4 signature for bedrock in the region,
// which re-signing the request as received reproduces (ablated: an
// unsigned request has none to match).
func TestRequest(t *testing.T) {
	c := &capture{}
	r, _ := http.NewRequest("POST", "https://provider.example/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":5,"stream":true,"messages":[]}`))
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Anthropic-Beta", "b1")
	if _, err := transport(c).RoundTrip(r); err != nil {
		t.Fatal(err)
	}
	got := c.req
	wantPath := "/model/arn%3Aaws%3Abedrock%3Aus-east-2%3A123456789012%3Ainference-profile%2Fus.anthropic.claude-sonnet/invoke-with-response-stream"
	switch {
	case got.URL.Host != "bedrock-runtime.us-east-2.amazonaws.com" || got.URL.EscapedPath() != wantPath:
		t.Fatalf("URL %s %s", got.URL.Host, got.URL.EscapedPath())
	case got.Header.Get("Anthropic-Version") != "" || got.Header.Get("Anthropic-Beta") != "":
		t.Fatalf("headers %v: the anthropic-* headers go in the body", got.Header)
	case got.Header.Get("Accept") != "application/vnd.amazon.eventstream":
		t.Fatalf("Accept %q", got.Header.Get("Accept"))
	case !strings.HasPrefix(got.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261115/us-east-2/bedrock/aws4_request"):
		t.Fatalf("Authorization %q", got.Header.Get("Authorization"))
	case got.Header.Get("X-Amz-Security-Token") != "token":
		t.Fatal("no session token: a role's credentials need it")
	}
	if !strings.Contains(string(c.body), `"anthropic_beta":["b1"]`) {
		t.Fatalf("body %s: the header's beta", c.body)
	}
	// Re-sign what arrived: the signature must be the same.
	again := got.Clone(context.Background())
	again.Header.Del("Authorization")
	again.Header.Del("X-Amz-Date")
	again.Header.Del("X-Amz-Security-Token")
	tr := transport(nil)
	if err := tr.sign(context.Background(), again, c.body); err != nil {
		t.Fatal(err)
	}
	if again.Header.Get("Authorization") != got.Header.Get("Authorization") {
		t.Fatalf("signature %q, re-signed %q", got.Header.Get("Authorization"), again.Header.Get("Authorization"))
	}

	// Not streamed: InvokeModel, its JSON answer as it came.
	c = &capture{}
	r, _ = http.NewRequest("POST", "https://provider.example/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	resp, err := transport(c).RoundTrip(r)
	if err != nil || !strings.HasSuffix(c.req.URL.Path, "/invoke") {
		t.Fatalf("%v %s", err, c.req.URL.Path)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != `{"usage":{}}` {
		t.Fatalf("body %s", b)
	}
}

// TestNoCredentials: a deployment with no credentials is refused with what
// to do, before anything is sent.
func TestNoCredentials(t *testing.T) {
	tr := transport(&capture{})
	tr.Credentials = nil
	r, _ := http.NewRequest("POST", "https://x/v1/messages", strings.NewReader(`{"model":"m"}`))
	if _, err := tr.RoundTrip(r); err == nil || !strings.Contains(err.Error(), "IRSA") {
		t.Fatalf("%v", err)
	}
}

// TestNotSent: a request that never left the gateway (a body that cannot
// be read or rewritten, credentials missing or that cannot be retrieved)
// fails with envelope.ErrNotSent, its message kept, and Base is never
// called; a failure of Base's round trip is not marked.
func TestNotSent(t *testing.T) {
	failing := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("no web identity token")
	})
	for name, c := range map[string]struct {
		creds aws.CredentialsProvider
		body  io.Reader
		want  string
	}{
		"nil credentials": {nil, strings.NewReader(`{"model":"m"}`), "IRSA"},
		"retrieve fails":  {failing, strings.NewReader(`{"model":"m"}`), "no web identity token"},
		"not an object":   {creds, strings.NewReader(`[1]`), "the request body"},
		"body read fails": {creds, io.MultiReader(strings.NewReader(`{`), errReader{}), "read failed"},
	} {
		base := &capture{}
		tr := transport(base)
		tr.Credentials = c.creds
		r, _ := http.NewRequest("POST", "https://x/v1/messages", c.body)
		_, err := tr.RoundTrip(r)
		if !errors.Is(err, envelope.ErrNotSent) || !strings.Contains(err.Error(), c.want) || base.req != nil {
			t.Errorf("%s: %v (sent %v): want ErrNotSent naming %q, nothing sent", name, err, base.req != nil, c.want)
		}
	}
	tr := transport(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("reset") }))
	r, _ := http.NewRequest("POST", "https://x/v1/messages", strings.NewReader(`{"model":"m"}`))
	if _, err := tr.RoundTrip(r); err == nil || errors.Is(err, envelope.ErrNotSent) {
		t.Fatalf("a failed send %v: must not be ErrNotSent", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// encode is an event stream of chunks, each an Anthropic event, then the
// exception given (if any).
func encode(t *testing.T, events []string, exception string) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, ev := range events {
		var m eventstream.Message
		m.Headers.Set(":message-type", eventstream.StringValue("event"))
		m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
		m.Payload = []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(ev)) + `","p":"abc"}`)
		if err := enc.Encode(&b, m); err != nil {
			t.Fatal(err)
		}
	}
	if exception != "" {
		var m eventstream.Message
		m.Headers.Set(":message-type", eventstream.StringValue("exception"))
		m.Headers.Set(":exception-type", eventstream.StringValue(exception))
		m.Payload = []byte(`{"message":"slow down"}`)
		enc.Encode(&b, m)
	}
	return b.Bytes()
}

var events = []string{
	`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	`{"type":"message_delta","usage":{"output_tokens":4}}`,
	`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":10,"outputTokenCount":4}}`,
}

func streamed(t *testing.T, stream []byte) ([]byte, error) {
	t.Helper()
	c := &capture{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/vnd.amazon.eventstream"}},
		Body: io.NopCloser(bytes.NewReader(stream))}}
	r, _ := http.NewRequest("POST", "https://x/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	resp, err := transport(c).RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}
	// One byte a read, as a slow provider's stream arrives.
	var out []byte
	buf := make([]byte, 7)
	for {
		n, err := resp.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// TestReframe: the event stream's chunks come out as the Messages SSE, each
// its event's type as the SSE event name, in order; an exception ends the
// stream as the dialect's error event, its type mapped (throttling a rate
// limit); a message whose CRC fails is a read error, never a quiet end.
func TestReframe(t *testing.T) {
	out, err := streamed(t, encode(t, events, ""))
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, ev := range events {
		var m struct{ Type string }
		json.Unmarshal([]byte(ev), &m)
		want.WriteString("event: " + m.Type + "\ndata: " + ev + "\n\n")
	}
	if string(out) != want.String() {
		t.Fatalf("SSE\n%s\nwant\n%s", out, want.String())
	}

	out, err = streamed(t, encode(t, nil, "throttlingException"))
	if err != nil || !strings.HasPrefix(string(out), "event: error\ndata: ") || !strings.Contains(string(out), `"type":"rate_limit_error"`) ||
		!strings.Contains(string(out), "slow down") {
		t.Fatalf("%v %s: an exception as the error event", err, out)
	}

	bad := encode(t, events[:1], "")
	bad[len(bad)-1] ^= 0xff // the message's CRC
	if _, err := streamed(t, bad); err == nil {
		t.Fatal("a message with a bad CRC was read")
	}
	if _, err := streamed(t, encode(t, events, "")[:20]); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("%v: a stream cut mid-message must be a read error", err)
	}
}

// TestErrorResponse: a Bedrock error answer keeps its status and comes back
// in Anthropic's error shape, its exception mapped.
func TestErrorResponse(t *testing.T) {
	for _, c := range []struct {
		status    int
		exception string
		want      string
	}{
		{429, "ThrottlingException", "rate_limit_error"},
		{503, "ServiceUnavailableException", "overloaded_error"},
		{400, "ValidationException:http://internal.amazon.com/coral/com.amazon.bedrock/", "invalid_request_error"},
		{403, "AccessDeniedException", "permission_error"},
	} {
		cp := &capture{resp: &http.Response{StatusCode: c.status, Header: http.Header{"X-Amzn-Errortype": {c.exception}},
			Body: io.NopCloser(strings.NewReader(`{"message":"no"}`))}}
		r, _ := http.NewRequest("POST", "https://x/v1/messages", strings.NewReader(`{"model":"m"}`))
		resp, err := transport(cp).RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		var e struct {
			Type  string
			Error struct{ Type, Message string }
		}
		json.Unmarshal(b, &e)
		if resp.StatusCode != c.status || e.Type != "error" || e.Error.Type != c.want || e.Error.Message != "bedrock: no" {
			t.Errorf("%s: status %d, %s", c.exception, resp.StatusCode, b)
		}
	}
}

// TestMetadata: an attempt's attribution goes to Bedrock as its request
// metadata, a JSON object of golem's IDs (no secret: the key's display
// ID), among the signed headers (Bedrock refuses it unsigned); a request
// with no attribution sends none.
func TestMetadata(t *testing.T) {
	c := &capture{}
	ctx := envelope.WithAttribution(context.Background(), &envelope.Attribution{RequestID: "r1", AttemptID: "r1.2",
		Principal: "alice", Scope: "team-a", Key: "glm_live_ab12"})
	r, _ := http.NewRequestWithContext(ctx, "POST", "https://x/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	if _, err := transport(c).RoundTrip(r); err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(c.req.Header.Get(MetadataHeader)), &meta); err != nil {
		t.Fatalf("metadata %q: %v", c.req.Header.Get(MetadataHeader), err)
	}
	want := map[string]string{"golem_request_id": "r1", "golem_attempt_id": "r1.2", "golem_principal": "alice",
		"golem_scope": "team-a", "golem_key": "glm_live_ab12"}
	for k, v := range want {
		if meta[k] != v {
			t.Fatalf("metadata %v: want %v", meta, want)
		}
	}
	if !strings.Contains(c.req.Header.Get("Authorization"), "x-amzn-bedrock-request-metadata") {
		t.Fatalf("Authorization %q: the metadata header must be signed", c.req.Header.Get("Authorization"))
	}

	c = &capture{}
	r, _ = http.NewRequest("POST", "https://x/v1/messages", strings.NewReader(`{"model":"m"}`))
	transport(c).RoundTrip(r)
	if c.req.Header.Get(MetadataHeader) != "" {
		t.Fatal("metadata without an attribution")
	}
	// An Entra token's call has no key: left out, not empty.
	if m := metadata(&envelope.Attribution{RequestID: "r", AttemptID: "r", Principal: "p", Scope: "s"}); strings.Contains(m, "golem_key") {
		t.Fatalf("%s", m)
	}
}

// TestCountTokens: Anthropic's token count goes to Bedrock's CountTokens
// over the body InvokeModel would take (no model, anthropic_version set, a
// max_tokens added, which InvokeModel needs and a count lacks), base64 in
// input.invokeModel.body; the answer comes back as Anthropic's.
func TestCountTokens(t *testing.T) {
	c := &capture{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"inputTokens":17}`))}}
	r, _ := http.NewRequest("POST", "https://x/v1/messages/count_tokens", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"max_tokens"}]}`))
	resp, err := transport(c).RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c.req.URL.EscapedPath(), "/count-tokens") {
		t.Fatalf("path %s", c.req.URL.EscapedPath())
	}
	var wrapped struct {
		Input struct{ InvokeModel struct{ Body string } }
	}
	json.Unmarshal(c.body, &wrapped)
	inner, err := base64.StdEncoding.DecodeString(wrapped.Input.InvokeModel.Body)
	if err != nil {
		t.Fatalf("%s: %v", c.body, err)
	}
	var got map[string]any
	json.Unmarshal(inner, &got)
	if got["model"] != nil || got["anthropic_version"] != Version || got["max_tokens"] != 1.0 || got["messages"] == nil {
		t.Fatalf("inner body %s", inner)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != `{"input_tokens":17}` {
		t.Fatalf("answer %s", b)
	}
}
