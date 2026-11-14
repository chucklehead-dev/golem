package snapshot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// checked is a snapshot whose one scope has the declared-worst-case check.
func checked(t *testing.T) string {
	t.Helper()
	s := &Snapshot{Schema: Schema, PublishSeq: 7, IssuedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC),
		Entitlements: Entitlements{Scopes: []Scope{{ID: "team-a", Models: []string{"claude"}, BudgetMicros: 1000,
			DeclaredWorstCaseCheck: true, PrincipalInFlight: 2, ScopeInFlight: 8}}}}
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRoundTrip(t *testing.T) {
	s, err := Decode([]byte(checked(t)))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Entitlements.Scopes[0].DeclaredWorstCaseCheck {
		t.Fatal("the check did not survive encoding")
	}
}

// TestDecodeFailsClosed: a snapshot is policy, and one the reader cannot
// interpret is refused, never read with the check off.
func TestDecodeFailsClosed(t *testing.T) {
	cur := checked(t)
	on := `"declaredWorstCaseCheck":true`
	for _, tc := range []struct{ name, body, want string }{
		{"a member it does not know", strings.Replace(cur, on, `"strict":true`, 1), `"strict"`},
		{"the name in another case", strings.Replace(cur, on, `"DeclaredWorstCaseCheck":true`, 1), "DeclaredWorstCaseCheck"},
		{"the name twice, the last off", strings.Replace(cur, on, on+`,"declaredWorstCaseCheck":false`, 1), "duplicate"},
		{"another schema", strings.Replace(cur, `"schema":1`, `"schema":9`, 1), "snapshot schema 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.body)) || tc.body == cur {
				t.Fatalf("fixture not built: %s", tc.body)
			}
			if _, err := Decode([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want a refusal naming %s", err, tc.want)
			}
		})
	}
}
