// Package jsonscan reads the top-level members of a request body without
// decoding it: the routing fields a dialect needs, their byte positions for
// splicing, and nothing else. Every dialect's pass-through scan is built on
// it (gateway.md §3 step 2, ADR 0001).
package jsonscan

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"sync"
)

// ErrNotObject is returned when the body is not a single JSON object.
var ErrNotObject = errors.New("jsonscan: the request body must be one JSON object")

// Span is a value's position in the body: body[Start:End] is its raw JSON.
type Span struct{ Start, End int }

// Visitor is called for each top-level member, with its unescaped name. It
// must consume the value, by Decoder.ReadValue or Decoder.SkipValue. The
// name's bytes are valid only during the call.
type Visitor func(name []byte, d *Decoder) error

// Decoder wraps a jsontext.Decoder over one body, so a visitor can record
// where a value lies.
type Decoder struct {
	dec *jsontext.Decoder
	// buf holds the body being scanned. jsontext reads a *bytes.Buffer in
	// place; from any other reader it copies the body into a buffer of its
	// own, which the pooled decoder then keeps: an 8 MiB request pinned
	// 16 MiB per P.
	buf  bytes.Buffer
	name []byte
}

// ReadValue returns the next value's raw JSON and its span in the body.
func (d *Decoder) ReadValue() (jsontext.Value, Span, error) {
	v, err := d.dec.ReadValue()
	if err != nil {
		return nil, Span{}, err
	}
	end := int(d.dec.InputOffset())
	return v, Span{end - len(v), end}, nil
}

// SkipValue consumes the next value; it is still checked (syntax and
// duplicate names at every depth), only not returned.
func (d *Decoder) SkipValue() error { return d.dec.SkipValue() }

var pool = sync.Pool{New: func() any {
	d := &Decoder{name: make([]byte, 0, 64)}
	d.dec = jsontext.NewDecoder(&d.buf)
	return d
}}

// Object scans body, which must be exactly one JSON object, calling visit
// for each top-level member. The whole document is read: a body with
// duplicate names at any depth, invalid UTF-8, or trailing data is refused,
// because an upstream parser could read such a body differently from golem
// (ADR 0037).
func Object(body []byte, visit Visitor) error {
	d := pool.Get().(*Decoder)
	d.buf = *bytes.NewBuffer(body)
	d.dec.Reset(&d.buf)
	defer func() { // the pooled decoder keeps no view of the body
		d.buf = bytes.Buffer{}
		d.dec.Reset(&d.buf)
		pool.Put(d)
	}()

	if d.dec.PeekKind() != '{' {
		return ErrNotObject
	}
	if _, err := d.dec.ReadToken(); err != nil {
		return err
	}
	for d.dec.PeekKind() != '}' {
		raw, err := d.dec.ReadValue() // the member name, still quoted
		if err != nil {
			return err
		}
		// An escaped name ("model") means the same member to the
		// upstream; compare names unescaped, or a body could route by one
		// model and be served by another.
		d.name, err = jsontext.AppendUnquote(d.name[:0], raw)
		if err != nil {
			return err
		}
		if err := visit(d.name, d); err != nil {
			return err
		}
	}
	if _, err := d.dec.ReadToken(); err != nil { // '}'
		return err
	}
	if _, err := d.dec.ReadToken(); err != io.EOF {
		if err == nil {
			return ErrNotObject
		}
		return err
	}
	return nil
}

// String unquotes a JSON string value, without allocating when it has no
// escapes (the usual case).
func String(dst []byte, v jsontext.Value) ([]byte, bool) {
	if v.Kind() != '"' {
		return dst, false
	}
	if bytes.IndexByte(v, '\\') < 0 {
		return append(dst[:0], v[1:len(v)-1]...), true
	}
	out, err := jsontext.AppendUnquote(dst[:0], v)
	return out, err == nil
}

// Quote appends s as a JSON string to dst.
func Quote(dst []byte, s string) []byte {
	dst, _ = jsontext.AppendQuote(dst, s)
	return dst
}

// Splice returns body with the value at s replaced by the JSON string s,
// appended to dst.
func Splice(dst, body []byte, s Span, value string) []byte {
	dst = append(dst[:0], body[:s.Start]...)
	dst, _ = jsontext.AppendQuote(dst, value)
	return append(dst, body[s.End:]...)
}
