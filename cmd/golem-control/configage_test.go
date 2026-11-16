package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/chucklehead-dev/golem/control/controller"
)

// TestConfigAgeMetric: golem.control.config.age is how long ago the
// checkout was last fetched, and absent while that is unknown.
func TestConfigAgeMetric(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	fetched := filepath.Join(t.TempDir(), "FETCH_HEAD")
	c := &controller.Controller{Checkout: &controller.Checkout{Fetched: fetched}, Now: func() time.Time { return now }}
	reader := sdkmetric.NewManualReader()
	if err := registerConfigAge(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), c); err != nil {
		t.Fatal(err)
	}
	age := func() (float64, bool) {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &rm); err != nil {
			t.Fatal(err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if g, ok := m.Data.(metricdata.Gauge[float64]); ok && m.Name == ConfigAge && len(g.DataPoints) == 1 {
					return g.DataPoints[0].Value, true
				}
			}
		}
		return 0, false
	}
	if v, ok := age(); ok {
		t.Fatalf("an age of %v with no fetch recorded", v)
	}
	if err := os.WriteFile(fetched, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fetched, now.Add(-90*time.Second), now.Add(-90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if v, ok := age(); !ok || v != 90 {
		t.Fatalf("age %v %v, want 90 seconds", v, ok)
	}
}
