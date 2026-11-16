package snapshot

import (
	"strings"
	"testing"
)

// TestConfigRevision: a snapshot carries its config's commit through an
// encoding, and an older publisher's numeric configRevision (always 0)
// still decodes, as "", so a gateway upgraded before the publisher reads
// it; anything else in its place is refused.
func TestConfigRevision(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	s := &Snapshot{Schema: Schema, PublishSeq: 1, Source: Source{ConfigRevision: commit}}
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(b)
	if err != nil || back.Source.ConfigRevision != commit {
		t.Fatalf("decoded %v: revision %q, want %s", err, back.Source.ConfigRevision, commit)
	}
	old := strings.Replace(string(b), `"configRevision":"`+commit+`"`, `"configRevision":0`, 1)
	if old == string(b) {
		t.Fatalf("no configRevision member in %s", b)
	}
	if back, err := Decode([]byte(old)); err != nil || back.Source.ConfigRevision != "" {
		t.Fatalf("an older publisher's snapshot: %v, revision %q", err, back.Source.ConfigRevision)
	}
	bad := strings.Replace(string(b), `"configRevision":"`+commit+`"`, `"configRevision":{}`, 1)
	if _, err := Decode([]byte(bad)); err == nil {
		t.Fatal("an object as the revision was read")
	}
}
