package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

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
	Usage: canon.Usage{Input: 100, Output: 50, CacheRead: 1000}, Cost: 1234, Currency: "USD", Status: 200, Outcome: "ok"}

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
		attrs[string(a.Key)] = a.Value.String()
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

	// The row is written at once.
	rows := r.ledgerRows(t)
	if len(rows) != 1 || rows[0]["cost_micros"] != 1234.0 || rows[0]["period"] != "2026-11-01" ||
		rows[0]["site"] != "us-east-2" || rows[0]["tokens_cache_read"] != 1000.0 || rows[0]["schema_version"] != 2.0 {
		t.Fatalf("row %v", rows)
	}
}

// TestRefused: a refusal is on its span (code, message, who), counted,
// and logged, a code and scope at most once a second, the next line
// saying how many it left out; it writes no ledger row.
func TestRefused(t *testing.T) {
	r := newRig(t)
	var logs bytes.Buffer
	r.t.refusalLog.log = slog.New(slog.NewJSONHandler(&logs, nil))
	no := Refusal{Code: contract.BudgetPoolExhausted, Message: "the scope's budget is spent", RequestID: "req-1",
		Principal: "alice", Scope: "team-a", Key: "glm_live_abc", Alias: "fast"}
	ctx, span := r.t.Start(t.Context(), "openai.chat")
	r.t.Refused(ctx, no)
	span.End()
	got := map[attribute.Key]attribute.Value{}
	for _, kv := range r.spans.Ended()[0].Attributes() {
		got[kv.Key] = kv.Value
	}
	if got["golem.error.code"].AsString() != "budget.pool_exhausted" || got["golem.principal"].AsString() != "alice" ||
		got["golem.scope"].AsString() != "team-a" || got["golem.request_id"].AsString() != "req-1" ||
		got["golem.error.message"].AsString() != no.Message || got[attrRequestModel].AsString() != "fast" {
		t.Fatalf("span attributes %v", got)
	}
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

	// A storm: one line, then the rest left out until a second has passed.
	for range 5 {
		r.t.Refused(t.Context(), no)
	}
	other := no
	other.Scope = "team-b"
	r.t.Refused(t.Context(), other) // another scope is not hidden by the storm
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"scope":"team-a"`) || !strings.Contains(lines[0], `"code":"budget.pool_exhausted"`) ||
		!strings.Contains(lines[0], `"request_id":"req-1"`) || !strings.Contains(lines[1], `"scope":"team-b"`) {
		t.Fatalf("log lines %q", lines)
	}
	if left, ok := r.t.refusalLog.take("budget.pool_exhausted", "team-a", time.Now().Add(2*time.Second)); !ok || left != 5 {
		t.Fatalf("after a second: logged %v, %d left out; want true, 5", ok, left)
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
	NewLedger(nil, "", "").row(&row, &call)
	for _, kv := range row {
		got = append(got, string(kv.Key))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger row columns differ from spend.sql (in order):\n  row: %v\n  ddl: %v", got, want)
	}
}

// TestRowPeriodIsTheCounters: a row names the period its call was counted
// in, not one worked out again from its time.
func TestRowPeriodIsTheCounters(t *testing.T) {
	r := newRig(t)
	c := call
	c.Period = "2026-10" // e.g. counted in October's counter
	r.t.Ended(t.Context(), c)
	if rows := r.ledgerRows(t); len(rows) != 1 || rows[0]["period"] != "2026-10-01" {
		t.Fatalf("rows %v", rows)
	}
}

// TestAttemptIDs: a call's first attempt is its request; a retry's ID is
// the RFC 9562 name-based UUID (version 5) of its number under the request
// ID, the value Python's uuid.uuid5(request_id, "2") gives, so it fits
// golem.spend's UUID columns and can be derived again from the request.
func TestAttemptIDs(t *testing.T) {
	const req = "019a8f2e-7c1d-7b3a-9e4f-0123456789ab"
	c := Call{RequestID: req}
	for _, n := range []int{0, 1} {
		if c.Attempt = n; c.AttemptID() != req {
			t.Fatalf("attempt %d's ID %q, want the request's", n, c.AttemptID())
		}
	}
	c.Attempt = 2
	if got, want := c.AttemptID(), "d2281828-c339-5118-9712-d9413ddf991f"; got != want {
		t.Fatalf("attempt 2's ID %q, want %q (uuid5(request_id, \"2\"))", got, want)
	}
	seen := map[string]bool{req: true}
	for n := 2; n < 50; n++ {
		id := AttemptUUID(req, n)
		if _, err := uuid.Parse(id); err != nil || seen[id] || id[14] != '5' {
			t.Fatalf("attempt %d's ID %q: want a new version-5 UUID (%v)", n, id, err)
		}
		seen[id] = true
	}
	if AttemptUUID("01a2-req", 2) == AttemptUUID("01a2-req", 3) {
		t.Fatal("a non-UUID request ID's attempts share an ID")
	}
	if _, err := uuid.Parse(AttemptUUID("01a2-req", 2)); err != nil {
		t.Fatal(err)
	}
}

// TestRowListAndMapTypes: funding and units are an OTLP list of maps and a
// map of integers, the shapes golem.spend's Array(Tuple(source, share)) and
// Map(String, UInt64) are read from (deploy/clickhouse/spend.sql's view),
// empty when the call has none; standalone's JSON lines write them as an
// array of objects and an object.
func TestRowListAndMapTypes(t *testing.T) {
	r := newRig(t)
	c := call
	c.Funding = []Funding{{"cc-1", 0.75}, {"cc-2", 0.25}}
	c.Units = map[string]int64{"web_search": 3, "images": 1}
	var row [ledgerColumns]attribute.KeyValue
	r.t.Ledger.row(&row, &c)
	got := map[string]attribute.Value{}
	for _, kv := range row {
		got[string(kv.Key)] = kv.Value
	}
	if f := got["funding"]; f.Type() != attribute.SLICE || len(f.AsSlice()) != 2 || f.AsSlice()[0].Type() != attribute.MAP {
		t.Fatalf("funding %v (%v): want a list of maps", f.String(), f.Type())
	}
	if u := got["units"]; u.Type() != attribute.MAP || len(u.AsMap()) != 2 || u.AsMap()[0].Value.Type() != attribute.INT64 {
		t.Fatalf("units %v (%v): want a map of integers", u.String(), u.Type())
	}
	r.t.Ended(t.Context(), c)
	r.t.Ended(t.Context(), call)
	rows := r.ledgerRows(t)
	f, _ := json.Marshal(rows[0]["funding"])
	u, _ := json.Marshal(rows[0]["units"])
	if string(f) != `[{"share":0.75,"source":"cc-1"},{"share":0.25,"source":"cc-2"}]` || string(u) != `{"images":1,"web_search":3}` {
		t.Fatalf("stdout funding %s, units %s", f, u)
	}
	f, _ = json.Marshal(rows[1]["funding"])
	u, _ = json.Marshal(rows[1]["units"])
	if string(f) != `[]` || string(u) != `{}` {
		t.Fatalf("a call without funding or units: %s, %s; want [] and {}", f, u)
	}
}

// TestRowAttribution: the row splits cache writes by lifetime, and records
// the price version and the provider account, the backend's name without
// one.
func TestRowAttribution(t *testing.T) {
	r := newRig(t)
	c := call
	c.Usage.CacheWrite, c.Usage.CacheWrite1h = 300, 200
	c.PriceVersion, c.ProviderAccount = "default/2026-11", "org-123"
	r.t.Ended(t.Context(), c)
	r.t.Ended(t.Context(), call)
	rows := r.ledgerRows(t)
	if got := rows[0]; got["tokens_cache_write_5m"] != 100.0 || got["tokens_cache_write_1h"] != 200.0 ||
		got["price_version"] != "default/2026-11" || got["provider_account"] != "org-123" {
		t.Fatalf("row %v", got)
	}
	if got := rows[1]; got["provider_account"] != "anthropic" || got["tokens_cache_write_1h"] != 0.0 {
		t.Fatalf("a call without an account: %v; want the backend's name", got)
	}
}
