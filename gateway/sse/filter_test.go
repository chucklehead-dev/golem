package sse

import (
	"bytes"
	"strings"
	"testing"
)

// filter runs a stream through a Filter in chunks of n bytes.
func filter(in string, n int, drop func(event, data []byte) bool) (string, []string) {
	var seen []string
	f := Filter{Drop: drop}
	f.OnEvent = func(_, d []byte) { seen = append(seen, string(d)) }
	var out bytes.Buffer
	for b := []byte(in); len(b) > 0; {
		k := min(n, len(b))
		out.Write(f.Pass(b[:k]))
		b = b[k:]
	}
	out.Write(f.End())
	return out.String(), seen
}

func TestFilter(t *testing.T) {
	usage := "data: {\"usage\":1}\n\n"
	for _, tc := range []struct{ name, in, want string }{
		{"drops the one event", "data: a\n\n" + usage + "data: [DONE]\n\n", "data: a\n\ndata: [DONE]\n\n"},
		{"keeps comments and keep-alives", ": ping\n\ndata: a\n\n: ping\n\n" + usage, ": ping\n\ndata: a\n\n: ping\n\n"},
		{"keeps unknown fields and ids", "id: 7\nretry: 10\ndata: a\n\n", "id: 7\nretry: 10\ndata: a\n\n"},
		{"CRLF", "data: a\r\n\r\n" + strings.ReplaceAll(usage, "\n", "\r\n") + "data: b\r\n\r\n", "data: a\r\n\r\ndata: b\r\n\r\n"},
		{"keeps an unterminated last event", "data: a\n\ndata: tail", "data: a\n\ndata: tail"},
		{"keeps an oversized event", "data: " + strings.Repeat("x", MaxEvent+10) + "\n\n" + usage,
			"data: " + strings.Repeat("x", MaxEvent+10) + "\n\n"},
	} {
		for _, n := range []int{1, 2, 3, 7, 64, 1 << 21} {
			got, _ := filter(tc.in, n, func(_, d []byte) bool { return string(d) == `{"usage":1}` })
			if got != tc.want {
				t.Fatalf("%s, chunks of %d: got %q, want %q", tc.name, n, trunc(got), trunc(tc.want))
			}
		}
	}
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// FuzzFilter: dropping nothing forwards the stream unchanged, however it is
// chunked, and the parser sees the same events as an unfiltered one.
func FuzzFilter(f *testing.F) {
	f.Add("event: a\ndata: x\n\n: c\r\ndata: y\r\rdata: z", 3)
	f.Add("data: a\r", 1)
	f.Fuzz(func(t *testing.T, in string, n int) {
		n = 1 + abs(n)%17
		got, seen := filter(in, n, func(_, _ []byte) bool { return false })
		if got != in {
			t.Fatalf("chunks of %d: %q became %q", n, in, got)
		}
		var plain []string
		p := Parser{OnEvent: func(_, d []byte) { plain = append(plain, string(d)) }}
		p.Write([]byte(in))
		if strings.Join(plain, "\x00") != strings.Join(seen, "\x00") {
			t.Fatalf("events %q, unfiltered %q", seen, plain)
		}
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TestReset: a reset parser or filter carries nothing of the last stream
// into the next: not a partial line, an event's data, a pending "\r", or an
// event being withheld.
func TestReset(t *testing.T) {
	var got []string
	f := Filter{Drop: func(_, d []byte) bool { return string(d) == "drop" }}
	f.OnEvent = func(_, d []byte) { got = append(got, string(d)) }
	f.Pass([]byte("data: drop\ndata: half\r"))
	f.Reset()
	out := string(f.Pass([]byte("\ndata: x\n\n"))) + string(f.End())
	if out != "\ndata: x\n\n" || len(got) != 1 || got[0] != "x" {
		t.Fatalf("after Reset: forwarded %q, events %q", out, got)
	}
}
