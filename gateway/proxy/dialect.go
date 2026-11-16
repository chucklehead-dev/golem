package proxy

import (
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"strings"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/dialect/anthropic/messages"
	"github.com/chucklehead-dev/golem/gateway/dialect/openai/chat"
	"github.com/chucklehead-dev/golem/gateway/dialect/openai/responses"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// Everything the request path does differently per dialect is here, as
// methods of the pooled exchange: the rest of the path is dialect-free.

// Dialect selects the frontend a Handler serves.
type Dialect int

const (
	AnthropicMessages Dialect = iota // POST /v1/messages
	OpenAIChat                       // POST /v1/chat/completions
	OpenAIResponses                  // POST /v1/responses (stateless: store false)
)

// Name is the dialect's name in config (Backend spec.dialect).
func (d Dialect) Name() string {
	switch d {
	case OpenAIChat:
		return "openai.chat"
	case OpenAIResponses:
		return "openai.responses"
	}
	return "anthropic.messages"
}

// forwardRequest are the caller's headers its provider may see: an API's
// own, never another's (an anthropic-beta on an OpenAI call is the
// caller's mistake, not a feature to approve or pass on).
func (d Dialect) forwardRequest() []string {
	if d == AnthropicMessages {
		return anthropicForward
	}
	return openAIForward
}

var (
	anthropicForward = []string{"Anthropic-Version", "Anthropic-Beta"} // read-only
	openAIForward    = []string{"Openai-Organization"}                 // read-only
)

// approvedVersions are the anthropic-version values restricted mode
// forwards. 2026-10-21 (the default of the SDKs' 2026-10 majors) was
// reviewed against what golem reads and enforces: it changes no usage,
// stop or stream field, and no policy field of contracts.md §8; its
// removals (temperature, top_p, top_k; documents in assistant turns;
// citations in user turns) are the provider's refusals, forwarded as such.
// Its SDKs send the key as Authorization: Bearer, which auth accepts.
var approvedVersions = map[string]bool{"2023-06-01": true, "2026-10-21": true} // read-only

// restrictedHeaders checks, in restricted mode, the feature headers the
// dialect forwards: an Anthropic call's betas (requestBetas) must be
// approved on the route and its API version approved here. Another API
// forwards no anthropic-* header (forwardRequest), so it checks none.
func (d Dialect) restrictedHeaders(hdr http.Header, rt *route.Route, betas []string) *refusal {
	if d != AnthropicMessages {
		return nil
	}
	for _, b := range betas {
		if !rt.Betas[b] {
			return &refusal{code: contract.AuthzFeatureForbidden, msg: "anthropic-beta " + b + " is not approved on this route (restricted mode); its managers can approve it"}
		}
	}
	// Every value, as every value is forwarded: an approved first value
	// must not carry an unapproved second past the check.
	for _, v := range hdr.Values("Anthropic-Version") {
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); !approvedVersions[p] {
				return &refusal{code: contract.AuthzFeatureForbidden, msg: "anthropic-version " + p + " is not approved (restricted mode)"}
			}
		}
	}
	return nil
}

// scanned is what routing and admission need of a request body.
type scanned struct {
	alias     []byte // valid until the exchange's next scan
	stream    bool
	maxTokens int64
	hasMax    bool
	// What the scope's policy weighs (contracts.md §8).
	n           int64 // completions asked for
	store       bool  // OpenAI's store: true (rewritten off where forbidden)
	state       bool  // provider-side state with no off switch (an Anthropic container)
	serverTools bool  // tools the provider runs: egress, and no cost bound
	unknown     int   // members golem does not know (restricted mode refuses them)
	// stateful names a member needing state the provider keeps, which M1
	// refuses whatever the scope allows (security-and-correctness.md §4):
	// a Responses continuation, conversation, background run, stored
	// prompt or provider file; storeTrue, a store: true asked by name.
	stateful  string
	storeTrue bool
}

