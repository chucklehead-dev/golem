package jsonscan

import (
	"bytes"
	"encoding/json/jsontext"
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

// TestObjectsReuse: a frame reused for a later object, in the same walk or
// the next, carries nothing of the one before it (its members, type or
// holder).
func TestObjectsReuse(t *testing.T) {
	for _, v := range []string{
		`[{"x":{"type":"a","file_id":1}},{"y":{"n":2}},{"z":[]}]`,
		`{"a":{"b":{"type":"t","c":1}},"d":{"e":{}}}`,
	} {
		want := referenceObjects(t, []byte(v), nil)
		for range 3 {
			if got := walk(t, []byte(v), nil); got != want {
				t.Fatalf("%s:\n%s\nwant\n%s", v, got, want)
			}
		}
	}
}

// FuzzObjects: Objects reports what a fresh recursive walk of the same
// document reports, object for object, whatever was walked before it.
func FuzzObjects(f *testing.F) {
	for _, s := range []string{
		`[{"t\u0079pe":"a\u005fb","x":{"type":"inner","n":null,"F\u0069le_ID":1},"skip":{"type":"hidden"}},[{"y":[]}],"s",1]`,
		`[{"x":{"type":"a","file_id":1}},{"y":{"n":2}},{"z":[]}]`,
		`{"Type":"a","type":1,"input":{"type":"b"}}`, `{"":{"":""}}`, `[]`, `1`,
	} {
		f.Add([]byte(s))
	}
	skip := func(name []byte) bool { return string(name) == "input" }
	f.Fuzz(func(t *testing.T, v []byte) {
		got := walk(t, v, skip)
		if want := referenceObjects(t, v, skip); got != want {
			t.Fatalf("%q:\n%s\nwant\n%s", v, got, want)
		}
	})
}

// walk is Objects' report of v, one line per object, or its error.
func walk(t *testing.T, v []byte, skip func([]byte) bool) string {
	t.Helper()
	var b strings.Builder
	err := Objects(v, skip, func(o *ObjectInfo) error {
		fmt.Fprintf(&b, "%d %q %q %q\n", o.Depth, o.In, o.Type, o.names)
		return nil
	})
	if err != nil {
		return "error"
	}
	return b.String()
}

// referenceObjects is walk's report from a recursive walk that allocates
// each object's state afresh.
func referenceObjects(t *testing.T, v []byte, skip func([]byte) bool) string {
	t.Helper()
	var b strings.Builder
	dec := jsontext.NewDecoder(bytes.NewReader(v))
	var value func(depth int, in []byte) error
	value = func(depth int, in []byte) error {
		switch dec.PeekKind() {
		case '[':
			if _, err := dec.ReadToken(); err != nil {
				return err
			}
			for dec.PeekKind() != ']' {
				if err := value(depth+1, nil); err != nil {
					return err
				}
			}
			_, err := dec.ReadToken()
			return err
		case '{':
		default:
			_, err := dec.ReadValue()
			return err
		}
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		var names, typ []byte
		for dec.PeekKind() != '}' {
			tok, err := dec.ReadToken()
			if err != nil {
				return err
			}
			name := []byte(tok.String())
			if dec.PeekKind() != 'n' {
				names = append(append(names, name...), 0)
			}
			switch {
			case skip != nil && skip(name):
				err = dec.SkipValue()
			case bytes.EqualFold(name, []byte("type")) && dec.PeekKind() == '"':
				tok, err = dec.ReadToken()
				typ = []byte(tok.String())
			default:
				err = value(depth+1, name)
			}
			if err != nil {
				return err
			}
		}
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		fmt.Fprintf(&b, "%d %q %q %q\n", depth, in, typ, names)
		return nil
	}
	if err := value(0, nil); err != nil {
		return "error"
	}
	return b.String()
}
