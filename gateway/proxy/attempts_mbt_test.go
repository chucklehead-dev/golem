package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"

	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
)

// The attempt loop against model/attempts.md's gateway instance (§11):
// its traces drive real calls through the request path to scripted
// providers. The model chooses what each provider does (an attempt
// unreached, refused, uncertain, or committed and then ended or cut); the
// gateway chooses, as in production, which deployment each attempt takes
// and when a call gives up, and those choices are checked against the
// model's: the instance is ORDERED, its choices the code's. After every
// step the gateway's state (each call's phase, attempts and commits, each
// deployment's attempts in flight, ejection and probe, the route's
// retries) must be the model's.
//
// A call is paused before each pick (Handler.beforePick), so the code
// never runs ahead of the trace: the model's attempt step releases it and
// waits for the request at a provider; its giveUp step releases it and
// waits for the call to end.

// mbtCall is one call of a trace, as the driver sees it.
type mbtCall struct {
	paused   chan chan struct{} // the call waits before a pick: release by closing the channel sent
	arrived  chan *arrival      // its attempts, as they reach a provider
	done     chan struct{}      // ServeHTTP has returned
	w        *mbtWriter
	pending  *arrival // the attempt in flight, not yet answered
	stream   *io.PipeWriter
	release  chan struct{} // the pause to release next
	phase    string
	attempts int
}

// arrival is an attempt at a provider, waiting for its answer.
type arrival struct {
	dep   string
	reply chan func() (*http.Response, error)
}

// mbtWriter records what the caller is sent, safe to read while the
// handler writes.
type mbtWriter struct {
	mu      sync.Mutex
	h       http.Header
	status  int
	headers chan struct{} // closed at WriteHeader
	body    bytes.Buffer
}

func (w *mbtWriter) Header() http.Header { return w.h }
func (w *mbtWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
		close(w.headers)
	}
}
func (w *mbtWriter) Write(b []byte) (int, error) {
	w.WriteHeader(200)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(b)
}
func (w *mbtWriter) Flush() {}

// mbtProvider is a deployment: each request it gets is handed to its
// call's driver, which answers it as the trace says.
type mbtProvider struct {
	dep   string
	calls func(string) *mbtCall
}

var callOf = regexp.MustCompile(`"max_tokens":(\d+)`)

func (p *mbtProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	m := callOf.FindSubmatch(body)
	if m == nil {
		return nil, errors.New("mbt: a request with no max_tokens")
	}
	n, _ := strconv.Atoi(string(m[1]))
	c := p.calls(fmt.Sprintf("c%d", n-100))
	a := &arrival{dep: p.dep, reply: make(chan func() (*http.Response, error))}
	c.arrived <- a
	select {
	case answer := <-a.reply:
		return answer()
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

type attemptsDriver struct {
	t       testing.TB
	ceiling int      // each deployment's MaxInFlight: the instance's QUOTA
	retries int      // the route's retry budget: its POOL_RETRIES
	deps    []string // its ORDER
	names   []string // its CALLS
	h       *Handler
	health  *route.Health
	rt      *route.Route
	calls   map[string]*mbtCall
}

// wait is how long the driver waits for the gateway to reach its next
// point: far longer than any step takes.
const wait = 5 * time.Second

func (d *attemptsDriver) call(name string) *mbtCall { return d.calls[name] }

func (d *attemptsDriver) Step(s *connect.Step) error {
	c := func() *mbtCall { return d.calls[connect.MustPick[string](s, "c")] }
	return connect.Switch{
		"init": func(*connect.Step) error {
			d.health = &route.Health{EjectAfter: 1, Ejection: time.Nanosecond, RetryBurst: float64(d.retries), RetryRatio: 1e-9,
				Now: (&tick{at: time.Unix(0, 0)}).now}
			d.rt = &route.Route{Alias: "claude", Dialect: AnthropicMessages.Name(), MaxAttempts: 2}
			for _, dep := range d.deps {
				d.rt.Pool = append(d.rt.Pool, &route.Target{Backend: "b", Deployment: dep, ID: dep, Model: "m", URL: upstreamURL,
					MaxInFlight: d.ceiling, Transport: &mbtProvider{dep: dep, calls: d.call}})
			}
			d.calls = map[string]*mbtCall{}
			for _, name := range d.names {
				d.calls[name] = &mbtCall{paused: make(chan chan struct{}, 1), arrived: make(chan *arrival, 1),
					done: make(chan struct{}), phase: "idle"}
			}
			d.h = &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Health: d.health,
				Snapshots: gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(d.rt)}),
				beforePick: func(r *http.Request) {
					release := make(chan struct{})
					d.calls[r.Header.Get("X-Mbt-Call")].paused <- release
					<-release
				}}
			return nil
		},
		"start": func(s *connect.Step) error {
			name := connect.MustPick[string](s, "c")
			m := d.calls[name]
			m.w = &mbtWriter{h: http.Header{}, headers: make(chan struct{})}
			n, _ := strconv.Atoi(name[1:])
			req := httptest.NewRequest("POST", "/v1/messages",
				bytes.NewReader(fmt.Appendf(nil, `{"model":"claude","max_tokens":%d,"stream":true,"messages":[]}`, 100+n)))
			req.Header.Set("X-Mbt-Call", name)
			go func() {
				defer close(m.done)
				defer func() { recover() }() // a cut response's abort
				d.h.ServeHTTP(m.w, req)
			}()
			return d.settle(m)
		},
		"attempt": func(s *connect.Step) error {
			m, dep := c(), connect.MustPick[string](s, "d")
			if m.release == nil {
				return fmt.Errorf("the gateway is not about to pick a deployment for this call (it is %s)", m.phase)
			}
			close(m.release)
			m.release = nil
			select {
			case a := <-m.arrived:
				if a.dep != dep {
					return fmt.Errorf("the gateway sent the attempt to %s; the model's choice is %s", a.dep, dep)
				}
				m.pending, m.phase = a, "trying"
				m.attempts++
				return nil
			case <-m.done:
				m.phase = "failed"
				return fmt.Errorf("the gateway ended the call (status %d) where the model attempts %s", m.w.status, dep)
			case <-time.After(wait):
				return errors.New("no attempt reached a provider")
			}
		},
		"failed": func(s *connect.Step) error {
			m := c()
			answer := map[string]func() (*http.Response, error){
				"unreached": func() (*http.Response, error) { return nil, dialFailed },
				// A refusal both ways the gateway meets one: a 503, and a
				// 200 whose first event is an error (alternately).
				"refused": func() (*http.Response, error) {
					if m.attempts%2 == 0 {
						return streamOf([][]byte{overloaded}), nil
					}
					return statusOf(503, ""), nil
				},
				"uncertain": func() (*http.Response, error) { return nil, errors.New("read: connection reset by peer") },
			}[connect.MustPick[string](s, "outcome")]
			m.pending.reply <- answer
			m.pending = nil
			return d.settle(m)
		},
		"commit": func(*connect.Step) error {
			m := c()
			pr, pw := io.Pipe()
			m.stream = pw
			m.pending.reply <- func() (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
			}
			m.pending = nil
			go pw.Write(anthropicStream(0)[0]) // message_start: the first event, which commits it
			select {
			case <-m.w.headers:
				m.phase = "streaming"
				return nil
			case <-time.After(wait):
				return errors.New("the stream did not commit at its first event")
			}
		},
		"end": func(s *connect.Step) error {
			m := c()
			if connect.MustPick[bool](s, "cut") {
				m.stream.CloseWithError(errors.New("connection reset by peer"))
			} else {
				fmt.Fprintf(m.stream, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", connect.MustPick[int](s, "cost"))
				m.stream.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
				m.stream.Close()
			}
			return d.settle(m)
		},
		"giveUp": func(*connect.Step) error {
			m := c()
			if m.release == nil {
				return fmt.Errorf("the gateway is not about to pick a deployment for this call (it is %s)", m.phase)
			}
			close(m.release)
			m.release = nil
			return d.settle(m)
		},
		"tailEnds": func(*connect.Step) error { return nil }, // the slot's: the limiter's, not here
	}.Step(s)
}

