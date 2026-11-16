package guard

import (
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFailuresRefuseTheSource: a source past its failed authentications
// is Failing, with when to retry (the request path refuses its failures
// then), while another source is not; it is not once its bucket refills.
// Admit, the request rate, is not its failures'.
func TestFailuresRefuseTheSource(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &Guard{FailRate: 1, FailBurst: 3, Now: func() time.Time { return now }}
	for range 3 {
		if no := g.Failing("a"); no != nil {
			t.Fatalf("failing within the burst: %s", no.Msg)
		}
		g.Failed("a")
	}
	no := g.Failing("a")
	if no == nil || no.RetryAfter != time.Second {
		t.Fatalf("past its failures: %+v, want failing for a second", no)
	}
	if g.Admit("a") != nil {
		t.Fatal("Admit refused for failures: a valid key from the source must still be looked up")
	}
	if g.Failing("b") != nil {
		t.Fatal("another source failing")
	}
	now = now.Add(time.Second)
	if g.Failing("a") != nil {
		t.Fatal("still failing after a refill")
	}
}

// TestRequestRate: a source's requests before authentication are a token
// bucket.
func TestRequestRate(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &Guard{Rate: 2, Burst: 4, Now: func() time.Time { return now }}
	for i := range 4 {
		if g.Admit("a") != nil {
			t.Fatalf("request %d refused within the burst", i)
		}
	}
	if no := g.Admit("a"); no == nil || no.RetryAfter != time.Second {
		t.Fatalf("past the burst: %+v", no)
	}
	now = now.Add(time.Second) // two more
	first, second, third := g.Admit("a"), g.Admit("a"), g.Admit("a")
	if first != nil || second != nil || third == nil {
		t.Fatal("a second refilled other than two")
	}
}

// TestSources: the header's last value is the source, else the connection's
// host; past MaxSources, sources whose buckets refilled are forgotten, and
// while none can be, new sources are not limited (one bucket they shared
// could be drained by one for all).
func TestSources(t *testing.T) {
	g := &Guard{SourceHeader: "X-Forwarded-For", FailRate: 1}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	if s := g.Source(r); s != "10.0.0.9" {
		t.Fatalf("no header: %q", s)
	}
	r.Header.Add("X-Forwarded-For", "6.6.6.6, 1.2.3.4")
	if s := g.Source(r); s != "1.2.3.4" {
		t.Fatalf("the proxy's appended value: %q", s)
	}

	now := time.Unix(1000, 0)
	g = &Guard{FailRate: 1, FailBurst: 1, MaxSources: 2, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}
	g.Failed("a")
	g.Failed("b")
	g.Failed("c") // no room: not counted
	if g.Failing("c") != nil || g.Failing("d") != nil {
		t.Fatal("a new source past MaxSources is limited: it must be left alone, not share a drainable bucket")
	}
	if g.Failing("a") == nil {
		t.Fatal("a kept source's failures forgotten")
	}
	if len(g.sources) != 2 {
		t.Fatalf("%d sources kept", len(g.sources))
	}
	now = now.Add(2 * time.Second)
	g.Failed("e")
	if g.Failing("e") == nil {
		t.Fatal("refilled sources not forgotten for a new one")
	}
	if len(g.sources) != 1 {
		t.Fatalf("%d sources after the sweep", len(g.sources))
	}
}

// TestNilGuard: a nil guard admits everything.
func TestNilGuard(t *testing.T) {
	var g *Guard
	g.Failed("a")
	if g.Admit(g.Source(httptest.NewRequest("GET", "/", nil))) != nil {
		t.Fatal("refused")
	}
}

// TestListener: past Max, Accept waits for a connection to close; past
// PerSource, a connection is closed as it is accepted; Close ends a waiting
// Accept.
func TestListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &Listener{Listener: inner, Max: 2, PerSource: 1}
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a := dial()
	first := <-accepted
	b := dial() // a second from 127.0.0.1: closed as accepted
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection past PerSource was not closed")
	}
	select {
	case c := <-accepted:
		t.Fatalf("accepted %v past PerSource", c.RemoteAddr())
	case <-time.After(100 * time.Millisecond):
	}
	first.Close()
	a.Close()
	c := dial()
	defer c.Close()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("a closed connection freed no room")
	}
	l.Close()
	select {
	case _, ok := <-accepted:
		if ok {
			t.Fatal("accepted after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end Accept")
	}
}

// TestListenerMax: past Max, Accept waits until a connection closes, and
// Close ends the wait.
func TestListenerMax(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &Listener{Listener: inner, Max: 1}
	go func() { c, _ := net.Dial("tcp", inner.Addr().String()); defer c.Close(); time.Sleep(5 * time.Second) }()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go func() { c2, _ := net.Dial("tcp", inner.Addr().String()); defer c2.Close(); time.Sleep(5 * time.Second) }()
	got := make(chan error, 1)
	go func() { _, err := l.Accept(); got <- err }()
	select {
	case err := <-got:
		t.Fatalf("accepted past Max: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	c.Close()
	if err := <-got; err != nil {
		t.Fatalf("after a close: %v", err)
	}
	l.Close()
	if _, err := l.Accept(); err == nil {
		t.Fatal("Accept after Close")
	}
}
