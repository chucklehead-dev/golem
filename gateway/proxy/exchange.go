package proxy

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/gateway/dialect/anthropic/messages"
	"github.com/chucklehead-dev/golem/gateway/dialect/openai/chat"
	"github.com/chucklehead-dev/golem/gateway/dialect/openai/responses"
	"github.com/chucklehead-dev/golem/gateway/sse"
)

const copyBuffer = 8 << 10 // events are mostly under 1 KB; a burst takes more reads

// exchange is one request's working memory for the life of its stream,
// pooled: the scan results, the usage readers, the copy buffer. The request
// bodies are not in it: they go back to their pool as soon as they have been
// sent, so a long stream holds about the copy buffer and the parser's
// buffers, not a 200 KB agent request (PRD §7: under 64 KiB per stream).
type exchange struct {
	buf        []byte
	msg        messages.Request
	msgUsage   messages.StreamUsage
	chat       chat.Request
	chatUsage  chat.StreamUsage
	resp       responses.Request
	respUsage  responses.StreamUsage
	parser     sse.Parser
	filter     sse.Filter // an OpenAI stream's, when golem injected include_usage
	kept       []byte     // a non-streamed response, up to maxRetained bytes, for its usage
	overflow   bool       // the response outgrew kept: its usage is not read from a part
	callerGone bool       // a write to the caller failed
	received   int64      // the response's bytes so far (MaxResponse; an estimate's bound)
	// streamErr: the stream carried its dialect's error event (an
	// Anthropic or Bedrock event: error, an OpenAI chunk with "error"),
	// after its output began: the provider failed it, however it ended.
	streamErr bool

	// The usage readers' methods, bound once: a method value allocates
	// each time it is taken, and the readers stay at these addresses.
	msgWant               func([]byte) bool
	msgOn, chatOn, respOn func(event, data []byte)

	// The caller's side of the response: a write that blocks for the
	// stall is a caller gone (as nginx's send_timeout), watched by
	// watch, and no bound runs while the provider is silent.
	w     http.ResponseWriter
	watch writeWatch

	lim bounds // the route's limits over the upstream call, timers reused

	first firstEvent // a stream's bytes until its first event: the commit point

	call inFlightCall // the call's entry in Handler.InFlight, while admitted

	tried []bool // the deployments the call has attempted (pick)
}

// exchanges pools the per-request working memory; New binds the usage
// readers' methods once, at the addresses they keep.
var exchanges = sync.Pool{New: func() any {
	ex := &exchange{buf: make([]byte, copyBuffer)}
	// Each also notes the dialect's error event (streamErr); Anthropic's
	// usage reader wants none, so its error events are not passed to it.
	ex.msgWant = func(event []byte) bool { return string(event) == "error" || ex.msgUsage.Want(event) }
	ex.msgOn = func(event, data []byte) {
		if isErrorEvent(AnthropicMessages, event, data) {
			ex.streamErr = true
			return
		}
		ex.msgUsage.OnEvent(event, data)
	}
	ex.chatOn = func(event, data []byte) {
		ex.streamErr = ex.streamErr || isErrorEvent(OpenAIChat, event, data)
		ex.chatUsage.OnEvent(event, data)
	}
	ex.respOn = func(event, data []byte) {
		ex.streamErr = ex.streamErr || isErrorEvent(OpenAIResponses, event, data)
		ex.respUsage.OnEvent(event, data)
	}
	ex.filter.Drop = dropUsageOnly
	ex.first.parser.OnEvent = ex.first.onEvent
	ex.lim = newBounds()
	ex.watch.t = time.AfterFunc(time.Hour, ex.watch.check)
	ex.watch.t.Stop()
	return ex
}}

// getExchange takes a pooled exchange, its per-call state cleared; the
// rest is reset where a call starts using it (scan, relay, startStream,
// startWrites, bound).
func getExchange() *exchange {
	ex := exchanges.Get().(*exchange)
	ex.callerGone, ex.received, ex.streamErr = false, 0, false
	return ex
}

// release returns the exchange to the pool. The parser and filter keep
// bounded buffers (their Reset); kept is let go if a response grew it large.
func (ex *exchange) release() {
	ex.watch.disarm() // before the response is let go: the watchdog may cut only its own call
	ex.w = nil        // the pool must not keep a finished response
	if cap(ex.kept) > maxRetained {
		ex.kept = nil // a large response's copy is let go, not the exchange
	}
	exchanges.Put(ex)
}

// startWrites readies the exchange to write a response to w.
func (ex *exchange) startWrites(w http.ResponseWriter, stall time.Duration) {
	ex.w = w
	ex.watch.arm(http.NewResponseController(w), stall)
}

