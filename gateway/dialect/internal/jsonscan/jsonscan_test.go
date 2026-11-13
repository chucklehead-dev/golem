package jsonscan

import (
	"encoding/json"
	"fmt"
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

func TestStringBytes(t *testing.T) {
	data := []byte(`{"delta":{"type":"text_delta","text":"Hello, \"world\""},"x":{"text":"ab"},"arguments":"{\"a\":1}"}`)
	if got := StringBytes(data, KeysOf("text", "arguments")); got != len(`Hello, \"world\"`)+2+len(`{\"a\":1}`) {
		t.Fatalf("StringBytes = %d", got)
	}
	if StringBytes([]byte(`{"text":"unterminated`), KeysOf("text")) != len("unterminated") {
		t.Fatal("an unterminated string counts to the end")
	}
}

// TestStringBytesFormatting: any valid JSON is measured alike, compact or
// not, its names escaped or not (a body re-encoded by a proxy, or written
// by hand to dodge the estimate).
func TestStringBytesFormatting(t *testing.T) {
	keys := KeysOf("content", "arguments")
	for _, in := range []string{
		`{"content":"many tokens"}`,
		`{"content" : "many tokens"}`,
		"{\n\t\"content\"\r\n:\n \"many tokens\"\n}",
		`{"cont\u0065nt":"many tokens"}`,
		`{"\u0063\u006f\u006e\u0074\u0065\u006e\u0074" :"many tokens"}`,
		`{"x":["content", {"y" :1}], "cont\u0065nt"	:	"many tokens", "content2":"zz"}`,
	} {
		if got := StringBytes([]byte(in), keys); got != len("many tokens") {
			t.Errorf("StringBytes(%s) = %d, want %d", in, got, len("many tokens"))
		}
	}
	// Strings that only look like names are not: a value followed by a
	// colon inside another string, and a name whose value is not a string.
	for in, want := range map[string]int{
		`{"a":"content: x","content":{"text":"y"}}`:                          0,
		`{"content":null,"b":"content","arguments" : "{\"content\":\"q\"}"}`: len(`{\"content\":\"q\"}`),
		`{"cont\u00":"bad escape"}`:                                          0,
	} {
		if got := StringBytes([]byte(in), keys); got != want {
			t.Errorf("StringBytes(%s) = %d, want %d", in, got, want)
		}
	}
}

func TestStringEnd(t *testing.T) {
	for in, want := range map[string]int{`abc"`: 3, `a\"b"`: 4, `a\\"b`: 3, `\\\"x"`: 5, `none`: 4, `"`: 0} {
		if got := stringEnd([]byte(in)); got != want {
			t.Errorf("stringEnd(%q) = %d, want %d", in, got, want)
		}
	}
}

// FuzzStringBytes: for any valid JSON, compact or spaced, its names
// escaped or not, StringBytes is the written length of the named keys'
// values, whatever they hold. The layout byte picks each gap's whitespace
// and which names are written with \u escapes.
func FuzzStringBytes(f *testing.F) {
	f.Add("hello", `say "hi"\n`, "x", uint64(0))
	f.Add(`":"`, `\`, `"`, uint64(0xffffffffffffffff))
	f.Add("content", "arguments", `" : "`, uint64(0x5a5a5a5a))
	f.Fuzz(func(t *testing.T, content, args, other string, layout uint64) {
		bits := layout
		take := func() uint64 { b := bits & 3; bits = bits>>2 | bits<<62; return b }
		ws := func() string { return [...]string{"", " ", "\n\t", " \r\n "}[take()] }
		str := func(s string) string { q, _ := json.Marshal(s); return string(q) }
		name := func(s string) string {
			if take() == 0 {
				return str(s)
			}
			var b strings.Builder
			b.WriteByte('"')
			for i, r := range s {
				if i%2 == int(take()&1) {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					b.WriteRune(r)
				}
			}
			b.WriteByte('"')
			return b.String()
		}
		member := func(k, v string) string { return ws() + name(k) + ws() + ":" + ws() + v + ws() }
		obj := func(members ...string) string { return "{" + strings.Join(members, ",") + "}" }
		arr := func(elems ...string) string { return "[" + ws() + strings.Join(elems, ","+ws()) + ws() + "]" }
		b := []byte(obj(
			member("id", str(other)),
			member("choices", arr(obj(member("delta", obj(
				member("content", str(content)),
				member("tool_calls", arr(obj(member("function", obj(
					member("arguments", str(args)),
					member("name", str(other)))))))))))),
		))
		if !json.Valid(b) {
			t.Fatalf("not JSON: %s", b)
		}
		if got, want := StringBytes(b, KeysOf("content", "arguments")), len(str(content))-2+len(str(args))-2; got != want {
			t.Fatalf("%s: %d, want %d", b, got, want)
		}
	})
}
