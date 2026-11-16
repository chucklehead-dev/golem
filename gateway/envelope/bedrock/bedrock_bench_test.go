package bedrock

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// BenchmarkReframe is a 200-delta Bedrock stream reframed as Messages SSE,
// read with an 8 KiB buffer as the relay reads it.
func BenchmarkReframe(b *testing.B) {
	evs := []string{events[0]}
	for range 200 {
		evs = append(evs, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"some generated words "}}`)
	}
	evs = append(evs, events[2], events[3])
	stream := encode(b, evs, "")
	buf := make([]byte, 8<<10)
	var br bytes.Reader
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for b.Loop() {
		br.Reset(stream)
		f := &reframer{src: io.NopCloser(&br), dec: eventstream.NewDecoder()}
		for {
			if _, err := f.Read(buf); err != nil {
				if err != io.EOF {
					b.Fatal(err)
				}
				break
			}
		}
	}
}

// answered is a Base that answers every request at once, reading nothing.
type answered struct{}

func (answered) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{}}`))}, nil
}

// BenchmarkRequest is an agent-sized (about 190 KB) Messages request
// through the envelope: read, rewritten for Bedrock, signed.
func BenchmarkRequest(b *testing.B) {
	var body bytes.Buffer
	body.WriteString(`{"model":"m","max_tokens":32000,"stream":true,"messages":[`)
	for i := range 120 {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"role":"user","content":[{"type":"text","text":%q}]}`, strings.Repeat("func main() { fmt.Println(\"hello\") }\n", 40))
	}
	body.WriteString(`]}`)
	t := transport(answered{})
	b.SetBytes(int64(body.Len()))
	b.ReportAllocs()
	for b.Loop() {
		r, _ := http.NewRequest("POST", "https://x/v1/messages", bytes.NewReader(body.Bytes()))
		resp, err := t.RoundTrip(r)
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}
