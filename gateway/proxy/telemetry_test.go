package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestLedgerRows: each call that reached a provider writes one ledger row,
// with the counter number the limiter gave its event (ADR 0042) and its
// outcome; a refusal writes none.
func TestLedgerRows(t *testing.T) {
	alice := key.New(key.Dev)
	s := &stub{events: anthropicStream(1)}
	h, lim := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes: [{name: team-a, models: [claude], budgetMicros: 100000000}]
  keys: [{key: `+alice+`, principal: alice, scope: team-a}]
`, s)
	var rows bytes.Buffer
	tel, err := telemetry.New(telemetry.Providers{Tracer: tracenoop.NewTracerProvider(), Meter: metricnoop.NewMeterProvider(),
		Logger: telemetry.Stdout(&rows).Logger}, "local", "standalone")
	if err != nil {
		t.Fatal(err)
	}
	h.Telemetry = tel
	lim.OnNumbered = func(_ string, e limit.Event, seq int64) { tel.Numbered(e.ID, seq) }

	stream := `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`
	for range 2 {
		s.events = anthropicStream(1)
		if w := send(h, alice, stream); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if w := send(h, "", stream); w.Code != 401 {
		t.Fatalf("no key: %d", w.Code)
	}
	var got []map[string]any
	for line := range strings.Lines(rows.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	if len(got) != 2 {
		t.Fatalf("%d rows, want one per call that reached the provider: %v", len(got), got)
	}
	for i, r := range got {
		if r["counter_seq"] != float64(i+1) || r["outcome"] != "completed" || r["principal_id"] != "alice" ||
			r["scope_id"] != "team-a" || r["cost_micros"] != 6204000.0 || r["currency"] != "USD" || r["waived"] != false ||
			r["model"] != "claude-sonnet" || r["tokens_input"] != 2048.0 {
			t.Errorf("row %d: %v", i, r)
		}
	}
}

// TestPeriodIsTheStart: the request path gives the limiter the call's
// start, so the counter it is numbered in and its ledger row name the same
// month however late the admission (here a limiter whose clock is a month
// ahead of the request's arrival).
func TestPeriodIsTheStart(t *testing.T) {
	alice := key.New(key.Dev)
	s := &stub{events: anthropicStream(1)}
	h, lim := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes: [{name: team-a, models: [claude]}]
  keys: [{key: `+alice+`, principal: alice, scope: team-a}]
`, s)
	lim.Now = func() time.Time { return time.Now().AddDate(0, 1, 0) }
	var rows bytes.Buffer
	tel, _ := telemetry.New(telemetry.Stdout(&rows), "local", "standalone")
	h.Telemetry = tel
	lim.OnNumbered = func(_ string, e limit.Event, seq int64) { tel.Numbered(e.ID, seq) }
	if w := send(h, alice, `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var row map[string]any
	json.Unmarshal(rows.Bytes(), &row)
	if want := time.Now().UTC().Format("2006-01") + "-01"; row["period"] != want || row["counter_seq"] != 1.0 {
		t.Fatalf("row %v: period %s, numbered in its counter", row, want)
	}
}
