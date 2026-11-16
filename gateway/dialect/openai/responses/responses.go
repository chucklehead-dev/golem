// Package responses is OpenAI's Responses API (POST /v1/responses): the
// pass-through scan and splice of a request, and usage from its response.
//
// M1 serves stateless responses only (security-and-correctness.md,
// ADR 0027): store is false whatever the caller or scope asks (an absent
// store, which the provider takes as true, is written false); a request
// that needs state the provider keeps (previous_response_id, a
// conversation, background execution, a stored prompt, an earlier item by
// reference, a provider-managed file or vector store) is refused, as Scan
// reports by Stateful.
package responses

import (
	"encoding/json/jsontext"
	"errors"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/dialect/internal/jsonscan"
	usage "github.com/chucklehead-dev/golem/gateway/dialect/internal/openaiusage"
)

// known lists the request members golem forwards without comment; others
// are forwarded and counted (restricted mode refuses them, ADR 0037).
var known = map[string]bool{ // read-only
	"model": true, "input": true, "instructions": true, "stream": true, "stream_options": true,
	"max_output_tokens": true, "store": true, "metadata": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "temperature": true, "top_p": true, "user": true, "text": true,
	"reasoning": true, "truncation": true, "include": true, "service_tier": true,
	"prompt_cache_key": true, "safety_identifier": true, "max_tool_calls": true, "top_logprobs": true,
	"previous_response_id": true, "conversation": true, "background": true, "prompt": true,
}

// serverTools are the tool types the provider runs itself: egress, units
// billed beyond tokens (ADR 0031).
var serverTools = map[string]bool{ // read-only
	"web_search": true, "web_search_preview": true, "file_search": true, "code_interpreter": true,
	"mcp": true, "image_generation": true, "computer_use_preview": true, "local_shell": true,
}

// Request is what routing and policy need from a request body.
type Request struct {
	Model        []byte
	ModelSpan    jsonscan.Span
	Stream       bool
	MaxTokens    int64 // max_output_tokens
	HasMaxTokens bool
	ServerTools  bool
	Unknown      int
	// StoreTrue: the request asks the provider to store the response, by
	// name (refused: M1 stores nothing).
	StoreTrue bool
	// Stateful names the first member that needs provider-side state, or
	// is empty.
	Stateful string

	storeSpan jsonscan.Span // the store value, if present (false or null)
}

// Scan's refusals.
var (
	ErrNoModel = errors.New(`responses: the request has no "model" string; name a model or alias`)
	ErrField   = errors.New("responses: a routing or policy field has the wrong type")
)

