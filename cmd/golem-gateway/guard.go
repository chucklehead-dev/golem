package main

import (
	"mime"
	"net"
	"net/http"

	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// localOnly guards a standalone gateway's API against the developer's own
// browser. Listening on loopback keeps other machines out, not web pages: a
// page can POST to http://localhost (a text/plain body needs no CORS
// preflight), and DNS rebinding gives a hostile name a loopback address.
// Without a LocalIdentity every request is the developer's (an implicit
// principal), spending their provider keys. So a request must:
//   - not come from another origin, by the browser's own headers
//     (http.CrossOriginProtection: Sec-Fetch-Site, else Origin); SDKs and
//     curl send neither;
//   - name a loopback host (rebinding sends the hostile name in Host:
//     loopbackOnly);
//   - have a JSON body, which no page can send cross-origin without a
//     preflight, and the gateway answers none.
func localOnly(d proxy.Dialect, next http.Handler) http.Handler {
	jsonBody := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			proxy.Refuse(w, d, contract.RequestInvalid, "send the request body with Content-Type: application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxy.Refuse(w, d, contract.AuthzOriginForbidden,
			"a web page may not use a standalone gateway: call it from your SDK or tools, not a browser")
	}))
	return cop.Handler(loopbackOnly(d, jsonBody))
}

// loopbackOnly is localOnly's host check alone, also for a GET (no body, so
// no Content-Type to require): a standalone gateway answers only to a
// loopback host name.
func loopbackOnly(d proxy.Dialect, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host // no port
		}
		if !isLoopback(host) {
			proxy.Refuse(w, d, contract.AuthzOriginForbidden,
				"this standalone gateway answers only to a loopback host name (localhost, 127.0.0.1, [::1]), not "+r.Host)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
