package watermark

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

var k = Key{"site-a", "team", "2026-11"}

// TestAgainstBruteForce: a site's counter numbers events 1..n; their rows
// and numbering records reach the ledger in any order, some twice. At
// every point, known spend plus what the counter holds above W is the true
// total: nothing lost, nothing counted twice (ADR 0042). Other sites'
// rows are always in the balance.
func TestAgainstBruteForce(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for seed := range 300 {
		rng := rand.New(rand.NewPCG(uint64(seed), 7))
		n := 1 + rng.IntN(12)
		type ev struct {
			id   string
			cost int64
		}
		var counter []ev
		total := int64(0)
		var arrivals []any
		for i := range n {
			e := ev{fmt.Sprint("e", i), int64(1 + rng.IntN(9))}
			counter = append(counter, e)
			total += e.cost
			row := Row{Event: e.id, Key: k, Cost: e.cost, Counted: true, At: now}
			num := Numbering{Event: e.id, Key: k, Seq: int64(i + 1)}
			arrivals = append(arrivals, row, num)
			if rng.IntN(4) == 0 {
				arrivals = append(arrivals, row) // at least once
			}
		}
		other := Row{Event: "x", Key: Key{"site-b", "team", "2026-11"}, Cost: 100, Counted: true, At: now}
		arrivals = append(arrivals, other, Numbering{Event: "x", Key: other.Key, Seq: 1})
		total += 100
		rng.Shuffle(len(arrivals), func(i, j int) { arrivals[i], arrivals[j] = arrivals[j], arrivals[i] })

		j := &Job{GapTimeout: time.Hour, Now: func() time.Time { return now }}
		var rows []Row
		var nums []Numbering
		for step, a := range arrivals {
			switch a := a.(type) {
			case Row:
				rows = append(rows, a)
			case Numbering:
				nums = append(nums, a)
			}
			res, _, err := j.Run(t.Context(), rows, nums)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range res {
				if r.Key != k {
					continue
				}
				// The brute force: what site-a's counter holds above W, and
				// the events that happened but whose rows are still in flight
				// elsewhere (the counter has them all).
				above := int64(0)
				for i, e := range counter {
					if int64(i+1) > r.Watermark {
						above += e.cost
					}
				}
				arrivedOther := false
				for _, row := range rows {
					arrivedOther = arrivedOther || row.Event == "x"
				}
				want := total
				if !arrivedOther {
					want -= 100 // site-b's event reaches site-a only through the ledger
				}
				if got := r.Spend + above; got != want {
					t.Fatalf("seed %d, step %d: spend %d + counter above W=%d %d = %d, want %d", seed, step, r.Spend, r.Watermark, above, got, want)
				}
			}
		}
	}
}

type fakeCounter map[int64][2]any

func (f fakeCounter) At(_ context.Context, _ Key, seq int64) (string, int64, bool, error) {
	e, ok := f[seq]
	if !ok {
		return "", 0, false, nil
	}
	return e[0].(string), e[1].(int64), true, nil
}

// TestGaps: a missing row stalls the mark until the timeout; then it is
// rebuilt from the counter, or, if the counter no longer has it, its number
// is declared lost and the mark moves on.
func TestGaps(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rows := []Row{{Event: "e1", Key: k, Cost: 3, Counted: true, At: now}, {Event: "e3", Key: k, Cost: 5, Counted: true, At: now}}
	nums := []Numbering{{"e1", k, 1}, {"e2", k, 2}, {"e3", k, 3}} // e2's row was lost

	for _, tc := range []struct {
		name      string
		counter   Counter
		wantSpend int64
		rebuilt   int
	}{
		{"rebuilt from the counter", fakeCounter{2: {"e2", int64(4)}}, 12, 1},
		{"declared lost", fakeCounter{}, 8, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := now
			j := &Job{GapTimeout: 15 * time.Minute, Counter: tc.counter, Now: func() time.Time { return clock }}
			res, _, _ := j.Run(t.Context(), rows, nums)
			if res[0].Watermark != 1 || res[0].Spend != 3 {
				t.Fatalf("%+v: stalled at 1, e3 left to the counter", res[0])
			}
			clock = clock.Add(16 * time.Minute)
			res, rebuilt, _ := j.Run(t.Context(), rows, nums)
			if res[0].Watermark != 3 || res[0].Spend != tc.wantSpend || len(rebuilt) != tc.rebuilt {
				t.Fatalf("%+v, rebuilt %v", res[0], rebuilt)
			}
			// The caller writes a rebuilt row to the ledger; either way the
			// mark stays passed.
			if res, _, _ := j.Run(t.Context(), append(rows, rebuilt...), nums); res[0].Watermark != 3 || res[0].Spend != tc.wantSpend {
				t.Fatalf("%+v: the gap stays resolved", res[0])
			}
		})
	}
}

// TestUnnumbered: a counted row without its number is the counter's until
// the timeout, then counts as never numbered; number 0 counts at once.
func TestUnnumbered(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clock := now
	j := &Job{GapTimeout: 15 * time.Minute, Now: func() time.Time { return clock }}
	rows := []Row{{Event: "a", Key: k, Cost: 3, Counted: true, At: now}, {Event: "b", Key: k, Cost: 5, Counted: true, At: now},
		{Event: "c", Key: k, Cost: 7, Counted: false, At: now}}
	nums := []Numbering{{"b", k, 0}}
	if res, _, _ := j.Run(t.Context(), rows, nums); res[0].Spend != 12 {
		t.Fatalf("%+v: b (never numbered) and c (uncounted) count; a is the counter's", res[0])
	}
	clock = clock.Add(16 * time.Minute)
	if res, _, _ := j.Run(t.Context(), rows, nums); res[0].Spend != 15 {
		t.Fatalf("%+v: past the timeout, a counts too", res[0])
	}
}
