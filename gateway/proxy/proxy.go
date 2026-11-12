// Package proxy is the request path (gateway.md §3). In this spike it is
// the fast path alone: read and scan the body, route by alias, splice the
// model, send upstream, and stream the response back through a tee that
// reads usage. Authentication, limits, hooks, retries and telemetry are
// later steps at the marked places.
package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/dialect/anthropic/messages"
	"github.com/chucklehead-dev/golem/gateway/dialect/openai/chat"
	"github.com/chucklehead-dev/golem/gateway/sse"
)

// Dialect selects the frontend a Handler serves.
type Dialect int

const (
	AnthropicMessages Dialect = iota // POST /v1/messages
	OpenAIChat                       // POST /v1/chat/completions
)

// Route is where an alias goes: the provider's model name, the endpoint,
// the transport and the headers that authenticate golem to the provider.
type Route struct {
	Model     string
	URL       *url.URL
	Transport http.RoundTripper
	Header    http.Header
}

// Result is what a finished call reports: the input to pricing, the
// counters and the access log (gateway.md §3 step 9).
type Result struct {
	Route  *Route
	Status int
	Usage  canon.Usage
	Err    error // a malformed usage report or a cut stream
}

// Handler serves one dialect's endpoint.
type Handler struct {
	Dialect Dialect
	Routes  map[string]*Route // by alias; a snapshot's, read-only
	MaxBody int64             // request body cap in bytes
	Record  func(*http.Request, Result)
}

// Request headers a caller may pass to the provider, and response headers
// returned to the caller (contracts.md §5: golem's own are never forwarded).
var (
	forwardRequest  = []string{"Anthropic-Version", "Anthropic-Beta", "Openai-Organization"} // read-only
	forwardResponse = []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After"}  // read-only
)

const copyBuffer = 16 << 10

// exchange is one request's working memory for the life of its stream,
// pooled: the scan results, the usage readers, the copy buffer. The request
// bodies are not in it: they go back to their pool as soon as they have been
// sent, so a long stream holds about the copy buffer and the parser's
// buffers, not a 200 KB agent request (PRD §7: under 64 KiB per stream).
type exchange struct {
	buf       []byte
	msg       messages.Request
	msgUsage  messages.StreamUsage
	chat      chat.Request
	chatUsage chat.StreamUsage
	parser    sse.Parser
	tail      []byte // a non-streamed response, for its usage
}

var exchanges = sync.Pool{New: func() any { return &exchange{buf: make([]byte, copyBuffer)} }}

// bodies pools request body buffers. maxRetained bounds what an idle entry
// keeps, so one large request does not pin its size in the pool.
var bodies = sync.Pool{New: func() any { b := make([]byte, 0, 16<<10); return &b }}

const maxRetained = 1 << 20

func getBody() *[]byte { return bodies.Get().(*[]byte) }

func putBody(b *[]byte) {
	if cap(*b) <= maxRetained {
		*b = (*b)[:0]
		bodies.Put(b)
	}
}

// upstreamBody is the rewritten request as the transport reads it; its
// buffer returns to the pool when the transport closes it, once sent.
type upstreamBody struct {
	bytes.Reader
	buf  *[]byte
	once sync.Once
}

func (u *upstreamBody) Close() error {
	u.once.Do(func() { putBody(u.buf) })
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ex := exchanges.Get().(*exchange)
	defer func() {
		if cap(ex.tail) <= maxRetained {
			exchanges.Put(ex)
		}
	}()

	in := getBody()
	defer func() {
		if in != nil {
			putBody(in)
		}
	}()
	if n := r.ContentLength; n > 0 && n <= h.MaxBody && int64(cap(*in)) < n+1 {
		*in = make([]byte, 0, n+1) // one allocation, not a doubling series
	}
	var err error
	*in, err = readAll((*in)[:0], http.MaxBytesReader(w, r.Body, h.MaxBody))
	body := *in
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.fail(w, http.StatusRequestEntityTooLarge, "request.too_large", "the request body is larger than this gateway accepts")
			return
		}
		h.fail(w, http.StatusBadRequest, "request.unreadable", "the request body could not be read")
		return
	}

	// Scan and route. (Authentication, hooks and limits go here.)
	var alias []byte
	var stream bool
	switch h.Dialect {
	case AnthropicMessages:
		err, alias, stream = ex.msg.Scan(body), ex.msg.Model, ex.msg.Stream
	case OpenAIChat:
		err, alias, stream = ex.chat.Scan(body), ex.chat.Model, ex.chat.Stream
	}
	if err != nil {
		h.fail(w, http.StatusBadRequest, "request.invalid", err.Error())
		return
	}
	route := h.Routes[string(alias)]
	if route == nil {
		h.fail(w, http.StatusNotFound, "route.unknown_model", "no route for this model; ask the scope's managers which aliases it may use")
		return
	}
	out := getBody()
	if cap(*out) < len(body)+len(route.Model)+64 {
		*out = make([]byte, 0, len(body)+len(route.Model)+64)
	}
	switch h.Dialect {
	case AnthropicMessages:
		*out = ex.msg.Rewrite((*out)[:0], body, route.Model)
	case OpenAIChat:
		*out = ex.chat.Rewrite((*out)[:0], body, route.Model)
	}
	putBody(in) // the caller's body is no longer needed
	in = nil

	// One attempt. (Retries and failover before the first byte go here; a
	// retry needs the rewritten body kept until the attempt is committed.)
	ub := &upstreamBody{buf: out}
	ub.Reset(*out)
	up := &http.Request{
		Method:        http.MethodPost,
		URL:           route.URL,
		Header:        make(http.Header, 4+len(route.Header)),
		Body:          ub,
		ContentLength: int64(len(*out)),
		Host:          route.URL.Host,
	}
	up = up.WithContext(r.Context())
	up.Header["Content-Type"] = jsonType
	for _, k := range forwardRequest {
		if v := r.Header[k]; v != nil {
			up.Header[k] = v
		}
	}
	for k, v := range route.Header {
		up.Header[k] = v
	}
	resp, err := route.Transport.RoundTrip(up)
	if err != nil {
		ub.Close()
		h.fail(w, http.StatusBadGateway, "upstream.unavailable", "the provider could not be reached")
		return
	}
	defer resp.Body.Close()

	for _, k := range forwardResponse {
		if v := resp.Header[k]; v != nil {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)

	res := Result{Route: route, Status: resp.StatusCode}
	if stream && resp.StatusCode == http.StatusOK {
		res.Usage, res.Err = h.stream(w, resp.Body, ex)
	} else {
		res.Usage, res.Err = h.whole(w, resp.Body, ex)
	}
	if h.Record != nil {
		h.Record(r, res)
	}
}

