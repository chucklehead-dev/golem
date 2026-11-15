package metrics

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/chucklehead-dev/golem/control/controller"
	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const config = `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, secretRef: {name: anthropic, key: api-key}}
  deployments:
  - {name: sonnet, url: "https://api.anthropic.com/v1/messages", model: claude-sonnet-4-5}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: claude}
spec: {alias: claude, pool: [{backend: anthropic, deployment: sonnet}]}
`

type ledger map[string]int64

func (l ledger) Spend(context.Context, string, time.Time) (map[string]int64, error) { return l, nil }

// rig is one golem-control replica over a directory bucket, its clock
// set by the test, and its metrics read by a manual reader.
type rig struct {
	t      *testing.T
	clock  time.Time
	c      *controller.Controller
	bucket *bucket.Dir
	reader *sdkmetric.ManualReader
}

func newRig(t *testing.T) *rig {
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sk, _ := ed25519.GenerateKey(nil)
	r := &rig{t: t, clock: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), bucket: b, reader: sdkmetric.NewManualReader()}
	now := func() time.Time { return r.clock }
	st := &state.Store{Bucket: b}
	r.c = &controller.Controller{Dir: dir, Lease: 4 * time.Hour, Now: now,
		Elector:    &lead.Elector{State: st, ID: "r1", Duration: 15 * time.Second, Now: now},
		Publishers: map[string]*publish.Publisher{"internal": {State: st, Bucket: b, Signer: sign.Ed25519{ID: "k", Key: sk}, Pool: "internal", Now: now}},
		Spend:      &spend.Job{Ledger: ledger{"team-a": 1}, Now: now}}
	if err := Register(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r.reader)), r.c); err != nil {
		t.Fatal(err)
	}
	return r
}

// lead renews (or takes) the lease at the rig's clock.
func (r *rig) lead() {
	if _, err := r.c.Elector.Try(r.t.Context()); err != nil {
		r.t.Fatal(err)
	}
}

// collect is every data point reported, by instrument name.
func (r *rig) collect() map[string][]point {
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(r.t.Context(), &rm); err != nil {
		r.t.Fatal(err)
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, float64(p.Value)})
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, p.Value})
				}
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, float64(p.Value)})
				}
			default:
				r.t.Fatalf("%s: unexpected data %T", m.Name, m.Data)
			}
		}
	}
	return out
}

type point struct {
	attrs attribute.Set
	value float64
}

// value is name's point with exactly attrs; it fails the test if there is
// none.
func value(t *testing.T, got map[string][]point, name string, attrs ...attribute.KeyValue) float64 {
	t.Helper()
	want := attribute.NewSet(attrs...)
	for _, p := range got[name] {
		if p.attrs.Equals(&want) {
			return p.value
		}
	}
	t.Fatalf("%s{%v}: not reported; have %v", name, want.Encoded(attribute.DefaultEncoder()), got[name])
	return 0
}

var pool = attribute.String("pool", "internal")

