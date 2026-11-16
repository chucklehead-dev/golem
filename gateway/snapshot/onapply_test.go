package snapshot

import (
	"testing"
	"time"
)

// TestOnApplyOutsideTheLock: OnApply (the limiter's update and the sweep
// of every call in flight) runs outside the holder's lock: while it runs,
// the floor can be read, as the bucket source's next poll does.
func TestOnApplyOutsideTheLock(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	h := &Holder{Now: func() time.Time { return now }}
	var order []uint64
	h.OnApply = func(c *Compiled) {
		read := make(chan uint64, 1)
		go func() { read <- h.Floor() }() // another goroutine's read, while the callback runs
		select {
		case f := <-read:
			order = append(order, f)
		case <-time.After(5 * time.Second):
			t.Error("Floor waited on OnApply: the callback runs under the holder's lock")
		}
	}
	for _, seq := range []uint64{1, 2} {
		if err := h.Apply(snap(seq, now.Add(time.Hour)), "h", fixed{"a/key": "sk"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("the floors read in the callbacks: %v; want 1 then 2", order)
	}
}
