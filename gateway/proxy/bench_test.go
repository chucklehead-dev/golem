package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/dialect/anthropic/messages"
	"github.com/chucklehead-dev/golem/lib/key"
)

// discard is a ResponseWriter that keeps nothing, with a Flush.
type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Flush()                      {}

// BenchmarkScan is the scan alone: the whole body is read, to refuse
// duplicates and trailing data, so its cost grows with the body.
func BenchmarkScan(b *testing.B) {
	for _, tc := range []struct {
		name string
		body []byte
	}{{"small", smallRequest}, {"agent", agentRequest}} {
		b.Run(tc.name, func(b *testing.B) {
			var r messages.Request
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				if err := r.Scan(tc.body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHandler is the gateway's fast path end to end, in memory: body
// read, scan, splice, upstream request, a 200-event stream copied and
// flushed per event with usage read. BenchmarkStub is the in-memory
// provider's own share, to subtract.
func BenchmarkHandler(b *testing.B) {
	for _, tc := range []struct {
		name string
		body []byte
	}{{"small", smallRequest}, {"agent", agentRequest}} {
		b.Run(tc.name, func(b *testing.B) {
			events := anthropicStream(200)
			s := &stub{events: events}
			h := handler(AnthropicMessages, s, nil)
			var br bytes.Reader
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			w := &discard{h: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				br.Reset(tc.body)
				req.Body = io.NopCloser(&br)
				clear(w.h)
				s.events = slices.Clone(events) // chunks may reslice an element
				h.ServeHTTP(w, req)
			}
		})
	}
}

func BenchmarkStub(b *testing.B) {
	events := anthropicStream(200)
	s := &stub{}
	req := httptest.NewRequest("POST", "/", nil)
	var br bytes.Reader
	b.ReportAllocs()
	for b.Loop() {
		br.Reset(smallRequest)
		req.Body = io.NopCloser(&br)
		s.events = slices.Clone(events)
		resp, _ := s.RoundTrip(req)
		io.Copy(io.Discard, resp.Body)
	}
}

// provider is a loopback provider that writes the stream one flushed event
// at a time, as a real one does.
func provider(events [][]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, e := range events {
			w.Write(e)
			f.Flush()
		}
	}))
}

func gateway(up *httptest.Server) *httptest.Server {
	u, _ := url.Parse(up.URL)
	t := up.Client().Transport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 256
	return httptest.NewServer(&Handler{Dialect: AnthropicMessages, MaxBody: 4 << 20, Snapshots: routes(AnthropicMessages, u, t)})
}

func client() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
}

func call(c *http.Client, url string, body []byte) (ttfb, total time.Duration, err error) {
	start := time.Now()
	resp, err := c.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 32<<10)
	if _, err := resp.Body.Read(buf); err != nil && err != io.EOF {
		return 0, 0, err
	}
	ttfb = time.Since(start)
	io.CopyBuffer(io.Discard, resp.Body, buf)
	return ttfb, time.Since(start), nil
}

