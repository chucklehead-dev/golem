// Package messages is Anthropic's Messages API (POST /v1/messages): the
// pass-through scan and splice of a request, and usage from its response,
// streamed or not.
package messages

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/dialect/internal/jsonscan"
)

// Flags mark policy-relevant members present in a request (contracts.md §8).
type Flags uint32

const (
	FlagMCPServers  Flags = 1 << iota // mcp_servers: egress from the provider
	FlagContainer                     // container: provider-side state
	FlagServerTools                   // a server tool (web search or fetch, code execution): egress, unbounded cost
)

// serverTools are the prefixes of versioned tool types the provider runs
// itself ("web_search_20250305"). Tool objects are decoded below: byte
// searches are unsafe for policy because insignificant whitespace and JSON
// escapes would bypass them.
var serverTools = []string{"web_search", "web_fetch", "code_execution"}

// serverTool reports whether tools (a request's tools array of objects)
// has a tool the provider runs itself. Each tool's type is read decoded
// (whitespace and escapes cannot hide it) and its member name compared
// without regard to case, as encoding/json would: a policy check fails
// closed.
func serverTool(tools []byte) (found bool, err error) {
	err = jsonscan.Elements(tools, func(t jsontext.Value) error {
		if t.Kind() != '{' {
			return ErrField
		}
		return jsonscan.Object(t, func(name []byte, d *jsonscan.Decoder) error {
			if !bytes.EqualFold(name, []byte("type")) {
				return d.SkipValue()
			}
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			if v.Kind() != '"' {
				return ErrField
			}
			var buf [64]byte
			typ, err := jsontext.AppendUnquote(buf[:0], v)
			if err != nil {
				return err
			}
			for _, prefix := range serverTools {
				found = found || bytes.HasPrefix(typ, []byte(prefix))
			}
			return nil
		})
	})
	return found, err
}

// known lists the request members golem forwards without comment; others
// are forwarded and counted (and logged by name, ADR 0037).
var known = map[string]bool{ // read-only
	"model": true, "messages": true, "max_tokens": true, "system": true, "metadata": true,
	"stop_sequences": true, "stream": true, "temperature": true, "top_p": true, "top_k": true,
	"tools": true, "tool_choice": true, "thinking": true, "service_tier": true,
	"mcp_servers": true, "container": true, "context_management": true,
}

// Request is what routing needs from a request body. Model is valid until
// the next Scan with the same Request.
type Request struct {
	Model        []byte
	ModelSpan    jsonscan.Span
	Stream       bool
	MaxTokens    int64
	HasMaxTokens bool
	Flags        Flags
	Unknown      int
}

var (
	ErrNoModel = errors.New(`messages: the request has no "model" string; name a model or alias`)
	ErrField   = errors.New("messages: a routing field has the wrong type")
)

