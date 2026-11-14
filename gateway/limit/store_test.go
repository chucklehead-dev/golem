package limit

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

// The store contract, run against every Store.

const nov = "2026-11"

// at is a minute of November, on the test's clock: the counter's minutes
// are the increments' own, whatever the store's clock.
func at(minute int) time.Time {
	return time.Now().Truncate(time.Minute).Add(time.Duration(minute-10) * time.Minute)
}

// inc is an increment of scope s in November, in the given minute.
func inc(call string, cost int64, minute int) Increment {
	return Increment{Scope: "s", Period: nov, Call: call, Cost: cost, At: at(minute)}
}

// acquire admits a slot with no budget: the slot half of Admit.
func acquire(t *testing.T, st Store, s Slot, principalLimit, scopeLimit int) bool {
	t.Helper()
	o, _, err := st.Admit(t.Context(), Admission{Slot: s, Period: nov, PrincipalLimit: principalLimit, ScopeLimit: scopeLimit})
	if err != nil {
		t.Fatal(err)
	}
	return o == Admitted
}

func testStore(t *testing.T, newStore func(t *testing.T) Store) {
	t.Run("limits", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		if !acquire(t, st, Slot{"a", "p1", "s"}, 1, 2) {
			t.Fatal("the first slot")
		}
		if acquire(t, st, Slot{"b", "p1", "s"}, 1, 2) {
			t.Fatal("a second slot for p1 with a principal limit of 1")
		}
		if !acquire(t, st, Slot{"c", "p2", "s"}, 1, 2) {
			t.Fatal("p2's first slot")
		}
		if acquire(t, st, Slot{"d", "p3", "s"}, 1, 2) {
			t.Fatal("a third slot in a scope limited to 2")
		}
		if !acquire(t, st, Slot{"d", "p3", "s"}, 1, 0) {
			t.Fatal("no scope limit: p3's first slot")
		}
		st.Release(ctx, Slot{"a", "p1", "s"})
		if !acquire(t, st, Slot{"b", "p1", "s"}, 1, 0) {
			t.Fatal("after release")
		}
		// Re-registering ignores limits: the calls are running.
		if err := st.Reacquire(ctx, []Slot{{"x", "p1", "s"}, {"y", "p1", "s"}}); err != nil {
			t.Fatal(err)
		}
		if acquire(t, st, Slot{"z", "p9", "s"}, 1, 4) {
			t.Fatal("the re-registered slots count against the scope")
		}
		// Complete frees the slot it records.
		if err := st.Complete(ctx, inc("b", 1, 0), Slot{"b", "p1", "s"}); err != nil {
			t.Fatal(err)
		}
		if !acquire(t, st, Slot{"b2", "p1", "s"}, 3, 0) || acquire(t, st, Slot{"b3", "p1", "s"}, 3, 0) {
			t.Fatal("after Complete, p1 holds x, y and b2: one slot was freed")
		}
	})
	t.Run("budget", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		st.Add(ctx, inc("e1", 3, 1))
		st.Add(ctx, inc("e2", 5, 2))
		for _, tc := range []struct {
			name string
			a    Admission
			want Outcome
		}{ // known: the snapshot's 2 (before minute 2) plus the 5 from it
			{"soft, under", Admission{Budget: 8}, Admitted},
			{"soft, at", Admission{Budget: 7}, OverBudget},
			{"declared-worst-case check, fits", Admission{Budget: 9, DeclaredWorstCaseCheck: true, Worst: 2}, Admitted},
			{"declared-worst-case check, does not", Admission{Budget: 9, DeclaredWorstCaseCheck: true, Worst: 3}, OverBudget},
			{"none", Admission{}, Admitted},
		} {
			a := tc.a
			a.Slot, a.Period, a.Since, a.Spend = Slot{tc.name, tc.name, "s"}, nov, at(2), 2
			o, known, err := st.Admit(ctx, a)
			if err != nil || o != tc.want || known != 7 {
				t.Errorf("%s: %v %d %v; want %v, known 7", tc.name, o, known, err, tc.want)
			}
		}
		// Over budget takes no slot.
		if got := len(slotsOf(t, st)); got != 3 {
			t.Errorf("%d slots held, want 3 (the admitted)", got)
		}
	})
	t.Run("counter", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		st.Add(ctx, inc("e1", 3, 0))
		st.Add(ctx, inc("e2", 5, 1))
		st.Add(ctx, inc("e3", 2, 1)) // the same minute: summed
		st.Add(ctx, inc("e2", 5, 1)) // an amount, not an event: twice is twice
		st.Add(ctx, Increment{Scope: "other", Period: nov, Cost: 100, At: at(1)})
		st.Add(ctx, Increment{Scope: "s", Period: "2026-12", Cost: 100, At: at(1)})
		for since, want := range map[time.Time]int64{{}: 15, at(0): 15, at(1): 12, at(1).Add(30 * time.Second): 12, at(2): 0} {
			if got, err := st.Since(ctx, "s", nov, since); err != nil || got != want {
				t.Errorf("Since(%v) = %d, %v; want %d", since, got, err, want)
			}
		}
		// A batch, whole.
		if err := st.AddAll(ctx, []Increment{inc("e4", 1, 3), inc("e5", 1, 4), {Scope: "other", Period: nov, Cost: 1, At: at(3)}}); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Since(ctx, "s", nov, at(3)); got != 2 {
			t.Errorf("after AddAll, Since(3) = %d, want 2", got)
		}
	})
}

