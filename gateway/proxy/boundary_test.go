package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
)

// TestFeatureModes: restricted mode (the default) forwards approved
// members, betas and versions only; an agent route, or a compatible scope,
// forwards unknown ones (ADR 0037, 0041 §3, §7.1).
func TestFeatureModes(t *testing.T) {
	h := handler(AnthropicMessages, &stub{events: anthropicStream(1)}, nil)
	c, _ := h.Snapshots.Current()
	rt := c.Routes.Lookup([]byte("claude"))
	rt.Betas = map[string]bool{"approved-2025": true}
	known := `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`
	unknown := `{"model":"claude","max_tokens":10,"stream":true,"novel_feature":{},"messages":[]}`
	for _, tc := range []struct {
		name, body, beta, version string
		agent                     bool
		features                  string
		want                      int
	}{
		{"restricted: known members", known, "", "", false, "", 200},
		{"restricted: an unknown member", unknown, "", "", false, "", 403},
		{"restricted: an approved beta", known, "approved-2025", "", false, "", 200},
		{"restricted: another beta", known, "approved-2025, novel-2026", "", false, "", 403},
		{"restricted: an unapproved version", known, "", "2099-01-01", false, "", 403},
		{"restricted: the first version", known, "", "2023-06-01", false, "", 200},
		{"restricted: the 2026-10 version", known, "", "2026-10-21", false, "", 200},
		{"restricted: an approved version, then another", known, "", "2023-06-01|2099-01-01", false, "", 403},
		{"restricted: two approved versions in one value", known, "", "2023-06-01, 2026-10-21", false, "", 200},
		{"an agent route: compatible", unknown, "novel-2026", "", true, "", 200},
		{"a compatible scope", unknown, "novel-2026", "", false, "compatible", 200},
		{"a restricted scope on an agent route", unknown, "", "", true, "restricted", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt.Agent = tc.agent
			p := &auth.Principal{ID: "p", Scope: "s", Limits: auth.Limits{Features: tc.features}}
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(tc.body))
			if tc.beta != "" {
				r.Header.Set("Anthropic-Beta", tc.beta)
			}
			for v := range strings.SplitSeq(tc.version, "|") {
				if v != "" {
					r.Header.Add("Anthropic-Version", v)
				}
			}
			ex := getExchange()
			req, _ := ex.scan(AnthropicMessages, []byte(tc.body))
			got := 200
			if no := h.features(r.Header, p, rt, req, requestBetas(AnthropicMessages, r.Header)); no != nil {
				got = no.code.Status
				if no.code.Name != "authz.feature_forbidden" {
					t.Fatalf("code %s", no.code.Name)
				}
			}
			if got != tc.want {
				t.Fatalf("%d, want %d", got, tc.want)
			}
		})
	}
}

// TestRequestBoundary: a body that does not arrive in time is refused
// before any admission, and a response past MaxResponse is cut (golem#5).
func TestRequestBoundary(t *testing.T) {
	t.Run("a trickling body", func(t *testing.T) {
		h := handler(AnthropicMessages, &stub{events: anthropicStream(1)}, nil)
		h.BodyTimeout = 100 * time.Millisecond
		gw, client := serve(t, h, false)
		pr, pw := io.Pipe()
		go func() { pw.Write([]byte(`{"model":`)) }() // and nothing more
		start := time.Now()
		resp, err := client.Post(gw, "application/json", pr)
		if err == nil {
			resp.Body.Close()
		}
		if err == nil && resp.StatusCode < 400 || time.Since(start) > 5*time.Second {
			t.Fatalf("%v %v after %v: a trickling body must be cut off", resp, err, time.Since(start))
		}
		pw.Close()
	})
	t.Run("a response past the ceiling", func(t *testing.T) {
		big := bytes.Repeat([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"), 2000)
		var res Result
		h := handler(OpenAIChat, roundTrip(func(r *http.Request) (*http.Response, error) {
			r.Body.Close()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(bytes.NewReader(big))}, nil
		}), func(_ *http.Request, r Result) { res = r })
		h.MaxResponse = 10 << 10
		func() {
			defer func() { recover() }() // the abort of a cut response
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"claude","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)))
		}()
		if res.Err == nil || !strings.Contains(res.Err.Error(), "larger than this gateway forwards") {
			t.Fatalf("result %+v: the response is cut past the ceiling", res)
		}
	})
}

// TestClaimedLengthNotTrusted: a body claiming the largest size allowed but
// sending a few bytes gets a buffer of at most preallocMax, not the claim.
func TestClaimedLengthNotTrusted(t *testing.T) {
	h := handler(AnthropicMessages, &stub{}, nil)
	h.MaxBody = 32 << 20
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	r.ContentLength = 32 << 20
	in := make([]byte, 0, 16)
	h.readBody(httptest.NewRecorder(), r, &in) // a test request does not hold the body to its claim
	if cap(in) > preallocMax {
		t.Fatalf("buffer of %d bytes for a claimed %d: want at most %d", cap(in), r.ContentLength, preallocMax)
	}
}