// scan reads the routing and policy fields of a request body.
func (ex *exchange) scan(d Dialect, body []byte) (scanned, error) {
	if d == AnthropicMessages {
		err := ex.msg.Scan(body)
		f := ex.msg.Flags
		return scanned{alias: ex.msg.Model, stream: ex.msg.Stream, maxTokens: ex.msg.MaxTokens, hasMax: ex.msg.HasMaxTokens,
			n: 1, state: f&messages.FlagContainer != 0, serverTools: f&(messages.FlagServerTools|messages.FlagMCPServers) != 0,
			unknown: ex.msg.Unknown}, err
	}
	if d == OpenAIResponses {
		err := ex.resp.Scan(body)
		r := &ex.resp
		return scanned{alias: r.Model, stream: r.Stream, maxTokens: r.MaxTokens, hasMax: r.HasMaxTokens, n: 1,
			serverTools: r.ServerTools, unknown: r.Unknown, stateful: r.Stateful, storeTrue: r.StoreTrue}, err
	}
	err := ex.chat.Scan(body)
	return scanned{alias: ex.chat.Model, stream: ex.chat.Stream, maxTokens: ex.chat.MaxTokens, hasMax: ex.chat.HasMaxTokens,
		n: ex.chat.N, store: ex.chat.Store, serverTools: ex.chat.Flags&chat.FlagWebSearch != 0, unknown: ex.chat.Unknown}, err
}

// forbidStorage turns the request's provider-side storage off (OpenAI's
// store: true becomes false, ADR 0027).
func (ex *exchange) forbidStorage(d Dialect) {
	if d == OpenAIChat {
		ex.chat.ForbidStorage()
	}
}

// rewrite appends the scanned body to dst for the provider: its model
// replaced and, for an OpenAI stream, usage requested.
func (ex *exchange) rewrite(d Dialect, dst, body []byte, model string) []byte {
	switch d {
	case AnthropicMessages:
		return ex.msg.Rewrite(dst, body, model)
	case OpenAIResponses:
		return ex.resp.Rewrite(dst, body, model) // store always false
	}
	return ex.chat.Rewrite(dst, body, model)
}

// startStream readies the usage reader for a streamed response. filter
// reports that the stream goes through ex.filter: golem asked an OpenAI
// provider for usage the caller did not, and the usage-only chunk must be
// withheld (some SDKs fail on a chunk with no choices).
func (ex *exchange) startStream(d Dialect) (filter bool) {
	switch d {
	case AnthropicMessages:
		ex.msgUsage = messages.StreamUsage{}
		ex.parser.Want, ex.parser.OnEvent = ex.msgWant, ex.msgOn
	case OpenAIResponses: // its terminal event carries usage: nothing to ask for
		ex.respUsage = responses.StreamUsage{}
		ex.parser.Want, ex.parser.OnEvent = nil, ex.respOn
	default:
		ex.chatUsage = chat.StreamUsage{}
		if ex.chat.InjectedUsage() {
			ex.filter.Want, ex.filter.OnEvent = nil, ex.chatOn
			ex.filter.Reset()
			return true
		}
		ex.parser.Want, ex.parser.OnEvent = nil, ex.chatOn
	}
	ex.parser.Reset()
	return false
}

// readWhole reads a non-streamed response's usage from its kept bytes; a
// response larger than those has its usage unread (errResponseUnread), and
// is charged its estimate.
func (ex *exchange) readWhole(d Dialect) {
	ex.msgUsage, ex.chatUsage, ex.respUsage = messages.StreamUsage{}, chat.StreamUsage{}, responses.StreamUsage{}
	switch {
	case ex.overflow && d == AnthropicMessages:
		ex.msgUsage.Err = errResponseUnread
	case ex.overflow && d == OpenAIResponses:
		ex.respUsage.Err = errResponseUnread
	case ex.overflow:
		ex.chatUsage.Err = errResponseUnread
	case d == AnthropicMessages:
		ex.msgUsage.Response(ex.kept)
	case d == OpenAIResponses:
		ex.respUsage.Response(ex.kept)
	default:
		ex.chatUsage.Response(ex.kept)
	}
}

