// Package chat is OpenAI's Chat Completions API (POST /v1/chat/completions):
// the pass-through scan and splice of a request, and usage from its
// response. A streamed request must ask for usage
// (stream_options.include_usage); golem adds it when the caller did not.
package chat

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
	FlagStore     Flags = 1 << iota // store: provider-side retention (ADR 0027)
	FlagN                           // n: several completions, several costs
	FlagWebSearch                   // web_search_options: egress, billed units
	FlagAudio                       // audio: another modality and price
)

// known lists the request members golem forwards without comment; others
// are forwarded and counted (restricted mode refuses them, ADR 0037).
var known = map[string]bool{ // read-only
	"model": true, "messages": true, "stream": true, "stream_options": true, "max_tokens": true,
	"max_completion_tokens": true, "n": true, "store": true, "metadata": true, "tools": true,
	"tool_choice": true, "temperature": true, "top_p": true, "user": true, "response_format": true,
	"seed": true, "stop": true, "logprobs": true, "top_logprobs": true, "presence_penalty": true,
	"frequency_penalty": true, "logit_bias": true, "parallel_tool_calls": true,
	"reasoning_effort": true, "modalities": true, "audio": true, "prediction": true,
	"service_tier": true, "web_search_options": true, "prompt_cache_key": true, "safety_identifier": true,
}

// Request is what routing needs from a request body.
type Request struct {
	Model        []byte
	ModelSpan    jsonscan.Span
	Stream       bool
	MaxTokens    int64 // max_completion_tokens, else max_tokens
	HasMaxTokens bool
	Flags        Flags
	Unknown      int
	N            int64 // completions asked for (n), 1 if absent
	Store        bool  // store: true, provider-side retention asked for

	storeSpan jsonscan.Span // the store value, to rewrite to false
	noStore   bool          // Rewrite sets store to false (ADR 0027)

	streamOptions jsonscan.Span // the stream_options value, if present
	optionsNull   bool          // stream_options is null: replaced whole
	includeUsage  jsonscan.Span // its include_usage value, if present (absolute)
	usageOn       bool
}

// Scan's refusals: a body with no model to route by, and a member golem
// reads (routing, policy or usage) with a value it cannot read.
var (
	ErrNoModel = errors.New(`chat: the request has no "model" string; name a model or alias`)
	ErrField   = errors.New("chat: a routing field has the wrong type")
)

// Scan reads the routing fields of body.
func (r *Request) Scan(body []byte) error {
	*r = Request{Model: r.Model[:0], N: 1}
	hasModel, hasCompletionTokens := false, false
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
			case 'f', 'n': // null: the default, not streamed
			default:
				return ErrField
			}
		case "max_tokens", "max_completion_tokens":
			completion := string(name) == "max_completion_tokens"
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			if v.Kind() == 'n' {
				return nil // null: absent, which the scope's ceiling refuses where it has one
			}
			n, ok := jsonscan.Int(v)
			if !ok || n < 0 {
				return ErrField
			}
			// max_completion_tokens supersedes max_tokens, whichever comes first.
			if completion || !hasCompletionTokens {
				r.MaxTokens, r.HasMaxTokens = n, true
			}
			hasCompletionTokens = hasCompletionTokens || completion
		case "stream_options":
			v, span, err := d.ReadValue()
			if err != nil {
				return err
			}
			return r.scanStreamOptions(v, span)
		case "store":
			r.Flags |= FlagStore
			v, span, err := d.ReadValue()
			if err != nil {
				return err
			}
			r.Store, r.storeSpan = v.Kind() == 't', span
		case "n":
			r.Flags |= FlagN
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			if v.Kind() == 'n' {
				return nil // null: the default, one
			}
			n, ok := jsonscan.Int(v)
			if !ok || n < 1 {
				return ErrField
			}
			r.N = n
		default:
			switch string(name) {
			case "web_search_options":
				r.Flags |= FlagWebSearch
			case "audio":
				r.Flags |= FlagAudio
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

// scanStreamOptions records where stream_options (at span in the body)
// and its include_usage lie, for Rewrite to ask for usage.
func (r *Request) scanStreamOptions(v jsontext.Value, span jsonscan.Span) error {
	if v.Kind() == 'n' {
		// Recorded, so Rewrite replaces it rather than adding a second
		// stream_options member (a duplicate, ADR 0037).
		r.streamOptions, r.optionsNull = span, true
		return nil
	}
	if v.Kind() != '{' {
		return ErrField
	}
	r.streamOptions = span
	return jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != "include_usage" {
			return d.SkipValue()
		}
		v, s, err := d.ReadValue()
		if err != nil {
			return err
		}
		r.includeUsage = jsonscan.Span{Start: span.Start + s.Start, End: span.Start + s.End} // in the body, not in v
		r.usageOn = v.Kind() == 't'
		return nil
	})
}

