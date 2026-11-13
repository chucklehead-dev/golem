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
	parser     sse.Parser
	filter     sse.Filter // an OpenAI stream's, when golem injected include_usage
	tail       []byte     // a non-streamed response, for its usage
	callerGone bool       // a write to the caller failed

	// The usage readers' methods, bound once: a method value allocates
	// each time it is taken, and the readers stay at these addresses.
	msgWant       func([]byte) bool
	msgOn, chatOn func(event, data []byte)

	// The write stall: no write to the caller may stay blocked longer than
	// stall. A watchdog ticking every half stall counts the writes started
	// and finished, and cuts one still in flight at three ticks running (by
	// a deadline in the past). Each write costs two atomic adds; a timer per
	// write cost a third of the fast path. A deadline left set between
	// writes would not do: under HTTP/2 it resets the stream when it passes,
	// written to or not, and a provider may be silent longer than stall.
	w                 http.ResponseWriter
	rc                *http.ResponseController
	stall             time.Duration
	started, finished atomic.Uint64
	watchdog          *time.Timer
	watchMu           sync.Mutex // guards the rest, for the watchdog
	watching          bool
	lastStarted       uint64
	seen              int
}

var exchanges = sync.Pool{New: func() any {
	ex := &exchange{buf: make([]byte, copyBuffer)}
	ex.msgWant, ex.msgOn, ex.chatOn = ex.msgUsage.Want, ex.msgUsage.OnEvent, ex.chatUsage.OnEvent
	ex.filter.Drop = dropUsageOnly
	ex.watchdog = time.AfterFunc(time.Hour, ex.tick)
	ex.watchdog.Stop()
	return ex
}}

// startWrites readies the exchange to write a response to w, and starts
// the watchdog; endWrites stops it.
func (ex *exchange) startWrites(w http.ResponseWriter, stall time.Duration) {
	ex.watchMu.Lock()
	defer ex.watchMu.Unlock()
	ex.w, ex.rc, ex.stall = w, http.NewResponseController(w), stall
	ex.watching, ex.seen = true, 0
	ex.watchdog.Reset(stall / 2)
}

func (ex *exchange) endWrites() {
	ex.watchMu.Lock()
	defer ex.watchMu.Unlock()
	ex.watching = false
	ex.watchdog.Stop()
}

// write writes b to the caller (and flushes it, for a stream).
func (ex *exchange) write(b []byte, flush bool) error {
	ex.started.Add(1)
	_, err := ex.w.Write(b)
	if err == nil && flush {
		ex.rc.Flush() // a dead connection fails the next write
	}
	ex.finished.Add(1)
	return err
}

// tick is the watchdog: the same write in flight at three ticks half a
// stall apart has been blocked at least the stall, and is cut.
func (ex *exchange) tick() {
	ex.watchMu.Lock()
	defer ex.watchMu.Unlock()
	if !ex.watching {
		return
	}
	s := ex.started.Load()
	if ex.finished.Load() < s && s == ex.lastStarted {
		ex.seen++
	} else {
		ex.seen = 0
	}
	ex.lastStarted = s
	if ex.seen >= 2 {
		ex.rc.SetWriteDeadline(time.Now()) // the blocked write fails: the caller is gone
		ex.watching = false
		return
	}
	ex.watchdog.Reset(ex.stall / 2)
}

// send forwards a stream's bytes to the caller as they come; a failed or
// stalled write is the caller gone (ADR 0041 §7.6).
func (ex *exchange) send(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if err := ex.write(b, true); err != nil {
		ex.callerGone = true
		return false
	}
	return true
}

// dropUsageOnly withholds the usage-only chunk golem asked for: the caller
// did not, and some SDKs fail on a chunk with no choices.
func dropUsageOnly(_, data []byte) bool { return chat.UsageOnly(data) }

func getExchange() *exchange {
	ex := exchanges.Get().(*exchange)
	ex.callerGone = false
	return ex
}

// release returns the exchange to the pool. The parser and filter keep
// bounded buffers (their Reset); tail is let go if a response grew it large.
func (ex *exchange) release() {
	ex.endWrites()
	ex.watchMu.Lock()
	ex.w, ex.rc = nil, nil // the pool must not keep a finished response
	ex.watchMu.Unlock()
	if cap(ex.tail) <= maxRetained {
		exchanges.Put(ex)
	}
}

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
