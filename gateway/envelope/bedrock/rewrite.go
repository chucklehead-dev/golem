package bedrock

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"sync"
)

// maxPresize is the largest Content-Length a request's body is read into
// at once; past it the read grows as it goes, so a wrong length cannot
// claim memory before the body arrives.
const maxPresize = 64 << 20

// readBody reads r's body whole, into one buffer of its Content-Length
// when that is known: io.ReadAll grew from 512 bytes, copying an agent's
// 200 KB request a dozen times.
func readBody(r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	if n := r.ContentLength; n > 0 && n <= maxPresize {
		b.Grow(int(n) + bytes.MinRead) // the last read's room, to see EOF without growing
	}
	_, err := b.ReadFrom(r.Body)
	return b.Bytes(), err
}

// topLevel is a decoder reused for rewrite's scan of a body's top-level
// members.
type topLevel struct {
	buf  bytes.Buffer
	dec  *jsontext.Decoder
	name []byte // an escaped name, unquoted
}

var topLevels = sync.Pool{New: func() any {
	s := &topLevel{}
	s.dec = jsontext.NewDecoder(&s.buf)
	return s
}}

// rewrite is a Messages body as Bedrock takes it: model and stream taken
// out (the model is the path's, streaming the action's), anthropic_version
// set, and the header's betas added as anthropic_beta (after the body's
// own, if it has any). It reports whether the request streams.
//
// Only the top level is read: each member is checked and skipped, and the
// ones kept are copied as their bytes, so an agent's history is copied
// once and never decoded into tokens and encoded again. The members keep
// their order; the whitespace between them is dropped, the members added
// go last.
func rewrite(body []byte, headerBetas []string) (out []byte, stream bool, err error) {
	s := topLevels.Get().(*topLevel)
	defer func() {
		// The pool keeps no view of the body.
		s.buf = bytes.Buffer{}
		s.dec.Reset(&s.buf)
		topLevels.Put(s)
	}()
	// The decoder reads a *bytes.Buffer in place, so InputOffset is a
	// position in body.
	s.buf = *bytes.NewBuffer(body)
	s.dec.Reset(&s.buf)
	dec := s.dec
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false, errors.New("not a JSON object")
	}
	out = make([]byte, 0, len(body)+len(`,"anthropic_version":""`)+len(Version)+betasLen(headerBetas))
	out = append(out, '{')
	hasVersion := false
	var bodyBetas []string
	for dec.PeekKind() != '}' {
		rawName, err := dec.ReadValue()
		if err != nil {
			return nil, false, err
		}
		start := int(dec.InputOffset()) - len(rawName)
		if s.name, err = jsontext.AppendUnquote(s.name[:0], rawName); err != nil {
			return nil, false, err
		}
		v, err := dec.ReadValue()
		if err != nil {
			return nil, false, err
		}
		switch string(s.name) {
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
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, body[start:dec.InputOffset()]...)
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false, err
	}
	if !hasVersion {
		out = appendName(out, "anthropic_version")
		out, _ = jsontext.AppendQuote(out, Version)
	}
	if all := append(bodyBetas, headerBetas...); len(all) > 0 {
		out = append(appendName(out, "anthropic_beta"), '[')
		for i, b := range all {
			if i > 0 {
				out = append(out, ',')
			}
			out, _ = jsontext.AppendQuote(out, b)
		}
		out = append(out, ']')
	}
	return append(out, '}'), stream, nil
}

// appendName appends a member's name and colon to out, an object being
// written, after a comma if it has members already.
func appendName(out []byte, name string) []byte {
	if len(out) > 1 {
		out = append(out, ',')
	}
	out, _ = jsontext.AppendQuote(out, name)
	return append(out, ':')
}

// betasLen is about the length of betas as a member of the body.
func betasLen(betas []string) int {
	n := len(`,"anthropic_beta":[]`)
	for _, b := range betas {
		n += len(b) + 3
	}
	return n
}
