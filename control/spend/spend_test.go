package spend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClickHouse: the query reads golem.spend_by_event FINAL with its values
// bound as parameters, and the rows become each scope's spend.
func TestClickHouse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := io.ReadAll(r.Body)
		switch {
		case r.Header.Get("X-ClickHouse-User") != "golem" || r.Header.Get("X-ClickHouse-Key") != "pw":
			http.Error(w, "Code: 516. Authentication failed", 401)
		case !strings.Contains(string(q), "FROM golem.spend_by_event FINAL") || !strings.Contains(string(q), "{before:DateTime64(3, 'UTC')}"):
			http.Error(w, "unexpected query "+string(q), 400)
		case r.URL.Query().Get("param_period") != "2026-11-01" || r.URL.Query().Get("param_before") != "2026-11-14 09:55:00.000":
			http.Error(w, "parameters "+r.URL.RawQuery, 400)
		default:
			io.WriteString(w, `{"scope_id":"team-a","spend":"1500000"}`+"\n"+`{"scope_id":"team-b","spend":"9"}`+"\n")
		}
	}))
	defer srv.Close()
	c := ClickHouse{URL: srv.URL, User: "golem", Password: "pw"}
	got, err := c.Spend(t.Context(), "2026-11", time.Date(2026, 11, 14, 9, 55, 0, 0, time.UTC))
	if err != nil || got["team-a"] != 1_500_000 || got["team-b"] != 9 || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	c.Password = "wrong"
	if _, err := c.Spend(t.Context(), "2026-11", time.Now()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused query: %v", err)
	}
}

// fakeLedger answers from a map, or fails.
type fakeLedger struct {
	spend  map[string]int64
	fail   error
	asked  []time.Time
	period string
}

func (f *fakeLedger) Spend(_ context.Context, period string, before time.Time) (map[string]int64, error) {
	f.asked, f.period = append(f.asked, before), period
	return f.spend, f.fail
}

// TestJob: the boundary is a whole minute an allowance back; a failure
// keeps the last result for its own period only; before the boundary
// passes a period's start, the period's spend is nothing.
func TestJob(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 30, 0, time.UTC)
	l := &fakeLedger{spend: map[string]int64{"team-a": 7}}
	j := &Job{Ledger: l, Now: func() time.Time { return now }}
	if _, ok := j.Current(); ok {
		t.Fatal("a result before any step")
	}
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	k, _ := j.Current()
	if want := time.Date(2026, 11, 14, 9, 55, 0, 0, time.UTC); !k.Since.Equal(want) || k.Period != "2026-11" || k.Spend["team-a"] != 7 {
		t.Fatalf("known %+v, want since %v", k, want)
	}
	l.fail = errors.New("connection refused")
	if err := j.Step(t.Context()); err == nil {
		t.Fatal("a failed read reported nothing")
	}
	if k2, ok := j.Current(); !ok || k2.Spend["team-a"] != 7 {
		t.Fatal("the last result was not kept through a failure")
	}
	now = time.Date(2026, 12, 1, 0, 3, 0, 0, time.UTC) // a new month, B still in November
	asked := len(l.asked)
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if k, _ := j.Current(); k.Period != "2026-12" || len(k.Spend) != 0 || len(l.asked) != asked {
		t.Fatalf("at a period's start: %+v, ledger asked %d more times", k, len(l.asked)-asked)
	}
	now = time.Date(2026, 12, 1, 0, 20, 0, 0, time.UTC)
	if err := j.Step(t.Context()); err == nil {
		t.Fatal("the ledger still fails")
	}
	if k, ok := j.Current(); ok && k.Period == "2026-12" && k.Spend["team-a"] == 7 {
		t.Fatal("November's spend was kept as December's")
	}
}

