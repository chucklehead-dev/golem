package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The provider side of the tests: canned requests and streams.

// smallRequest is a short chat turn (about 1 KB).
var smallRequest = []byte(`{"model":"claude","max_tokens":1024,"stream":true,"system":"You are a helpful assistant.","messages":[{"role":"user","content":"` + strings.Repeat("Summarise the attached text. ", 30) + `"}]}`)

// agentRequest is a coding agent's turn: a long system prompt, tool
// definitions and history (about 200 KB, typical of Claude Code mid-session).
var agentRequest = func() []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"claude","max_tokens":32000,"stream":true,"system":[{"type":"text","text":`)
	fmt.Fprintf(&b, "%q", strings.Repeat("You are an agent for software engineering. Follow the instructions. ", 400))
	b.WriteString(`,"cache_control":{"type":"ephemeral"}}],"tools":[`)
	for i := range 40 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":%q,"input_schema":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path"]}}`, i, strings.Repeat("Reads or writes a file. ", 20))
	}
	b.WriteString(`],"messages":[`)
	for i := range 120 {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		fmt.Fprintf(&b, `{"role":%q,"content":[{"type":"text","text":%q}]}`, role, strings.Repeat("func main() { fmt.Println(\"hello\") }\n", 30))
	}
	b.WriteString(`]}`)
	return b.Bytes()
}()

// anthropicStream is a streamed answer of n text deltas, one event per Read
// as a provider flushes them.
func anthropicStream(n int) [][]byte {
	ev := [][]byte{[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet\",\"usage\":{\"input_tokens\":2048,\"cache_read_input_tokens\":40000,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")}
	for range n {
		ev = append(ev, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"some generated words \"}}\n\n"))
	}
	ev = append(ev,
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
		[]byte(fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", n*4)),
		[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	return ev
}

// chunks reads one chunk per Read call.
type chunks struct {
	c [][]byte
	i int
}

func (c *chunks) Read(p []byte) (int, error) {
	if c.i == len(c.c) {
		return 0, io.EOF
	}
	n := copy(p, c.c[c.i])
	if n == len(c.c[c.i]) {
		c.i++
	} else {
		c.c[c.i] = c.c[c.i][n:]
	}
	return n, nil
}

// stub is an in-memory provider: it records the request body and answers
// with the given chunks.
type stub struct {
	events [][]byte
	got    []byte
	ch     chunks
	header http.Header
}

func (s *stub) RoundTrip(r *http.Request) (*http.Response, error) {
	s.got, _ = readAll(s.got[:0], r.Body)
	r.Body.Close() // as a transport does once the body is sent
	s.ch = chunks{c: s.events}
	if s.header == nil {
		s.header = http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {"req_1"}}
	}
	return &http.Response{StatusCode: 200, Header: s.header, Body: io.NopCloser(&s.ch)}, nil
}

func flat(c [][]byte) []byte { return bytes.Join(c, nil) }
