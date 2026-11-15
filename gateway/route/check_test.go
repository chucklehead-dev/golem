package route

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckerEjectsAndReadmits: a box whose health check fails is ejected
// with no call sent to it; its check is not repeated before its interval;
// a pass during its ejection does not cut it short, and one after it
// readmits the box. The check carries the backend's credential.
func TestCheckerEjectsAndReadmits(t *testing.T) {
	var up atomic.Bool
	var checks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/v1/chat/completions")
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	h := &Health{Now: clock}
	c := &Checker{Health: h, Now: clock}
	box := &Target{Backend: "vllm", Deployment: "gpu0", URL: u, Header: http.Header{"Authorization": {"Bearer k"}},
		HealthPath: "/v1/models", HealthInterval: 5 * time.Second}
	other := &Target{Backend: "vllm", Deployment: "nocheck", URL: u}
	ts := []*Target{box, other}

	c.Check(t.Context(), ts)
	if !h.Deployment(box).Ejected {
		t.Fatal("a failed check did not eject the box")
	}
	if l, _ := h.Acquire(box, ""); l != nil {
		t.Fatal("an ejected box took a call")
	}
	if h.Deployment(other).Ejected || checks.Load() != 1 {
		t.Fatalf("checks %d: only the box has a check", checks.Load())
	}
	up.Store(true)
	now = now.Add(time.Second)
	c.Check(t.Context(), ts)
	if checks.Load() != 1 {
		t.Fatal("checked again before its interval")
	}
	now = now.Add(5 * time.Second) // the interval is past, the 10 s ejection not
	c.Check(t.Context(), ts)
	if checks.Load() != 2 || !h.Deployment(box).Ejected {
		t.Fatal("a pass cut the ejection short")
	}
	now = now.Add(5 * time.Second)
	c.Check(t.Context(), ts)
	if h.Deployment(box).Ejected {
		t.Fatal("a pass after the ejection did not readmit the box")
	}
	if l, _ := h.Acquire(box, ""); l == nil {
		t.Fatal("the readmitted box took no call")
	}
}
