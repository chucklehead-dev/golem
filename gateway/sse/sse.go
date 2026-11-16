// Package sse splits a server-sent event stream into events as it passes
// through the gateway, whatever the chunking: the response is copied to the
// caller as it arrives and the same bytes are written here (gateway.md).
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
	cr      bool    // the last write ended in "\r"
	crEnded bool    // and that "\r" ended an event
	ended   bool    // the last line dispatched an event
	f       *Filter // if filtering: told where each event's bytes end
}

// keepBuffer bounds the buffers a Reset keeps: a stream's usual events,
// not the largest one it ever saw (MaxEvent).
const keepBuffer = 16 << 10

// Reset readies the parser for a new stream, keeping its callbacks and its
// buffers up to keepBuffer: a pooled parser then allocates nothing for an
// ordinary stream.
func (p *Parser) Reset() {
	*p = Parser{Want: p.Want, OnEvent: p.OnEvent, line: keep(p.line), event: keep(p.event), data: keep(p.data)}
}

func keep(b []byte) []byte {
	if cap(b) > keepBuffer {
		return nil
	}
	return b[:0]
}

// Write consumes the next bytes of the stream. It never fails: the stream
// belongs to the caller, and a malformed one is only less informative.
func (p *Parser) Write(b []byte) (int, error) {
	n := len(b)
	if n == 0 {
		return 0, nil // keeps a pending "\r": an empty write is no line's end
	}
	orig, from := b, 0 // the raw bytes, for a filter
	if p.cr && b[0] == '\n' {
		b = b[1:] // the "\n" of a "\r\n" split across writes
		from = 1
		if p.f != nil {
			p.f.split(p.crEnded)
		}
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
				p.cr, p.crEnded = true, p.ended
			} else if b[i+1] == '\n' {
				i++
			}
		}
		b = b[i+1:]
		if p.ended {
			p.ended = false
			if p.f != nil {
				to := len(orig) - len(b)
				p.f.end(orig[from:to])
				from = to
			}
		}
	}
	if p.f != nil {
		p.f.more(orig[from:])
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
	name, value := line, []byte(nil) // a line without a colon is a field with no value
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		name, value = line[:i], line[i+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
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
	p.ended = true
	if p.f != nil {
		p.f.drop = p.hasData && !p.over && p.f.Drop != nil && p.f.Drop(p.event, p.data)
	}
	p.event, p.data = p.event[:0], p.data[:0]
	p.hasData, p.skip, p.over = false, false, false
}

// Filter passes a stream through byte for byte but for the events Drop
// rejects: comments and keep-alives, unknown fields, an event too large to
// hold, and a last event the stream never terminated all go through as they
// came. Its Parser reads every event as usual (OnEvent sees the dropped
// ones too). A Filter holds at most one event's bytes.
type Filter struct {
	Parser
	// Drop reports whether an event is withheld from the caller; it is
	// asked after OnEvent, with the same arguments.
	Drop func(event, data []byte) bool

	out      []byte
	block    []byte // the current event's bytes, while it may be dropped
	passing  bool   // the current event outgrew MaxEvent: passed through
	drop     bool
	lastDrop bool // the last event ended was dropped
}

// Reset readies the filter for a new stream, keeping its callbacks and
// bounded buffers, as Parser.Reset.
func (f *Filter) Reset() {
	f.Parser.Reset()
	f.out, f.block = keep(f.out), keep(f.block)
	f.passing, f.drop, f.lastDrop = false, false, false
}

// Pass consumes the next bytes of the stream and returns those to forward,
// valid until the next call.
func (f *Filter) Pass(b []byte) []byte {
	f.out = f.out[:0]
	f.Parser.f = f
	f.Parser.Write(b)
	return f.out
}

// End returns what is left at the stream's end: an event never terminated.
func (f *Filter) End() []byte {
	f.out = append(f.out[:0], f.block...)
	f.block, f.passing = f.block[:0], false
	return f.out
}

// more takes bytes of an event not yet ended.
func (f *Filter) more(b []byte) {
	switch {
	case f.passing:
		f.out = append(f.out, b...)
	case len(f.block)+len(b) > MaxEvent:
		f.out = append(append(f.out, f.block...), b...)
		f.block, f.passing = f.block[:0], true
	default:
		f.block = append(f.block, b...)
	}
}

// end takes an event's last bytes, its terminating blank line included.
func (f *Filter) end(b []byte) {
	if f.passing {
		f.out = append(f.out, b...)
		f.passing, f.lastDrop = false, false
		return
	}
	if !f.drop {
		f.out = append(append(f.out, f.block...), b...)
	}
	f.block, f.lastDrop, f.drop = f.block[:0], f.drop, false
}

// split takes the "\n" of a "\r\n" split across writes: the last event's
// if its "\r" ended it, else the current one's.
func (f *Filter) split(ended bool) {
	switch {
	case !ended:
		f.more([]byte{'\n'})
	case !f.lastDrop:
		f.out = append(f.out, '\n')
	}
}
