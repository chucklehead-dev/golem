package bedrock

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// TestStreamDecoder: messages the AWS SDK encodes, with headers of every
// type around the ones read, decode to the same headers and payload; a
// flipped bit in the prelude or anywhere after it fails a checksum; a
// stream cut anywhere within a message is io.ErrUnexpectedEOF, and only
// one cut between messages is io.EOF.
func TestStreamDecoder(t *testing.T) {
	var m eventstream.Message
	m.Headers.Set("flag", eventstream.BoolValue(true))
	m.Headers.Set("off", eventstream.BoolValue(false))
	m.Headers.Set("b", eventstream.Int8Value(-1))
	m.Headers.Set("s", eventstream.Int16Value(2))
	m.Headers.Set(":message-type", eventstream.StringValue("event"))
	m.Headers.Set("i", eventstream.Int32Value(3))
	m.Headers.Set("l", eventstream.Int64Value(4))
	m.Headers.Set("raw", eventstream.BytesValue([]byte{0, 1, 2}))
	m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
	m.Headers.Set("at", eventstream.TimestampValue(time.UnixMilli(5)))
	m.Headers.Set("id", eventstream.UUIDValue{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	m.Headers.Set(":exception-type", eventstream.StringValue("throttlingException"))
	m.Headers.Set(":error-code", eventstream.StringValue("E"))
	m.Payload = []byte(`{"bytes":"e30="}`)
	var stream bytes.Buffer
	enc := eventstream.NewEncoder()
	for range 2 {
		if err := enc.Encode(&stream, m); err != nil {
			t.Fatal(err)
		}
	}
	all := stream.Bytes()
	one := len(all) / 2

	var d streamDecoder
	var got message
	src := bytes.NewReader(all)
	for i := range 2 {
		if err := d.next(src, &got); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if string(got.messageType) != "event" || string(got.eventType) != "chunk" || string(got.exceptionType) != "throttlingException" ||
			string(got.errorCode) != "E" || string(got.payload) != string(m.Payload) {
			t.Fatalf("message %d: %q %q %q %q %q", i, got.messageType, got.eventType, got.exceptionType, got.errorCode, got.payload)
		}
	}
	if err := d.next(src, &got); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last message: %v, want io.EOF", err)
	}

	for i := range one * 8 {
		bad := bytes.Clone(all[:one])
		bad[i/8] ^= 1 << (i % 8)
		err := d.next(bytes.NewReader(bad), &got)
		if err == nil {
			t.Fatalf("bit %d of byte %d flipped: decoded", i%8, i/8)
		}
		// A length is not trusted (nor its buffer allocated) before the
		// prelude's own checksum holds.
		if i/8 < preludeLen && !strings.Contains(err.Error(), "prelude") {
			t.Fatalf("bit %d of byte %d flipped: %v, want the prelude's checksum to fail", i%8, i/8, err)
		}
	}
	for n := 1; n < one; n++ {
		if err := d.next(bytes.NewReader(all[:n]), &got); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut after %d of %d bytes: %v, want io.ErrUnexpectedEOF", n, one, err)
		}
	}
}

// TestStreamDecoderMalformed: a message whose checksums hold but whose
// framing or headers do not is refused, not read past its end.
func TestStreamDecoderMalformed(t *testing.T) {
	for name, headers := range map[string][]byte{
		"a name past the end":        {9, 'a'},
		"no value type":              {1, 'a'},
		"an unknown value type":      {1, 'a', 10},
		"a string past the end":      {1, 'a', 7, 0, 9, 'x'},
		"a string with no length":    {1, 'a', 7, 0},
		"a fixed value past the end": {1, 'a', 4, 0, 0},
	} {
		var d streamDecoder
		var got message
		if err := d.next(bytes.NewReader(frame(headers, nil)), &got); !errors.Is(err, errMalformed) {
			t.Errorf("%s: %v, want errMalformed", name, err)
		}
	}
	var d streamDecoder
	var got message
	short := frame(nil, nil)
	copy(short[4:], []byte{0, 0, 0, 1}) // more headers than the message holds
	resum(short)
	if err := d.next(bytes.NewReader(short), &got); !errors.Is(err, errMalformed) {
		t.Errorf("headers longer than the message: %v, want errMalformed", err)
	}
}

// frame is an event-stream message of raw headers and payload, its
// checksums correct.
func frame(headers, payload []byte) []byte {
	total := preludeLen + len(headers) + len(payload) + 4
	b := make([]byte, total)
	b[0], b[1], b[2], b[3] = byte(total>>24), byte(total>>16), byte(total>>8), byte(total)
	n := len(headers)
	b[4], b[5], b[6], b[7] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(b[preludeLen:], headers)
	copy(b[preludeLen+len(headers):], payload)
	resum(b)
	return b
}

// resum sets a message's two checksums for its contents.
func resum(b []byte) {
	put := func(at int, v uint32) {
		b[at], b[at+1], b[at+2], b[at+3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	put(8, crc32.ChecksumIEEE(b[:8]))
	put(len(b)-4, crc32.ChecksumIEEE(b[:len(b)-4]))
}

// TestMemberScanner: the reframer's scanner finds what member, the
// allocating reader of error bodies, finds: names and values unescaped,
// the first string so named, nothing in what is not an object.
func TestMemberScanner(t *testing.T) {
	var s memberScanner
	for _, raw := range []string{
		`{"bytes":"e30=","p":"abc"}`, `{"p":1,"bytes":"e30="}`, `{"bytes":1,"bytes":"x"}`,
		`{"bytes":null}`, `{"a":{"bytes":"inner"}}`, `["bytes"]`, `{"bytes":"x"`, `{}`, ``, `{"bytes":""}`,
	} {
		want, wantOK := member([]byte(raw), "bytes")
		got, ok := s.member([]byte(raw), "bytes")
		if string(got) != want || ok != wantOK {
			t.Errorf("%s: %q %v, want %q %v", raw, got, ok, want, wantOK)
		}
	}
}
