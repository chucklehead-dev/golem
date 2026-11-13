package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// cutAfter is a provider stream that ends, cut, after its first n chunks.
type cutAfter struct {
	chunks [][]byte
	status int
}

func (c cutAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return &http.Response{StatusCode: c.status, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(io.MultiReader(bytes.NewReader(bytes.Join(c.chunks, nil)), errReader{}))}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// priced is a handler over one route with a price, recording the call's
// result and its ledger row.
func priced(t *testing.T, d Dialect, rt http.RoundTripper) (*Handler, *bytes.Buffer, *Result) {
	h := routes(d, upstreamURL, rt)
	c, _ := h.Current()
	c.Prices = map[string]snapshot.Price{"claude-sonnet": {Model: "claude-sonnet", InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000}}
	c.Snapshot.Currency = "USD"
	var rows bytes.Buffer
	tel, _ := telemetry.New(telemetry.Stdout(&rows), "local", "standalone")
	var res Result
	return &Handler{Dialect: d, MaxBody: 1 << 20, Snapshots: h, Telemetry: tel, Record: func(_ *http.Request, r Result) { res = r }}, &rows, &res
}

func rowOf(t *testing.T, rows *bytes.Buffer) map[string]any {
	var row map[string]any
	if err := json.Unmarshal(rows.Bytes(), &row); err != nil {
		t.Fatalf("one row expected: %q", rows)
	}
	return row
}

func serveCut(h *Handler, path, body string) {
	defer func() { recover() }() // the abort of a cut response
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, strings.NewReader(body)))
}

// TestEstimates: a response the provider never finished reporting is
// charged what was produced, counted from what was seen, never zero, and
// its row says which share of its cost is estimated (ADR 0012).
func TestEstimates(t *testing.T) {
	t.Run("an OpenAI stream cut before its usage", func(t *testing.T) {
		chunk := []byte(`data: {"choices":[{"delta":{"content":"twelve bytes"}}],"usage":null}` + "\n\n")
		h, rows, res := priced(t, OpenAIChat, cutAfter{[][]byte{chunk, chunk, chunk}, 200})
		body := `{"model":"claude","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}]}`
		serveCut(h, "/v1/chat/completions", body)
		// Input from the request's size; output each chunk's content at 4
		// bytes a token (12 bytes: 3 each), its framing not counted.
		if want := (canon.Usage{Input: int64(len(body)) / 4, Output: 9}); res.Usage != want || res.Cost == 0 {
			t.Fatalf("usage %+v cost %d, want %+v", res.Usage, res.Cost, want)
		}
		if row := rowOf(t, rows); row["estimated_fraction"] != 1.0 {
			t.Fatalf("estimated_fraction %v: all of it estimated", row["estimated_fraction"])
		}
	})
	t.Run("an OpenAI stream with one large chunk, cut", func(t *testing.T) {
		big := []byte(`data: {"choices":[{"delta":{"content":"` + strings.Repeat("y", 4000) + `"}}],"usage":null}` + "\n\n")
		h, _, res := priced(t, OpenAIChat, cutAfter{[][]byte{big}, 200})
		serveCut(h, "/v1/chat/completions", `{"model":"claude","stream":true,"messages":[]}`)
		if res.Usage.Output != 1000 { // its content, at 4 bytes a token, not one token
			t.Fatalf("usage %+v: a large chunk is charged by its content", res.Usage)
		}
	})
	// Events either side of any size: each is charged its content, so a
	// provider batching output into events of a few hundred bytes is not
	// undercounted (golem#6).
	for _, size := range []int{511, 512, 513} {
		t.Run(fmt.Sprintf("an OpenAI event of %d bytes, cut", size), func(t *testing.T) {
			head, tail := `{"choices":[{"delta":{"content":"`, `"}}],"usage":null}`
			content := strings.Repeat("z", size-len(head)-len(tail))
			data := head + content + tail
			if len(data) != size {
				t.Fatalf("event of %d bytes", len(data))
			}
			h, _, res := priced(t, OpenAIChat, cutAfter{[][]byte{[]byte("data: " + data + "\n\n")}, 200})
			serveCut(h, "/v1/chat/completions", `{"model":"claude","stream":true,"messages":[]}`)
			if want := int64(len(content)+3) / 4; res.Usage.Output != want || want < 100 {
				t.Fatalf("output %d, want %d: the event's content, not a token", res.Usage.Output, want)
			}
		})
	}
	// Valid JSON in any layout is charged alike: spaced around its colons,
	// its member names escaped, or both.
	for name, data := range map[string]string{
		"spaced":  `{"choices" : [ {"delta" : {"content" : "%s"} } ], "usage" : null}`,
		"escaped": `{"choices":[{"delta":{"cont\u0065nt":"%s"}}],"usage":null}`,
		"mixed":   "{\"choices\":[{\"delta\":{ \t\"\\u0063ontent\"\t: \"%s\"}}],\"usage\":null}", // an SSE data line holds no newline
	} {
		t.Run("an OpenAI event "+name+", cut", func(t *testing.T) {
			ev := []byte("data: " + fmt.Sprintf(data, strings.Repeat("w", 4000)) + "\n\n")
			h, _, res := priced(t, OpenAIChat, cutAfter{[][]byte{ev}, 200})
			serveCut(h, "/v1/chat/completions", `{"model":"claude","stream":true,"messages":[]}`)
			if res.Usage.Output != 1000 {
				t.Fatalf("output %d, want 1000: the event's content, however laid out", res.Usage.Output)
			}
		})
	}
	t.Run("an Anthropic stream cut before its message_delta", func(t *testing.T) {
		ev := anthropicStream(5)
		h, rows, res := priced(t, AnthropicMessages, cutAfter{ev[:7], 200}) // message_start, block start, 5 deltas
		serveCut(h, "/v1/messages", string(smallRequest))
		// Input is the provider's; output: 5 deltas of 21 bytes, 27 tokens.
		if res.Usage.Input != 2048 || res.Usage.CacheRead != 40000 || res.Usage.Output != 27 {
			t.Fatalf("usage %+v: input reported, output estimated", res.Usage)
		}
		if f := rowOf(t, rows)["estimated_fraction"].(float64); f <= 0 || f >= 1 {
			t.Fatalf("estimated_fraction %v: the output's share only", f)
		}
	})
	t.Run("a whole stream", func(t *testing.T) {
		h, rows, res := priced(t, AnthropicMessages, &stub{events: anthropicStream(3)})
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest)))
		if res.Usage.Output != 12 || rowOf(t, rows)["estimated_fraction"] != 0.0 {
			t.Fatalf("usage %+v: the provider's report, not estimated", res.Usage)
		}
	})
	t.Run("a provider error", func(t *testing.T) {
		h, rows, res := priced(t, OpenAIChat, cutAfter{[][]byte{[]byte(`{"error":{"message":"overloaded"}}`)}, 529})
		serveCut(h, "/v1/chat/completions", `{"model":"claude","stream":true,"messages":[]}`)
		if res.Cost != 0 || rowOf(t, rows)["estimated_fraction"] != 0.0 {
			t.Fatalf("cost %d: a provider bills no error response", res.Cost)
		}
	})
}
