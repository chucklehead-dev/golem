package telemetry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	libsnap "github.com/chucklehead-dev/golem/lib/snapshot"
)

// collectState is each state instrument's one value, by name.
func collectState(t *testing.T, r *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			var n int
			switch d := m.Data.(type) {
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[m.Name], n = p.Value, n+1
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out[m.Name], n = float64(p.Value), n+1
				}
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out[m.Name], n = float64(p.Value), n+1
				}
			}
			if n > 1 {
				t.Fatalf("%s: %d points, want one (no attributes beyond the resource)", m.Name, n)
			}
		}
	}
	return out
}

// TestStateGauges: a held snapshot's age and lease remaining, read at
// collection by the injected clock; the limiter's outage, waivers and
// held increments; the denylist's entries.
func TestStateGauges(t *testing.T) {
	issued := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	now := issued.Add(90 * time.Second)
	h := snapshot.Static(&snapshot.Compiled{Snapshot: &libsnap.Snapshot{Schema: libsnap.Schema, PublishSeq: 42,
		IssuedAt: issued, ValidUntil: issued.Add(4 * time.Hour)}})
	store := limit.NewMem()
	l := limit.New(store)
	deny := &auth.DenyFile{Path: filepath.Join(t.TempDir(), "deny")}
	if err := os.WriteFile(deny.Path, []byte("principal mallory\nprincipal eve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deny.Load(); err != nil {
		t.Fatal(err)
	}
	r := sdkmetric.NewManualReader()
	if err := ObserveState(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)), State{Holder: h, Limiter: l, Deny: deny,
		Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	got := collectState(t, r)
	for name, want := range map[string]float64{
		MetricSnapshotAge:     90,
		MetricLeaseRemaining:  (4*time.Hour - 90*time.Second).Seconds(),
		MetricSnapshotSeq:     42,
		MetricLimiterDown:     0,
		MetricWaivers:         0,
		MetricDenylistEntries: 2,
	} {
		if v, ok := got[name]; !ok || v != want {
			t.Errorf("%s = %v (reported %v), want %v", name, v, ok, want)
		}
	}
	if _, ok := got[MetricPointerOverdue]; ok {
		t.Error("a pointer's overdue flag reported with no bucket source")
	}

	t.Run("the site store down", func(t *testing.T) {
		store.SetDown(true)
		call := limit.Call{ID: "r1", Principal: "p1", Scope: "s1", Started: now}
		tk, err := l.Admit(t.Context(), call)
		if err != nil || !tk.Waived() {
			t.Fatalf("Admit = %v: want a waiver", err)
		}
		tk.Complete(t.Context(), 1000)
		now = issued.Add(4*time.Hour + time.Minute)
		got := collectState(t, r)
		for name, want := range map[string]float64{
			MetricLimiterDown:    1,
			MetricWaivers:        1,
			MetricLeaseRemaining: -60, // run out a minute ago
		} {
			if v := got[name]; v != want {
				t.Errorf("%s = %v, want %v", name, v, want)
			}
		}
	})
}

// TestStateGaugesBeforeASnapshot: nothing applied, nothing reported of it.
func TestStateGaugesBeforeASnapshot(t *testing.T) {
	r := sdkmetric.NewManualReader()
	if err := ObserveState(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)), State{Holder: &snapshot.Holder{}}); err != nil {
		t.Fatal(err)
	}
	got := collectState(t, r)
	for _, name := range []string{MetricSnapshotAge, MetricLeaseRemaining, MetricSnapshotSeq} {
		if _, ok := got[name]; ok {
			t.Errorf("%s reported before any snapshot", name)
		}
	}
}
