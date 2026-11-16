package entra

import (
	"net/http"
	"testing"
)

// BenchmarkVerify is one token's verification on the request path: its
// signature checked against the cached key set and its claims read.
func BenchmarkVerify(b *testing.B) {
	ks, pk := newKeySet(b)
	e := &Verifier{Tenant: "tid", Audiences: []string{"api://golem"}, Client: &http.Client{Transport: ks}}
	if err := e.Refresh(b.Context()); err != nil {
		b.Fatal(err)
	}
	tok := token(b, pk, "k1")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.Verify(tok); err != nil {
			b.Fatal(err)
		}
	}
}
