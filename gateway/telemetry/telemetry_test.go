package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/lib/contract"
)

type rig struct {
	t     *Telemetry
	spans *tracetest.SpanRecorder
	reads *sdkmetric.ManualReader
	rows  *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	spans := tracetest.NewSpanRecorder()
	reads := sdkmetric.NewManualReader()
	var rows bytes.Buffer
	p := Providers{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)),
		Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reads)), Logger: Stdout(&rows).Logger}
	tel, err := New(p, "us-east-2", "internal")
	if err != nil {
		t.Fatal(err)
	}
	return &rig{tel, spans, reads, &rows}
}

func (r *rig) ledgerRows(t *testing.T) []map[string]any {
	var out []map[string]any
	for line := range strings.Lines(r.rows.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

var call = Call{RequestID: "01a2-req", Principal: "alice", PrincipalKind: "user", Key: "glm_live_abcdefg", Scope: "team-a",
	Frontend: "anthropic.messages", Backend: "anthropic.messages", Alias: "claude", Model: "claude-sonnet-4-5",
	Deployment: "anthropic/sonnet", Provider: "anthropic",
	Started: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), Ended: time.Date(2026, 11, 13, 10, 0, 2, 0, time.UTC),
	Usage: canon.Usage{Input: 100, Output: 50, CacheRead: 1000}, Cost: 1234, Currency: "USD", Status: 200, Outcome: "ok", Counted: true}

func TestEnded(t *testing.T) {
	r := newRig(t)
	ctx, span := r.t.Start(t.Context(), "anthropic.messages")
	r.t.Ended(ctx, call)
	span.End()

	// The span: GenAI conventions and golem's attributes.
	got := r.spans.Ended()
	if len(got) != 1 || got[0].Name() != "chat claude" {
		t.Fatalf("spans %v", got)
	}
	attrs := map[string]string{}
	for _, a := range got[0].Attributes() {
		attrs[string(a.Key)] = a.Value.Emit()
	}
	for k, v := range map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic",
		"gen_ai.request.model": "claude", "gen_ai.response.model": "claude-sonnet-4-5",
		"gen_ai.usage.input_tokens": "1100", "gen_ai.usage.output_tokens": "50", "golem.cost_micros": "1234", "golem.scope": "team-a"} {
		if attrs[k] != v {
			t.Errorf("span %s = %q, want %q", k, attrs[k], v)
		}
	}

	// The metrics: token usage by type, and duration.
	var rm metricdata.ResourceMetrics
	r.reads.Collect(context.Background(), &rm)
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	if !names["gen_ai.client.token.usage"] || !names["gen_ai.client.operation.duration"] {
		t.Fatalf("metrics %v", names)
	}

	// The ledger row waits for its counter number, then is written with it.
	if rows := r.ledgerRows(t); len(rows) != 0 {
		t.Fatalf("a counted call's row was written before its number: %v", rows)
	}
	r.t.Ledger.Numbered("team-a", call.RequestID, 42)
	rows := r.ledgerRows(t)
	if len(rows) != 1 || rows[0]["counter_seq"] != 42.0 || rows[0]["cost_micros"] != 1234.0 || rows[0]["period"] != "2026-11-01" ||
		rows[0]["site"] != "us-east-2" || rows[0]["tokens_cache_read"] != 1000.0 {
		t.Fatalf("row %v", rows)
	}
}

func TestUncountedAndOverflow(t *testing.T) {
	r := newRig(t)
	c := call
	c.Counted = false
	r.t.Ended(t.Context(), c)
	if rows := r.ledgerRows(t); len(rows) != 1 || rows[0]["counter_seq"] != 0.0 {
		t.Fatalf("an uncounted call's row is written at once, unnumbered: %v", rows)
	}
	r.rows.Reset()
	r.t.Ledger.MaxWaiting = 1
	for _, id := range []string{"a", "b"} {
		c := call
		c.RequestID = id
		r.t.Ended(t.Context(), c)
	}
	rows := r.ledgerRows(t)
	if len(rows) != 1 || rows[0]["event_id"] != "a" || r.t.Ledger.Unnumbered.Load() != 1 {
		t.Fatalf("past MaxWaiting the oldest row is written unnumbered: %v", rows)
	}
}

func TestRefused(t *testing.T) {
	r := newRig(t)
	ctx, span := r.t.Start(t.Context(), "openai.chat")
	r.t.Refused(ctx, contract.BudgetPoolExhausted)
	span.End()
	var rm metricdata.ResourceMetrics
	r.reads.Collect(context.Background(), &rm)
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found = found || m.Name == "golem.refusals"
		}
	}
	if !found || len(r.ledgerRows(t)) != 0 {
		t.Fatal("a refusal is counted and writes no ledger row")
	}
}

// TestRowMatchesLedgerDDL: the row's attributes are golem.spend's columns
// (deploy/clickhouse/spend.sql), so the collector's mapping is the
// identity.
func TestRowMatchesLedgerDDL(t *testing.T) {
	b, err := os.ReadFile("../../deploy/clickhouse/spend.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := string(b)
	start := strings.Index(ddl, "CREATE TABLE IF NOT EXISTS golem.spend (")
	end := strings.Index(ddl[start:], "\n)")
	var want []string
	for line := range strings.Lines(ddl[start : start+end]) {
		f := strings.Fields(line)
		if len(f) >= 2 && !strings.HasPrefix(f[0], "--") && f[0] != "CREATE" {
			want = append(want, f[0])
		}
	}
	var got []string
	var row [ledgerColumns]attribute.KeyValue
	NewLedger(nil, "", "").row(&row, &call, 1)
	for _, kv := range row {
		got = append(got, string(kv.Key))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger row columns differ from spend.sql (in order):\n  row: %v\n  ddl: %v", got, want)
	}
}

// TestOrderDoesNotGrow: rows numbered promptly leave no trace behind.
func TestOrderDoesNotGrow(t *testing.T) {
	r := newRig(t)
	for i := range 1000 {
		c := call
		c.RequestID = string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		r.t.Ended(t.Context(), c)
		r.t.Ledger.Numbered("team-a", c.RequestID, int64(i+1))
	}
	if n := len(r.t.Ledger.order); n > 1 {
		t.Fatalf("order holds %d entries after every row was numbered", n)
	}
}
