package snapshot

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// TestRepeat: both sources' loop reports a failure once, not at every
// tick, until it changes or clears; Dir's first try waits an interval
// (its caller loaded already), Bucket's does not.
func TestRepeat(t *testing.T) {
	e1, e2 := errors.New("one"), errors.New("two")
	for _, immediately := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			results := []error{e1, e1, nil, e1, e2, e2}
			var tried []time.Duration
			var reported []string
			start := time.Now()
			ctx, cancel := context.WithCancel(t.Context())
			try := func() error {
				tried = append(tried, time.Since(start))
				err := results[0]
				if results = results[1:]; len(results) == 0 {
					cancel()
				}
				return err
			}
			repeat(ctx, 0, immediately, try, func(err error) { reported = append(reported, err.Error()) })
			if want := []string{"one", "one", "two"}; !slices.Equal(reported, want) {
				t.Errorf("reported %q, want %q", reported, want)
			}
			first := time.Duration(0)
			if !immediately {
				first = time.Second
			}
			if tried[0] != first || tried[1]-tried[0] != time.Second {
				t.Errorf("immediately %v: tried at %v, want first at %v and then every second (the default)", immediately, tried, first)
			}
		})
	}
}
