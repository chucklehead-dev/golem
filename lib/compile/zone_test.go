package compile

import (
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestPeriodZoneStated: a publication states the publisher's period zone,
// a compiled one and a renewed one alike, so a gateway of another zone can
// refuse it.
func TestPeriodZoneStated(t *testing.T) {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	objs := objects(t, map[string]string{"golem.yaml": good + priced})
	o := Options{PublishSeq: 1, Now: at, Lease: 4 * time.Hour, PeriodZone: "Europe/London",
		Entitlements: func([]string) snapshot.Entitlements { return snapshot.Entitlements{} }}
	s, err := Compile(objs, o)
	if err != nil {
		t.Fatal(err)
	}
	if s.PeriodZone != "Europe/London" {
		t.Fatalf("compiled: period zone %q, want Europe/London", s.PeriodZone)
	}
	o.PublishSeq, o.PeriodZone = 2, "UTC"
	r, err := Renew(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.PeriodZone != "UTC" {
		t.Fatalf("renewed: period zone %q, want the renewing publisher's, UTC", r.PeriodZone)
	}
	back, err := snapshot.Decode(mustEncode(t, r))
	if err != nil || back.PeriodZone != "UTC" {
		t.Fatalf("decoded: %v, period zone %q", err, back.PeriodZone)
	}
}

func mustEncode(t *testing.T, s *snapshot.Snapshot) []byte {
	t.Helper()
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
