package compile

import (
	"reflect"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestRenew: a published snapshot renewed is what compiling its config
// again would publish, with the new sequence, lease and entitlements, the
// state's scopes joined with the policies it published; a scope created
// since is published as DefaultScope and reported; a standalone snapshot
// is not renewed.
func TestRenew(t *testing.T) {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	ents := func(scopes ...snapshot.Scope) func([]string) snapshot.Entitlements {
		return func([]string) snapshot.Entitlements { return snapshot.Entitlements{Scopes: scopes} }
	}
	objs := objects(t, map[string]string{"golem.yaml": good + priced + policy("team-a", "{models: [claude], budgetMicros: 9, budgetMode: enforce}")})
	prev, err := Compile(objs, Options{PublishSeq: 7, Now: at, Lease: 4 * time.Hour, Entitlements: ents(snapshot.Scope{ID: "team-a"})})
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	spent := snapshot.Scope{ID: "team-a", Period: "2026-11", KnownSpend: 3, Since: later}
	var reported, again []Problem
	o := Options{PublishSeq: 8, Now: later, Lease: 4 * time.Hour, Entitlements: ents(spent, snapshot.Scope{ID: "team-b"}),
		Unmatched: func(p Problem) { reported = append(reported, p) }}
	got, err := Renew(prev, o)
	if err != nil {
		t.Fatal(err)
	}
	o.Unmatched = func(p Problem) { again = append(again, p) }
	want, err := Compile(objs, o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renewed %+v\nwant (compiled again) %+v", got, want)
	}
	sc := got.Entitlements.Scopes
	if got.PublishSeq != 8 || !got.ValidUntil.Equal(later.Add(4*time.Hour)) || len(sc) != 2 ||
		sc[0].BudgetMode != BudgetEnforce || sc[0].KnownSpend != 3 || !reflect.DeepEqual(sc[1], DefaultScope("team-b")) ||
		len(reported) != 1 || !reflect.DeepEqual(reported, again) {
		t.Fatalf("renewed %+v, reported %v", got, reported)
	}
	alone, err := Compile(objects(t, map[string]string{"golem.yaml": good}), standalone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Renew(alone, Options{PublishSeq: 2, Now: later, Lease: time.Hour}); err == nil {
		t.Fatal("a standalone snapshot renewed")
	}
}
