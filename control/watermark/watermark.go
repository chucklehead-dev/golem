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

	l := index(rows, nums)
	marks := map[Key]int64{}
	for k, top := range l.highest {
		w, rebuilt, err := j.advance(ctx, l, k, top, now, timeout)
		if err != nil {
			return nil, nil, err
		}
		marks[k] = w
		reconstructed = append(reconstructed, rebuilt...)
	}
	results = l.known(marks, now, timeout)
	j.forget(marks)
	return results, reconstructed, nil
}

// ledger is the ledger as one run reads it.
type ledger struct {
	byEvent map[string]Row   // the rows, by event
	numOf   map[string]int64 // an event's number at its site
	have    map[Key]map[int64]bool
	// highest is each counter's highest number given: a number whose row
	// has not arrived stalls the mark too, since the counter has it and
	// the ledger not.
	highest map[Key]int64
}

// index reads the rows and numbering records: the numbers whose rows the
// ledger holds, per counter, and each counter's highest number.
func index(rows []Row, nums []Numbering) *ledger {
	l := &ledger{byEvent: make(map[string]Row, len(rows)), numOf: map[string]int64{}, have: map[Key]map[int64]bool{}, highest: map[Key]int64{}}
	for _, r := range rows {
		l.byEvent[r.Event] = r
	}
	// The lowest number wins if an event's record repeats.
	for _, n := range nums {
		if old, ok := l.numOf[n.Event]; !ok || n.Seq < old {
			l.numOf[n.Event] = n.Seq
		}
	}
	for ev, seq := range l.numOf {
		if r, ok := l.byEvent[ev]; ok && seq != 0 {
			l.holds(r.Key, seq)
			l.highest[r.Key] = max(l.highest[r.Key], seq)
		}
	}
	for _, n := range nums {
		if n.Seq > 0 {
			l.highest[n.Key] = max(l.highest[n.Key], n.Seq)
		}
	}
	return l
}

// holds records that the ledger has the row numbered seq in k's counter.
func (l *ledger) holds(k Key, seq int64) {
	if l.have[k] == nil {
		l.have[k] = map[int64]bool{}
	}
	l.have[k][seq] = true
}

// advance moves k's mark up the contiguous prefix of numbers the ledger
// holds (or that were declared lost), toward top. A missing number stalls
// it until the gap timeout; then its row is rebuilt from the counter
// (added to l, and returned for the ledger), or the number is declared lost
// and the mark moves past it (its cost leaves known spend; reconciliation
// books it, ADR 0031).
func (j *Job) advance(ctx context.Context, l *ledger, k Key, top int64, now time.Time, timeout time.Duration) (w int64, rebuilt []Row, err error) {
	for w < top {
		next := gap{k, w + 1}
		if l.have[k][next.seq] || j.lost[next] {
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
		r, ok, err := j.reconstruct(ctx, k, next.seq, now)
		switch {
		case err != nil:
			return 0, nil, err
		case ok:
			if _, real := l.byEvent[r.Event]; !real {
				l.byEvent[r.Event] = r
				rebuilt = append(rebuilt, r)
			}
			l.numOf[r.Event] = next.seq
			l.holds(k, next.seq)
		default:
			j.lost[next] = true
		}
		delete(j.gaps, next)
	}
	return w, rebuilt, nil
}

// known is each site's result: the mark, and the known spend its snapshot
// carries. That is every row of the scope and period, less this site's
// counted rows its counter covers: numbered above the mark, or not yet
// numbered and not yet past the timeout. A site has a result for each
// scope and period it has rows or numbers in, sorted by scope, period and
// site.
func (l *ledger) known(marks map[Key]int64, now time.Time, timeout time.Duration) []Result {
	type scopePeriod struct{ scope, period string }
	balance := map[scopePeriod]int64{}
	covered := map[Key]int64{}
	keys := map[Key]bool{}
	for k := range marks {
		keys[k] = true
	}
	for _, r := range l.byEvent {
		keys[r.Key] = true
		balance[scopePeriod{r.Scope, r.Period}] += r.Cost
		if !r.Counted {
			continue
		}
		seq, numbered := l.numOf[r.Event]
		switch {
		case numbered && seq == 0: // never reached the counter
		case numbered && seq > marks[r.Key]:
			covered[r.Key] += r.Cost
		case !numbered && now.Sub(r.At) < timeout: // its number is on its way
			covered[r.Key] += r.Cost
		}
	}
	var results []Result
	for k := range keys {
		results = append(results, Result{Key: k, Watermark: marks[k], Spend: balance[scopePeriod{k.Scope, k.Period}] - covered[k]})
	}
	slices.SortFunc(results, func(a, b Result) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Period, b.Period), cmp.Compare(a.Site, b.Site))
	})
	return results
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
