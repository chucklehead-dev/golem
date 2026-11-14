package snapshot

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// checked is a snapshot whose one scope has the declared-worst-case check.
func checked(t *testing.T) []byte {
	t.Helper()
	s := &Snapshot{Schema: Schema, PublishSeq: 7, IssuedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC),
		Entitlements: Entitlements{Scopes: []Scope{{ID: "team-a", Models: []string{"claude"}, BudgetMicros: 1000,
			DeclaredWorstCaseCheck: true, PrincipalInFlight: 2, ScopeInFlight: 8}}}}
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// schema1Reader is the decoder of schema 1's gateways, frozen: lenient
// json.Unmarshal, then the exact schema. It stands for an old gateway
// reading what a new publisher writes.
func schema1Reader(b []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Schema != 1 {
		return nil, &SchemaError{Got: s.Schema}
	}
	return &s, nil
}

func TestRoundTrip(t *testing.T) {
	s, err := Decode(checked(t))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Entitlements.Scopes[0].DeclaredWorstCaseCheck {
		t.Fatal("the check did not survive encoding")
	}
}

// TestRenameFailsClosed: the rename of strict to declaredWorstCaseCheck
// (ADR 0044) is never read as the check turned off, in either direction of
// a rolling upgrade, nor by a member this reader does not know (golem#6).
func TestRenameFailsClosed(t *testing.T) {
	cur := string(checked(t))
	schema1 := strings.Replace(strings.Replace(cur, `"schema":2`, `"schema":1`, 1), `"declaredWorstCaseCheck":true`, `"strict":true`, 1)
	for _, tc := range []struct {
		name, body, want string
	}{
		{"an old publisher's snapshot (schema 1, strict)", schema1, "snapshot schema 1"},
		{"strict under the new schema", strings.Replace(cur, `"declaredWorstCaseCheck":true`, `"strict":true`, 1), `"strict"`},
		{"both names", strings.Replace(cur, `"declaredWorstCaseCheck":true`, `"strict":true,"declaredWorstCaseCheck":true`, 1), `"strict"`},
		{"the name in another case", strings.Replace(cur, `"declaredWorstCaseCheck":true`, `"DeclaredWorstCaseCheck":true`, 1), "DeclaredWorstCaseCheck"},
		{"the name twice, the last off", strings.Replace(cur, `"declaredWorstCaseCheck":true`, `"declaredWorstCaseCheck":true,"declaredWorstCaseCheck":false`, 1), "duplicate"},
		{"an unknown top-level member", strings.Replace(cur, `"publishSeq"`, `"future":1,"publishSeq"`, 1), `"future"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.body)) || tc.body == cur {
				t.Fatalf("fixture not built: %s", tc.body)
			}
			s, err := Decode([]byte(tc.body))
			if err == nil {
				t.Fatalf("accepted, check %v: a snapshot this reader cannot interpret must be refused", s.Entitlements.Scopes[0].DeclaredWorstCaseCheck)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q, want it to name %s", err, tc.want)
			}
		})
	}
	t.Run("a new publisher's snapshot, read by a schema-1 gateway", func(t *testing.T) {
		_, err := schema1Reader([]byte(cur))
		if se, ok := errors.AsType[*SchemaError](err); !ok || se.Got != Schema {
			t.Fatalf("schema-1 reader: %v; it must refuse schema %d, not read the check as off", err, Schema)
		}
	})
}