// BenchmarkLoopback is a client, the gateway and a provider over loopback
// HTTP/1.1; "direct" skips the gateway. The difference is the overhead,
// including the extra hop's own HTTP work.
func BenchmarkLoopback(b *testing.B) {
	up := provider(anthropicStream(200))
	defer up.Close()
	gw := gateway(up)
	defer gw.Close()
	for _, tc := range []struct{ name, url string }{{"direct", up.URL}, {"proxied", gw.URL}} {
		for _, body := range []struct {
			name string
			b    []byte
		}{{"small", smallRequest}, {"agent", agentRequest}} {
			b.Run(tc.name+"/"+body.name, func(b *testing.B) {
				c := client()
				b.ReportAllocs()
				for b.Loop() {
					if _, _, err := call(c, tc.url, body.b); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func spike(t *testing.T) {
	if os.Getenv("GOLEM_SPIKE") == "" {
		t.Skip("a spike measurement: set GOLEM_SPIKE=1")
	}
}

// TestOverheadPercentiles measures time to first byte and to the end of the
// stream, direct and proxied, at a given concurrency, and reports the
// percentiles of each and of the difference (docs/spikes/fast-path.md).
func TestOverheadPercentiles(t *testing.T) {
	spike(t)
	up := provider(anthropicStream(200))
	defer up.Close()
	gw := gateway(up)
	defer gw.Close()
	const n = 1000
	for _, conc := range []int{1, 32} {
		for _, body := range []struct {
			name string
			b    []byte
		}{{"small", smallRequest}, {"agent", agentRequest}} {
			run := func(url string) (ttfb, total []time.Duration) {
				c := client()
				var mu sync.Mutex
				var wg sync.WaitGroup
				work := make(chan struct{}, n)
				for range n {
					work <- struct{}{}
				}
				close(work)
				for range conc {
					wg.Go(func() {
						for range work {
							a, z, err := call(c, url, body.b)
							if err != nil {
								t.Error(err)
								return
							}
							mu.Lock()
							ttfb, total = append(ttfb, a), append(total, z)
							mu.Unlock()
						}
					})
				}
				wg.Wait()
				return ttfb, total
			}
			run(up.URL) // warm both paths
			run(gw.URL)
			dA, dZ := run(up.URL)
			pA, pZ := run(gw.URL)
			t.Logf("concurrency %2d, %-5s  TTFB p50 %v→%v (+%v)  p99 %v→%v (+%v) | stream p50 %v→%v (+%v)  p99 %v→%v (+%v)",
				conc, body.name,
				pct(dA, 50), pct(pA, 50), pct(pA, 50)-pct(dA, 50), pct(dA, 99), pct(pA, 99), pct(pA, 99)-pct(dA, 99),
				pct(dZ, 50), pct(pZ, 50), pct(pZ, 50)-pct(dZ, 50), pct(dZ, 99), pct(pZ, 99), pct(pZ, 99)-pct(dZ, 99))
		}
	}
}

func pct(d []time.Duration, p int) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[(len(s)-1)*p/100].Round(time.Microsecond)
}

// TestMemoryPerStream holds many streams open through the gateway and
// measures the memory each costs, against the same streams held direct.
func TestMemoryPerStream(t *testing.T) {
	spike(t)
	const n = 300
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(anthropicStream(0)[0])
		w.(http.Flusher).Flush()
		<-release
	}))
	up.Config.SetKeepAlivesEnabled(false)
	defer up.Close()
	gw := gateway(up)
	defer gw.Close()

	measure := func(url string, body []byte) uint64 {
		release = make(chan struct{})
		c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n}}
		before := inUse()
		var open, done sync.WaitGroup
		open.Add(n)
		for range n {
			done.Go(func() {
				resp, err := c.Post(url, "application/json", bytes.NewReader(body))
				if err != nil {
					t.Error(err)
					open.Done()
					return
				}
				buf := make([]byte, 512)
				resp.Body.Read(buf)
				open.Done()
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			})
		}
		open.Wait()
		after := inUse()
		if f := os.Getenv("GOLEM_HEAPPROFILE"); f != "" && url == gw.URL && len(body) > 100000 {
			out, _ := os.Create(f)
			pprof.Lookup("heap").WriteTo(out, 0)
			out.Close()
		}
		close(release)
		done.Wait()
		c.CloseIdleConnections()
		return (after - before) / n
	}
	for _, body := range []struct {
		name string
		b    []byte
	}{{"small", smallRequest}, {"agent", agentRequest}} {
		direct := measure(up.URL, body.b)
		proxied := measure(gw.URL, body.b)
		t.Logf("%-5s request: %d B per stream direct, %d B proxied: the gateway's share %d B (%.1f KiB)",
			body.name, direct, proxied, proxied-direct, float64(proxied-direct)/1024)
	}
}

// inUse is heap and goroutine stacks in use, after two collections: a
// sync.Pool keeps what was returned to it until the second.
func inUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse
}

// BenchmarkHandlerAdmitted is the same path with a key authenticated, the
// alias authorized, and the call admitted, priced and completed by the
// in-memory limiter.
func BenchmarkHandlerAdmitted(b *testing.B) {
	k := key.New(key.Dev)
	events := anthropicStream(200)
	s := &stub{events: events}
	h, _ := standaloneHandler(b, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude]}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+k+`, principal: alice, scope: team-a}]
`, s)
	var br bytes.Reader
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Api-Key", k)
	w := &discard{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		br.Reset(smallRequest)
		req.Body = io.NopCloser(&br)
		clear(w.h)
		s.events = slices.Clone(events)
		h.ServeHTTP(w, req)
	}
}
