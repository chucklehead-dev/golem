package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

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
	got := splitRows(t, &rows)
	if len(got) != 2 {
		t.Fatalf("%d rows; want one per call that reached the provider", len(got))
	}
	for i, r := range got {
		if r["outcome"] != "completed" || r["principal_id"] != "alice" ||
			r["scope_id"] != "team-a" || r["cost_micros"] != 6204000.0 || r["currency"] != "USD" || r["waived"] != false ||
			r["model"] != "claude-sonnet" || r["tokens_input"] != 2048.0 {
			t.Errorf("row %d: %v", i, r)
		}
	}
	// The counter holds what the rows charge, once each.
	if k := lim.Known(t.Context(), "team-a"); k != 2*6204000 {
		t.Fatalf("known spend %d, want the two rows' 12408000", k)
	}
}

// TestPeriodIsTheStart: the request path gives the limiter the call's
// start, so the counter it is added to and its ledger row name the same
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
	if w := send(h, alice, `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := splitRows(t, &rows)
	if want := time.Now().UTC().Format("2006-01") + "-01"; len(got) != 1 || got[0]["period"] != want {
		t.Fatalf("rows %v: period %s", got, want)
	}
	// The counter of the start's month has the call: known spend this
	// month (the limiter's clock back) is its cost.
	lim.Now = time.Now
	if k := lim.Known(t.Context(), "team-a"); k == 0 || float64(k) != got[0]["cost_micros"] {
		t.Fatalf("known spend this month %d, the row's cost %v: the call is in its start's counter", k, got[0]["cost_micros"])
	}
}

// splitRows reads standalone telemetry's lines: ledger rows.
func splitRows(t *testing.T, b *bytes.Buffer) (rows []map[string]any) {
	t.Helper()
	for line := range strings.Lines(b.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["row_kind"]; !ok {
			t.Fatalf("a line that is not a ledger row: %v", m)
		}
		rows = append(rows, m)
	}
	return rows
}
