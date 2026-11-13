package proxy

import (
	"bytes"
	"io"
	"net/http"
	"sync"

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
}

var exchanges = sync.Pool{New: func() any { return &exchange{buf: make([]byte, copyBuffer)} }}

func getExchange() *exchange {
	ex := exchanges.Get().(*exchange)
	ex.callerGone = false
	return ex
}

// release returns the exchange to the pool. tail is the one buffer an
// exchange grows and keeps (the parser and the filter are new for each
// stream), so a large one is let go.
func (ex *exchange) release() {
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
