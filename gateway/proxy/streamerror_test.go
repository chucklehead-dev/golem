package proxy

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	libsnapshot "github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestStreamErrorAfterOutput: a stream that delivered output and then its
// provider's error event (Anthropic's event: error, an OpenAI chunk with
// "error") failed, whatever it delivered: its row says failed,
// upstream.stream_failed, of the provider, and it is charged what it
// produced, as before.
func TestStreamErrorAfterOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		d      Dialect
		body   string
		events [][]byte
	}{
		"anthropic": {AnthropicMessages, string(smallRequest), append(anthropicStream(2)[:3], overloaded)},
		"openai": {OpenAIChat, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`, [][]byte{
			[]byte(`data: {"choices":[{"delta":{"content":"some words"}}],"usage":null}` + "\n\n"),
			[]byte(`data: {"error":{"message":"The server had an error while processing your request.","type":"server_error"}}` + "\n\n"),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h, rows := streamHandler(t, tc.d, &stub{events: tc.events})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader(tc.body)))
			if w.Code != 200 {
				t.Fatalf("status %d %s", w.Code, w.Body)
			}
			all := rowsOf(t, rows)
			if len(all) != 1 || all[0]["outcome"] != "failed" || all[0]["error_code"] != "upstream.stream_failed" ||
				all[0]["error_origin"] != "provider" || all[0]["cost_micros"].(float64) <= 0 {
				t.Fatalf("rows %v: want one, failed upstream.stream_failed of the provider, charged its output", all)
			}
		})
	}
	// A stream with no error event is complete (the contrast).
	h, rows := streamHandler(t, AnthropicMessages, &stub{events: anthropicStream(2)})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewReader(smallRequest)))
	if all := rowsOf(t, rows); len(all) != 1 || all[0]["outcome"] != "completed" {
		t.Fatalf("rows %v: a whole stream is completed", all)
	}
}

// TestBedrockStreamErrorAfterOutput: Bedrock's exception mid-stream,
// reframed by its envelope as the dialect's error event, fails the call
// as an error event does.
func TestBedrockStreamErrorAfterOutput(t *testing.T) {
	stub := &bedrockStub{creds: aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "token"}, failAfter: 3}
	h, alice, _ := bedrockHandler(t, stub, staticCreds)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("X-Api-Key", alice)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Hello from Bedrock") || !strings.Contains(w.Body.String(), "event: error") {
		t.Fatalf("status %d %s: want the output, then the error event", w.Code, w.Body)
	}
	rows := rowsOf(t, stub.rows.snapshot())
	if len(rows) != 1 || rows[0]["outcome"] != "failed" || rows[0]["error_code"] != "upstream.stream_failed" || rows[0]["error_origin"] != "provider" {
		t.Fatalf("rows %v: want the call failed, upstream.stream_failed of the provider", rows)
	}
}

// streamHandler serves "claude" in d from one provider, its rows written
// to the buffer returned.
func streamHandler(t *testing.T, d Dialect, p http.RoundTripper) (*Handler, *bytes.Buffer) {
	t.Helper()
	rt := &route.Route{Alias: "claude", Dialect: d.Name(), MaxAttempts: 1, Agent: true,
		Pool: []*route.Target{{Backend: "b", Deployment: "1", Model: "m", URL: upstreamURL, Transport: p}}}
	var rows bytes.Buffer
	tel, err := telemetry.New(telemetry.Stdout(&rows), "local", "test")
	if err != nil {
		t.Fatal(err)
	}
	snap := &gsnap.Compiled{Routes: route.NewTable(rt), Prices: map[string]libsnapshot.Price{"m": {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}}
	return &Handler{Dialect: d, MaxBody: 1 << 20, Snapshots: gsnap.Static(snap), Telemetry: tel}, &rows
}

// bedrockException is a Bedrock stream's exception message, of type typ.
func bedrockException(typ, msg string) eventstream.Message {
	var m eventstream.Message
	m.Headers.Set(":message-type", eventstream.StringValue("exception"))
	m.Headers.Set(":exception-type", eventstream.StringValue(typ))
	m.Payload = []byte(`{"message":"` + msg + `"}`)
	return m
}

// bedrockChunk is a Bedrock stream's chunk carrying ev.
func bedrockChunk(ev string) eventstream.Message {
	var m eventstream.Message
	m.Headers.Set(":message-type", eventstream.StringValue("event"))
	m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
	m.Payload = []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(ev)) + `"}`)
	return m
}
