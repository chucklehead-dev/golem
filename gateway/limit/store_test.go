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

func testStore(t *testing.T, newStore func(t *testing.T) Store) {
	t.Run("limits", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		ok, err := st.Acquire(ctx, Slot{"a", "p1", "s"}, 1, 2)
		if !ok || err != nil {
			t.Fatal(ok, err)
		}
		if ok, _ := st.Acquire(ctx, Slot{"b", "p1", "s"}, 1, 2); ok {
			t.Fatal("a second slot for p1 with a principal limit of 1")
		}
		if ok, _ := st.Acquire(ctx, Slot{"c", "p2", "s"}, 1, 2); !ok {
			t.Fatal("p2's first slot")
		}
		if ok, _ := st.Acquire(ctx, Slot{"d", "p3", "s"}, 1, 2); ok {
			t.Fatal("a third slot in a scope limited to 2")
		}
		if ok, _ := st.Acquire(ctx, Slot{"d", "p3", "s"}, 1, 0); !ok {
			t.Fatal("no scope limit: p3's first slot")
		}
		st.Release(ctx, Slot{"a", "p1", "s"})
		if ok, _ := st.Acquire(ctx, Slot{"b", "p1", "s"}, 1, 0); !ok {
			t.Fatal("after release")
		}
		// Re-registering ignores limits: the calls are running.
		if err := st.Reacquire(ctx, []Slot{{"x", "p1", "s"}, {"y", "p1", "s"}}); err != nil {
			t.Fatal(err)
		}
		if ok, _ := st.Acquire(ctx, Slot{"z", "p9", "s"}, 1, 4); ok {
			t.Fatal("the re-registered slots count against the scope")
		}
	})
	t.Run("counter", func(t *testing.T) {
		ctx, st := t.Context(), newStore(t)
		for _, tc := range []struct {
			e    Event
			want int64
		}{{Event{"e1", 3}, 1}, {Event{"e2", 5}, 2}, {Event{"e1", 3}, 1}} { // twice is once
			if seq, err := st.Add(ctx, "s", tc.e); err != nil || seq != tc.want {
				t.Fatalf("Add(%v) = %d, %v; want %d", tc.e, seq, err, tc.want)
			}
		}
		st.Add(ctx, "other", Event{"e9", 100})
		for w, want := range map[int64]int64{0: 8, 1: 5, 2: 0, 7: 0} {
			if got, over, err := st.After(ctx, "s", w); err != nil || over || got != want {
				t.Errorf("After(%d) = %d, %v, %v; want %d", w, got, over, err, want)
			}
		}
	})
}

func sortEvents(es []Event) {
	slices.SortFunc(es, func(a, b Event) int { return strings.Compare(a.ID, b.ID) })
}

func TestMemStore(t *testing.T) { testStore(t, func(*testing.T) Store { return NewMem() }) }

// valkeyAddr is a Valkey (or Redis) to test against: GOLEM_VALKEY_ADDR, or a
// redis-server or valkey-server started for the test.
func valkeyAddr(t *testing.T) string {
	if a := os.Getenv("GOLEM_VALKEY_ADDR"); a != "" {
		return a
	}
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
	cmd := exec.Command(bin, "--port", fmt.Sprint(port), "--save", "", "--appendonly", "no")
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
func newValkey(t *testing.T, addr string) *Valkey {
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
		st.Acquire(ctx, Slot{"a", "p1", "s"}, 1, 0)
		st.Acquire(ctx, Slot{"b", "p2", "s"}, 1, 0)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			st.Reacquire(ctx, []Slot{{"a", "p1", "s"}}) // a's gateway renews; b's crashed
			if got, _ := st.ScopeSlots(ctx, "s"); slices.Equal(got, []string{"a"}) {
				// An expired slot is dropped when the next acquire looks.
				if ok, _ := st.Acquire(ctx, Slot{"c", "p2", "s"}, 1, 0); !ok {
					t.Fatal("p2's crashed call still holds its slot")
				}
				return
			}
			st.Acquire(ctx, Slot{"probe", "p-probe", "s"}, 1, 0) // trims expired slots
			st.Release(ctx, Slot{"probe", "p-probe", "s"})
			time.Sleep(50 * time.Millisecond) // TTLs are on the server's clock
		}
		t.Fatal("b's slot never expired")
	})
	t.Run("trimmed history over-counts, never under-counts", func(t *testing.T) {
		ctx, st := t.Context(), newValkey(t, addr)
		st.Retain = 100 * time.Millisecond
		st.Add(ctx, "s", Event{"old", 1})
		st.Add(ctx, "s", Event{"mid", 2})
		time.Sleep(250 * time.Millisecond)
		st.Add(ctx, "s", Event{"new", 4}) // trims numbers 1 and 2
		if got, over, _ := st.After(ctx, "s", 1); !over || got != 7 {
			t.Fatalf("After(1) = %d, over %v; want the whole total (7), flagged", got, over)
		}
		if got, over, _ := st.After(ctx, "s", 3); over || got != 0 {
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
		if _, err := st.Acquire(ctx, Slot{"a", "p", "s"}, 1, 0); err == nil || !strings.Contains(err.Error(), ErrUnavailable.Error()) {
			t.Fatalf("%v, want ErrUnavailable", err)
		}
	})
}
