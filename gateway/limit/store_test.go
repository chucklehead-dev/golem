package limit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

// The store contract, run against every Store.

// acquire admits a slot with no budget: the slot half of Admit.
func acquire(t *testing.T, st Store, s Slot, principalLimit, scopeLimit int) bool {
	t.Helper()
	o, err := st.Admit(t.Context(), Admission{Slot: s, PrincipalLimit: principalLimit, ScopeLimit: scopeLimit})
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
	})
	t.Run("a principal's limit is per scope", func(t *testing.T) {
		// p1's call in s2 does not count against its limit in s, nor
		// fill it: another scope's calls cannot exhaust a principal's
		// slots here. Scope and principal IDs that would join to the same
		// string ("a:b" + "c", "a" + "b:c") are distinct.
		ctx, st := t.Context(), newStore(t)
		if !acquire(t, st, Slot{"a", "p1", "s2"}, 1, 0) || !acquire(t, st, Slot{"b", "p1", "s"}, 1, 0) {
			t.Fatal("p1's first call in each scope")
		}
		if acquire(t, st, Slot{"c", "p1", "s"}, 1, 0) {
			t.Fatal("a second call for p1 in s with a principal limit of 1")
		}
		if !acquire(t, st, Slot{"d", "c", "a:b"}, 1, 0) || !acquire(t, st, Slot{"e", "b:c", "a"}, 1, 0) {
			t.Fatal("scope a:b's principal c and scope a's principal b:c are not one")
		}
		st.Release(ctx, Slot{"a", "p1", "s2"})
		if acquire(t, st, Slot{"c", "p1", "s"}, 1, 0) {
			t.Fatal("releasing p1's call in s2 freed its slot in s")
		}
	})
	t.Run("budget", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		for _, tc := range []struct {
			name          string
			known, budget int64
			want          Outcome
		}{
			{"under", 7, 8, Admitted},
			{"at", 7, 7, OverBudget},
			{"over", 9, 7, OverBudget},
			{"none", 1 << 50, 0, Admitted},
			{"negative", 0, -1, OverBudget},
		} {
			o, err := st.Admit(ctx, Admission{Slot: Slot{tc.name, tc.name, "s"}, KnownSpend: tc.known, Budget: tc.budget})
			if err != nil || o != tc.want {
				t.Errorf("%s: %v %v; want %v", tc.name, o, err, tc.want)
			}
		}
		// Over budget takes no slot.
		if got := len(slotsOf(t, st)); got != 2 {
			t.Errorf("%d slots held, want 2 (the admitted)", got)
		}
	})
	t.Run("rate buckets", func(t *testing.T) {
		// A minute's worth to start; a request past it waits about the
		// refill of one; several meters are taken from all or none.
		ctx, st := t.Context(), newStore(t)
		take := func(ms ...Meter) (bool, int, time.Duration) {
			t.Helper()
			ok, over, wait, err := st.Take(ctx, ms)
			if err != nil {
				t.Fatal(err)
			}
			return ok, over, wait
		}
		r := Meter{Key: "r", PerMinute: 2}
		if a, _, _ := take(r); !a {
			t.Fatal("the first request")
		}
		if b, _, _ := take(r); !b {
			t.Fatal("the second request, within the minute's two")
		}
		if ok, _, wait := take(r); ok || wait <= 25*time.Second || wait > 30*time.Second {
			t.Fatalf("a third: %v, wait %v; want refused for about half a minute", ok, wait)
		}
		fresh := Meter{Key: "f", PerMinute: 1}
		if ok, over, _ := take(fresh, r); ok || over != 1 {
			t.Fatalf("a full second meter: %v over %d, want refused over 1", ok, over)
		}
		if ok, _, _ := take(fresh); !ok {
			t.Fatal("the fresh meter was taken from by a refused take")
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
	t.Run("an unreachable server is ErrUnavailable", func(t *testing.T) {
		c, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, DisableCache: true})
		if err != nil {
			t.Fatal(err)
		}
		st := NewValkey(c, "closed")
		c.Close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := st.Admit(ctx, Admission{Slot: Slot{"a", "p", "s"}}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%v, want ErrUnavailable", err)
		}
	})
}

// TestTransportFailureIsUnavailable: a transport failure is ErrUnavailable and keeps its cause, so a
// caller can tell a deadline from a refused connection; a server's reply
// is not unavailability.
func TestTransportFailureIsUnavailable(t *testing.T) {
	err := wrap(context.DeadlineExceeded)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v: want both ErrUnavailable and its cause", err)
	}
	if wrap(nil) != nil || wrap(valkey.Nil) != nil {
		t.Fatal("no error, or a nil reply, is not a failure")
	}
}
