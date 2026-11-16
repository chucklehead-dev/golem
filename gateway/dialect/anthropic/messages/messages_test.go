package messages

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/sse"
)

// TestFileReferences: content that refers to a file the provider stores
// is Stateful (M1 refuses it), found decoded at any depth whatever its
// escapes; a file's name in text or in a tool call's input is not one.
func TestFileReferences(t *testing.T) {
	for _, c := range []struct{ name, messages, want string }{
		{"document file source", `[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]`, "messages (a file source)"},
		{"image file source, escaped", `[{"role":"user","content":[{"type":"image","s\u006furce":{"t\u0079pe":"fi\u006ce","file\u005fid":"file_1"}}]}]`, "messages (a file source)"},
		{"in a tool result", `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"file","file_id":"file_1"}}]}]}]`, "messages (a file source)"},
		{"a container upload", `[{"role":"user","content":[{"type":"container_upload","file_id":"file_1"}]}]`, "messages (a file_id)"},
		{"a file_id with another source type", `[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"","file_id":"file_1"}}]}]`, "messages (a file_id)"},
		{"base64 source", `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]`, ""},
		{"text naming a file", `[{"role":"user","content":"{\"source\":{\"type\":\"file\",\"file_id\":\"x\"}}"}]`, ""},
		{"a tool's input", `[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"read","input":{"file_id":"f","source":{"type":"file"}}}]}]`, ""},
	} {
		var r Request
		if err := r.Scan([]byte(`{"model":"m","messages":` + c.messages + `}`)); err != nil || r.Stateful != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, r.Stateful, err, c.want)
		}
	}
}

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
		{"server tool with whitespace", `{"model":"m","tools":[{"type" : "web_search_20250305"}]}`,
			Request{Model: []byte("m"), Flags: FlagServerTools}, false},
		{"server tool with escaped type member", `{"model":"m","tools":[{"t\u0079pe":"code_execution_20250825"}]}`,
			Request{Model: []byte("m"), Flags: FlagServerTools}, false},
		{"client tool", `{"model":"m","tools":[{"type":"custom","name":"lookup"}]}`,
			Request{Model: []byte("m")}, false},
		{"server tool, escaped type value", `{"model":"m","tools":[{"type":"web\u005fsearch_20250305"}]}`,
			Request{Model: []byte("m"), Flags: FlagServerTools}, false},
		{"server tool, member name's case", `{"model":"m","tools":[{"Type":"web_fetch_20250910"}]}`,
			Request{Model: []byte("m"), Flags: FlagServerTools}, false},
		{"tools not an array", `{"model":"m","tools":{"type":"web_search_20250305"}}`, Request{}, true},
		{"a tool not an object", `{"model":"m","tools":["web_search_20250305"]}`, Request{}, true},
		{"no model", `{"messages":[]}`, Request{}, true},
		{"model not a string", `{"model":1}`, Request{}, true},
		{"null stream and max_tokens: absent", `{"model":"m","stream":null,"max_tokens":null}`, Request{Model: []byte("m")}, false},
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

// TestCacheCreationBreakdown: cache_creation's one-hour writes are
// CacheWrite1h, the rest of cache_creation_input_tokens the five-minute
// writes; parts that exceed the total are charged as the parts; a report
// without the breakdown has no one-hour writes; a malformed one is an error.
func TestCacheCreationBreakdown(t *testing.T) {
	for _, c := range []struct {
		usage string
		want  canon.Usage
		err   bool
	}{
		{`{"input_tokens":3,"output_tokens":7,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}`,
			canon.Usage{Input: 3, Output: 7, CacheWrite: 300, CacheWrite1h: 200}, false},
		{`{"output_tokens":7,"cache_creation":{"ephemeral_1h_input_tokens":50,"ephemeral_5m_input_tokens":20},"cache_creation_input_tokens":10}`,
			canon.Usage{Output: 7, CacheWrite: 70, CacheWrite1h: 50}, false},
		{`{"output_tokens":7,"cache_creation_input_tokens":10,"cache_creation":null}`, canon.Usage{Output: 7, CacheWrite: 10}, false},
		{`{"output_tokens":7,"cache_creation":{"ephemeral_1h_input_tokens":-1}}`, canon.Usage{}, true},
	} {
		var u StreamUsage
		u.Response([]byte(`{"content":[],"usage":` + c.usage + `}`))
		if c.err {
			if u.Err == nil || u.Final {
				t.Errorf("%s: Err=%v Final=%v, want a non-final invalid report", c.usage, u.Err, u.Final)
			}
			continue
		}
		if u.Usage != c.want || u.Err != nil || u.Usage.CacheWrite5m() != c.want.CacheWrite-c.want.CacheWrite1h {
			t.Errorf("%s: usage = %+v, %v; want %+v", c.usage, u.Usage, u.Err, c.want)
		}
	}
	// In a stream, message_start's report stands when message_delta's has
	// no breakdown.
	s := strings.Replace(stream, `"cache_creation_input_tokens":100,`, `"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":60},`, 1)
	want := streamUsage
	want.CacheWrite1h = 60
	if got, err := usageOf(s); err != nil || got != want {
		t.Fatalf("stream usage = %+v, %v; want %+v", got, err, want)
	}
}

func TestInvalidUsageDoesNotBecomeFinal(t *testing.T) {
	for _, body := range []string{
		`{"content":[{"type":"text","text":"hello"}],"usage":{}}`,
		`{"content":[],"usage":{"input_tokens":1}}`,
		`{"content":[],"usage":{"input_tokens":1,"output_tokens":-1}}`,
	} {
		var u StreamUsage
		u.Response([]byte(body))
		if u.Final || u.Err == nil {
			t.Errorf("%s: Final=%v Err=%v, want a non-final invalid report", body, u.Final, u.Err)
		}
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