// settle waits for call m to reach its next point after an answer: paused
// before a pick (waiting), or over (done after a commit, else failed).
func (d *attemptsDriver) settle(m *mbtCall) error {
	select {
	case release := <-m.paused:
		m.release, m.phase = release, "waiting"
	case <-m.done:
		m.phase = "failed"
		if m.w.status == 200 {
			m.phase = "done"
		}
	case <-time.After(wait):
		return errors.New("the call neither paused before a pick nor ended")
	}
	return nil
}

// attemptsState is the model's state the gateway is compared on.
type attemptsState struct {
	Phase    map[string]string
	Attempts map[string]int
	Commits  map[string]int
	Load     map[string]int
	Ejected  map[string]bool
	Probes   map[string]int
	Retries  int
}

func (d *attemptsDriver) State() (any, error) {
	st := attemptsState{Phase: map[string]string{}, Attempts: map[string]int{}, Commits: map[string]int{},
		Load: map[string]int{}, Ejected: map[string]bool{}, Probes: map[string]int{}}
	for name, m := range d.calls {
		st.Phase[name], st.Attempts[name] = m.phase, m.attempts
		if m.w != nil && m.w.status == 200 {
			st.Commits[name] = 1
		} else {
			st.Commits[name] = 0
		}
	}
	for _, t := range d.rt.Pool {
		dh := d.health.Deployment(t)
		st.Load[t.Deployment], st.Ejected[t.Deployment] = dh.InFlight, dh.Ejected
		if dh.Probing {
			st.Probes[t.Deployment] = 1
		} else {
			st.Probes[t.Deployment] = 0
		}
	}
	st.Retries = int(float64(d.retries) - d.health.Retries("claude") + 0.5)
	return st, nil
}

// tick is a clock that moves a second each time it is read: an ejection
// (a nanosecond here) is over by the next attempt, as the model's probe
// may come at any point.
type tick struct {
	mu sync.Mutex
	at time.Time
}

func (t *tick) now() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.at = t.at.Add(time.Second)
	return t.at
}

func TestAttemptsModel(t *testing.T) {
	if os.Getenv("QUINT_BIN") == "" {
		if _, err := exec.LookPath("quint"); err != nil {
			t.Skip("quint not installed (or set QUINT_BIN)")
		}
	}
	t.Run("ceiling one", func(t *testing.T) {
		connect.Run(t, connect.Config{Spec: "../../model/attempts.qnt", Main: "gateway", MaxSamples: 50, MaxSteps: 20,
			Require: []string{"failedOver", "ejectedThenProbed", "retriesSpent", "attemptsSpent"}},
			func(t testing.TB) connect.Driver {
				return &attemptsDriver{t: t, ceiling: 1, retries: 2,
					deps: []string{"d1", "d2", "d3"}, names: []string{"c1", "c2"}}
			})
	})
	t.Run("ceiling two", func(t *testing.T) {
		connect.Run(t, connect.Config{Spec: "../../model/attempts.qnt", Main: "gatewayWide", MaxSamples: 50, MaxSteps: 20,
			Require: []string{"probePassedBy"}},
			func(t testing.TB) connect.Driver {
				return &attemptsDriver{t: t, ceiling: 2, retries: 2,
					deps: []string{"d1", "d2"}, names: []string{"c1", "c2", "c3"}}
			})
	})
}
