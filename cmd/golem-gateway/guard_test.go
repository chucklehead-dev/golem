package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/proxy"
)

func TestLocalOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := localOnly(proxy.OpenAIChat, ok)
	for _, tc := range []struct {
		name   string
		host   string
		header map[string]string
		want   int
		code   string
	}{
		{"an SDK", "127.0.0.1:8080", map[string]string{"Content-Type": "application/json"}, http.StatusTeapot, ""},
		{"localhost, charset", "localhost:8080", map[string]string{"Content-Type": "application/json; charset=utf-8"}, http.StatusTeapot, ""},
		{"IPv6", "[::1]:8080", map[string]string{"Content-Type": "application/json"}, http.StatusTeapot, ""},
		{"DNS rebinding", "evil.example:8080", map[string]string{"Content-Type": "application/json"}, 403, "authz.origin_forbidden"},
		{"a loopback-looking name", "127.0.0.1.evil.example", map[string]string{"Content-Type": "application/json"}, 403, "authz.origin_forbidden"},
		{"a page's simple POST", "localhost:8080", map[string]string{"Content-Type": "text/plain"}, 400, "request.invalid"},
		{"a page, by Sec-Fetch-Site", "localhost:8080",
			map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, 403, "authz.origin_forbidden"},
		{"a page, by Origin", "localhost:8080",
			map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}, 403, "authz.origin_forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+tc.host+"/v1/chat/completions", strings.NewReader("{}"))
			r.Host = tc.host
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("%d %s; want %d %s", w.Code, w.Body, tc.want, tc.code)
			}
		})
	}
}
