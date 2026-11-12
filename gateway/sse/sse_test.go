package sse

import (
	"fmt"
	"slices"
	"testing"
)

func events(chunks ...string) []string {
	var got []string
	p := Parser{OnEvent: func(e, d []byte) { got = append(got, fmt.Sprintf("%s|%s", e, d)) }}
	for _, c := range chunks {
		p.Write([]byte(c))
	}
	return got
}

func TestParser(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"one event", "event: a\ndata: x\n\n", []string{"a|x"}},
		{"no name", "data: x\n\n", []string{"|x"}},
		{"lines joined", "data: x\ndata: y\n\n", []string{"|x\ny"}},
		{"CRLF", "event: a\r\ndata: x\r\n\r\n", []string{"a|x"}},
		{"CR", "data: x\rdata: y\r\r", []string{"|x\ny"}},
		{"comments and keep-alives", ": ping\n\ndata: x\n\n", []string{"|x"}},
		{"no space after colon", "data:x\n\n", []string{"|x"}},
		{"unterminated last event", "data: x\n", nil},
		{"name alone dispatches nothing", "event: a\n\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := events(tc.in); !slices.Equal(got, tc.want) {
				t.Fatalf("events = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWantSkips(t *testing.T) {
	var got []string
	p := Parser{
		Want:    func(e []byte) bool { return string(e) == "keep" },
		OnEvent: func(e, d []byte) { got = append(got, string(e)+"|"+string(d)) },
	}
	p.Write([]byte("event: skip\ndata: 1\n\nevent: keep\ndata: 2\n\n"))
	if want := []string{"keep|2"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestMaxEvent(t *testing.T) {
	var p Parser
	n := 0
	p.OnEvent = func(_, _ []byte) { n++ }
	big := make([]byte, MaxEvent+1)
	for i := range big {
		big[i] = 'x'
	}
	p.Write([]byte("data: "))
	p.Write(big)
	p.Write([]byte("\n\ndata: ok\n\n"))
	if n != 1 || p.Dropped != 1 {
		t.Fatalf("delivered %d, dropped %d; want the oversized event dropped and the next delivered", n, p.Dropped)
	}
}

// FuzzChunking: the events are the same however the stream is split.
func FuzzChunking(f *testing.F) {
	f.Add("event: a\r\ndata: x\r\ndata: y\r\n\r\ndata: z\n\n", uint8(3), uint8(17))
	f.Add("data: a\r\ndata: b\r\n\r\n", uint8(8), uint8(9))
	f.Fuzz(func(t *testing.T, in string, a, b uint8) {
		if len(in) == 0 {
			return
		}
		i, j := int(a)%len(in), int(b)%len(in)
		if i > j {
			i, j = j, i
		}
		whole, split := events(in), events(in[:i], in[i:j], in[j:])
		if !slices.Equal(whole, split) {
			t.Fatalf("%q split at %d, %d: %q, whole: %q", in, i, j, split, whole)
		}
	})
}