// Scan reads the routing and policy fields of body.
func (r *Request) Scan(body []byte) error {
	*r = Request{Model: r.Model[:0]}
	hasModel := false
	err := jsonscan.Request(body, func(name []byte, d *jsonscan.Decoder) error {
		n := string(name)
		if !known[n] {
			r.Unknown++
			return d.SkipValue()
		}
		v, span, err := d.ReadValue()
		if err != nil {
			return err
		}
		switch n {
		case "model":
			m, ok := jsonscan.String(r.Model, v)
			if !ok {
				return ErrField
			}
			r.Model, r.ModelSpan, hasModel = m, span, true
		case "stream":
			var ok bool
			if r.Stream, ok = jsonscan.Bool(v); !ok {
				return ErrField
			}
		case "max_output_tokens":
			if v.Kind() == 'n' {
				return nil
			}
			m, ok := jsonscan.Int(v)
			if !ok || m < 0 {
				return ErrField
			}
			r.MaxTokens, r.HasMaxTokens = m, true
		case "store":
			switch v.Kind() {
			case 't':
				r.StoreTrue = true
			case 'f', 'n': // null is the provider's default, true: written false too
				r.storeSpan = span
			default:
				return ErrField // a wrong type must not pass as not storing
			}
		case "previous_response_id", "conversation", "prompt":
			if v.Kind() != 'n' {
				r.stateful(n)
			}
		case "background":
			if v.Kind() == 't' {
				r.stateful(n)
			}
		case "input", "instructions":
			if v.Kind() != '[' {
				return nil // text
			}
			return r.references(n, v)
		case "tools":
			if v.Kind() != '[' {
				return nil
			}
			return jsonscan.Elements(v, func(elem jsontext.Value) error {
				return jsonscan.Object(elem, func(name []byte, d *jsonscan.Decoder) error {
					if string(name) != "type" {
						return d.SkipValue()
					}
					t, _, err := d.ReadValue()
					if err != nil {
						return err
					}
					var buf [32]byte
					s, _ := jsonscan.String(buf[:0], t)
					if serverTools[string(s)] {
						r.ServerTools = true
					}
					if string(s) == "file_search" {
						r.stateful("tools (file_search: a provider vector store)")
					}
					return nil
				})
			})
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

// references marks Stateful if items (input or instructions, an array of
// items) refers to state another call stored: a provider-managed file (a
// file_id, not null, in any object: an input_file's, an input_image's, a
// screenshot's) or an earlier item (an item_reference, or an item with an
// id and neither type nor role, which the API takes as one). The tree is
// decoded, its member names compared unescaped: a JSON escape cannot hide
// either.
func (r *Request) references(member string, items []byte) error {
	return jsonscan.Objects(items, nil, func(o *jsonscan.ObjectInfo) error {
		switch {
		case o.Has("file_id"):
			r.stateful(member + " (a file_id)")
		case string(o.Type) == "item_reference",
			o.Depth == 1 && o.Type == nil && !o.HasExactly("role") && o.Has("id"):
			r.stateful(member + " (an item_reference)")
		}
		return nil
	})
}

func (r *Request) stateful(member string) {
	if r.Stateful == "" {
		r.Stateful = member
	}
}

// Rewrite appends body to dst with the model replaced and store false: a
// store member present (false or null) set to false, else one added after
// the model.
func (r *Request) Rewrite(dst, body []byte, model string) []byte {
	var quoted [128]byte
	edits := [2]jsonscan.Edit{{Span: r.ModelSpan, With: jsonscan.Quote(quoted[:0], model)}}
	if r.storeSpan.End > 0 {
		edits[1] = jsonscan.Edit{Span: r.storeSpan, With: jsonFalse}
	} else {
		edits[1] = jsonscan.Edit{Span: jsonscan.Span{Start: r.ModelSpan.End, End: r.ModelSpan.End}, With: storeAfterModel}
	}
	if edits[1].Span.Start < edits[0].Span.Start { // edits apply in body order
		edits[0], edits[1] = edits[1], edits[0]
	}
	return jsonscan.Apply(dst, body, edits[:]...)
}

var (
	jsonFalse       = []byte("false")          // read-only
	storeAfterModel = []byte(`,"store":false`) // read-only
)

// StreamUsage reads usage from a response: a stream's terminal event
// (response.completed, .incomplete or .failed, its response's usage), or
// a whole response's usage object.
type StreamUsage struct {
	Usage canon.Usage
	Err   error
	Final bool // the usage is the provider's
}

// terminal reports a stream event that carries the response's usage.
func terminal(event []byte) bool {
	switch string(event) {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// OnEvent reads one stream event; only a terminal one is scanned.
func (u *StreamUsage) OnEvent(event, data []byte) {
	if !terminal(event) {
		return
	}
	err := jsonscan.Object(data, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != "response" {
			return d.SkipValue()
		}
		v, _, err := d.ReadValue()
		if err != nil {
			return err
		}
		u.Response(v)
		return nil
	})
	if err != nil && u.Err == nil {
		u.Err = err
	}
}

// Response reads usage from a response object.
func (u *StreamUsage) Response(body []byte) {
	got, seen, err := usage.Read(body, usageNames, ErrField)
	if err != nil {
		if u.Err == nil {
			u.Err = err
		}
		return
	}
	if seen {
		u.Usage, u.Final = got, true
	}
}

var usageNames = usage.Names{Input: "input_tokens", Output: "output_tokens", // read-only
	InputDetails: "input_tokens_details", OutputDetails: "output_tokens_details"}

// IsError reports a stream event that is the response's failure before
// any output: an error event (the commit point refuses it).
func IsError(event []byte) bool { return string(event) == "error" }
