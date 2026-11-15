// Package bedrock is the envelope for Claude on Amazon Bedrock (ADR 0001:
// an envelope carries a dialect's request to a provider and its answer
// back, translating nothing). The Anthropic Messages body goes to
// InvokeModel or InvokeModelWithResponseStream with its model in the path
// and its betas in the body, signed with SigV4; a streamed answer's AWS
// event stream comes back as the Messages SSE the request path reads, a
// Bedrock exception as the dialect's error event, so the commit point
// (gateway/proxy/commit.go) refuses an exception before any output and
// the attempt loop fails over (ADR 0051).
//
// It is an http.RoundTripper over the deployment's transport: the request
// path sees an Anthropic provider.
package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/chucklehead-dev/golem/gateway/envelope"
)

// Version is the Messages API version Bedrock takes in the body.
const Version = "bedrock-2023-05-31"

// Transport sends one deployment's requests to Bedrock.
type Transport struct {
	// Base is the deployment's transport (its dialer's address checks,
	// its connection pool).
	Base http.RoundTripper
	// Endpoint is the Bedrock runtime's base URL, by default
	// https://bedrock-runtime.<Region>.amazonaws.com (a VPC endpoint's
	// otherwise).
	Endpoint *url.URL
	// Region is the deployment's, which the signature names.
	Region string
	// Model is the Bedrock model ID, inference profile ID or ARN.
	Model string
	// Credentials signs each request: the pod's role (IRSA, Pod Identity),
	// refreshed by the AWS SDK's provider chain.
	Credentials aws.CredentialsProvider
	// Now is the signing clock (tests set it).
	Now func() time.Time

	signer *v4.Signer
}

// MetadataHeader carries a request's metadata to Bedrock's invocation
// logs (InvokeModel's per-request metadata tagging).
const MetadataHeader = "X-Amzn-Bedrock-Request-Metadata"

// metadata is an attribution as Bedrock's request metadata: a JSON object
// of strings, the empty ones left out.
func metadata(a *envelope.Attribution) string {
	b := []byte{'{'}
	for _, kv := range [...][2]string{{"golem_request_id", a.RequestID}, {"golem_attempt_id", a.AttemptID},
		{"golem_principal", a.Principal}, {"golem_scope", a.Scope}, {"golem_key", a.Key}} {
		if kv[1] == "" {
			continue
		}
		if len(b) > 1 {
			b = append(b, ',')
		}
		b, _ = jsontext.AppendQuote(b, kv[0])
		b = append(b, ':')
		b, _ = jsontext.AppendQuote(b, kv[1])
	}
	return string(append(b, '}'))
}

// EndpointFor is a region's Bedrock runtime endpoint.
func EndpointFor(region string) *url.URL {
	return &url.URL{Scheme: "https", Host: "bedrock-runtime." + region + ".amazonaws.com"}
}

// RoundTrip rewrites the request for Bedrock, signs it, and sends it; a
// streamed answer comes back as SSE, an error as Anthropic's error shape.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	count := strings.HasSuffix(r.URL.Path, "/count_tokens")
	body, stream, err := rewrite(body, betas(r.Header))
	if err != nil {
		return nil, fmt.Errorf("bedrock: the request body: %w", err)
	}
	action := "invoke"
	switch {
	case count:
		// Anthropic's token count is Bedrock's CountTokens, over the body
		// InvokeModel would take (which needs max_tokens; a count's has
		// none), base64 inside its input.
		action, stream = "count-tokens", false
		if body, err = countBody(body); err != nil {
			return nil, err
		}
	case stream:
		action = "invoke-with-response-stream"
	}
	// The model is one path segment, escaped as the AWS SDK escapes it (an
	// inference profile's ARN has colons and slashes); the signature is
	// over the path as sent.
	u := *t.Endpoint
	base := strings.TrimSuffix(u.Path, "/")
	u.Path = base + "/model/" + t.Model + "/" + action
	u.RawPath = strings.TrimSuffix(t.Endpoint.EscapedPath(), "/") + "/model/" + modelSegment(t.Model) + "/" + action
	up := r.Clone(r.Context())
	up.URL, up.Host = &u, u.Host
	up.Body, up.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	up.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	up.Header = r.Header.Clone()
	up.Header.Del("Anthropic-Version")
	up.Header.Del("Anthropic-Beta")
	up.Header.Set("Content-Type", "application/json")
	if stream {
		up.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		up.Header.Set("Accept", "application/json")
	}
	// The attempt's attribution as Bedrock's request metadata, set before
	// signing (Bedrock refuses the header unsigned): its invocation logs
	// then carry golem's request and attempt IDs beside the principal,
	// scope and key, so a call joins the ledger row for it.
	if a := envelope.AttributionOf(r.Context()); a != nil {
		up.Header.Set(MetadataHeader, metadata(a))
	}
	if err := t.sign(r.Context(), up, body); err != nil {
		return nil, err
	}
	resp, err := t.Base.RoundTrip(up)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode != http.StatusOK:
		return anthropicError(resp)
	case count:
		return countAnswer(resp)
	case stream:
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Body = &reframer{src: resp.Body, dec: eventstream.NewDecoder()}
	}
	return resp, nil
}