// send writes b to the caller (and flushes it, for a stream). A failed
// write, or one the caller has not taken within the stall (writeWatch), is
// the caller gone (ADR 0041 §7.6): send then reports false.
func (ex *exchange) send(b []byte, flush bool) bool {
	if len(b) == 0 {
		return true
	}
	ww := &ex.watch
	ww.started.Store(time.Now().UnixNano())
	_, err := ex.w.Write(b)
	if err == nil && flush {
		ww.rc.Flush() // a dead connection fails the next write
	}
	ww.started.Store(0)
	if err != nil {
		ex.callerGone = true
		return false
	}
	return true
}

// writeWatch bounds each write to the caller by the stall, armed once a
// call rather than a deadline set and cleared around each write. Under
// HTTP/2 (h2c is what the gateway serves) each SetWriteDeadline posts to
// the connection's serve goroutine, and clearing one makes the next set
// allocate a timer: per write, that was most of a stream's allocations.
//
// Each write stamps its start and clears the stamp when it returns; a
// reusable timer checks every half stall, and a write in progress for the
// stall or longer is cut by a deadline of now (which resets an HTTP/2
// stream and fails a blocked HTTP/1 write). A stalled write is so cut
// between one and one and a half stalls after it began.
//
// The timer is the pooled exchange's, reused from call to call. A check
// left over from an earlier call is harmless: it cuts only a write of the
// call armed now that has been in progress for the stall.
type writeWatch struct {
	t       *time.Timer
	started atomic.Int64 // UnixNano when the write in progress began; 0: none

	mu    sync.Mutex
	rc    *http.ResponseController // the call's; nil: not armed
	stall time.Duration
}

// arm watches rc's writes for the call; a stall of 0 bounds none.
func (ww *writeWatch) arm(rc *http.ResponseController, stall time.Duration) {
	ww.started.Store(0)
	ww.mu.Lock()
	ww.rc, ww.stall = rc, stall
	ww.mu.Unlock()
	if stall > 0 {
		ww.t.Reset(stall / 2)
	}
}

// disarm stops watching. Once it returns no check touches the call's
// response: a check holds the lock while it cuts.
func (ww *writeWatch) disarm() {
	ww.mu.Lock()
	ww.rc = nil
	ww.mu.Unlock()
	ww.t.Stop()
}

// check cuts a write in progress for the stall, and checks again in half
// a stall.
func (ww *writeWatch) check() {
	ww.mu.Lock()
	defer ww.mu.Unlock()
	if ww.rc == nil || ww.stall <= 0 {
		return
	}
	if s := ww.started.Load(); s != 0 && time.Now().UnixNano()-s >= int64(ww.stall) {
		ww.rc.SetWriteDeadline(time.Now()) // unsupported (a test's recorder): nothing to cut
	}
	ww.t.Reset(ww.stall / 2)
}

// forward sends one read of the response to the caller and to its usage
// reader: a stream's parser or filter, or a whole response's kept copy. It
// reports false if the caller is gone.
func (ex *exchange) forward(b []byte, streamed, filter bool) bool {
	switch {
	case filter:
		return ex.send(ex.filter.Pass(b), true)
	case streamed:
		ok := ex.send(b, true)
		ex.parser.Write(b) // the provider produced it, whether or not the caller took it
		return ok
	}
	if !ex.send(b, false) {
		return false
	}
	// Once a read does not fit, nothing after it is kept either: a later,
	// smaller read appended to the head would splice two parts of the body
	// into one the usage reader could misread.
	if ex.overflow = ex.overflow || len(ex.kept)+len(b) > maxRetained; !ex.overflow {
		ex.kept = append(ex.kept, b...)
	}
	return true
}

// dropUsageOnly withholds the usage-only chunk golem asked for: the caller
// did not, and some SDKs fail on a chunk with no choices.
func dropUsageOnly(_, data []byte) bool { return chat.UsageOnly(data) }

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

// upstreamBody is the rewritten request as the transport reads it; its
// buffer returns to the pool when the transport closes it, once sent. The
// HTTP/2 transport may close it from another goroutine while a write is
// still reading it (a stream reset), so the two are serialised: a buffer is
// never pooled while a read copies from it.
type upstreamBody struct {
	mu  sync.Mutex
	r   bytes.Reader
	buf *[]byte
}

func newUpstreamBody(buf *[]byte) *upstreamBody {
	u := &upstreamBody{buf: buf}
	u.r.Reset(*buf)
	return u
}

func (u *upstreamBody) Read(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.buf == nil {
		return 0, http.ErrBodyReadAfterClose
	}
	return u.r.Read(p)
}

// Close returns the buffer and drops every reference to it: the transport
// keeps the request, and so this body, for the whole response, and a
// reference left here would pin a 200 KB agent request per open stream.
func (u *upstreamBody) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.buf != nil {
		u.r.Reset(nil)
		putBody(u.buf)
		u.buf = nil
	}
	return nil
}