// slotsOf lists the calls holding scope s's slots.
func slotsOf(t *testing.T, st Store) []string {
	switch st := st.(type) {
	case *Mem:
		var out []string
		for _, s := range st.Slots() {
			out = append(out, s.Call)
		}
		return out
	case *Valkey:
		out, err := st.ScopeSlots(t.Context(), "s")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Fatalf("no slot listing for %T", st)
	return nil
}

func TestMemStore(t *testing.T) { testStore(t, func(*testing.T) Store { return NewMem() }) }

// valkeyAddr is a Valkey (or Redis) to test against: GOLEM_VALKEY_ADDR, or a
// redis-server or valkey-server started for the test.
func valkeyAddr(t testing.TB) string {
	if a := os.Getenv("GOLEM_VALKEY_ADDR"); a != "" {
		return a
	}
	return startServer(t)
}

// startServer starts a local valkey-server or redis-server with extra
// arguments (a replica's --replicaof) and returns its address.
func startServer(t testing.TB, args ...string) string {
	bin, err := exec.LookPath("valkey-server")
	if err != nil {
		if bin, err = exec.LookPath("redis-server"); err != nil {
			t.Skip("no Valkey: set GOLEM_VALKEY_ADDR or install valkey-server")
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	// Its own directory: a replica writes its synced copy there (dump.rdb).
	cmd := exec.Command(bin, append([]string{"--port", fmt.Sprint(port), "--save", "", "--appendonly", "no", "--dir", t.TempDir()}, args...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for range 100 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond) // the server starting: an external process
	}
	t.Fatal("the Valkey server did not start")
	return ""
}

var siteN int

// newValkey returns a store over a fresh site prefix of one server.
func newValkey(t testing.TB, addr string) *Valkey {
	c, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	siteN++
	return NewValkey(c, fmt.Sprintf("test-%d-%d", os.Getpid(), siteN))
}

func TestValkeyStore(t *testing.T) {
	addr := valkeyAddr(t)
	testStore(t, func(t *testing.T) Store { return newValkey(t, addr) })

	t.Run("slots expire unless renewed", func(t *testing.T) {
		ctx, st := t.Context(), newValkey(t, addr)
		st.TTL = 200 * time.Millisecond
		acquire(t, st, Slot{"a", "p1", "s"}, 1, 0)
		acquire(t, st, Slot{"b", "p2", "s"}, 1, 0)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			st.Reacquire(ctx, []Slot{{"a", "p1", "s"}}) // a's gateway renews; b's crashed
			if got, _ := st.ScopeSlots(ctx, "s"); slices.Equal(got, []string{"a"}) {
				// An expired slot is dropped when the next acquire looks.
				if !acquire(t, st, Slot{"c", "p2", "s"}, 1, 0) {
					t.Fatal("p2's crashed call still holds its slot")
				}
				return
			}
			acquire(t, st, Slot{"probe", "p-probe", "s"}, 1, 0) // trims expired slots
			st.Release(ctx, Slot{"probe", "p-probe", "s"})
			time.Sleep(50 * time.Millisecond) // TTLs are on the server's clock
		}
		t.Fatal("b's slot never expired")
	})
	t.Run("a counter keeps a retention's minutes and its whole total", func(t *testing.T) {
		ctx, st := t.Context(), newValkey(t, addr)
		st.Retain = 2 * time.Minute
		now := time.Now()
		var incs []Increment
		for m := range 80 { // 80 minutes back to now, 1 each
			incs = append(incs, Increment{Scope: "s", Period: nov, Cost: 1, At: now.Add(time.Duration(m-79) * time.Minute)})
		}
		if err := st.AddAll(ctx, incs); err != nil {
			t.Fatal(err)
		}
		// A boundary an hour back reads only the retained minutes; none
		// reads the whole total.
		_, known, err := st.Admit(ctx, Admission{Slot: Slot{"a", "p", "s"}, Period: nov, Since: now.Add(-time.Hour), Now: now})
		if err != nil || known != 3 {
			t.Fatalf("known %d, %v: want the 3 minutes within retention", known, err)
		}
		if _, known, _ := st.Admit(ctx, Admission{Slot: Slot{"b", "p2", "s"}, Period: nov, Now: now}); known != 80 {
			t.Fatalf("known %d with no boundary, want the total, 80", known)
		}
		if n, _ := st.c.Do(ctx, st.c.B().Hlen().Key(st.counterKey("s", nov)).Build()).AsInt64(); n > 2+60+1 {
			t.Fatalf("%d fields kept: past a retention and its margin, the old minutes go", n)
		}
	})
	t.Run("an unreachable server is ErrUnavailable", func(t *testing.T) {
		c, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, DisableCache: true})
		if err != nil {
			t.Fatal(err)
		}
		st := NewValkey(c, "closed")
		c.Close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, _, err := st.Admit(ctx, Admission{Slot: Slot{"a", "p", "s"}, Period: nov}); err == nil || !strings.Contains(err.Error(), ErrUnavailable.Error()) {
			t.Fatalf("%v, want ErrUnavailable", err)
		}
	})
}

