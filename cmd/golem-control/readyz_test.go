package main

import (
	"net/http/httptest"
	"testing"

	"github.com/chucklehead-dev/golem/control/controller"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// TestReadyz: golem-control is ready once it leads (the state loaded, the
// fence claimed), not merely once its probes listen.
func TestReadyz(t *testing.T) {
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	c := &controller.Controller{State: &state.Store{Bucket: b}, ID: "r1"}
	probe := func() int {
		w := httptest.NewRecorder()
		readyz(c)(w, httptest.NewRequest("GET", "/readyz", nil))
		return w.Code
	}
	if code := probe(); code != 503 {
		t.Fatalf("readyz %d before the fence is claimed, want 503", code)
	}
	if err := c.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	if code := probe(); code != 200 {
		t.Fatalf("readyz %d once leading, want 200", code)
	}
}
