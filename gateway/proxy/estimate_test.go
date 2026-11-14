package proxy

import (
	"bytes"
	"encoding/json"
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
// charged by rules that err high, never an average and never zero:
// every byte received a token, whatever its fields or framing, at most the
// request's output ceiling; input, if unreported, every request byte. Its
// row says which share of its cost is estimated (ADR 0012; golem#3).
func TestEstimates(t *testing.T) {
	openAI := func(t *testing.T, request string, chunks ...[]byte) (*bytes.Buffer, *Result) {
		h, rows, res := priced(t, OpenAIChat, cutAfter{chunks, 200})
		serveCut(h, "/v1/chat/completions", request)
		return rows, res
	}
	received := func(chunks ...[]byte) int64 { return int64(len(bytes.Join(chunks, nil))) }
	t.Run("an OpenAI stream cut before its usage", func(t *testing.T) {
		chunk := []byte(`data: {"choices":[{"delta":{"content":"twelve bytes"}}],"usage":null}` + "\n\n")
		body := `{"model":"claude","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}]}`
		rows, res := openAI(t, body, chunk, chunk, chunk)
		if want := (canon.Usage{Input: int64(len(body)), Output: received(chunk, chunk, chunk)}); res.Usage != want || res.Cost == 0 {
			t.Fatalf("usage %+v cost %d, want %+v", res.Usage, res.Cost, want)
		}
		if row := rowOf(t, rows); row["estimated_fraction"] != 1.0 {
			t.Fatalf("estimated_fraction %v: all of it estimated", row["estimated_fraction"])
		}
	})
	t.Run("an OpenAI stream cut at the request's output ceiling", func(t *testing.T) {
		chunk := []byte(`data: {"choices":[{"delta":{"content":"` + strings.Repeat("y", 4000) + `"}}],"usage":null}` + "\n\n")
		_, res := openAI(t, `{"model":"claude","stream":true,"max_tokens":50,"n":2,"messages":[]}`, chunk)
		if res.Usage.Output != 100 { // max_tokens × n: the provider produced no more
			t.Fatalf("output %d, want 100: at most the ceiling", res.Usage.Output)
		}
	})
	// Token-dense output, a field golem does not know, a malformed event:
	// none is read, so none is undercounted.
	for name, chunk := range map[string]string{
		"token-dense content": `data: {"choices":[{"delta":{"content":"` + strings.Repeat("\\u4e00", 500) + `"}}]}` + "\n\n",
		"an unknown field":    `data: {"choices":[{"delta":{"content":"","audio":{"data":"` + strings.Repeat("A", 4000) + `"}}}]}` + "\n\n",
		"a malformed event":   `data: {"choices":[{"delta":{"content":"` + strings.Repeat("z", 4000) + "\n\n",
		"one large event":     `data: {"choices" : [{"delta":{"cont\\u0065nt" : "` + strings.Repeat("w", 4000) + `"}}]}` + "\n\n",
	} {
		t.Run("an OpenAI stream, cut, with "+name, func(t *testing.T) {
			_, res := openAI(t, `{"model":"claude","stream":true,"messages":[]}`, []byte(chunk))
			if want := received([]byte(chunk)); res.Usage.Output != want {
				t.Fatalf("output %d, want %d: every byte received", res.Usage.Output, want)
			}
		})
	}
	t.Run("an Anthropic stream cut before its message_delta", func(t *testing.T) {
		ev := anthropicStream(5)[:7] // message_start, block start, 5 deltas
		h, rows, res := priced(t, AnthropicMessages, cutAfter{ev, 200})
		serveCut(h, "/v1/messages", strings.Replace(string(smallRequest), `"max_tokens":1024`, `"max_tokens":8192`, 1))
		// Input is the provider's; output every byte received.
		if res.Usage.Input != 2048 || res.Usage.CacheRead != 40000 || res.Usage.Output != received(ev...) {
			t.Fatalf("usage %+v, want output %d: input reported, output bounded", res.Usage, received(ev...))
		}
		if f := rowOf(t, rows)["estimated_fraction"].(float64); f <= 0 || f >= 1 {
			t.Fatalf("estimated_fraction %v: the output's share only", f)
		}
		h, _, res = priced(t, AnthropicMessages, cutAfter{ev, 200})
		serveCut(h, "/v1/messages", string(smallRequest))
		if res.Usage.Output != 1024 {
			t.Fatalf("output %d, want max_tokens 1024: at most the ceiling", res.Usage.Output)
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
