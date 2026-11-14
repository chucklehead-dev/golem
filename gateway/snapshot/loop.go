package snapshot

import (
	"context"
	"time"
)

// repeat is both sources' Run: it calls try at every interval (default one
// second) until ctx ends, first right away if immediately is set. A
// failure is passed to failed the first time it is seen, not at every
// tick, and again only once it changes or has cleared.
func repeat(ctx context.Context, interval time.Duration, immediately bool, try func() error, failed func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var last string
	for first := true; ; first = false {
		if !first || immediately {
			switch err := try(); {
			case err == nil:
				last = ""
			case err.Error() != last:
				failed(err)
				last = err.Error()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
