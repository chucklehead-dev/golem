package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	libsnap "github.com/chucklehead-dev/golem/lib/snapshot"
)

// points are each instrument's points by name, then by their attributes
// written "k=v,k=v" (in the set's order, sorted by key): a counter's or a
// gauge's value, a histogram's count of records.
type points map[string]map[string]float64

// collect reads r's instruments: their points, and histograms' sums.
func collect(t *testing.T, r *sdkmetric.ManualReader) (counts, sums points) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	counts, sums = points{}, points{}
	put := func(p points, name string, attrs attribute.Set, v float64) {
		if p[name] == nil {
			p[name] = map[string]float64{}
		}
		var b strings.Builder
		for i, kv := range attrs.ToSlice() {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(string(kv.Key) + "=" + kv.Value.String())
		}
		p[name][b.String()] += v
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					put(counts, m.Name, p.Attributes, float64(p.Value))
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					put(counts, m.Name, p.Attributes, float64(p.Value))
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					put(counts, m.Name, p.Attributes, float64(p.Count))
					put(sums, m.Name, p.Attributes, p.Sum)
				}
			}
		}
	}
	return counts, sums
}

// TestCallMetrics: a call that failed over once counts two attempts by
// their deployments and results, one call by its route, last deployment,
// outcome and error origin, and its time to first token, all by bounded
// attributes only (no principal, scope or request).
func TestCallMetrics(t *testing.T) {
	r := newRig(t)
	first := call
	first.Deployment, first.Attempt, first.AttemptResult = "anthropic/east", 1, AttemptRefused
	first.Outcome, first.ErrorOrigin, first.Status = "failed", "provider", 529
	r.t.Retried(t.Context(), first)
	second := call
	second.Attempt, second.AttemptResult, second.Outcome = 2, AttemptCommitted, "completed"
	second.Committed = second.Started.Add(1500 * time.Millisecond)
	r.t.Ended(t.Context(), second)

	counts, sums := collect(t, r.reads)
	want := map[string]float64{
		"golem.attempt.result=refused,golem.deployment=anthropic/east":       1,
		"golem.attempt.result=committed,golem.deployment=" + call.Deployment: 1,
	}
	if got := counts[MetricAttempts]; !equal(got, want) {
		t.Errorf("%s = %v, want %v", MetricAttempts, got, want)
	}
	if got, want := counts[MetricCalls], map[string]float64{
		"gen_ai.request.model=claude,golem.deployment=" + call.Deployment + ",golem.error.origin=,golem.outcome=completed": 1,
	}; !equal(got, want) {
		t.Errorf("%s = %v, want %v (one per call, not per attempt)", MetricCalls, got, want)
	}
	ttftAttrs := "gen_ai.request.model=claude,gen_ai.response.model=" + call.Model + ",golem.deployment=" + call.Deployment
	if got := counts[MetricTTFT]; len(got) != 1 || got[ttftAttrs] != 1 || sums[MetricTTFT][ttftAttrs] != 1.5 {
		t.Errorf("%s = %v (sums %v), want one record of 1.5 s at %s", MetricTTFT, got, sums[MetricTTFT], ttftAttrs)
	}

	t.Run("no first token", func(t *testing.T) {
		r := newRig(t)
		uncertain := call
		uncertain.AttemptResult, uncertain.Outcome, uncertain.ErrorOrigin, uncertain.Status = AttemptUncertain, "uncertain", "provider", 503
		r.t.Ended(t.Context(), uncertain)
		refusedRelayed := call
		refusedRelayed.AttemptResult, refusedRelayed.Outcome, refusedRelayed.ErrorOrigin, refusedRelayed.Status = AttemptRefused, "failed", "provider", 429
		refusedRelayed.Committed = call.Started.Add(time.Second) // its headers: an error's, no token
		r.t.Ended(t.Context(), refusedRelayed)
		withdrawn := call
		withdrawn.Outcome, withdrawn.ErrorOrigin, withdrawn.Status = "terminated", "policy", 403
		r.t.Ended(t.Context(), withdrawn)
		counts, _ := collect(t, r.reads)
		if got := counts[MetricTTFT]; len(got) != 0 {
			t.Errorf("%s = %v: nothing answered below 400, no first token", MetricTTFT, got)
		}
		if got, want := counts[MetricAttempts], map[string]float64{
			"golem.attempt.result=uncertain,golem.deployment=" + call.Deployment: 1,
			"golem.attempt.result=refused,golem.deployment=" + call.Deployment:   1,
		}; !equal(got, want) {
			t.Errorf("%s = %v, want %v (a withdrawn call made no attempt)", MetricAttempts, got, want)
		}
		if got, want := counts[MetricCalls], map[string]float64{
			"gen_ai.request.model=claude,golem.deployment=" + call.Deployment + ",golem.error.origin=provider,golem.outcome=uncertain": 1,
			"gen_ai.request.model=claude,golem.deployment=" + call.Deployment + ",golem.error.origin=provider,golem.outcome=failed":    1,
			"gen_ai.request.model=claude,golem.deployment=" + call.Deployment + ",golem.error.origin=policy,golem.outcome=terminated":  1,
		}; !equal(got, want) {
			t.Errorf("%s = %v, want %v", MetricCalls, got, want)
		}
	})
}

