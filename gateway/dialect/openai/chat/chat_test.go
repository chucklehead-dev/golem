package chat

import (
	"encoding/json"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/sse"
)

func TestRewriteRequestsUsage(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not streamed", `{"model":"a","messages":[]}`, `{"model":"gpt","messages":[]}`},
		{"no stream_options", `{"model":"a","stream":true}`, `{"model":"gpt","stream_options":{"include_usage":true},"stream":true}`},
		{"empty stream_options", `{"model":"a","stream":true,"stream_options":{}}`, `{"model":"gpt","stream":true,"stream_options":{"include_usage":true}}`},
		{"other stream_options", `{"model":"a","stream":true,"stream_options":{"include_obfuscation":false}}`,
			`{"model":"gpt","stream":true,"stream_options":{"include_usage":true,"include_obfuscation":false}}`},
		{"usage refused", `{"model":"a","stream":true,"stream_options":{"include_usage":false}}`,
			`{"model":"gpt","stream":true,"stream_options":{"include_usage":true}}`},
		{"usage already on", `{"model":"a","stream":true,"stream_options":{"include_usage":true}}`,
			`{"model":"gpt","stream":true,"stream_options":{"include_usage":true}}`},
		{"null stream_options", `{"model":"a","stream":true,"stream_options":null}`,
			`{"model":"gpt","stream":true,"stream_options":{"include_usage":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r Request
			if err := r.Scan([]byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			if got := string(r.Rewrite(nil, []byte(tc.body), "gpt")); got != tc.want {
				t.Fatalf("Rewrite = %s\nwant      %s", got, tc.want)
			}
		})
	}
}

func TestUsageOnly(t *testing.T) {
	for data, want := range map[string]bool{
		`{"choices":[],"usage":{"prompt_tokens":1}}`:             true,
		`{"choices":[ ],"usage":{"prompt_tokens":1}}`:            true,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`: false,
		`{"choices":[],"usage":null}`:                            false,
		`{"choices":[{"delta":{"content":"x"}}],"usage":null}`:   false,
		`[DONE]`: false,
	} {
		if got := UsageOnly([]byte(data)); got != want {
			t.Errorf("UsageOnly(%s) = %v, want %v", data, got, want)
		}
	}
}

func TestScanMaxTokens(t *testing.T) {
	var r Request
	if err := r.Scan([]byte(`{"model":"a","max_completion_tokens":50,"max_tokens":10,"store":true,"n":2}`)); err != nil {
		t.Fatal(err)
	}
	if r.MaxTokens != 50 || r.Flags != FlagStore|FlagN {
		t.Fatalf("MaxTokens %d flags %b; want max_completion_tokens (50) to win, store and n flagged", r.MaxTokens, r.Flags)
	}
}

// FuzzRewrite: the rewrite is valid JSON, sets the model and, for a stream,
// include_usage, and leaves every other member alone.
func FuzzRewrite(f *testing.F) {
	f.Add([]byte(`{"model":"a","stream":true,"stream_options":{"x":1},"messages":[]}`), "gpt")
	f.Add([]byte(`{"stream":true,"model":"a"}`), "m")
	f.Add([]byte(`{"model":"a","stream":true,"stream_options":null}`), "m") // once a duplicate member
	f.Fuzz(func(t *testing.T, body []byte, model string) {
		var r Request
		if r.Scan(body) != nil {
			return
		}
		out := r.Rewrite(nil, body, model)
		var before, after map[string]json.RawMessage
		if err := json.Unmarshal(body, &before); err != nil {
			t.Fatalf("scan accepted %q: %v", body, err)
		}
		if err := json.Unmarshal(out, &after); err != nil {
			t.Fatalf("rewrite of %q gave invalid JSON %q: %v", body, out, err)
		}
		var r2 Request
		if err := r2.Scan(out); err != nil {
			t.Fatalf("rewrite of %q gave %q, which does not scan: %v", body, out, err)
		}
		if r.Stream && !r2.usageOn {
			t.Fatalf("rewrite of a stream %q does not request usage: %q", body, out)
		}
		for k, v := range before {
			if k == "model" || k == "stream_options" {
				continue
			}
			if string(after[k]) != string(v) {
				t.Fatalf("rewrite changed %q: %s -> %s", k, v, after[k])
			}
		}
	})
}

const stream = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}],"usage":null}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"the usage is"}}],"usage":null}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":30,"total_tokens":1230,"prompt_tokens_details":{"cached_tokens":1000},"completion_tokens_details":{"reasoning_tokens":12}}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestStreamUsage(t *testing.T) {
	var u StreamUsage
	p := sse.Parser{OnEvent: u.OnEvent}
	p.Write([]byte(stream))
	if want := (canon.Usage{Input: 200, Output: 30, CacheRead: 1000, Reasoning: 12}); u.Usage != want || u.Err != nil {
		t.Fatalf("usage = %+v, %v; want %+v", u.Usage, u.Err, want)
	}
}

func TestMayHaveUsage(t *testing.T) {
	for in, want := range map[string]bool{
		`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`:                       false,
		`{"choices":[],"usage":{"prompt_tokens":1}}`:                                  true,
		`{"usage" : null ,"x":1}`:                                                     false,
		"{\"usage\"\n:\n{}}":                                                          true,
		`{"choices":[{"delta":{"content":"usage"}}],"usage":null}`:                    false,
		`{"choices":[{"delta":{"content":"\"usage\":{}"}}]}`:                          false,
		`{"choices":[{"delta":{"content":"usage"}}],"usage":{"completion_tokens":2}}`: true,
		`{"choices":[]}`: false,
		`{"choices":[],"\u0075sage":{"prompt_tokens":1}}`: true, // escaped: to the scan
	} {
		if got := mayHaveUsage([]byte(in)); got != want {
			t.Errorf("mayHaveUsage(%s) = %v, want %v", in, got, want)
		}
	}
}

// FuzzMayHaveUsage: the pre-check never hides a usage object the scan
// would read.
func FuzzMayHaveUsage(f *testing.F) {
	f.Add(`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	f.Add(`{"usage":null}`)
	f.Fuzz(func(t *testing.T, chunk string) {
		var u StreamUsage
		u.Response([]byte(chunk))
		if u.Err == nil && u.Usage != (canon.Usage{}) && !mayHaveUsage([]byte(chunk)) {
			t.Fatalf("%q has usage %+v, but the pre-check skips it", chunk, u.Usage)
		}
	})
}
