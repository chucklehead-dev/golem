package jsonscan

import (
	"bytes"
	"encoding/json/jsontext"
)

// ObjectInfo is what Objects reports of one object within a value: where
// it lies, its "type" and which members it has. Its slices are valid only
// during the visit.
type ObjectInfo struct {
	// Depth is the number of objects and arrays enclosing it within the
	// value walked: 0 for the value itself, 1 for an element of a
	// top-level array.
	Depth int
	// In is the unescaped name of the member whose value it is; nil for
	// an array's element or the value itself.
	In []byte
	// Type is its "type" member's string (the name compared without
	// regard to case, the string unescaped), or nil when it has none or
	// it is not a string.
	Type []byte

	names []byte // its members' unescaped names whose values are not null, each followed by a 0
}

// Has reports whether the object has a member named name (compared
// without regard to case, as encoding/json would) whose value is not null.
func (o *ObjectInfo) Has(name string) bool {
	for rest := o.names; len(rest) > 0; {
		i := bytes.IndexByte(rest, 0)
		if bytes.EqualFold(rest[:i], []byte(name)) {
			return true
		}
		rest = rest[i+1:]
	}
	return false
}

// HasExactly is Has with name compared exactly: for a member whose
// presence exempts the object from a refusal, which a different case must
// not earn.
func (o *ObjectInfo) HasExactly(name string) bool {
	for rest := o.names; len(rest) > 0; {
		i := bytes.IndexByte(rest, 0)
		if string(rest[:i]) == name {
			return true
		}
		rest = rest[i+1:]
	}
	return false
}

// frame is an object or array being walked.
type frame struct {
	array bool
	info  ObjectInfo
	in    []byte // info.In's storage
	typ   []byte // info.Type's storage
}

// Objects calls visit for every object within v, one JSON value, at any
// depth, once each object's members have all been read (so an inner
// object is visited before the one holding it). Member names are compared
// unescaped: a policy check by member name must not be bypassed by a JSON
// escape. A member whose name skip reports true (nil: none) has its value
// skipped unwalked: arbitrary JSON a policy has no business reading (a
// tool call's input). The value is read once, token by token.
func Objects(v []byte, skip func(name []byte) bool, visit func(o *ObjectInfo) error) error {
	d := decoder(v)
	defer d.release()
	var stack []frame
	push := func(array bool, in []byte) {
		stack = append(stack, frame{array: array})
		f := &stack[len(stack)-1]
		if in != nil {
			f.in = append(f.in, in...)
			f.info.In = f.in
		}
		f.info.Depth = len(stack) - 1
	}
	// begin reads the next value: a container opened and pushed, a scalar
	// read and returned.
	begin := func(in []byte) (jsontext.Value, error) {
		switch d.dec.PeekKind() {
		case '{', '[':
			tok, err := d.dec.ReadToken()
			if err != nil {
				return nil, err
			}
			push(tok.Kind() == '[', in)
			return nil, nil
		}
		return d.dec.ReadValue()
	}
	if _, err := begin(nil); err != nil {
		return err
	}
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		switch d.dec.PeekKind() {
		case '}', ']':
			if _, err := d.dec.ReadToken(); err != nil {
				return err
			}
			done := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !done.array {
				if err := visit(&done.info); err != nil {
					return err
				}
			}
			continue
		}
		if f.array {
			if _, err := begin(nil); err != nil {
				return err
			}
			continue
		}
		raw, err := d.dec.ReadValue() // the member name, still quoted
		if err != nil {
			return err
		}
		if d.name, err = jsontext.AppendUnquote(d.name[:0], raw); err != nil {
			return err
		}
		if skip != nil && skip(d.name) {
			if d.dec.PeekKind() != 'n' {
				f.info.names = append(append(f.info.names, d.name...), 0)
			}
			if err := d.dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		if d.dec.PeekKind() != 'n' {
			f.info.names = append(append(f.info.names, d.name...), 0)
		}
		isType := bytes.EqualFold(d.name, []byte("type"))
		val, err := begin(d.name)
		if err != nil {
			return err
		}
		if isType && val != nil && val.Kind() == '"' {
			if f.typ, err = jsontext.AppendUnquote(f.typ[:0], val); err != nil {
				return err
			}
			f.info.Type = f.typ
		}
	}
	return nil
}
