// Package bedrock is the envelope for Claude on Amazon Bedrock. An envelope
// carries a dialect's request to a provider and its answer back,
// translating nothing. The Anthropic Messages body goes to InvokeModel or
// InvokeModelWithResponseStream, signed with SigV4. The model goes in the
// path and the betas in the body.
//
// A streamed answer's AWS event stream comes back as the Messages SSE the
// request path reads. A Bedrock exception comes back as the dialect's error
// event. So the commit point refuses an exception before any output, and
// the attempt loop fails over. ADR 0001, ADR 0051.
//
// It is an http.RoundTripper over the deployment's transport: the request
// path sees an Anthropic provider.
package bedrock

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	// Log is where a refusal of the gateway's credentials is logged; nil
	// logs to slog.Default().
	Log *slog.Logger

	signerOnce sync.Once // RoundTrip is concurrent
	signer     *v4.Signer
}

func (t *Transport) log() *slog.Logger { return cmp.Or(t.Log, slog.Default()) }

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
	body, err := readBody(r)
	r.Body.Close()
	if err != nil {
		return nil, envelope.NotSent(err)
	}
	count := strings.HasSuffix(r.URL.Path, "/count_tokens")
	body, stream, err := rewrite(body, betas(r.Header))
	if err != nil {
		return nil, envelope.NotSent(fmt.Errorf("bedrock: the request body: %w", err))
	}
	action := "invoke"
	switch {
	case count:
		// Anthropic's token count is Bedrock's CountTokens, over the body
		// InvokeModel would take (which needs max_tokens; a count's has
		// none), base64 inside its input.
		action, stream, body = "count-tokens", false, countBody(body)
	case stream:
		action = "invoke-with-response-stream"
	}
	// The model is one path segment, escaped as the AWS SDK escapes it (an
	// inference profile's ARN has colons and slashes); the signature is
	// over the path as sent.
	u := *t.Endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/model/" + t.Model + "/" + action
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
	// Nothing has left the gateway until Base's round trip: a failure to
	// sign (credentials that cannot be retrieved) is free and fails over.
	if err := t.sign(r.Context(), up, body); err != nil {
		return nil, envelope.NotSent(err)
	}
	resp, err := t.Base.RoundTrip(up)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode != http.StatusOK:
		return anthropicError(resp, t.log())
	case count:
		return countAnswer(resp)
	case stream:
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Body = &reframer{src: resp.Body, log: t.log()}
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
	t.signerOnce.Do(func() { t.signer = v4.NewSigner() })
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
func countBody(invoke []byte) []byte {
	if !hasMember(invoke, "max_tokens") {
		invoke = append(bytes.TrimSuffix(bytes.TrimSpace(invoke), []byte("}")), []byte(`,"max_tokens":1}`)...)
	}
	b := []byte(`{"input":{"invokeModel":{"body":"`)
	b = base64.StdEncoding.AppendEncode(b, invoke)
	return append(b, `"}}}`...)
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
	return withJSON(resp, []byte(`{"input_tokens":`+strconv.FormatInt(n, 10)+"}")), nil
}

// withJSON is resp with body, a JSON document, as its body.
func withJSON(resp *http.Response, body []byte) *http.Response {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Content-Type", "application/json")
	return resp
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

// stringList is a JSON array of strings, or not ok.
func stringList(v jsontext.Value) (out []string, ok bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(v))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '[' {
		return nil, false
	}
	for dec.PeekKind() != ']' {
		tok, err := dec.ReadToken()
		if err != nil || tok.Kind() != '"' {
			return nil, false
		}
		out = append(out, tok.String())
	}
	return out, true
}

// errorType maps a Bedrock exception to the Anthropic error type that the
// request path and the SDKs read. A rate limit and an overload are retried,
// and the rest are not. ADR 0051.
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
func anthropicError(resp *http.Response, log *slog.Logger) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	exception := resp.Header.Get("X-Amzn-Errortype")
	exception, _, _ = strings.Cut(exception, ":")
	if credentialsRefused(exception) || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return withJSON(resp, refusedJSON(log, errorType(exception), exception, resp.StatusCode, raw)), nil
	}
	return withJSON(resp, errorJSON(errorType(exception), bedrockMessage(raw))), nil
}

// credentialsRefused reports an exception that refuses the gateway's own
// credentials or rights (its role), not the caller's request.
func credentialsRefused(exception string) bool {
	switch strings.ToLower(strings.TrimSuffix(exception, "Exception")) {
	case "accessdenied", "unrecognizedclient", "expiredtoken", "invalidsignature", "incompletesignature",
		"missingauthenticationtoken", "invalidclienttokenid", "signaturedoesnotmatch", "unauthorized", "notauthorized":
		return true
	}
	return false
}

// refusedMessage is what a caller is told when Bedrock refuses the
// gateway's credentials: Bedrock's own text names golem's role ARN and
// account, which are not the caller's to learn, so it is logged instead.
const refusedMessage = "the gateway's credentials for this provider were refused; the platform team is alerted"

// refusedJSON is an Anthropic error carrying golem's message for a
// credentials refusal, Bedrock's text logged for the platform team.
func refusedJSON(log *slog.Logger, typ, exception string, status int, raw []byte) []byte {
	log.Error("bedrock: the provider refused the gateway's credentials; check the gateway's role and its bedrock:InvokeModel permissions (operations.md)",
		"exception", exception, "status", status, "message", bedrockMessage(raw))
	b := []byte(`{"type":"error","error":{"type":`)
	b, _ = jsontext.AppendQuote(b, typ)
	b = append(b, `,"message":`...)
	b, _ = jsontext.AppendQuote(b, refusedMessage)
	return append(b, "}}"...)
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