// InjectedUsage reports whether Rewrite asks the provider for usage the
// caller did not request: the stream's usage-only chunk must then be
// withheld from the caller, whose code may read choices[0] of every chunk
// (test/compat found both official SDKs' usual loops crash on it).
func (r *Request) InjectedUsage() bool { return r.Stream && !r.usageOn }

// UsageOnly reports whether a stream chunk is the usage report alone: an
// empty choices array and a usage object.
func UsageOnly(data []byte) bool {
	if !mayHaveUsage(data) {
		return false // every chunk but one: no scan
	}
	emptyChoices, usage := false, false
	err := jsonscan.Object(data, func(name []byte, d *jsonscan.Decoder) error {
		switch string(name) {
		case "choices":
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			emptyChoices = v.Kind() == '[' && len(bytes.TrimSpace(v[1:len(v)-1])) == 0
		case "usage":
			v, _, err := d.ReadValue()
			if err != nil {
				return err
			}
			usage = v.Kind() == '{'
		default:
			return d.SkipValue()
		}
		return nil
	})
	return err == nil && emptyChoices && usage
}

// ForbidStorage makes Rewrite turn a store: true into false: provider-side
// retention the scope forbids (ADR 0027).
func (r *Request) ForbidStorage() { r.noStore = r.Store }

// Rewrite appends body to dst with the model replaced and, for a stream,
// usage requested.
func (r *Request) Rewrite(dst, body []byte, model string) []byte {
	var quoted [128]byte
	edits := [3]jsonscan.Edit{{Span: r.ModelSpan, With: jsonscan.Quote(quoted[:0], model)}}
	n := 1
	if r.noStore {
		edits[n] = jsonscan.Edit{Span: r.storeSpan, With: jsonFalse}
		n++
	}
	if r.InjectedUsage() {
		edits[n] = r.usageEdit(body)
		n++
	}
	return jsonscan.Apply(dst, body, edits[:n]...)
}

// usageEdit is the edit that asks for a stream's usage, wherever the
// caller's body leaves room for it.
func (r *Request) usageEdit(body []byte) jsonscan.Edit {
	switch {
	case r.optionsNull: // "stream_options": null
		return jsonscan.Edit{Span: r.streamOptions, With: usageOptions}
	case r.includeUsage.End > 0: // "include_usage": false
		return jsonscan.Edit{Span: r.includeUsage, With: jsonTrue}
	case r.streamOptions.End > 0: // stream_options without it
		at := r.streamOptions.Start + 1
		with := usageMember
		if len(bytes.TrimSpace(body[at:r.streamOptions.End-1])) == 0 { // {}
			with = with[:len(with)-1]
		}
		return jsonscan.Edit{Span: jsonscan.Span{Start: at, End: at}, With: with}
	}
	// No stream_options: add one after the model.
	return jsonscan.Edit{Span: jsonscan.Span{Start: r.ModelSpan.End, End: r.ModelSpan.End}, With: usageAfterModel}
}

