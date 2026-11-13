// Package chat is OpenAI's Chat Completions API (POST /v1/chat/completions):
// the pass-through scan and splice of a request, and usage from its
// response. A streamed request must ask for usage
// (stream_options.include_usage); golem adds it when the caller did not.
package chat

import (
	"bytes"
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

	streamOptions jsonscan.Span // the stream_options value, if present
	optionsNull   bool          // stream_options is null: replaced whole
	includeUsage  jsonscan.Span // its include_usage value, if present (absolute)
	usageOn       bool
}

var (
	ErrNoModel = errors.New(`chat: the request has no "model" string; name a model or alias`)
	ErrField   = errors.New("chat: a routing field has the wrong type")
)

// Scan reads the routing fields of body.
func (r *Request) Scan(body []byte) error {
	*r = Request{Model: r.Model[:0]}
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
			case 'f', 'n':
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
				return nil
			}
			n, ok := jsonscan.Int(v)
			if !ok || n < 0 {
				return ErrField
			}
			if completion || !hasCompletionTokens {
				r.MaxTokens, r.HasMaxTokens = n, true
			}
			hasCompletionTokens = hasCompletionTokens || completion
		case "stream_options":
			v, span, err := d.ReadValue()
			if err != nil {
				return err
			}
			if v.Kind() == 'n' {
				// Recorded, so Rewrite replaces it rather than adding a
				// second stream_options member (a duplicate, ADR 0037).
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
				r.includeUsage = jsonscan.Span{Start: span.Start + s.Start, End: span.Start + s.End}
				r.usageOn = v.Kind() == 't'
				return nil
			})
		default:
			switch string(name) {
			case "store":
				r.Flags |= FlagStore
			case "n":
				r.Flags |= FlagN
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

// InjectedUsage reports whether Rewrite asks the provider for usage the
// caller did not request: the stream's usage-only chunk must then be
// withheld from the caller, whose code may read choices[0] of every chunk
// (test/compat found both official SDKs' usual loops crash on it).
func (r *Request) InjectedUsage() bool { return r.Stream && !r.usageOn }

// UsageOnly reports whether a stream chunk is the usage report alone: an
// empty choices array and a usage object.
func UsageOnly(data []byte) bool {
	if !bytes.Contains(data, usageKey) {
		return false // most chunks: no scan
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

// Rewrite appends body to dst with the model replaced and, for a stream,
// usage requested.
func (r *Request) Rewrite(dst, body []byte, model string) []byte {
	var quoted [128]byte
	edits := [2]jsonscan.Edit{{Span: r.ModelSpan, With: jsonscan.Quote(quoted[:0], model)}}
	n := 1
	if r.Stream && !r.usageOn {
		switch {
		case r.optionsNull: // "stream_options": null
			edits[1] = jsonscan.Edit{Span: r.streamOptions, With: []byte(`{"include_usage":true}`)}
		case r.includeUsage.End > 0: // "include_usage": false
			edits[1] = jsonscan.Edit{Span: r.includeUsage, With: []byte("true")}
		case r.streamOptions.End > 0: // stream_options without it
			at := r.streamOptions.Start + 1
			with := []byte(`"include_usage":true,`)
			if len(bytes.TrimSpace(body[at:r.streamOptions.End-1])) == 0 { // {}
				with = with[:len(with)-1]
			}
			edits[1] = jsonscan.Edit{Span: jsonscan.Span{Start: at, End: at}, With: with}
		default: // no stream_options: add one after the model
			edits[1] = jsonscan.Edit{Span: jsonscan.Span{Start: r.ModelSpan.End, End: r.ModelSpan.End},
				With: []byte(`,"stream_options":{"include_usage":true}`)}
		}
		n = 2
	}
	return jsonscan.Apply(dst, body, edits[:n]...)
}

// StreamUsage reads usage from a response: the last chunk of a stream that
// asked for it, or a whole response's usage object.
type StreamUsage struct {
	Usage canon.Usage
	Err   error
}

var usageKey = []byte(`"usage"`)

// OnEvent reads one chunk; only a chunk mentioning usage is scanned.
func (u *StreamUsage) OnEvent(_, data []byte) {
	if !bytes.Contains(data, usageKey) {
		return
	}
	u.Response(data)
}

// Response reads usage from a JSON object (a chunk or a whole response).
// OpenAI's prompt_tokens include the cached ones; canon.Usage.Input does not.
func (u *StreamUsage) Response(body []byte) {
	var got canon.Usage
	seen := false
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
				return readInt(d, &got.Input)
			case "completion_tokens":
				return readInt(d, &got.Output)
			case "prompt_tokens_details":
				return readDetail(d, "cached_tokens", &got.CacheRead)
			case "completion_tokens_details":
				return readDetail(d, "reasoning_tokens", &got.Reasoning)
			}
			return d.SkipValue()
		})
	})
	if err != nil {
		if u.Err == nil {
			u.Err = err
		}
		return
	}
	if seen {
		got.Input -= min(got.CacheRead, got.Input)
		u.Usage = got
	}
}

func readInt(d *jsonscan.Decoder, dst *int64) error {
	v, _, err := d.ReadValue()
	if err != nil || v.Kind() == 'n' {
		return err
	}
	n, ok := jsonscan.Int(v)
	if !ok {
		return ErrField
	}
	*dst = n
	return nil
}

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
