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

// The attempt loop against model/attempts.md's gateway instance:
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

// attemptsDriver is the gateway under one trace: connect.Bind maps each of
// the model's actions to one of its methods, the steps' picks to their
// parameters by name, and its state to attemptsState.
type attemptsDriver struct {
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

// bindAttempts is the binding for an instance: its deployments' ceiling,
// the route's retry budget, its deployments and calls.
func bindAttempts(ceiling, retries int, deps, names []string) *connect.Binding[*attemptsDriver] {
	return connect.Bind(func(testing.TB) *attemptsDriver {
		d := &attemptsDriver{ceiling: ceiling, retries: retries, deps: deps, names: names}
		d.init()
		return d
	}).
		Action("start", (*attemptsDriver).Start, "c").
		// The deployment an attempt takes is the gateway's choice, which the
		// model picks: Observes compares the one it reached with d.
		Action("attempt", (*attemptsDriver).Attempt, "c").Observes("attempt", "d").
		Action("failed", (*attemptsDriver).Failed, "c", "outcome").
		Action("commit", (*attemptsDriver).Commit, "c").
		Action("end", (*attemptsDriver).End, "c", "cut", "cost").
		Action("giveUp", (*attemptsDriver).GiveUp, "c").
		Skip("tailEnds"). // the slot's: the limiter's, not here
		State((*attemptsDriver).state)
}

func (d *attemptsDriver) init() {
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
}

// Start sends call c, which runs until it pauses before its first pick.
func (d *attemptsDriver) Start(c string) error {
	m := d.calls[c]
	m.w = &mbtWriter{h: http.Header{}, headers: make(chan struct{})}
	n, _ := strconv.Atoi(c[1:])
	req := httptest.NewRequest("POST", "/v1/messages",
		bytes.NewReader(fmt.Appendf(nil, `{"model":"claude","max_tokens":%d,"stream":true,"messages":[]}`, 100+n)))
	req.Header.Set("X-Mbt-Call", c)
	go func() {
		defer close(m.done)
		defer func() { recover() }() // a cut response's abort
		d.h.ServeHTTP(m.w, req)
	}()
	return d.settle(m)
}

// Attempt lets call c pick, and reports the deployment its attempt
// reached.
func (d *attemptsDriver) Attempt(c string) (string, error) {
	m := d.calls[c]
	if m.release == nil {
		return "", fmt.Errorf("the gateway is not about to pick a deployment for this call (it is %s)", m.phase)
	}
	close(m.release)
	m.release = nil
	select {
	case a := <-m.arrived:
		m.pending, m.phase = a, "trying"
		m.attempts++
		return a.dep, nil
	case <-m.done:
		m.phase = "failed"
		return "", fmt.Errorf("the gateway ended the call (status %d) where the model attempts", m.w.status)
	case <-time.After(wait):
		return "", errors.New("no attempt reached a provider")
	}
}

// Failed answers call c's attempt as outcome says: unreached, refused (a
// 503, or a 200 whose first event is an error, alternately) or uncertain
// (a reset before the headers, or a 200 reset before its first event,
// alternately).
func (d *attemptsDriver) Failed(c, outcome string) error {
	m := d.calls[c]
	answer := map[string]func() (*http.Response, error){
		"unreached": func() (*http.Response, error) { return nil, dialFailed },
		"refused": func() (*http.Response, error) {
			if m.attempts%2 == 0 {
				return streamOf([][]byte{overloaded}), nil
			}
			return statusOf(503, ""), nil
		},
		"uncertain": func() (*http.Response, error) {
			if m.attempts%2 == 0 { // the headers, then a reset before the first event
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(failing{})}, nil
			}
			return nil, errors.New("read: connection reset by peer")
		},
	}[outcome]
	m.pending.reply <- answer
	m.pending = nil
	return d.settle(m)
}

// Commit answers call c's attempt with a stream and its first event.
func (d *attemptsDriver) Commit(c string) error {
	m := d.calls[c]
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
}

// End ends call c's stream: cut, or finished at cost output tokens.
func (d *attemptsDriver) End(c string, cut bool, cost int) error {
	m := d.calls[c]
	if cut {
		m.stream.CloseWithError(errors.New("connection reset by peer"))
	} else {
		fmt.Fprintf(m.stream, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", cost)
		m.stream.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		m.stream.Close()
	}
	return d.settle(m)
}

// GiveUp lets call c pick when the model has it give up: it must end.
func (d *attemptsDriver) GiveUp(c string) error {
	m := d.calls[c]
	if m.release == nil {
		return fmt.Errorf("the gateway is not about to pick a deployment for this call (it is %s)", m.phase)
	}
	close(m.release)
	m.release = nil
	return d.settle(m)
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

func (d *attemptsDriver) state() attemptsState {
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
	return st
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

//quint:checks attempts.oneCommit, attempts.withinBound, attempts.workWithinLimit, attempts.quotaHonoured, attempts.attemptsBounded, attempts.retriesCapped, attempts.probeOnly
func TestAttemptsModel(t *testing.T) {
	if os.Getenv("QUINT_BIN") == "" {
		if _, err := exec.LookPath("quint"); err != nil {
			t.Skip("quint not installed (or set QUINT_BIN)")
		}
	}
	t.Run("ceiling one", func(t *testing.T) {
		bindAttempts(1, 2, []string{"d1", "d2", "d3"}, []string{"c1", "c2"}).Run(t, connect.Config{Spec: "../../model/attempts.qnt", Main: "gateway", MaxSamples: 50, MaxSteps: 20,
			Require: []string{"failedOver", "ejectedThenProbed", "retriesSpent", "attemptsSpent"}})
	})
	t.Run("ceiling two", func(t *testing.T) {
		bindAttempts(2, 2, []string{"d1", "d2"}, []string{"c1", "c2", "c3"}).Run(t, connect.Config{Spec: "../../model/attempts.qnt", Main: "gatewayWide", MaxSamples: 50, MaxSteps: 20,
			Require: []string{"probePassedBy"}})
	})
}
