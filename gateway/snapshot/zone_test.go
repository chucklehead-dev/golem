package snapshot

import (
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestPeriodZone: a gateway refuses a snapshot whose budget periods are in
// another zone than its own -period-zone, naming both; one stating no zone
// (an older publisher) applies.
func TestPeriodZone(t *testing.T) {
	for name, tc := range map[string]struct {
		published, gateway string
		want               string // "" applies, else a part of the refusal
	}{
		"the same zone":           {"Europe/London", "Europe/London", ""},
		"another zone":            {"Europe/London", "UTC", "Europe/London (golem-control's -period-zone), this gateway's -period-zone is UTC"},
		"none stated":             {"", "UTC", ""},
		"a gateway checking none": {"Europe/London", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPublished(t)
			gw := p.gateway(t)
			gw.PeriodZone = tc.gateway
			if _, err := p.pub.Publish(t.Context(), publish.Lease{Holder: "r1", Epoch: 1}, 1, func(seq uint64) (*snapshot.Snapshot, error) {
				return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, PeriodZone: tc.published, ValidUntil: time.Now().Add(time.Hour).UTC()}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if tc.want != "" {
				mustRefuse(t, gw, tc.want)
				return
			}
			if err := gw.Poll(t.Context()); err != nil || applied(gw) != 1 {
				t.Fatalf("poll %v, applied %d: want it applied", err, applied(gw))
			}
		})
	}
}
