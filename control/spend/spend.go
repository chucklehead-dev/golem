// Package spend is golem-control's known-spend job (ADR 0048,
// control-plane.md §6): each scope's spend this budget period, from the
// ledger in ClickHouse, of the calls that ended before a boundary B, a
// minute an ingestion allowance before now. Snapshots carry it with B, and
// a gateway adds its site counter's minutes from B on, so each call is
// counted on one side of B only (contracts.md §3).
package spend

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	budget "github.com/chucklehead-dev/golem/lib/period"
)

// Known is the ledger's spend for one period, by scope, of the calls that
// ended before Since.
type Known struct {
	Period string           // "2026-11"
	Since  time.Time        // B: a whole minute
	Spend  map[string]int64 // micro-units, by scope ID
}

// Ledger reads the deduplicated ledger.
type Ledger interface {
	// Spend sums each scope's cost in period over the calls that ended
	// before before.
	Spend(ctx context.Context, period string, before time.Time) (map[string]int64, error)
}

// Age is how far the result's boundary is behind now: an allowance and a
// minute at most when the ledger answers, more while it does not (a
// result is kept through failures). For a metric.
func (k Known) Age(now time.Time) time.Duration { return now.Sub(k.Since) }

// Job keeps Known current.
type Job struct {
	Ledger Ledger
	// Allowance is how long after a call's end its row is surely in
	// ClickHouse (default five minutes): B is that far behind now.
	Allowance time.Duration
	// Location is the budget periods' time zone: the gateways' -period-zone.
	Location *time.Location
	Now      func() time.Time // the clock (tests set it)

	// A result is kept through failures, but a gateway adds to its spend
	// only the counter's minutes it still retains: from B, or no further
	// back than its retention. A gateway may use a snapshot for its lease
	// after publication, so a result whose boundary is more than
	// Retention - Lease - Margin old is dropped (MaxAge): the spend
	// between its boundary and the retention's start would be counted
	// nowhere. Snapshots then carry no known spend for the period, and
	// gateways count their counter's whole total.

	// Retention is the gateways' counter retention (default six hours,
	// limit.Mem's and limit.Valkey's Retain).
	Retention time.Duration
	// Lease is the snapshots' authorization lease (default four hours,
	// golem-control's -lease).
	Lease time.Duration
	// Margin covers the publication's path to the gateways and their
	// clocks (default ten minutes).
	Margin time.Duration

	mu   sync.Mutex
	last *Known
}

// Defaults of the Job's durations.
const (
	DefaultAllowance = 5 * time.Minute
	DefaultRetention = 6 * time.Hour
	DefaultLease     = 4 * time.Hour
	DefaultMargin    = 10 * time.Minute
)

// MaxAge is how old a result's boundary may be and still be published:
// Retention - Lease - Margin.
func (j *Job) MaxAge() time.Duration {
	return cmp.Or(j.Retention, DefaultRetention) - cmp.Or(j.Lease, DefaultLease) - cmp.Or(j.Margin, DefaultMargin)
}

// Step reads the ledger's spend at a new boundary. On a failure the last
// result is kept for its own period (a new period's spend is not the old
// one's) while its boundary is within MaxAge, and the error returned;
// Current says what to publish.
func (j *Job) Step(ctx context.Context) error {
	now := j.now()
	loc := cmp.Or(j.Location, time.UTC)
	allowance := cmp.Or(j.Allowance, DefaultAllowance)
	// A fresh result is an allowance and a minute old: a MaxAge below that
	// would drop every one, and publish no known spend at all.
	if maxAge := j.MaxAge(); maxAge <= allowance+time.Minute {
		j.set(nil)
		return fmt.Errorf("known spend: the retention %v less the lease %v and the margin %v leaves %v, no more than the allowance %v and a minute: lengthen the gateways' retention or shorten the lease",
			cmp.Or(j.Retention, DefaultRetention), cmp.Or(j.Lease, DefaultLease), cmp.Or(j.Margin, DefaultMargin), maxAge, allowance)
	}
	since := now.Add(-allowance).Truncate(time.Minute)
	period := budget.Of(now, loc)
	// A boundary still in the previous period counts nothing of this one:
	// this period starts from nothing until B passes its start.
	if budget.Of(since, loc) != period {
		j.set(&Known{Period: period, Since: since, Spend: map[string]int64{}})
		return nil
	}
	s, err := j.Ledger.Spend(ctx, period, since)
	if err != nil {
		j.forgetOtherThan(period)
		if b, dropped := j.forgetOlder(now); dropped {
			return fmt.Errorf("known spend for %s: %w (the last result, to %s, is older than %v and dropped: snapshots carry no known spend until the ledger answers)",
				period, err, b.UTC().Format(time.RFC3339), j.MaxAge())
		}
		return fmt.Errorf("known spend for %s: %w (the last result is kept)", period, err)
	}
	j.set(&Known{Period: period, Since: since, Spend: s})
	return nil
}