// Scan reads the routing fields of body, which must be one JSON object
// without duplicate names.
func (r *Request) Scan(body []byte) error {
	*r = Request{Model: r.Model[:0]}
	hasModel := false
	err := jsonscan.Object(body, func(name []byte, d *jsonscan.Decoder) error {
		switch string(name) {
		case "model":
			v, span, err := d.ReadValue()
			if err != nil {
				return err
			}
			m, ok := jsonscan.String(r.Model, v)
			if !ok {
				return ErrField
			}
			r.Model, r.ModelSpan, hasModel = m, span, true
		case "stream":
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			switch v.Kind() {
			case 't':
				r.Stream = true
			case 'f':
			default:
				return ErrField
			}
		case "max_tokens":
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			n, ok := jsonscan.Int(v)
			if !ok || n < 0 {
				return ErrField
			}
			r.MaxTokens, r.HasMaxTokens = n, true
		case "tools":
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			server, err := serverTool(v)
			if err != nil {
				return ErrField // a shape the check could not read must not pass as no server tool
			}
			if server {
				r.Flags |= FlagServerTools
			}
		default:
			switch string(name) {
			case "mcp_servers":
				r.Flags |= FlagMCPServers
			case "container":
				r.Flags |= FlagContainer
			}
			if !known[string(name)] {
				r.Unknown++
			}
			return d.SkipValue()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !hasModel {
		return ErrNoModel
	}
	return nil
}

// Rewrite appends body to dst with the model replaced by model, which comes
// from validated config and so is valid UTF-8.
func (r *Request) Rewrite(dst, body []byte, model string) []byte {
	return jsonscan.Splice(dst, body, r.ModelSpan, model)
}

// StreamUsage reads usage from a streamed response: message_start carries
// the input side, each message_delta the cumulative output.
type StreamUsage struct {
	Usage canon.Usage
	Err   error // the first malformed usage event, if any
	// Final: the output count is the provider's (a message_delta, or a
	// whole response's usage). Else a cut stream's output is estimated
	// from the content seen: a whole response's ContentBytes, or Deltas
	// events whose data the parser skipped (and measured).
	Final        bool
	ContentBytes int64
	Deltas       int64
}

// DeltaEnvelope is a content delta's data without its content: what an
// estimate takes off each skipped delta's bytes.
var DeltaEnvelope = int64(len(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`))

// contentKeys hold a delta's or a response's content: text, thinking and a
// tool call's JSON.
var contentKeys = jsonscan.KeysOf("text", "thinking", "partial_json")

// Want skips the data of events that carry no usage: content deltas are
// counted, and their data measured by the parser, not copied or scanned.
func (u *StreamUsage) Want(event []byte) bool {
	switch string(event) {
	case "message_start", "message_delta":
		return true
	case "content_block_delta":
		u.Deltas++
	}
	return false
}

// OnEvent reads one event's usage.
func (u *StreamUsage) OnEvent(event, data []byte) {
	var err error
	switch string(event) {
	case "message_start":
		err = jsonscan.Object(data, func(name []byte, d *jsonscan.Decoder) error {
			if string(name) != "message" {
				return d.SkipValue()
			}
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			return jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
				if string(name) != "usage" {
					return d.SkipValue()
				}
				_, err = u.readUsage(d)
				return err
			})
		})
	case "message_delta":
		err = jsonscan.Object(data, func(name []byte, d *jsonscan.Decoder) error {
			if string(name) != "usage" {
				return d.SkipValue()
			}
			var output bool
			output, err = u.readUsage(d)
			if err == nil && !output {
				return ErrField
			}
			u.Final = err == nil
			return err
		})
	}
	if err != nil && u.Err == nil {
		u.Err = err
	}
}

// Response reads usage from a whole (non-streamed) response body.
func (u *StreamUsage) Response(body []byte) {
	u.ContentBytes = int64(jsonscan.StringBytes(body, contentKeys))
	seen := false
	err := jsonscan.Object(body, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != "usage" {
			return d.SkipValue()
		}
		seen = true
		var output bool
		output, err := u.readUsage(d)
		if err == nil && !output {
			return ErrField
		}
		u.Final = err == nil
		return err
	})
	if err == nil && !seen {
		u.Final = false
	}
	if err != nil && u.Err == nil {
		u.Err = err
	}
}

// readUsage reads a usage object; counts are cumulative, so the last wins.
func (u *StreamUsage) readUsage(d *jsonscan.Decoder) (hasOutput bool, err error) {
	v, _, err := d.ReadValue()
	if err != nil {
		return false, err
	}
	err = jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
		var dst *int64
		isOutput := false
		switch string(name) {
		case "input_tokens":
			dst = &u.Usage.Input
		case "output_tokens":
			dst = &u.Usage.Output
			isOutput = true
		case "cache_read_input_tokens":
			dst = &u.Usage.CacheRead
		case "cache_creation_input_tokens":
			dst = &u.Usage.CacheWrite
		default:
			return d.SkipValue()
		}
		v, _, err := d.ReadValue()
		if err != nil {
			return err
		}
		if v.Kind() == 'n' { // null: not reported in this event
			return nil
		}
		n, ok := jsonscan.Int(v)
		if !ok || n < 0 {
			return ErrField
		}
		*dst = n
		hasOutput = hasOutput || isOutput
		return nil
	})
	return hasOutput, err
}