// TestReplicaAcks: with ReplicaAcks the add and its WAIT go on one
// connection, so WAIT counts the add's own write; an add fewer replicas
// acknowledged is counted. (Sent alone, WAIT runs on a connection of its
// own and returns at once, guaranteeing nothing.)
func TestReplicaAcks(t *testing.T) {
	if os.Getenv("GOLEM_VALKEY_ADDR") != "" || testing.Short() {
		t.Skip("needs a local server to attach a replica to, and two seconds")
	}
	primary := startServer(t)
	host, port, _ := net.SplitHostPort(primary)
	replica := startServer(t, "--replicaof", host, port, "--enable-debug-command", "yes")
	st := newValkey(t, primary)
	ctx := t.Context()
	st.ReplicaAcks = 1
	deadline := time.Now().Add(10 * time.Second)
	for { // the replica's first sync
		st.Unreplicated.Store(0)
		if err := st.Add(ctx, inc(fmt.Sprint("sync", time.Now().UnixNano()), 1, 0)); err != nil {
			t.Fatal(err)
		}
		if st.Unreplicated.Load() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the replica never acknowledged")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := st.Complete(ctx, inc("c1", 2, 0), Slot{"c1", "p", "s"}); err != nil || st.Unreplicated.Load() != 0 {
		t.Fatalf("Complete = %v, unreplicated %d", err, st.Unreplicated.Load())
	}
	// A stalled replica cannot acknowledge the add: WAIT on the add's own
	// connection times out and the add is counted. A WAIT on another
	// connection waits only for that connection's own offset (where the
	// primary stood when it last ran a command), which a replica that has
	// caught up has acknowledged: it would return 1, wrongly. So: a client
	// whose connections are warm (as a gateway's pooled ones are), then a
	// pause for the replica to acknowledge everything (it does so about
	// once a second), then the stall and the add.
	fresh := newValkey(t, primary)
	fresh.ReplicaAcks = 1
	if err := fresh.Add(ctx, inc("warm", 1, 0)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	fresh.Unreplicated.Store(0)
	rc, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{replica}, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	stalled := make(chan error, 1)
	go func() {
		stalled <- rc.Do(context.Background(), rc.B().Arbitrary("DEBUG", "SLEEP").Args("0.5").Build()).Error()
	}()
	time.Sleep(50 * time.Millisecond)
	if err := fresh.Add(ctx, inc("stalled", 1, 0)); err != nil || fresh.Unreplicated.Load() != 1 {
		t.Fatalf("err %v, unreplicated %d: an add the stalled replica never saw must be counted", err, fresh.Unreplicated.Load())
	}
	if err := <-stalled; err != nil {
		t.Fatal(err)
	}
	st.Unreplicated.Store(0)
	st.ReplicaAcks = 2 // more than there are
	if err := st.Add(ctx, inc("x", 1, 0)); err != nil || st.Unreplicated.Load() != 1 {
		t.Fatalf("err %v, unreplicated %d: one replica short must be counted", err, st.Unreplicated.Load())
	}
}