// The bytes Rewrite splices in; package-level, so an edit holding one is
// not a heap allocation per request.
var (
	jsonFalse       = []byte("false")                                    // read-only
	jsonTrue        = []byte("true")                                     // read-only
	usageOptions    = []byte(`{"include_usage":true}`)                   // read-only
	usageMember     = []byte(`"include_usage":true,`)                    // read-only; resliced, never written
	usageAfterModel = []byte(`,"stream_options":{"include_usage":true}`) // read-only
)

// StreamUsage reads usage from a response: the last chunk of a stream that
// asked for it, or a whole response's usage object.
type StreamUsage struct {
	Usage canon.Usage
	Err   error
	// Final: the usage is the provider's. Else the gateway estimates it
	// from the response's bytes (proxy's estimate).
	Final bool
}

var usageKey = []byte(`"usage"`)

// mayHaveUsage reports whether a chunk may carry a usage object: it has a
// "usage" member that is not null. A stream golem asked usage of carries
// "usage":null in every chunk but the last, so a check for the key alone
// skips nothing, and each chunk was scanned in full, twice (2.3 µs a chunk:
// a 200-chunk stream cost more than the whole fast path). A raw quote
// cannot occur inside a JSON string (it is escaped), so each match is a
// key, or a string value equal to "usage", never followed by a colon.
func mayHaveUsage(data []byte) bool {
	if bytes.Contains(data, []byte(`\u`)) {
		return true // a key may be spelled with escapes ("\u0075sage"): scan
	}
	for rest := data; ; {
		i := bytes.Index(rest, usageKey)
		if i < 0 {
			return false
		}
		rest = bytes.TrimLeft(rest[i+len(usageKey):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue // a value, not a key
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		if !bytes.HasPrefix(rest, []byte("null")) {
			return true
		}
	}
}

// OnEvent reads one chunk; only a chunk that may carry usage is scanned.
func (u *StreamUsage) OnEvent(_, data []byte) {
	if mayHaveUsage(data) {
		u.Response(data)
	}
}

// Response reads usage from a JSON object (a chunk or a whole response).
// OpenAI's prompt_tokens include the cached ones; canon.Usage.Input does not.
func (u *StreamUsage) Response(body []byte) {
	var got canon.Usage
	seen, input, output := false, false, false
	err := jsonscan.Object(body, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != "usage" {
			return d.SkipValue()
		}
		v, _, err := d.ReadValue()
		if err != nil || v.Kind() != '{' {
			return err // null usage: an ordinary chunk
		}
		seen = true
		return jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
			switch string(name) {
			case "prompt_tokens":
				err := readInt(d, &got.Input)
				input = err == nil
				return err
			case "completion_tokens":
				err := readInt(d, &got.Output)
				output = err == nil
				return err
			case "prompt_tokens_details":
				return readDetail(d, "cached_tokens", &got.CacheRead)
			case "completion_tokens_details":
				return readDetail(d, "reasoning_tokens", &got.Reasoning)
			}
			return d.SkipValue()
		})
	})
	if err == nil && seen && (!input || !output) {
		err = ErrField // a partial report must not suppress estimation
	}
	if err != nil {
		if u.Err == nil {
			u.Err = err
		}
		return
	}
	if seen {
		got.Input -= min(got.CacheRead, got.Input)
		u.Usage = got
		u.Final = true
	}
}

// readInt reads a token count into dst: a non-negative integer, or null
// (not reported, dst unchanged).
func readInt(d *jsonscan.Decoder, dst *int64) error {
	v, _, err := d.ReadValue()
	if err != nil || v.Kind() == 'n' {
		return err
	}
	n, ok := jsonscan.Int(v)
	if !ok || n < 0 {
		return ErrField
	}
	*dst = n
	return nil
}

// readDetail reads one count from a usage details object, if it is one.
func readDetail(d *jsonscan.Decoder, field string, dst *int64) error {
	v, _, err := d.ReadValue()
	if err != nil || v.Kind() != '{' {
		return err
	}
	return jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != field {
			return d.SkipValue()
		}
		return readInt(d, dst)
	})
}
