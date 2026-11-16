package controller

import (
	"strings"
	"testing"
)

// TestPeriodZonePublished: golem-control states its period zone in every
// snapshot; a gateway of the same zone applies it, one of another refuses
// it naming both.
func TestPeriodZonePublished(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	r1.PeriodZone = "Europe/London"
	w.gateway.PeriodZone = "Europe/London"
	w.lead("r1")
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	c, err := w.gateway.Holder.Current()
	if err != nil || c.Snapshot.PeriodZone != "Europe/London" {
		t.Fatalf("the gateway holds %v (%v): want a snapshot stating Europe/London", c, err)
	}

	w.gateway.PeriodZone = "UTC"
	w.write("haiku.yaml", route("haiku"))
	if p, err := r1.Step(t.Context()); !p || err != nil {
		t.Fatalf("second publication: %v %v", p, err)
	}
	err = w.gateway.Poll(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Europe/London") || !strings.Contains(err.Error(), "-period-zone is UTC") {
		t.Fatalf("poll: %v, want a refusal naming both zones", err)
	}
	if seq, _, _ := w.applied(); seq != 1 {
		t.Fatalf("the gateway holds %d after a refusal, want 1", seq)
	}
}
