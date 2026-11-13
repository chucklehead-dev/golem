package proxy

import (
	"errors"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/dialect/anthropic/messages"
	"github.com/chucklehead-dev/golem/gateway/dialect/openai/chat"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// Everything the request path does differently per dialect is here, as
// methods of the pooled exchange: the rest of the path is dialect-free.

// Dialect selects the frontend a Handler serves.
type Dialect int

const (
	AnthropicMessages Dialect = iota // POST /v1/messages
	OpenAIChat                       // POST /v1/chat/completions
)

// Name is the dialect's name in config (Backend spec.dialect).
func (d Dialect) Name() string {
	if d == OpenAIChat {
		return "openai.chat"
	}
	return "anthropic.messages"
}

// scanned is what routing and admission need of a request body.
type scanned struct {
	alias     []byte // valid while the body is
	stream    bool
	maxTokens int64
	hasMax    bool
}

// scan reads the routing fields of a request body.
func (ex *exchange) scan(d Dialect, body []byte) (scanned, error) {
	if d == AnthropicMessages {
		err := ex.msg.Scan(body)
		return scanned{ex.msg.Model, ex.msg.Stream, ex.msg.MaxTokens, ex.msg.HasMaxTokens}, err
	}
	err := ex.chat.Scan(body)
	return scanned{ex.chat.Model, ex.chat.Stream, ex.chat.MaxTokens, ex.chat.HasMaxTokens}, err
}

// rewrite appends the scanned body to dst for the provider: its model
// replaced and, for an OpenAI stream, usage requested.
func (ex *exchange) rewrite(d Dialect, dst, body []byte, model string) []byte {
	if d == AnthropicMessages {
		return ex.msg.Rewrite(dst, body, model)
	}
	return ex.chat.Rewrite(dst, body, model)
}

// startStream readies the usage reader for a streamed response. filter
// reports that the stream goes through ex.filter: golem asked an OpenAI
// provider for usage the caller did not, and the usage-only chunk must be
// withheld (some SDKs fail on a chunk with no choices).
func (ex *exchange) startStream(d Dialect) (filter bool) {
	if d == AnthropicMessages {
		ex.msgUsage = messages.StreamUsage{}
		ex.parser.Want, ex.parser.OnEvent = ex.msgWant, ex.msgOn
		ex.parser.Reset()
		return false
	}
	ex.chatUsage = chat.StreamUsage{}
	if !ex.chat.InjectedUsage() {
		ex.parser.Want, ex.parser.OnEvent = nil, ex.chatOn
		ex.parser.Reset()
		return false
	}
	ex.filter.Want, ex.filter.OnEvent = nil, ex.chatOn
	ex.filter.Reset()
	return true
}

// readWhole reads a non-streamed response's usage from its kept bytes.
func (ex *exchange) readWhole(d Dialect) {
	if d == AnthropicMessages {
		ex.msgUsage = messages.StreamUsage{}
		ex.msgUsage.Response(ex.tail)
		return
	}
	ex.chatUsage = chat.StreamUsage{}
	ex.chatUsage.Response(ex.tail)
}

// usage is the response's usage and any error reading it, joined with cut.
func (ex *exchange) usage(d Dialect, cut error) (canon.Usage, error) {
	if d == AnthropicMessages {
		return ex.msgUsage.Usage, errors.Join(cut, ex.msgUsage.Err)
	}
	return ex.chatUsage.Usage, errors.Join(cut, ex.chatUsage.Err)
}

// appendError appends a dialect's error object for a contract code, with
// golem's code, a message that says what to do next, and the request ID
// (contracts.md §1).
func appendError(b []byte, d Dialect, c contract.Code, msg, reqID string) []byte {
	if d == AnthropicMessages {
		b = append(b, `{"type":"error","error":{"type":"`...)
		b = append(b, c.Name...)
	} else {
		b = append(b, `{"error":{"code":"`...)
		b = append(b, c.Name...)
		b = append(b, `","type":"`...)
		b = append(b, c.Origin...)
	}
	b = append(b, `","message":`...)
	b = appendQuoted(b, msg)
	b = append(b, `,"request_id":"`...)
	b = append(b, reqID...)
	return append(b, `"}}`...)
}

// streamError is upstream.stream_failed as an event in a dialect's stream.
func streamError(d Dialect, reqID string) []byte {
	const msg = "the provider's response was cut off before it completed; retry the request"
	var b []byte
	if d == AnthropicMessages {
		b = append(b, "event: error\n"...)
	}
	b = append(b, "data: "...)
	b = appendError(b, d, contract.UpstreamStreamFailed, msg, reqID)
	return append(b, "\n\n"...)
}
