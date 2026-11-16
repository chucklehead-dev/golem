package proxy

import (
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/chucklehead-dev/golem/gateway/telemetry"
)

// metered is poolHandler's handler with its metrics read by a manual
// reader.
func metered(t *testing.T, maxAttempts int, providers ...http.RoundTripper) (*Handler, *sdkmetric.ManualReader) {
	t.Helper()
	h, rows := poolHandler(t, maxAttempts, providers...)
	reads := sdkmetric.NewManualReader()
	tel, err := telemetry.New(telemetry.Providers{Tracer: tracenoop.NewTracerProvider(),
		Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reads)), Logger: telemetry.Stdout(rows).Logger}, "local", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.Telemetry = tel
	return h, reads
}

// sums are the instruments by's points, by name and then by the values of
// the attributes by names (comma-separated), joined by spaces: a counter's
// sum, a histogram's count.
func sums(t *testing.T, r *sdkmetric.ManualReader, by map[string]string) map[string]map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]int64{}
	add := func(name string, attrs attribute.Set, n int64) {
		var key []string
		for _, k := range strings.Split(by[name], ",") {
			v, _ := attrs.Value(attribute.Key(k))
			key = append(key, v.String())
		}
		if out[name] == nil {
			out[name] = map[string]int64{}
		}
		out[name][strings.Join(key, " ")] += n
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if _, ok := by[m.Name]; !ok {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					add(m.Name, p.Attributes, p.Value)
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					add(m.Name, p.Attributes, int64(p.Count))
				}
			}
		}
	}
	return out
}

// TestFailoverMetrics: a call that failed over counts two attempts, the
// first by how its deployment failed it and the second committed; one
// call, completed on the second deployment; and the committed stream's
// time to first token. A call whose only attempt was uncertain counts it
// so, with the provider as its error's origin, and records no first token.
func TestFailoverMetrics(t *testing.T) {
	by := map[string]string{telemetry.MetricAttempts: "golem.deployment,golem.attempt.result",
		telemetry.MetricCalls: "golem.deployment,golem.outcome,golem.error.origin", telemetry.MetricTTFT: "golem.deployment"}
	for name, tc := range map[string]struct {
		first  func(int32) (*http.Response, error)
		result string
	}{
		"529":                    {func(int32) (*http.Response, error) { return statusOf(529, ""), nil }, telemetry.AttemptRefused},
		"overloaded first event": {func(int32) (*http.Response, error) { return streamOf([][]byte{overloaded}), nil }, telemetry.AttemptRefused},
		"dial failed":            {func(int32) (*http.Response, error) { return nil, dialFailed }, telemetry.AttemptUnreached},
	} {
		t.Run(name, func(t *testing.T) {
			d1 := &counting{fn: tc.first}
			d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(2)), nil }}
			h, reads := metered(t, 2, d1, d2)
			if w := serveOne(h); w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			got := sums(t, reads, by)
			if want := map[string]int64{"b/1 " + tc.result: 1, "b/2 committed": 1}; !maps.Equal(got[telemetry.MetricAttempts], want) {
				t.Errorf("attempts %v, want %v", got[telemetry.MetricAttempts], want)
			}
			if want := map[string]int64{"b/2 completed ": 1}; !maps.Equal(got[telemetry.MetricCalls], want) {
				t.Errorf("calls %v, want %v", got[telemetry.MetricCalls], want)
			}
			if want := map[string]int64{"b/2": 1}; !maps.Equal(got[telemetry.MetricTTFT], want) {
				t.Errorf("ttft %v, want %v: the committed stream's first token", got[telemetry.MetricTTFT], want)
			}
		})
	}
	t.Run("uncertain", func(t *testing.T) {
		d1 := &counting{fn: func(int32) (*http.Response, error) { return nil, errors.New("read: connection reset by peer") }}
		d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
		h, reads := metered(t, 2, d1, d2)
		serveOne(h)
		got := sums(t, reads, by)
		if want := map[string]int64{"b/1 uncertain": 1}; !maps.Equal(got[telemetry.MetricAttempts], want) {
			t.Errorf("attempts %v, want %v", got[telemetry.MetricAttempts], want)
		}
		if want := map[string]int64{"b/1 uncertain provider": 1}; !maps.Equal(got[telemetry.MetricCalls], want) {
			t.Errorf("calls %v, want %v", got[telemetry.MetricCalls], want)
		}
		if n := len(got[telemetry.MetricTTFT]); n != 0 {
			t.Errorf("ttft %v: an uncertain call has no first token", got[telemetry.MetricTTFT])
		}
	})
	t.Run("the last attempt's refusal relayed", func(t *testing.T) {
		d1 := &counting{fn: func(int32) (*http.Response, error) { return statusOf(529, ""), nil }}
		h, reads := metered(t, 1, d1)
		serveOne(h)
		got := sums(t, reads, by)
		if want := map[string]int64{"b/1 refused": 1}; !maps.Equal(got[telemetry.MetricAttempts], want) {
			t.Errorf("attempts %v, want %v", got[telemetry.MetricAttempts], want)
		}
		if want := map[string]int64{"b/1 failed provider": 1}; !maps.Equal(got[telemetry.MetricCalls], want) {
			t.Errorf("calls %v, want %v", got[telemetry.MetricCalls], want)
		}
		if n := len(got[telemetry.MetricTTFT]); n != 0 {
			t.Errorf("ttft %v: an error's headers are no first token", got[telemetry.MetricTTFT])
		}
	})
}