var jsonType = []string{"application/json"} // read-only

// stream copies the response as it arrives, flushing each read, and feeds
// the same bytes to the dialect's usage reader.
func (h *Handler) stream(w http.ResponseWriter, body io.Reader, ex *exchange) (canon.Usage, error) {
	rc := http.NewResponseController(w)
	ex.parser = sse.Parser{}
	switch h.Dialect {
	case AnthropicMessages:
		ex.msgUsage = messages.StreamUsage{}
		ex.parser.Want, ex.parser.OnEvent = ex.msgUsage.Want, ex.msgUsage.OnEvent
	case OpenAIChat:
		ex.chatUsage = chat.StreamUsage{}
		ex.parser.OnEvent = ex.chatUsage.OnEvent
	}
	var cut error
	for {
		n, err := body.Read(ex.buf)
		if n > 0 {
			if _, werr := w.Write(ex.buf[:n]); werr != nil {
				cut = werr // the caller went away (cancellation: ADR 0041 §7.6)
				break
			}
			rc.Flush()
			ex.parser.Write(ex.buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			cut = err
			break
		}
	}
	return h.usage(ex, cut)
}

// whole copies a non-streamed response, keeping a copy of up to maxRetained
// bytes to read its usage from.
func (h *Handler) whole(w http.ResponseWriter, body io.Reader, ex *exchange) (canon.Usage, error) {
	ex.tail = ex.tail[:0]
	var cut error
	for {
		n, err := body.Read(ex.buf)
		if n > 0 {
			if _, werr := w.Write(ex.buf[:n]); werr != nil {
				cut = werr
				break
			}
			if len(ex.tail)+n <= maxRetained {
				ex.tail = append(ex.tail, ex.buf[:n]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			cut = err
			break
		}
	}
	switch h.Dialect {
	case AnthropicMessages:
		ex.msgUsage = messages.StreamUsage{}
		ex.msgUsage.Response(ex.tail)
	case OpenAIChat:
		ex.chatUsage = chat.StreamUsage{}
		ex.chatUsage.Response(ex.tail)
	}
	return h.usage(ex, cut)
}

func (h *Handler) usage(ex *exchange, cut error) (canon.Usage, error) {
	switch h.Dialect {
	case AnthropicMessages:
		return ex.msgUsage.Usage, errors.Join(cut, ex.msgUsage.Err)
	default:
		return ex.chatUsage.Usage, errors.Join(cut, ex.chatUsage.Err)
	}
}

// fail writes a refusal in the caller's dialect (contracts.md §1).
func (h *Handler) fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header()["Content-Type"] = jsonType
	w.WriteHeader(status)
	var b []byte
	switch h.Dialect {
	case AnthropicMessages:
		b = append(b, `{"type":"error","error":{"type":"`...)
		b = append(b, code...)
		b = append(b, `","message":`...)
	default:
		b = append(b, `{"error":{"code":"`...)
		b = append(b, code...)
		b = append(b, `","message":`...)
	}
	b = appendQuoted(b, msg)
	b = append(b, "}}"...)
	w.Write(b)
}

// readAll reads r into dst, growing it as needed.
func readAll(dst []byte, r io.Reader) ([]byte, error) {
	for {
		if len(dst) == cap(dst) {
			dst = append(dst, 0)[:len(dst)]
		}
		n, err := r.Read(dst[len(dst):cap(dst)])
		dst = dst[:len(dst)+n]
		if err == io.EOF {
			return dst, nil
		}
		if err != nil {
			return dst, err
		}
	}
}
