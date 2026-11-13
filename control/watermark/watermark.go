// Package watermark is golem-control's known-spend job (ADR 0042, 0043):
// from the ledger's rows and numbering records it computes, for each site,
// scope and budget period, the watermark W (every event of that site's
// counter up to W is in the ledger) and the known spend a snapshot carries
// (the ledger's balance with that site's events numbered above W left
// out, which the site's counter covers). It holds no store: the caller
// reads ClickHouse (or a test's ledger) and publishes the results.
package watermark

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// Key is a site's counter for a scope in a budget period.
type Key struct {
	Site, Scope, Period string
}

// Row is a ledger row as the job needs it: deduplicated by event.
type Row struct {
	Event string
	Key
	Cost int64
	// Counted: the site's counter counts the event, so a numbering record
	// is to come. At is when the row was written (its call's end).
	Counted bool
	At      time.Time
}

// Numbering is an event's number in its site's counter; Seq 0: the event
// never reached the counter.
type Numbering struct {
	Event string
	Key
	Seq int64
}

// Counter reads a site's counter (a site-local reader: golem-control reads
// no site's Valkey itself): the event a number was given to, for a gap's
// reconstruction (ADR 0043). ok is false when the counter no longer holds
// it (trimmed, or lost in a failover).
type Counter interface {
	At(ctx context.Context, k Key, seq int64) (event string, cost int64, ok bool, err error)
}

// Result is a site's watermark and its snapshot's known spend for a scope
// and period.
type Result struct {
	Key
	Watermark, Spend int64
}

// Job keeps the state that outlives one run: when each gap was first seen,
// and the numbers declared lost.
type Job struct {
	// GapTimeout is how long a missing number stalls its watermark before
	// it is reconstructed or declared lost (default 15 minutes, ADR 0042).
	// A counted row without a number for this long is taken as never
	// numbered (its increment lost with its gateway).
	GapTimeout time.Duration
	// Counter, if set, reconstructs a gap's row before it is declared lost.
	Counter Counter
	Now     func() time.Time

	gaps map[gap]time.Time
	lost map[gap]bool
}

type gap struct {
	Key
	seq int64
}

// Run computes the results from the ledger as it stands. reconstructed are
// the rows it rebuilt from the counter, for the caller to write to the
// ledger (marked as such) so later runs and reports have them.
func (j *Job) Run(ctx context.Context, rows []Row, nums []Numbering) (results []Result, reconstructed []Row, err error) {
	if j.gaps == nil {
		j.gaps, j.lost = map[gap]time.Time{}, map[gap]bool{}
	}
	now := time.Now()
	if j.Now != nil {
		now = j.Now()
	}
	timeout := cmp.Or(j.GapTimeout, 15*time.Minute)

	byEvent := make(map[string]Row, len(rows))
	for _, r := range rows {
		byEvent[r.Event] = r
	}
	// An event's number at its site; the lowest wins if a record repeats.
	numOf := map[string]int64{}
	for _, n := range nums {
		if old, ok := numOf[n.Event]; !ok || n.Seq < old {
			numOf[n.Event] = n.Seq
		}
	}
	// The numbers whose rows the ledger holds, per counter.
	have := map[Key]map[int64]bool{}
	highest := map[Key]int64{}
	for ev, seq := range numOf {
		r, ok := byEvent[ev]
		if !ok || seq == 0 {
			continue
		}
		if have[r.Key] == nil {
			have[r.Key] = map[int64]bool{}
		}
		have[r.Key][seq] = true
		highest[r.Key] = max(highest[r.Key], seq)
	}
	// Numbers given but whose rows have not arrived also stall the mark:
	// the counter has them, the ledger not.
	for _, n := range nums {
		if n.Seq > 0 {
			highest[n.Key] = max(highest[n.Key], n.Seq)
		}
	}

	marks := map[Key]int64{}
	for k, top := range highest {
		w := int64(0)
		for w < top {
			next := gap{k, w + 1}
			if have[k][next.seq] || j.lost[next] {
				w++
				continue
			}
			first, seen := j.gaps[next]
			if !seen {
				j.gaps[next] = now
				break
			}
			if now.Sub(first) < timeout {
				break
			}
			// Past the timeout: rebuild the row from the counter, else declare
			// the number lost and move the mark past it (its cost leaves known
			// spend; reconciliation books it, ADR 0031).
			if r, ok, err := j.reconstruct(ctx, k, next.seq, now); err != nil {
				return nil, nil, err
			} else if ok {
				if _, real := byEvent[r.Event]; !real {
					byEvent[r.Event] = r
					reconstructed = append(reconstructed, r)
				}
				numOf[r.Event] = next.seq
				if have[k] == nil {
					have[k] = map[int64]bool{}
				}
				have[k][next.seq] = true
			} else {
				j.lost[next] = true
			}
			delete(j.gaps, next)
		}
		marks[k] = w
	}

	// Known spend for each site's counter: every row of the scope and
	// period, less this site's counted rows the counter covers (numbered
	// above W, or not yet numbered and not yet past the timeout).
	balance := map[[2]string]int64{}
	sites := map[[2]string]map[string]bool{}
	for _, r := range byEvent {
		sp := [2]string{r.Scope, r.Period}
		balance[sp] += r.Cost
		if sites[sp] == nil {
			sites[sp] = map[string]bool{}
		}
		sites[sp][r.Site] = true
	}
	for k := range marks {
		sp := [2]string{k.Scope, k.Period}
		if sites[sp] == nil {
			sites[sp] = map[string]bool{}
		}
		sites[sp][k.Site] = true
	}
	covered := map[Key]int64{}
	for _, r := range byEvent {
		if !r.Counted {
			continue
		}
		seq, numbered := numOf[r.Event]
		switch {
		case numbered && seq == 0: // never reached the counter
		case numbered && seq > marks[r.Key]:
			covered[r.Key] += r.Cost
		case !numbered && now.Sub(r.At) < timeout: // its number is on its way
			covered[r.Key] += r.Cost
		}
	}
	for sp, ss := range sites {
		for site := range ss {
			k := Key{site, sp[0], sp[1]}
			results = append(results, Result{Key: k, Watermark: marks[k], Spend: balance[sp] - covered[k]})
		}
	}
	slices.SortFunc(results, func(a, b Result) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Period, b.Period), cmp.Compare(a.Site, b.Site))
	})
	j.forget(marks)
	return results, reconstructed, nil
}

func (j *Job) reconstruct(ctx context.Context, k Key, seq int64, now time.Time) (Row, bool, error) {
	if j.Counter == nil {
		return Row{}, false, nil
	}
	ev, cost, ok, err := j.Counter.At(ctx, k, seq)
	if err != nil || !ok {
		return Row{}, false, err
	}
	return Row{Event: ev, Key: k, Cost: cost, Counted: true, At: now}, true, nil
}

// forget drops the first sightings of gaps the marks have passed. Lost
// numbers are kept: each run works the marks out afresh, and passes over
// them again.
func (j *Job) forget(marks map[Key]int64) {
	for g := range j.gaps {
		if g.seq <= marks[g.Key] {
			delete(j.gaps, g)
		}
	}
}
