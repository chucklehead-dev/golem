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

// acquire admits a slot with no budget: the slot half of Admit.
func acquire(t *testing.T, st Store, s Slot, principalLimit, scopeLimit int) bool {
	t.Helper()
	o, _, _, err := st.Admit(t.Context(), Admission{Slot: s, Period: nov, PrincipalLimit: principalLimit, ScopeLimit: scopeLimit})
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
		if _, err := st.Complete(ctx, nov, Slot{"b", "p1", "s"}, Event{"b", 1}); err != nil {
			t.Fatal(err)
		}
		if !acquire(t, st, Slot{"b2", "p1", "s"}, 3, 0) || acquire(t, st, Slot{"b3", "p1", "s"}, 3, 0) {
			t.Fatal("after Complete, p1 holds x, y and b2: one slot was freed")
		}
	})
	t.Run("budget", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		st.Add(ctx, "s", nov, Event{"e1", 3})
		st.Add(ctx, "s", nov, Event{"e2", 5})
		for _, tc := range []struct {
			name string
			a    Admission
			want Outcome
		}{ // known: the snapshot's 2 (as of number 1) plus the 5 after it
			{"soft, under", Admission{Budget: 8}, Admitted},
			{"soft, at", Admission{Budget: 7}, OverBudget},
			{"strict, fits", Admission{Budget: 9, Strict: true, Worst: 2}, Admitted},
			{"strict, does not", Admission{Budget: 9, Strict: true, Worst: 3}, OverBudget},
			{"none", Admission{}, Admitted},
		} {
			a := tc.a
			a.Slot, a.Period, a.Watermark, a.Spend = Slot{tc.name, tc.name, "s"}, nov, 1, 2
			o, known, over, err := st.Admit(ctx, a)
			if err != nil || o != tc.want || known != 7 || over {
				t.Errorf("%s: %v %d %v %v; want %v, known 7", tc.name, o, known, over, err, tc.want)
			}
		}
		// Over budget takes no slot.
		if got := len(slotsOf(t, st)); got != 3 {
			t.Errorf("%d slots held, want 3 (the admitted)", got)
		}
	})
	t.Run("counter", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		for _, tc := range []struct {
			e    Event
			want int64
		}{{Event{"e1", 3}, 1}, {Event{"e2", 5}, 2}, {Event{"e1", 3}, 1}} { // twice is once
			if seq, err := st.Add(ctx, "s", nov, tc.e); err != nil || seq != tc.want {
				t.Fatalf("Add(%v) = %d, %v; want %d", tc.e, seq, err, tc.want)
			}
		}
		st.Add(ctx, "other", nov, Event{"e9", 100})
		st.Add(ctx, "s", "2026-12", Event{"e10", 100})
		for w, want := range map[int64]int64{0: 8, 1: 5, 2: 0, 7: 0} {
			if got, over, err := st.After(ctx, "s", nov, w); err != nil || over || got != want {
				t.Errorf("After(%d) = %d, %v, %v; want %d", w, got, over, err, want)
			}
		}
		if seq, _ := st.Add(ctx, "s", "2026-12", Event{"e11", 1}); seq != 2 {
			t.Errorf("December's second event numbered %d", seq)
		}
		// A batch, in order, a repeat keeping its number.
		seqs, err := st.AddAll(ctx, []Increment{{"s", nov, Event{"e3", 1}}, {"s", nov, Event{"e1", 3}}, {"other", nov, Event{"e4", 1}}})
		if err != nil || !slices.Equal(seqs, []int64{3, 1, 2}) {
			t.Errorf("AddAll = %v, %v; want [3 1 2]", seqs, err)
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

func sortEvents(es []Event) {
	slices.SortFunc(es, func(a, b Event) int { return strings.Compare(a.ID, b.ID) })
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
	cmd := exec.Command(bin, append([]string{"--port", fmt.Sprint(port), "--save", "", "--appendonly", "no"}, args...)...)
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
	t.Run("trimmed history over-counts, never under-counts", func(t *testing.T) {
		ctx, st := t.Context(), newValkey(t, addr)
		st.Retain = 100 * time.Millisecond
		st.Add(ctx, "s", nov, Event{"old", 1})
		st.Add(ctx, "s", nov, Event{"mid", 2})
		time.Sleep(250 * time.Millisecond)
		st.Add(ctx, "s", nov, Event{"new", 4}) // trims numbers 1 and 2
		if got, over, _ := st.After(ctx, "s", nov, 1); !over || got != 7 {
			t.Fatalf("After(1) = %d, over %v; want the whole total (7), flagged", got, over)
		}
		if got, over, _ := st.After(ctx, "s", nov, 3); over || got != 0 {
			t.Fatalf("After(3) = %d, %v", got, over)
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
		if _, _, _, err := st.Admit(ctx, Admission{Slot: Slot{"a", "p", "s"}, Period: nov}); err == nil || !strings.Contains(err.Error(), ErrUnavailable.Error()) {
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
		if _, err := st.Add(ctx, "s", nov, Event{fmt.Sprint("sync", time.Now().UnixNano()), 1}); err != nil {
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
	if seq, err := st.Complete(ctx, nov, Slot{"c1", "p", "s"}, Event{"c1", 2}); err != nil || seq < 2 || st.Unreplicated.Load() != 0 {
		t.Fatalf("Complete = %d, %v, unreplicated %d", seq, err, st.Unreplicated.Load())
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
	if _, err := fresh.Add(ctx, "s", nov, Event{"warm", 1}); err != nil {
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
	if _, err := fresh.Add(ctx, "s", nov, Event{"stalled", 1}); err != nil || fresh.Unreplicated.Load() != 1 {
		t.Fatalf("err %v, unreplicated %d: an add the stalled replica never saw must be counted", err, fresh.Unreplicated.Load())
	}
	if err := <-stalled; err != nil {
		t.Fatal(err)
	}
	st.Unreplicated.Store(0)
	st.ReplicaAcks = 2 // more than there are
	if _, err := st.Add(ctx, "s", nov, Event{"x", 1}); err != nil || st.Unreplicated.Load() != 1 {
		t.Fatalf("err %v, unreplicated %d: one replica short must be counted", err, st.Unreplicated.Load())
	}
}
