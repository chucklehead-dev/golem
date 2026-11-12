// Package sse splits a server-sent event stream into events as it passes
// through the gateway, whatever the chunking: the response is copied to the
// caller as it arrives and the same bytes are written here (gateway.md §3
// step 8).
package sse

import "bytes"

// MaxEvent bounds the data one event may accumulate; a larger event is
// dropped (counted in Parser.Dropped), never buffered without bound.
const MaxEvent = 1 << 20

// Parser is an io.Writer that calls OnEvent for each complete event. It
// keeps at most one partial line and one event's data.
type Parser struct {
	// Want, if set, is asked for each event name as soon as it is known; a
	// false answer skips the event's data without copying it (an Anthropic
	// content delta, which carries no usage).
	Want func(event []byte) bool
	// OnEvent receives the event's name (empty if none) and its data lines
	// joined by "\n". Both are valid only during the call.
	OnEvent func(event, data []byte)
	// Dropped counts events over MaxEvent.
	Dropped int

	line    []byte
	event   []byte
	data    []byte
	hasData bool
	skip    bool
	over    bool
	cr      bool // the last write ended in "\r"
}

// Write consumes the next bytes of the stream. It never fails: the stream
// belongs to the caller, and a malformed one is only less informative.
func (p *Parser) Write(b []byte) (int, error) {
	n := len(b)
	if n == 0 {
		return 0, nil // keeps a pending "\r": an empty write is no line's end
	}
	if p.cr && len(b) > 0 && b[0] == '\n' {
		b = b[1:] // the "\n" of a "\r\n" split across writes
	}
	p.cr = false
	for len(b) > 0 {
		i := indexEOL(b)
		if i < 0 {
			p.appendLine(b)
			break
		}
		var line []byte
		if len(p.line) > 0 {
			p.appendLine(b[:i])
			line = p.line
		} else {
			line = b[:i]
		}
		p.field(line)
		p.line = p.line[:0]
		// "\r\n" is one terminator; a lone "\r" is one too.
		if b[i] == '\r' {
			if i+1 == len(b) {
				p.cr = true
			} else if b[i+1] == '\n' {
				i++
			}
		}
		b = b[i+1:]
	}
	return n, nil
}

// indexEOL finds the first "\n" or "\r", with the vectorised IndexByte:
// a byte loop here was 40% of the gateway's CPU on a stream.
func indexEOL(b []byte) int {
	i := bytes.IndexByte(b, '\n')
	limit := b
	if i >= 0 {
		limit = b[:i]
	}
	if j := bytes.IndexByte(limit, '\r'); j >= 0 {
		return j
	}
	return i
}

func (p *Parser) appendLine(b []byte) {
	if len(p.line)+len(b) > MaxEvent {
		p.over = true
		return
	}
	p.line = append(p.line, b...)
}

func (p *Parser) field(line []byte) {
	if len(line) == 0 {
		p.dispatch()
		return
	}
	if line[0] == ':' {
		return // a comment, often a keep-alive
	}
	name, value := line, []byte(nil)
	for i, c := range line {
		if c == ':' {
			name, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			break
		}
	}
	switch string(name) {
	case "event":
		p.event = append(p.event[:0], value...)
		if p.Want != nil && !p.Want(p.event) {
			p.skip = true
		}
	case "data":
		if p.skip || p.over {
			return
		}
		if len(p.data)+len(value)+1 > MaxEvent {
			p.over = true
			return
		}
		if p.hasData {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, value...)
		p.hasData = true
	}
}

func (p *Parser) dispatch() {
	switch {
	case p.over:
		p.Dropped++
	case p.hasData && !p.skip && p.OnEvent != nil:
		p.OnEvent(p.event, p.data)
	}
	p.event, p.data = p.event[:0], p.data[:0]
	p.hasData, p.skip, p.over = false, false, false
}