// sign signs up with SigV4 for Bedrock in the deployment's region.
func (t *Transport) sign(ctx context.Context, up *http.Request, body []byte) error {
	if t.Credentials == nil {
		return errors.New("bedrock: no AWS credentials: the gateway needs a role (IRSA or Pod Identity) or AWS_* environment credentials")
	}
	creds, err := t.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("bedrock: AWS credentials: %w", err)
	}
	if t.signer == nil {
		t.signer = v4.NewSigner()
	}
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	sum := sha256.Sum256(body)
	return t.signer.SignHTTP(ctx, creds, up, hex.EncodeToString(sum[:]), "bedrock", t.Region, now())
}

// modelSegment is a model ID as one escaped path segment.
func modelSegment(model string) string {
	return strings.ReplaceAll(url.PathEscape(model), ":", "%3A")
}

// countBody is CountTokens' request for an InvokeModel body: the body,
// with a max_tokens if it has none, base64 in input.invokeModel.body.
func countBody(invoke []byte) ([]byte, error) {
	if !hasMember(invoke, "max_tokens") {
		invoke = append(bytes.TrimSuffix(bytes.TrimSpace(invoke), []byte("}")), []byte(`,"max_tokens":1}`)...)
	}
	b := []byte(`{"input":{"invokeModel":{"body":"`)
	b = base64.StdEncoding.AppendEncode(b, invoke)
	return append(b, `"}}}`...), nil
}

// hasMember reports whether a JSON object has a top-level member.
func hasMember(raw []byte, want string) bool {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return false
	}
	for dec.PeekKind() == '"' {
		name, err := dec.ReadToken()
		if err != nil {
			return false
		}
		if name.String() == want {
			return true
		}
		if err := dec.SkipValue(); err != nil {
			return false
		}
	}
	return false
}

// countAnswer is CountTokens' answer ({"inputTokens": n}) in Anthropic's
// shape ({"input_tokens": n}).
func countAnswer(resp *http.Response) (*http.Response, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var n int64 = -1
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.ReadToken(); err == nil && tok.Kind() == '{' {
		for dec.PeekKind() == '"' {
			name, err := dec.ReadToken()
			if err != nil {
				break
			}
			key := name.String()
			tok, err := dec.ReadToken()
			if err != nil {
				break
			}
			if key == "inputTokens" && tok.Kind() == '0' {
				if v, err := tok.Int(); err == nil {
					n = v
				}
			}
		}
	}
	if n < 0 {
		return nil, fmt.Errorf("bedrock: a token count with no inputTokens: %.200s", raw)
	}
	body := []byte(`{"input_tokens":` + strconv.FormatInt(n, 10) + "}")
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

// betas are the request's anthropic-beta values, each comma list split.
func betas(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Anthropic-Beta") {
		for b := range strings.SplitSeq(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// rewrite is a Messages body as Bedrock takes it: model and stream taken
// out (the model is the path's, streaming the action's), anthropic_version
// set, and the header's betas added as anthropic_beta (after the body's
// own, if it has any). It reports whether the request streams.
func rewrite(body []byte, headerBetas []string) (out []byte, stream bool, err error) {
	dec := jsontext.NewDecoder(bytes.NewReader(body))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false, errors.New("not a JSON object")
	}
	enc.WriteToken(jsontext.BeginObject)
	hasVersion := false
	var bodyBetas []string
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, false, err
		}
		name := tok.String() // a token is valid only until the next read
		v, err := dec.ReadValue()
		if err != nil {
			return nil, false, err
		}
		switch name {
		case "model":
			continue
		case "stream":
			stream = string(v) == "true"
			continue
		case "anthropic_beta":
			var bs []string
			if err := jsonUnmarshalStrings(v, &bs); err != nil {
				return nil, false, errors.New("anthropic_beta is not a list of strings")
			}
			bodyBetas = bs
			continue
		case "anthropic_version":
			hasVersion = true
		}
		enc.WriteToken(jsontext.String(name))
		enc.WriteValue(v)
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false, err
	}
	if !hasVersion {
		enc.WriteToken(jsontext.String("anthropic_version"))
		enc.WriteToken(jsontext.String(Version))
	}
	if all := append(bodyBetas, headerBetas...); len(all) > 0 {
		enc.WriteToken(jsontext.String("anthropic_beta"))
		enc.WriteToken(jsontext.BeginArray)
		for _, b := range all {
			enc.WriteToken(jsontext.String(b))
		}
		enc.WriteToken(jsontext.EndArray)
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return nil, false, err
	}
	return bytes.TrimSpace(buf.Bytes()), stream, nil
}

