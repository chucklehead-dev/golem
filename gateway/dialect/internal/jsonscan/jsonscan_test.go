package jsonscan

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

func names(t *testing.T, body string) ([]string, error) {
	t.Helper()
	var got []string
	err := Object([]byte(body), func(name []byte, d *Decoder) error {
		got = append(got, string(name))
		v, s, err := d.ReadValue()
		if err != nil {
			return err
		}
		if string(v) != body[s.Start:s.End] {
			t.Errorf("span %v of %q is %q, the value is %q", s, name, body[s.Start:s.End], v)
		}
		return nil
	})
	return got, err
}

func TestObject(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		ok         bool
	}{
		{"members", `{"model":"a","stream":true,"n":{"x":[1,2]}}`, []string{"model", "stream", "n"}, true},
		{"whitespace", " \n{ \"model\" : \"a\" }\n ", []string{"model"}, true},
		{"empty", `{}`, nil, true},
		{"escaped name is unescaped", `{"model":"a"}`, []string{"model"}, true},
		{"duplicate", `{"model":"a","model":"b"}`, nil, false},
		{"duplicate by escape", `{"model":"a","model":"b"}`, nil, false},
		{"nested duplicate", `{"m":{"x":1,"x":2}}`, nil, false},
		{"trailing object", `{"model":"a"}{"model":"b"}`, nil, false},
		{"trailing garbage", `{"model":"a"} x`, nil, false},
		{"array", `[1]`, nil, false},
		{"empty body", ``, nil, false},
		{"invalid UTF-8", "{\"model\":\"\xff\"}", nil, false},
		{"truncated", `{"model":"a"`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := names(t, tc.body)
			if (err == nil) != tc.ok {
				t.Fatalf("Object(%q) error = %v, want ok=%v", tc.body, err, tc.ok)
			}
			if tc.ok && !slices.Equal(got, tc.want) {
				t.Fatalf("names = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSplice(t *testing.T) {
	body := `{"model":"alias","x":1}`
	var span Span
	Object([]byte(body), func(name []byte, d *Decoder) error {
		_, s, err := d.ReadValue()
		if string(name) == "model" {
			span = s
		}
		return err
	})
	got := string(Splice(nil, []byte(body), span, `claude "4"`))
	if want := `{"model":"claude \"4\"","x":1}`; got != want {
		t.Fatalf("Splice = %s, want %s", got, want)
	}
}

// FuzzObject: whatever Object accepts, a strict decoder accepts too, with
// the same top-level names; golem and the upstream read one document.
func FuzzObject(f *testing.F) {
	for _, s := range []string{`{"model":"a","stream":true}`, `{"model":"a"}`, `{"a":{"b":[1,{"c":null}]}}`, `{"a":1,"a":2}`, `{}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var got []string
		err := Object(body, func(name []byte, d *Decoder) error {
			got = append(got, string(name))
			return d.SkipValue()
		})
		if err != nil {
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("Object accepted %q, encoding/json refuses it: %v", body, err)
		}
		if len(got) != len(m) {
			t.Fatalf("Object saw %q, encoding/json %q (a duplicate got through)", got, slices.Collect(maps.Keys(m)))
		}
		for _, n := range got {
			if _, ok := m[n]; !ok {
				t.Fatalf("Object saw %q, which encoding/json does not", n)
			}
		}
	})
}

// TestRequestDepth: a request body nested MaxDepth deep is scanned, one
// level more is refused, and brackets inside strings (escaped quotes too)
// are not nesting.
func TestRequestDepth(t *testing.T) {
	nest := func(n int) []byte {
		return []byte(`{"model":"m","x":` + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + `}`)
	}
	skip := func(_ []byte, d *Decoder) error { return d.SkipValue() }
	if err := Request(nest(MaxDepth), skip); err != nil {
		t.Fatalf("at MaxDepth: %v", err)
	}
	if err := Request(nest(MaxDepth+1), skip); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("past MaxDepth: %v", err)
	}
	inString := []byte(`{"model":"m","x":"` + strings.Repeat(`[{\"`, 500) + `"}`)
	if err := Request(inString, skip); err != nil {
		t.Fatalf("brackets in a string: %v", err)
	}
	// A string ending in an escaped backslash ends there: what follows nests.
	after := []byte(`{"model":"m\\\\","x":` + strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth) + `}`)
	if err := Request(after, skip); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("nesting after an escaped backslash: %v", err)
	}
}

func BenchmarkRequestDepth(b *testing.B) {
	body := []byte(`{"model":"m","messages":[` + strings.Repeat(`{"role":"user","content":[{"type":"text","text":"hello [world] {x} \"quoted\""}]},`, 2000) + `{}]}`)
	b.SetBytes(int64(len(body)))
	b.Run("depth", func(b *testing.B) {
		for b.Loop() {
			shallow(body, MaxDepth)
		}
	})
	b.Run("object", func(b *testing.B) {
		for b.Loop() {
			_ = Object(body, func(_ []byte, d *Decoder) error { return d.SkipValue() })
		}
	})
}