// TestMetrics: a leader's publication, then a tampered checkpoint, as the
// collector sees them.
func TestMetrics(t *testing.T) {
	r := newRig(t)
	got := r.collect()
	if v := value(t, got, Leader); v != 0 {
		t.Fatalf("leading before taking the lease: %v", v)
	}
	if _, ok := got[PublishTime]; ok {
		t.Fatalf("a publication time before any publication: %v", got[PublishTime])
	}

	r.lead()
	if p, err := r.c.Step(t.Context()); !p || err != nil {
		t.Fatalf("published %v, %v", p, err)
	}
	published := r.clock
	got = r.collect()
	for name, want := range map[string]float64{
		PublishTime:       float64(published.Unix()),
		PublishSeq:        1,
		PublishValidUntil: float64(published.Add(4 * time.Hour).Unix()),
	} {
		if v := value(t, got, name, pool); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	if v := value(t, got, Leader); v != 1 {
		t.Errorf("%s = %v, want 1", Leader, v)
	}
	if v := value(t, got, LeaderEpoch); v != 1 {
		t.Errorf("%s = %v, want 1", LeaderEpoch, v)
	}
	for _, reason := range controller.Reasons {
		if v := value(t, got, PublishFailures, pool, attribute.String("reason", reason)); v != 0 {
			t.Errorf("failures{%s} = %v after a clean publication", reason, v)
		}
	}
	// Known spend's boundary: the allowance (five minutes) back, to the minute.
	period := attribute.String("period", "2026-11")
	if v := value(t, got, SpendAge, period); v != 300 {
		t.Errorf("%s = %v, want 300", SpendAge, v)
	}
	if v := value(t, got, SpendStale, period); v != 0 {
		t.Errorf("%s = %v, want 0", SpendStale, v)
	}
	if v := value(t, got, Unpriced); v != 0 {
		t.Errorf("%s = %v", Unpriced, v)
	}
	if v := value(t, got, ConfigRenewing); v != 0 {
		t.Errorf("%s = %v", ConfigRenewing, v)
	}

	t.Run("a tampered checkpoint is counted, and the last publication stands", func(t *testing.T) {
		if _, err := r.bucket.Put(t.Context(), snapshot.CheckpointKey("internal"), []byte("not a checkpoint"), ""); err != nil {
			t.Fatal(err)
		}
		r.clock = r.clock.Add(61 * time.Second) // the renewal is due
		r.lead()
		if p, err := r.c.Step(t.Context()); p || !errors.Is(err, publish.ErrTampered) {
			t.Fatalf("published %v, %v; want ErrTampered", p, err)
		}
		got := r.collect()
		if v := value(t, got, PublishFailures, pool, attribute.String("reason", controller.ReasonTampered)); v != 1 {
			t.Fatalf("failures{tampered} = %v, want 1", v)
		}
		if v := value(t, got, PublishFailures, pool, attribute.String("reason", controller.ReasonOther)); v != 0 {
			t.Fatalf("failures{other} = %v, want 0", v)
		}
		if v := value(t, got, PublishTime, pool); v != float64(published.Unix()) {
			t.Fatalf("%s moved to %v on a failure", PublishTime, v)
		}
		if v := value(t, got, PublishSeq, pool); v != 1 {
			t.Fatalf("%s = %v", PublishSeq, v)
		}
	})

	t.Run("an unpublishable config is reported", func(t *testing.T) {
		// A route over a backend that does not exist: the edit is refused
		// and the last good config renewed (the pool is still tampered).
		broken := "apiVersion: golem.dev/v1alpha1\nkind: ModelRoute\nmetadata: {name: x}\nspec: {alias: x, pool: [{backend: nowhere, deployment: d}]}\n"
		if err := os.WriteFile(filepath.Join(r.c.Dir, "broken.yaml"), []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		r.clock = r.clock.Add(2 * time.Second) // the tampered pool's first retry (at most two seconds on)
		r.lead()
		r.c.Step(t.Context())
		got := r.collect()
		if v := value(t, got, ConfigRenewing); v != 1 {
			t.Fatalf("%s = %v, want 1", ConfigRenewing, v)
		}
		if v := value(t, got, PublishFailures, pool, attribute.String("reason", controller.ReasonConfigRenewing)); v != 1 {
			t.Fatalf("failures{config_renewing} = %v, want 1", v)
		}
	})

	t.Run("not leading", func(t *testing.T) {
		r.clock = r.clock.Add(time.Minute) // past the lease's 15 seconds, unrenewed
		got := r.collect()
		if v := value(t, got, Leader); v != 0 {
			t.Fatalf("%s = %v after the lease lapsed", Leader, v)
		}
		if v := value(t, got, LeaderEpoch); v != 0 {
			t.Fatalf("%s = %v after the lease lapsed", LeaderEpoch, v)
		}
	})
}

// TestOutbox: the outbox's length is the document's, read without a copy.
func TestOutbox(t *testing.T) {
	r := newRig(t)
	if _, err := r.c.Elector.State.Update(t.Context(), func(d *state.Document) error {
		d.Audit = append(d.Audit, state.AuditEvent{At: r.clock, Actor: "root", Action: "a"}, state.AuditEvent{At: r.clock, Actor: "root", Action: "b"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v := value(t, r.collect(), AuditOutbox); v != 2 {
		t.Fatalf("%s = %v, want 2", AuditOutbox, v)
	}
}
