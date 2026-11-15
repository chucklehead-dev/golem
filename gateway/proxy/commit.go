package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/chucklehead-dev/golem/gateway/sse"
)

// The commit point (m1-plan.md step 1). A streamed attempt is committed to
// its caller when its first event is written, not when its provider's
// headers arrive: a provider may answer 200 and then send an error as its
// first event (Anthropic's overloaded_error, a rate limit), and an attempt
// that failed so has produced nothing and told the caller nothing. Until
// then the gateway writes nothing; the attempt can still be refused
// cleanly, and (with the attempt loop) retried. Every envelope delivers its
// dialect's own framing, a failure before output included as the dialect's
// error event, so this is the one place an attempt's commitment is decided.
//
// Only the first event is held (bounded by maxFirst; past it the attempt is
// committed whatever it holds): nothing else of a stream is buffered.

// maxFirst bounds the bytes held while the first event is awaited: a first
// event is small (Anthropic's message_start, OpenAI's role chunk).
const maxFirst = 64 << 10

// firstEvent holds a stream's bytes until its first event is complete, and
// then replays them ahead of the rest of the body (an io.Reader).
type firstEvent struct {
	parser sse.Parser
	d      Dialect
	done   bool // the first event is complete
	failed bool // and it was an error
	held   []byte
	off    int   // how much of held Read has replayed
	err    error // the read that ended the wait, if it failed
	body   io.Reader
}

// await reads body until its first event is complete (or it ends, fails,
// or exceeds maxFirst), reading into buf. It reports whether that event
// was an error: the attempt failed before commit. Either way f is then the
// body to relay, its held bytes first.
func (f *firstEvent) await(d Dialect, body io.Reader, buf []byte) (failed bool) {
	f.d, f.done, f.failed, f.err, f.body, f.off = d, false, false, nil, body, 0
	f.held = keep(f.held)
	f.parser.Reset()
	for !f.done && len(f.held) < maxFirst {
		n, err := body.Read(buf)
		f.held = append(f.held, buf[:n]...)
		f.parser.Write(buf[:n])
		if err != nil {
			f.err = err
			break
		}
	}
	return f.failed
}

func (f *firstEvent) onEvent(event, data []byte) {
	if !f.done {
		f.done = true
		f.failed = isErrorEvent(f.d, event, data)
	}
}

// Read replays the held bytes, then the read error that ended the wait if
// there was one, then the body.
func (f *firstEvent) Read(p []byte) (int, error) {
	if f.off < len(f.held) {
		n := copy(p, f.held[f.off:])
		f.off += n
		return n, nil
	}
	if f.err != nil {
		return 0, f.err
	}
	return f.body.Read(p)
}

// keep is a held buffer to reuse, if it is small.
func keep(b []byte) []byte {
	if cap(b) > 16<<10 {
		return nil
	}
	return b[:0]
}

// isErrorEvent reports whether a stream's event is the dialect's error:
// Anthropic's event: error, or an OpenAI chunk whose first member is
// "error".
func isErrorEvent(d Dialect, event, data []byte) bool {
	if d == AnthropicMessages {
		return string(event) == "error"
	}
	data = bytes.TrimLeft(data, " \t\r\n")
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	data = bytes.TrimLeft(data[1:], " \t\r\n")
	return bytes.HasPrefix(data, []byte(`"error"`))
}

// providerRetryAfter is a provider's response's Retry-After in seconds,
// or 0.
func providerRetryAfter(resp *http.Response) time.Duration {
	s, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || s <= 0 {
		return 0
	}
	return time.Duration(min(s, 3600)) * time.Second
}