func jsonUnmarshalStrings(v jsontext.Value, out *[]string) error {
	dec := jsontext.NewDecoder(bytes.NewReader(v))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '[' {
		return errors.New("not a list")
	}
	for dec.PeekKind() != ']' {
		tok, err := dec.ReadToken()
		if err != nil || tok.Kind() != '"' {
			return errors.New("not a string")
		}
		*out = append(*out, tok.String())
	}
	return nil
}

// errorType is an Anthropic error type for a Bedrock exception: what the
// request path and the SDKs read (a rate limit and an overload retried,
// ADR 0051; the rest not).
func errorType(exception string) string {
	switch strings.ToLower(strings.TrimSuffix(exception, "Exception")) {
	case "throttling":
		return "rate_limit_error"
	case "serviceunavailable", "modelnotready":
		return "overloaded_error"
	case "validation":
		return "invalid_request_error"
	case "accessdenied":
		return "permission_error"
	case "resourcenotfound":
		return "not_found_error"
	}
	return "api_error"
}

// anthropicError is a Bedrock error response in Anthropic's shape, its
// status kept (429 throttling, 503 unavailable, 400 validation): the
// caller sees the dialect it called.
func anthropicError(resp *http.Response) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	exception := resp.Header.Get("X-Amzn-Errortype")
	if i := strings.IndexAny(exception, ":"); i >= 0 {
		exception = exception[:i]
	}
	msg := bedrockMessage(raw)
	body := errorJSON(errorType(exception), msg)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

// bedrockMessage is a Bedrock error body's message, or the body itself.
func bedrockMessage(raw []byte) string {
	if m, ok := member(raw, "message"); ok {
		return m
	}
	if m, ok := member(raw, "Message"); ok {
		return m
	}
	return strings.TrimSpace(string(raw))
}

// member is a JSON object's top-level string member, unquoted.
func member(raw []byte, want string) (string, bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return "", false
	}
	for dec.PeekKind() == '"' {
		name, err := dec.ReadToken()
		if err != nil {
			return "", false
		}
		if name.String() == want && dec.PeekKind() == '"' {
			tok, err := dec.ReadToken()
			return tok.String(), err == nil
		}
		if err := dec.SkipValue(); err != nil {
			return "", false
		}
	}
	return "", false
}

func errorJSON(typ, msg string) []byte {
	b := []byte(`{"type":"error","error":{"type":`)
	b, _ = jsontext.AppendQuote(b, typ)
	b = append(b, `,"message":`...)
	b, _ = jsontext.AppendQuote(b, "bedrock: "+msg)
	return append(b, "}}"...)
}

// reframer reads an AWS event stream and yields the Messages SSE it
// carries: each chunk's base64 bytes, an Anthropic event, as
// `event: <type>`, `data: <json>`; an exception as the dialect's error
// event, then the end. A message whose CRC fails, or a stream cut, is a
// read error: the request path's cut.
type reframer struct {
	src  io.ReadCloser
	dec  *eventstream.Decoder
	buf  []byte // payload buffer, reused
	out  []byte // SSE not yet read
	done bool
}

func (f *reframer) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		if f.done {
			return 0, io.EOF
		}
		if err := f.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

func (f *reframer) Close() error { return f.src.Close() }

// next decodes one event-stream message into f.out.
func (f *reframer) next() error {
	msg, err := f.dec.Decode(f.src, f.buf[:0])
	if err == io.EOF {
		f.done = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("bedrock: the event stream: %w", err)
	}
	f.buf = msg.Payload[:0]
	header := func(name string) string {
		if v := msg.Headers.Get(name); v != nil {
			return v.String()
		}
		return ""
	}
	switch header(":message-type") {
	case "event":
		if header(":event-type") != "chunk" {
			return nil // an event that carries no Messages event
		}
		ev, err := chunkEvent(msg.Payload)
		if err != nil {
			return err
		}
		typ := eventType(ev)
		f.out = append(append(append(append(append(f.out[:0], "event: "...), typ...), "\ndata: "...), ev...), "\n\n"...)
	case "exception", "error":
		name := header(":exception-type")
		if name == "" {
			name = header(":error-code")
		}
		ev := errorJSON(errorType(name), bedrockMessage(msg.Payload))
		f.out = append(append(append(f.out[:0], "event: error\ndata: "...), ev...), "\n\n"...)
		f.done = true
	}
	return nil
}

// chunkEvent is a chunk's Anthropic event: its "bytes", base64.
func chunkEvent(payload []byte) ([]byte, error) {
	raw, ok := member(payload, "bytes")
	if !ok {
		return nil, errors.New("bedrock: a chunk with no bytes")
	}
	out, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("bedrock: a chunk's bytes: %w", err)
	}
	return out, nil
}

// eventType is an Anthropic event's "type" member (its SSE event name).
func eventType(ev []byte) string {
	if t, ok := member(ev, "type"); ok && t != "" {
		return t
	}
	return "message"
}
