package bedrock

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"testing"
	"unicode/utf8"
)

// TestRewriteAsReencoded: the splice is the body the full re-encode made,
// byte for byte when the body is compact without escaped names (as every
// SDK sends it), and otherwise the same members in the same order with
// the same values (only whitespace and escapes differ, which no JSON
// reader sees). An error is an error in both.
func TestRewriteAsReencoded(t *testing.T) {
	for _, body := range rewriteSeeds {
		checkRewrite(t, []byte(body), []string{"b", "c"})
	}
}

// FuzzRewriteBody: TestRewriteAsReencoded for any body.
func FuzzRewriteBody(f *testing.F) {
	for _, s := range rewriteSeeds {
		f.Add([]byte(s), "b")
	}
	f.Fuzz(func(t *testing.T, body []byte, beta string) {
		// A header's beta that is not UTF-8 the re-encode dropped, its
		// write failing unseen; the splice sends it with U+FFFD, which
		// Bedrock refuses as no beta it knows. Neither is a valid request.
		if !utf8.ValidString(beta) {
			return
		}
		var betas []string
		if beta != "" {
			betas = []string{beta}
		}
		checkRewrite(t, body, betas)
	})
}

var rewriteSeeds = []string{
	`{"model":"claude","max_tokens":10,"stream":true,"anthropic_beta":["a"],"messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","messages":[]}`,
	`{"messages":[],"anthropic_version":"v","model":"m"}`,
	`{}`, `{"model":"m"}`, `{"stream":false}`, `[1]`, `{"a":1,"a":2}`, `{"anthropic_beta":"x"}`,
	` { "m\u006fdel" : "m" , "max_tokens" : 5 , "messages" : [ {"role" : "user"} ] , "\u0061" : "\u00e9" } `,
	`{"model":"m","stream":true,"x":{"y":[1,2,{"z":null}]}}`,
}

func checkRewrite(t *testing.T, body []byte, betas []string) {
	t.Helper()
	got, gotStream, gotErr := rewrite(body, betas)
	want, wantStream, wantErr := reencoded(body, betas)
	if (gotErr != nil) != (wantErr != nil) {
		t.Fatalf("%q: error %v, the re-encode's %v", body, gotErr, wantErr)
	}
	if gotErr != nil {
		return
	}
	if gotStream != wantStream {
		t.Fatalf("%q: stream %v, the re-encode's %v", body, gotStream, wantStream)
	}
	if bytes.Equal(got, want) {
		return
	}
	if compact(body) && !bytes.Contains(body, []byte(`\`)) {
		t.Fatalf("%q:\n%s\nthe re-encode's\n%s", body, got, want)
	}
	if !sameJSON(got, want) {
		t.Fatalf("%q: not the same JSON:\n%s\nthe re-encode's\n%s", body, got, want)
	}
}

// compact reports whether a valid JSON value has no whitespace between
// its tokens.
func compact(v []byte) bool {
	c := jsontext.Value(bytes.Clone(v))
	return c.Compact() == nil && bytes.Equal(c, v)
}

// sameJSON reports whether a and b are the same tokens in the same order,
// strings compared unescaped.
func sameJSON(a, b []byte) bool {
	da, db := jsontext.NewDecoder(bytes.NewReader(a)), jsontext.NewDecoder(bytes.NewReader(b))
	for {
		ta, ea := da.ReadToken()
		tb, eb := db.ReadToken()
		if ea != nil || eb != nil {
			return errors.Is(ea, eb) || (ea != nil && eb != nil && ea.Error() == eb.Error())
		}
		if ta.Kind() != tb.Kind() || ta.String() != tb.String() {
			return false
		}
	}
}

// reencoded is rewrite as it was: the whole body decoded token by token
// and encoded again, the reference the splice is checked against.
func reencoded(body []byte, headerBetas []string) (out []byte, stream bool, err error) {
	dec := jsontext.NewDecoder(bytes.NewReader(body))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false, errors.New("not a JSON object")
	}
	enc.WriteToken(jsontext.BeginObject)
	hasVersion := false
	var bodyBetas []string
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, false, err
		}
		name := tok.String() // a token is valid only until the next read
		v, err := dec.ReadValue()
		if err != nil {
			return nil, false, err
		}
		switch name {
		case "model":
			continue
		case "stream":
			stream = string(v) == "true"
			continue
		case "anthropic_beta":
			bs, ok := stringList(v)
			if !ok {
				return nil, false, errors.New("anthropic_beta is not a list of strings")
			}
			bodyBetas = bs
			continue
		case "anthropic_version":
			hasVersion = true
		}
		enc.WriteToken(jsontext.String(name))
		enc.WriteValue(v)
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false, err
	}
	if !hasVersion {
		enc.WriteToken(jsontext.String("anthropic_version"))
		enc.WriteToken(jsontext.String(Version))
	}
	if all := append(bodyBetas, headerBetas...); len(all) > 0 {
		enc.WriteToken(jsontext.String("anthropic_beta"))
		enc.WriteToken(jsontext.BeginArray)
		for _, b := range all {
			enc.WriteToken(jsontext.String(b))
		}
		enc.WriteToken(jsontext.EndArray)
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return nil, false, err
	}
	return bytes.TrimSpace(buf.Bytes()), stream, nil
}
