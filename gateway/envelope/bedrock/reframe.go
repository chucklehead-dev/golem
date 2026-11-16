package bedrock

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// reframer reads an AWS event stream and yields the Messages SSE it
// carries: each chunk's base64 bytes, an Anthropic event, as
// `event: <type>`, `data: <json>`; an exception as the dialect's error
// event, then the end. A message whose CRC fails, or a stream cut, is a
// read error: the request path's cut.
//
// A stream is hundreds of events, so every buffer is reused from one
// event to the next: the message, the decoded event, the SSE not yet
// read (read from an offset, so its capacity is kept), and one JSON
// decoder for the members it reads.
type reframer struct {
	src  io.ReadCloser
	dec  streamDecoder
	msg  message
	json memberScanner
	ev   []byte // the chunk's event, decoded
	out  []byte // the SSE of the last message
	off  int    // how much of out has been read
	done bool
	log  *slog.Logger // where a refusal of the gateway's credentials is logged
}

func (f *reframer) Read(p []byte) (int, error) {
	for f.off == len(f.out) {
		if f.done {
			return 0, io.EOF
		}
		if err := f.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.out[f.off:])
	f.off += n
	return n, nil
}

func (f *reframer) Close() error { return f.src.Close() }

// next decodes one event-stream message into f.out.
func (f *reframer) next() error {
	f.out, f.off = f.out[:0], 0
	err := f.dec.next(f.src, &f.msg)
	if errors.Is(err, io.EOF) {
		f.done = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("bedrock: the event stream: %w", err)
	}
	m := &f.msg
	switch string(m.messageType) {
	case "event":
		if string(m.eventType) != "chunk" {
			return nil // an event that carries no Messages event
		}
		if err := f.chunkEvent(m.payload); err != nil {
			return err
		}
		typ, ok := f.json.member(f.ev, "type")
		if !ok || len(typ) == 0 {
			typ = []byte("message")
		}
		f.out = append(append(append(append(append(f.out, "event: "...), typ...), "\ndata: "...), f.ev...), "\n\n"...)
	case "exception", "error":
		name := string(m.exceptionType)
		if name == "" {
			name = string(m.errorCode)
		}
		ev := errorJSON(errorType(name), bedrockMessage(m.payload))
		if credentialsRefused(name) {
			ev = refusedJSON(cmp.Or(f.log, slog.Default()), errorType(name), name, 0, m.payload)
		}
		f.out = append(append(append(f.out, "event: error\ndata: "...), ev...), "\n\n"...)
		f.done = true
	}
	return nil
}

// chunkEvent decodes a chunk's Anthropic event, its "bytes" in base64,
// into f.ev.
func (f *reframer) chunkEvent(payload []byte) error {
	raw, ok := f.json.member(payload, "bytes")
	if !ok {
		return errors.New("bedrock: a chunk with no bytes")
	}
	var err error
	if f.ev, err = base64.StdEncoding.AppendDecode(f.ev[:0], raw); err != nil {
		return fmt.Errorf("bedrock: a chunk's bytes: %w", err)
	}
	return nil
}

// memberScanner finds a JSON object's top-level string member without
// allocating: one decoder reused over the object in place, names compared
// as their raw bytes unless escaped.
type memberScanner struct {
	buf  bytes.Buffer
	dec  *jsontext.Decoder
	name []byte // an escaped name, unquoted
	val  []byte // an escaped value, unquoted
}

// member is raw's top-level member named want, a string, unquoted: the
// first so named whose value is a string. It is valid until the next call.
func (s *memberScanner) member(raw []byte, want string) ([]byte, bool) {
	if s.dec == nil {
		s.dec = jsontext.NewDecoder(&s.buf)
	}
	// The decoder reads a *bytes.Buffer in place, without copying raw.
	s.buf = *bytes.NewBuffer(raw)
	s.dec.Reset(&s.buf)
	if tok, err := s.dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false
	}
	for s.dec.PeekKind() == '"' {
		name, err := s.dec.ReadValue()
		if err != nil {
			return nil, false
		}
		if s.named(name, want) && s.dec.PeekKind() == '"' {
			v, err := s.dec.ReadValue()
			if err != nil {
				return nil, false
			}
			if bytes.IndexByte(v, '\\') < 0 {
				return v[1 : len(v)-1], true
			}
			s.val, err = jsontext.AppendUnquote(s.val[:0], v)
			return s.val, err == nil
		}
		if err := s.dec.SkipValue(); err != nil {
			return nil, false
		}
	}
	return nil, false
}

// named reports whether name, a quoted JSON string, is want unescaped.
func (s *memberScanner) named(name jsontext.Value, want string) bool {
	if bytes.IndexByte(name, '\\') < 0 {
		return string(name[1:len(name)-1]) == want
	}
	var err error
	s.name, err = jsontext.AppendUnquote(s.name[:0], name)
	return err == nil && string(s.name) == want
}