func (j *Job) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Job) set(k *Known) {
	j.mu.Lock()
	j.last = k
	j.mu.Unlock()
}

// forgetOtherThan drops the last result unless it is of period: an older
// result stays exact for its own period (its boundary is earlier, and the
// gateways add the minutes from it), but not for another.
func (j *Job) forgetOtherThan(period string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last != nil && j.last.Period != period {
		j.last = nil
	}
}

// forgetOlder drops the last result if its boundary is more than MaxAge
// before now, and reports the boundary dropped.
func (j *Job) forgetOlder(now time.Time) (time.Time, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last == nil || j.last.Age(now) <= j.MaxAge() {
		return time.Time{}, false
	}
	b := j.last.Since
	j.last = nil
	return b, true
}

// Current is the latest result, or false if there is none yet or it is
// older than MaxAge: a snapshot then carries no known spend, and gateways
// count their site's counter alone.
func (j *Job) Current() (Known, bool) {
	j.forgetOlder(j.now())
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last == nil {
		return Known{}, false
	}
	return *j.last, true
}

// Age is the current result's age (Known.Age), or false if there is none:
// for a metric.
func (j *Job) Age() (time.Duration, bool) {
	k, ok := j.Current()
	if !ok {
		return 0, false
	}
	return k.Age(j.now()), true
}

// ClickHouse is the ledger over ClickHouse's HTTP interface: one query,
// its values bound as parameters (never in the query's text).
type ClickHouse struct {
	URL            string // e.g. https://clickhouse.golem.svc:8443
	User, Password string
	Database       string // default golem (tests use their own)
	Client         *http.Client
}

// spendQuery sums the period's events that ended before the boundary, one
// row per event_id (deploy/clickhouse/spend.sql: golem.spend_by_event,
// deduplicated by its engine; FINAL makes that exact at read time, within
// the one partition the period names).
const spendQuery = `SELECT scope_id, toString(sum(cost_micros)) AS spend
FROM {db}.spend_by_event FINAL
WHERE period = {period:Date} AND ended_at < {before:DateTime64(3, 'UTC')}
GROUP BY scope_id
FORMAT JSONEachRow`

// Spend runs the query.
func (c ClickHouse) Spend(ctx context.Context, period string, before time.Time) (map[string]int64, error) {
	q := url.Values{
		"param_period": {budget.FirstDay(period)},
		"param_before": {before.UTC().Format("2006-01-02 15:04:05.000")},
	}
	query := strings.Replace(spendQuery, "{db}", cmp.Or(c.Database, "golem"), 1)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+"/?"+q.Encode(), strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.Header.Set("X-ClickHouse-User", c.User)
		req.Header.Set("X-ClickHouse-Key", c.Password)
	}
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ClickHouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return readSpend(io.LimitReader(resp.Body, 64<<20))
}

// readSpend reads the query's JSONEachRow rows: scope to spend.
func readSpend(r io.Reader) (map[string]int64, error) {
	out := map[string]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var row struct {
			Scope string `json:"scope_id"`
			Spend string `json:"spend"` // a string: Int64 sums are quoted in JSON by default
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
		n, err := strconv.ParseInt(row.Spend, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
		out[row.Scope] = n
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

var _ Ledger = ClickHouse{}
