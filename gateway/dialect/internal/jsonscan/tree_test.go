package jsonscan

import (
	"fmt"
	"strings"
	"testing"
)

// TestObjects: every object at any depth, inner first, with its depth, the
// member holding it and its type unescaped; members named unescaped, nulls
// absent; a skipped member's value unread.
func TestObjects(t *testing.T) {
	v := `[{"t\u0079pe":"a\u005fb","x":{"type":"inner","n":null,"F\u0069le_ID":1},"skip":{"type":"hidden"}},[{"y":[]}],"s",1]`
	var got []string
	err := Objects([]byte(v), func(name []byte) bool { return string(name) == "skip" }, func(o *ObjectInfo) error {
		got = append(got, fmt.Sprintf("%d %q %q n=%v file_id=%v skip=%v", o.Depth, o.In, o.Type, o.Has("n"), o.Has("file_id"), o.Has("skip")))
		return nil
	})
	want := []string{
		`2 "x" "inner" n=false file_id=true skip=false`,
		`1 "" "a_b" n=false file_id=false skip=true`,
		`2 "" "" n=false file_id=false skip=false`,
	}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%v\n%s\nwant\n%s", err, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := Objects([]byte(`[{"a":1,"a":2}]`), nil, func(*ObjectInfo) error { return nil }); err == nil {
		t.Fatal("a duplicate name passed")
	}
}
