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

// TestJobDropsStaleResult: a result kept through a ledger outage is
// published only while a gateway on a snapshot carrying it, for the whole
// lease, still retains the counter's minutes from its boundary. Past that
// (here 6h - 4h - 10m) the minutes between the boundary and the retention's
// start would be counted nowhere: the result is dropped, and snapshots
// carry no known spend, so gateways count their counter's whole total.
func TestJobDropsStaleResult(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 30, 0, time.UTC)
	l := &fakeLedger{spend: map[string]int64{"team-a": 7}}
	j := &Job{Ledger: l, Now: func() time.Time { return now }}
	if j.MaxAge() != 6*time.Hour-4*time.Hour-10*time.Minute {
		t.Fatalf("max age %v by default", j.MaxAge())
	}
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if age, ok := j.Age(); !ok || age != 5*time.Minute+30*time.Second {
		t.Fatalf("age %v, %v: want the allowance and the half minute truncated", age, ok)
	}
	l.fail = errors.New("connection refused")
	now = now.Add(time.Hour)
	if err := j.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "the last result is kept") {
		t.Fatalf("an hour into the outage: %v", err)
	}
	if k, ok := j.Current(); !ok || k.Spend["team-a"] != 7 {
		t.Fatal("an hour into the outage the last result is still publishable")
	}
	now = now.Add(time.Hour)
	if err := j.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Fatalf("two hours into the outage: %v", err)
	}
	if k, ok := j.Current(); ok {
		t.Fatalf("a result to %v published at %v: gateways would read no further back than the retention", k.Since, now)
	}
	if _, ok := j.Age(); ok {
		t.Fatal("an age for no result")
	}
	// Current drops it too, without a Step in between.
	l.fail = nil
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, ok := j.Current(); ok {
		t.Fatal("a result two hours old, published")
	}
	// A configuration that would drop every result is an error.
	j = &Job{Ledger: l, Now: func() time.Time { return now }, Lease: 6 * time.Hour}
	if err := j.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "shorten the lease") {
		t.Fatalf("a lease as long as the retention: %v", err)
	}
	if _, ok := j.Current(); ok {
		t.Fatal("a result published under a lease as long as the retention")
	}
}
