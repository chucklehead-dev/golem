package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// rtStub is a provider safe for concurrent calls, its stream in memory.
type rtStub struct{ events [][]byte }

func (s rtStub) RoundTrip(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(&chunks{c: slices.Clone(s.events)}), Request: r}, nil
}

// noDL hides SetWriteDeadline (and Unwrap) from ResponseController.
type noDL struct{ w http.ResponseWriter }

func (n noDL) Header() http.Header         { return n.w.Header() }
func (n noDL) Write(b []byte) (int, error) { return n.w.Write(b) }
func (n noDL) WriteHeader(c int)           { n.w.WriteHeader(c) }
func (n noDL) Flush()                      { n.w.(http.Flusher).Flush() }

// frontServer serves the stream, over h2c too if h2; without deadline the
// writer hides SetWriteDeadline.
func frontServer(h2 bool, deadline bool) *httptest.Server {
	h := handler(AnthropicMessages, rtStub{anthropicStream(200)}, nil)
	h.Reading = &Reading{PerPrincipal: -1}
	var hh http.Handler = h
	if !deadline {
		hh = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(noDL{w}, r) })
	}
	srv := httptest.NewUnstartedServer(hh)
	var p http.Protocols
	p.SetHTTP1(true)
	if h2 {
		p.SetUnencryptedHTTP2(true)
	}
	srv.Config.Protocols = &p
	srv.Start()
	return srv
}

// frontClient speaks HTTP/1.1, or h2c if h2.
func frontClient(h2 bool) *http.Client {
	var p http.Protocols
	if h2 {
		p.SetUnencryptedHTTP2(true)
	} else {
		p.SetHTTP1(true)
	}
	return &http.Client{Transport: &http.Transport{Protocols: &p, MaxIdleConnsPerHost: 512}}
}

// frontCall makes one call and reads its stream whole.
func frontCall(b *testing.B, c *http.Client, url string, buf []byte) {
	resp, err := c.Post(url, "application/json", bytes.NewReader(smallRequest))
	if err != nil {
		b.Fatal(err)
	}
	n, err := io.CopyBuffer(io.Discard, resp.Body, buf)
	resp.Body.Close()
	if err != nil || n < 20000 {
		b.Errorf("short stream: %d bytes, %v", n, err)
	}
}

// frontPar is RunParallel's parallelism: streams in flight per CPU.
var frontPar = 16

// BenchmarkFront serves a 205-event stream over loopback, HTTP/1.1 and h2c
// (what the gateway serves behind the mesh), with the caller's write
// stall in force and with the writer's deadline hidden (the floor: no
// write bound at all), one stream at a time and many on one connection.
func BenchmarkFront(b *testing.B) {
	for _, proto := range []struct {
		name string
		h2   bool
	}{{"h1", false}, {"h2c", true}} {
		for _, dl := range []struct {
			name string
			on   bool
		}{{"deadline", true}, {"nodeadline", false}} {
			srv := frontServer(proto.h2, dl.on)
			b.Run(proto.name+"/"+dl.name+"/serial", func(b *testing.B) {
				c := frontClient(proto.h2)
				buf := make([]byte, 32<<10)
				b.ReportAllocs()
				for b.Loop() {
					frontCall(b, c, srv.URL, buf)
				}
			})
			b.Run(proto.name+"/"+dl.name+"/parallel64", func(b *testing.B) {
				c := frontClient(proto.h2)
				b.SetParallelism(frontPar)
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					buf := make([]byte, 32<<10)
					for pb.Next() {
						frontCall(b, c, srv.URL, buf)
					}
				})
			})
			srv.Close()
		}
	}
}

// BenchmarkFront256 is BenchmarkFront with 256 streams in flight.
func BenchmarkFront256(b *testing.B) {
	frontPar = 64 // 4 procs x 64 = 256 concurrent streams
	defer func() { frontPar = 16 }()
	BenchmarkFront(b)
}