var errResponseUnread = errors.New("proxy: a whole response over 1 MiB: its usage was not read")

// reported is what the dialect's usage reader has read: the usage so far,
// whether it is the provider's final report, and the first malformed one.
func (ex *exchange) reported(d Dialect) (u canon.Usage, final bool, err error) {
	switch d {
	case AnthropicMessages:
		return ex.msgUsage.Usage, ex.msgUsage.Final, ex.msgUsage.Err
	case OpenAIResponses:
		return ex.respUsage.Usage, ex.respUsage.Final, ex.respUsage.Err
	}
	return ex.chatUsage.Usage, ex.chatUsage.Final, ex.chatUsage.Err
}

// usage is the response's usage and any error reading it, joined with cut.
func (ex *exchange) usage(d Dialect, cut error) (canon.Usage, error) {
	u, _, err := ex.reported(d)
	return u, errors.Join(cut, err)
}

// estimate completes a response's usage when the provider's report is
// missing (a stream cut, or a response without its usage), by rules chosen
// to err high rather than to average: no text tokenizer golem serves makes
// a token of less than a byte, and every output token the provider sent is
// in the response's bytes, so output is every byte received, whatever its
// framing or fields, at most the request's output ceiling (max_tokens × n);
// input, if the provider gave none, every byte of the request (as
// price.Worst). It can still be low (a provider's work past a cut, input
// that bytes do not bound): an estimate, not a guarantee (ADR 0044). It
// replaces no count the provider gave; reconciliation corrects it (ADR
// 0031). It returns the usage to charge and the estimated part of it (zero
// when the report was whole).
func (ex *exchange) estimate(d Dialect, requestBytes, ceiling int64) (total, estimated canon.Usage) {
	u, final, _ := ex.reported(d)
	if final {
		return u, canon.Usage{}
	}
	total = u
	out := ex.received
	if ceiling > 0 {
		out = min(out, ceiling)
	}
	if out > u.Output {
		total.Output, estimated.Output = out, out-u.Output
	}
	if u.Input == 0 && u.CacheRead == 0 && u.CacheWrite == 0 { // no message_start, or OpenAI's (only at the end)
		total.Input = requestBytes
		estimated.Input = total.Input
	}
	return total, estimated
}

// outputCeiling is the most output the request allows, or 0 if it set no
// max_tokens (admission has checked the product cannot overflow).
func outputCeiling(req scanned) int64 {
	if !req.hasMax {
		return 0
	}
	return req.maxTokens * req.n
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

func appendQuoted(b []byte, s string) []byte {
	b, _ = jsontext.AppendQuote(b, s) // fails only on invalid UTF-8, which it replaces
	return b
}

// streamError is upstream.stream_failed as an event in a dialect's stream.
func streamError(d Dialect, reqID string) []byte {
	const msg = "the provider's response was cut off before it completed; retry the request"
	return errorEvent(d, contract.UpstreamStreamFailed, msg, reqID)
}

// errorEvent is a contract code as the error event of a dialect's stream:
// Anthropic's event: error, Chat's data-only error object, the Responses
// API's error event.
func errorEvent(d Dialect, c contract.Code, msg, reqID string) []byte {
	var b []byte
	if d == OpenAIResponses {
		b = append(b, `event: error`+"\n"+`data: {"type":"error","code":"`...)
		b = append(b, c.Name...)
		b = append(b, `","message":`...)
		b = appendQuoted(b, msg)
		b = append(b, `,"param":null,"request_id":"`...)
		b = append(b, reqID...)
		return append(b, `"}`+"\n\n"...)
	}
	if d == AnthropicMessages {
		b = append(b, "event: error\n"...)
	}
	b = append(b, "data: "...)
	b = appendError(b, d, c, msg, reqID)
	return append(b, "\n\n"...)
}