// TestJobReportsStaleResult: a result kept through a ledger outage stays
// published (gateways floor known spend at their counter's period total,
// so a stale result never counts less than none would), but once its
// boundary is more than MaxAge old (here 6h - 4h - 10m) a gateway on a
// snapshot carrying it, for its whole lease, reads past its retention:
// Stale and Step's error say so, for an alert.
func TestJobReportsStaleResult(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 30, 0, time.UTC)
	l := &fakeLedger{spend: map[string]int64{"team-a": 7}}
	j := &Job{Ledger: l, Now: func() time.Time { return now }}
	if j.MaxAge() != 6*time.Hour-4*time.Hour-10*time.Minute || j.Validate() != nil {
		t.Fatalf("max age %v, %v by default", j.MaxAge(), j.Validate())
	}
	if j.Stale() {
		t.Fatal("stale with no result")
	}
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if age, ok := j.Age(); !ok || age != 5*time.Minute+30*time.Second || j.Stale() {
		t.Fatalf("age %v, %v, stale %v: want the allowance and the half minute truncated", age, ok, j.Stale())
	}
	l.fail = errors.New("connection refused")
	now = now.Add(time.Hour)
	if err := j.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "the last result is kept)") {
		t.Fatalf("an hour into the outage: %v", err)
	}
	if k, ok := j.Current(); !ok || k.Spend["team-a"] != 7 || j.Stale() {
		t.Fatalf("an hour into the outage: %+v, %v, stale %v", k, ok, j.Stale())
	}
	now = now.Add(time.Hour)
	if err := j.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("two hours into the outage: %v", err)
	}
	if k, ok := j.Current(); !ok || k.Spend["team-a"] != 7 || !j.Stale() {
		t.Fatalf("two hours into the outage: %+v, %v, stale %v: want the result kept, and reported stale", k, ok, j.Stale())
	}
	// The ledger answers: fresh again.
	l.fail = nil
	if err := j.Step(t.Context()); err != nil || j.Stale() {
		t.Fatalf("%v, stale %v after the ledger answered", err, j.Stale())
	}
	// A configuration under which every result is stale.
	j = &Job{Ledger: l, Now: func() time.Time { return now }, Lease: 6 * time.Hour}
	if err := j.Validate(); err == nil || !strings.Contains(err.Error(), "shorten the lease") {
		t.Fatalf("a lease as long as the retention: %v", err)
	}
}

// ingestionLedger is a fakeLedger that also says each site's latest row.
type ingestionLedger struct {
	fakeLedger
	latest map[string]time.Time
	fail   error
}

func (l *ingestionLedger) Latest(context.Context) (map[string]time.Time, error) {
	return l.latest, l.fail
}

// TestJobReportsIngestionLag: each site's lag is now less its latest
// ingested row; a site whose lag exceeds the allowance is behind (the
// boundary would miss its late rows), as is a site seen before and now
// without rows; a failed read keeps the last one, so the lags grow; the
// boundary is not moved for any of it.
func TestJobReportsIngestionLag(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 30, 0, time.UTC)
	l := &ingestionLedger{fakeLedger: fakeLedger{spend: map[string]int64{"team-a": 7}}, latest: map[string]time.Time{
		"eu-1": now.Add(-90 * time.Second), // within the allowance
		"us-1": now.Add(-7 * time.Minute),  // past it
	}}
	j := &Job{Ledger: l, Now: func() time.Time { return now }}
	if lags := j.Lags(); len(lags) != 0 {
		t.Fatalf("lags before a read: %v", lags)
	}
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	lags := j.Lags()
	if eu := lags["eu-1"]; eu.Lag != 90*time.Second || eu.Behind || eu.NoRows {
		t.Errorf("eu-1 %+v: want 90s, not behind", eu)
	}
	if us := lags["us-1"]; us.Lag != 7*time.Minute || !us.Behind {
		t.Errorf("us-1 %+v: want 7m, behind the 5m allowance", us)
	}
	if k, _ := j.Current(); !k.Since.Equal(time.Date(2026, 11, 14, 9, 55, 0, 0, time.UTC)) {
		t.Errorf("the boundary moved to %v for a lagging site", k.Since)
	}
	// us-1's rows are gone (a rebuilt table); eu-1 caught up.
	l.latest = map[string]time.Time{"eu-1": now}
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	lags = j.Lags()
	if us := lags["us-1"]; !us.NoRows || !us.Behind {
		t.Errorf("us-1 %+v: want no rows, behind", us)
	}
	if eu := lags["eu-1"]; eu.Lag != 0 || eu.Behind {
		t.Errorf("eu-1 %+v: want caught up", eu)
	}
	// The ledger stops answering: the last read is kept, and its lag grows.
	l.fail = errors.New("connection refused")
	now = now.Add(6 * time.Minute)
	if err := j.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "ingestion lag") {
		t.Fatalf("a failed read: %v", err)
	}
	if eu := j.Lags()["eu-1"]; eu.Lag != 6*time.Minute || !eu.Behind {
		t.Errorf("eu-1 %+v six minutes into an outage: want behind", eu)
	}
	// A Ledger that is not an Ingestion reports no lag.
	j = &Job{Ledger: &fakeLedger{}, Now: func() time.Time { return now }}
	if err := j.Step(t.Context()); err != nil || len(j.Lags()) != 0 {
		t.Fatalf("%v, lags %v", err, j.Lags())
	}
}
