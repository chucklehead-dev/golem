package jsonscan

import (
	"encoding/json"
	"maps"
	"slices"
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

func TestStringBytes(t *testing.T) {
	data := []byte(`{"delta":{"type":"text_delta","text":"Hello, \"world\""},"x":{"text":"ab"},"arguments":"{\"a\":1}"}`)
	if got := StringBytes(data, "text", "arguments"); got != len(`Hello, \"world\"`)+2+len(`{\"a\":1}`) {
		t.Fatalf("StringBytes = %d", got)
	}
	if StringBytes([]byte(`{"text":"unterminated`), "text") != len("unterminated") {
		t.Fatal("an unterminated string counts to the end")
	}
}
