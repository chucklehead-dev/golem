package compile

import (
	"reflect"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestRenew: a published snapshot renewed is what compiling its config
// again would publish, with the new sequence, lease and entitlements; an
// unpriced budgeted scope is left without its alias, or refused, as
// Compile does; a standalone snapshot is not renewed.
func TestRenew(t *testing.T) {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	ents := func(scopes ...snapshot.Scope) func([]string) snapshot.Entitlements {
		return func([]string) snapshot.Entitlements { return snapshot.Entitlements{Scopes: scopes} }
	}
	objs := objects(t, map[string]string{"golem.yaml": good})
	prev, err := Compile(objs, Options{PublishSeq: 7, Now: at, Lease: 4 * time.Hour, Entitlements: ents()})
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	budgeted := snapshot.Scope{ID: "team-a", Models: []string{"claude"}, BudgetMicros: 1}
	var reported, again []Problem
	o := Options{PublishSeq: 8, Now: later, Lease: 4 * time.Hour, Entitlements: ents(budgeted, snapshot.Scope{ID: "team-b", Models: []string{"claude"}}),
		Unpriced: func(p Problem) { reported = append(reported, p) }}
	got, err := Renew(prev, o)
	if err != nil {
		t.Fatal(err)
	}
	o.Unpriced = func(p Problem) { again = append(again, p) }
	want, err := Compile(objs, o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renewed %+v\nwant (compiled again) %+v", got, want)
	}
	if got.PublishSeq != 8 || !got.ValidUntil.Equal(later.Add(4*time.Hour)) || len(got.Entitlements.Scopes[0].Models) != 0 ||
		len(reported) != 1 || !reflect.DeepEqual(reported, again) {
		t.Fatalf("renewed %+v, reported %v", got, reported)
	}
	o.Unpriced = nil
	if _, err := Renew(prev, o); err == nil {
		t.Fatal("an unpriced budgeted scope renewed without Unpriced")
	}
	alone, err := Compile(objs, standalone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Renew(alone, Options{PublishSeq: 2, Now: later, Lease: time.Hour}); err == nil {
		t.Fatal("a standalone snapshot renewed")
	}
}
