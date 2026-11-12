package messages

import (
	"encoding/json"
	"maps"
	"testing"
	"unicode/utf8"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/sse"
)

func TestScan(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       Request
		err        bool
	}{
		{"routing fields", `{"model":"claude","max_tokens":1024,"stream":true,"messages":[]}`,
			Request{Model: []byte("claude"), Stream: true, MaxTokens: 1024, HasMaxTokens: true}, false},
		{"escaped model", `{"model":"claude","messages":[]}`, Request{Model: []byte("claude")}, false},
		{"policy flags and unknown", `{"model":"m","mcp_servers":[],"container":"c","novel":1}`,
			Request{Model: []byte("m"), Flags: FlagMCPServers | FlagContainer, Unknown: 1}, false},
		{"no model", `{"messages":[]}`, Request{}, true},
		{"model not a string", `{"model":1}`, Request{}, true},
		{"stream not a bool", `{"model":"m","stream":"yes"}`, Request{}, true},
		{"negative max_tokens", `{"model":"m","max_tokens":-1}`, Request{}, true},
		{"fractional max_tokens", `{"model":"m","max_tokens":1.5}`, Request{}, true},
		{"duplicate model", `{"model":"a","model":"b"}`, Request{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r Request
			err := r.Scan([]byte(tc.body))
			if (err != nil) != tc.err {
				t.Fatalf("Scan error = %v, want error %v", err, tc.err)
			}
			if err != nil {
				return
			}
			r.ModelSpan = tc.want.ModelSpan
			if string(r.Model) != string(tc.want.Model) || r.Stream != tc.want.Stream || r.MaxTokens != tc.want.MaxTokens ||
				r.HasMaxTokens != tc.want.HasMaxTokens || r.Flags != tc.want.Flags || r.Unknown != tc.want.Unknown {
				t.Fatalf("Scan = %+v, want %+v", r, tc.want)
			}
		})
	}
}

// FuzzRewrite: a rewrite changes the model and nothing else.
func FuzzRewrite(f *testing.F) {
	f.Add([]byte(`{"model":"alias","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`), "claude-sonnet")
	f.Add([]byte(`{"model":"a"}`), `q"uote`)
	f.Fuzz(func(t *testing.T, body []byte, model string) {
		var r Request
		if r.Scan(body) != nil || !utf8.ValidString(model) {
			return // model names come from validated config: valid UTF-8
		}
		out := r.Rewrite(nil, body, model)
		var before, after map[string]any
		if err := json.Unmarshal(body, &before); err != nil {
			t.Fatalf("scan accepted %q: %v", body, err)
		}
		if err := json.Unmarshal(out, &after); err != nil {
			t.Fatalf("rewrite of %q gave invalid JSON %q: %v", body, out, err)
		}
		if model != after["model"] {
			t.Fatalf("rewrite set model %q, want %q", after["model"], model)
		}
		delete(before, "model")
		delete(after, "model")
		if !maps.EqualFunc(before, after, func(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return string(x) == string(y) }) {
			t.Fatalf("rewrite changed more than the model:\n%s\n%s", body, out)
		}
	})
}

// A stream as the API sends it (abridged): usage on message_start, the
// cumulative output on message_delta.
const stream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude","stop_reason":null,"usage":{"input_tokens":25,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, \"usage\": {}"}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

var streamUsage = canon.Usage{Input: 25, Output: 15, CacheRead: 2000, CacheWrite: 100}

func usageOf(chunks ...string) (canon.Usage, error) {
	var u StreamUsage
	p := sse.Parser{Want: u.Want, OnEvent: u.OnEvent}
	for _, c := range chunks {
		p.Write([]byte(c))
	}
	return u.Usage, u.Err
}

func TestStreamUsage(t *testing.T) {
	got, err := usageOf(stream)
	if err != nil || got != streamUsage {
		t.Fatalf("usage = %+v, %v; want %+v", got, err, streamUsage)
	}
}

func TestResponseUsage(t *testing.T) {
	var u StreamUsage
	u.Response([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":7,"cache_read_input_tokens":null}}`))
	if want := (canon.Usage{Input: 3, Output: 7}); u.Usage != want || u.Err != nil {
		t.Fatalf("usage = %+v, %v; want %+v", u.Usage, u.Err, want)
	}
}

// FuzzStreamChunking: usage is the same however the stream is split.
func FuzzStreamChunking(f *testing.F) {
	f.Add(uint16(10), uint16(200))
	f.Fuzz(func(t *testing.T, a, b uint16) {
		i, j := int(a)%len(stream), int(b)%len(stream)
		if i > j {
			i, j = j, i
		}
		got, err := usageOf(stream[:i], stream[i:j], stream[j:])
		if err != nil || got != streamUsage {
			t.Fatalf("split at %d, %d: usage = %+v, %v; want %+v", i, j, got, err, streamUsage)
		}
	})
}