func equal(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestCallMetricsAllocate: a call's new metrics (its time to first token,
// the call and its attempts) cost no allocation once their attribute sets
// are cached: the lookups' keys stay on the stack.
func TestCallMetricsAllocate(t *testing.T) {
	tel := newRig(t).t
	c := call
	c.AttemptResult, c.Outcome, c.Committed = AttemptCommitted, "completed", call.Started.Add(time.Second)
	ctx := t.Context()
	record := func() {
		rs := tel.routeSets(&c)
		tel.ttft.Record(ctx, c.Committed.Sub(c.Started).Seconds(), rs.ttft...)
		tel.calls.Add(ctx, 1, tel.endSets(&c)...)
		tel.attempted(ctx, rs, &c)
	}
	record()
	if n := testing.AllocsPerRun(100, record); n > 0 {
		t.Fatalf("%v allocations a call: want none from its metrics", n)
	}
}

// TestDeploymentGauges: each of the applied snapshot's deployments' attempts
// in flight and ejection, read from the gateway's Health at collection.
func TestDeploymentGauges(t *testing.T) {
	east := &route.Target{Backend: "bedrock", Deployment: "east", Model: "m"}
	west := &route.Target{Backend: "bedrock", Deployment: "west", Model: "m"}
	rt := &route.Route{Alias: "claude", MaxAttempts: 2, Pool: []*route.Target{east, west}}
	h := snapshot.Static(&snapshot.Compiled{Snapshot: &libsnap.Snapshot{Schema: libsnap.Schema, IssuedAt: time.Now()},
		Routes: route.NewTable(rt)})
	health := &route.Health{EjectAfter: 1, Ejection: time.Hour}
	r := sdkmetric.NewManualReader()
	if err := ObserveState(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)), State{Holder: h, Health: health}); err != nil {
		t.Fatal(err)
	}

	failed, _ := health.Acquire(east, "alice")
	failed.Fail(0) // ejects east, EjectAfter 1
	held, _ := health.Acquire(west, "alice")
	defer held.Release()
	held2, _ := health.Acquire(west, "bob")
	defer held2.Release()

	counts, _ := collect(t, r)
	for name, want := range map[string]map[string]float64{
		MetricInFlight: {"golem.deployment=bedrock/east": 0, "golem.deployment=bedrock/west": 2},
		MetricEjected:  {"golem.deployment=bedrock/east": 1, "golem.deployment=bedrock/west": 0},
	} {
		if got := counts[name]; !equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	t.Run("without Health", func(t *testing.T) {
		r := sdkmetric.NewManualReader()
		if err := ObserveState(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)), State{Holder: h}); err != nil {
			t.Fatal(err)
		}
		counts, _ := collect(t, r)
		if _, ok := counts[MetricInFlight]; ok {
			t.Errorf("%s reported without a Health", MetricInFlight)
		}
	})
}
