// Package spend is golem-control's known-spend job. It sums each scope's
// spend this budget period from the ledger in ClickHouse, counting the calls
// that ended before a boundary.
//
// The boundary is a whole minute, an ingestion allowance before now.
// Snapshots carry the sums with their boundary, and a gateway's known spend
// is the snapshot's alone (ADR 0031).
package spend

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/lib/period"
)

// Result is one read of the ledger: each scope's spend in one period, of the
// calls that ended before Since.
type Result struct {
	Period string // as period.Of names it, "2026-11"
	// Since is the boundary: only calls that ended before it are counted.
	// Always a whole minute.
	Since time.Time
	Totals
}

// Totals is spend in micro-units, by scope ID.
type Totals struct {
	Scopes map[string]int64
}

// noSpend is Totals with every scope at nothing.
func noSpend() Totals { return Totals{Scopes: map[string]int64{}} }

// Ledger reads the deduplicated ledger.
type Ledger interface {
	// Spend sums each scope's cost in period over the calls that ended
	// before boundary.
	Spend(ctx context.Context, period string, boundary time.Time) (Totals, error)
}

// Ingestion is a Ledger that also says how far each site's rows have
// arrived. The boundary assumes every call that ended before it is in the
// ledger. That holds only while each site's rows arrive within the
// allowance. A Ledger without Ingestion reports no lag.
type Ingestion interface {
	// Latest is, by site, the latest ended_at of any row ingested from it.
	Latest(ctx context.Context) (map[string]time.Time, error)
}

// SiteLag is one site's ingestion lag as of the job's clock.
type SiteLag struct {
	// Latest is the latest ended_at ingested from the site; zero with
	// NoRows.
	Latest time.Time
	// NoRows reports that the site was in an earlier read and is in the
	// ledger no more. The table was rebuilt or truncated, or this is
	// another ClickHouse.
	NoRows bool
	// Lag is now − Latest; zero with NoRows.
	Lag time.Duration
	// Behind reports that Lag exceeds the allowance, or NoRows. Known
	// spend then misses the site's calls that ended before the boundary
	// and have not arrived. A site that served nothing lately is behind
	// too, because the ledger cannot tell idle from late.
	Behind bool
}

// Age is how far the result's boundary is behind now. It is at most an
// allowance and a minute while the ledger answers, and grows while it does
// not. For a metric.
func (r Result) Age(now time.Time) time.Duration { return now.Sub(r.Since) }

// Job keeps a Result current.
type Job struct {
	Ledger Ledger
	// Allowance is how long after a call ends its row is surely in
	// ClickHouse. The boundary is this far behind now. Default five minutes.
	Allowance time.Duration
	// Location is the budget periods' time zone, the gateways' -period-zone.
	Location *time.Location
	Now      func() time.Time // the clock (tests set it)

	// StaleAfter is how old a result's boundary may be before the result
	// is reported stale, for an alert. A result is kept through failures
	// and stays published, because older spend is never wrong spend.
	// Default 90 minutes.
	StaleAfter time.Duration

	mu     sync.Mutex
	last   *Result
	latest map[string]time.Time // by site, as last read; zero: no rows any more
}

// Defaults of the Job's durations.
const (
	DefaultAllowance  = 5 * time.Minute
	DefaultStaleAfter = 90 * time.Minute
)

// MaxAge is how old a result's boundary may be before it is stale.
func (j *Job) MaxAge() time.Duration { return cmp.Or(j.StaleAfter, DefaultStaleAfter) }

// Validate reports a configuration under which every result is stale. A
// fresh result is already an allowance and a minute old. golem-control
// checks it at start.
func (j *Job) Validate() error {
	allowance := cmp.Or(j.Allowance, DefaultAllowance)
	if maxAge := j.MaxAge(); maxAge <= allowance+time.Minute {
		return fmt.Errorf("known spend: -spend-stale-after %v is no more than the allowance %v and a minute: every result would be stale; lengthen it", maxAge, allowance)
	}
	return nil
}

// Step reads the ledger's spend at a new boundary and, from an Ingestion,
// each site's latest row. On a failure the last result is kept if it is of
// the current period, and the error says whether it is stale. A lagging
// site never moves the boundary; Lags reports it, for an alert.
func (j *Job) Step(ctx context.Context) error {
	return errors.Join(j.stepSpend(ctx), j.stepLatest(ctx))
}

// stepLatest reads each site's latest ingested row. On a failure the last
// read is kept, so the lags grow with the clock and the alert stays on.
func (j *Job) stepLatest(ctx context.Context) error {
	in, ok := j.Ledger.(Ingestion)
	if !ok {
		return nil
	}
	got, err := in.Latest(ctx)
	if err != nil {
		return fmt.Errorf("ingestion lag: %w (the last read is kept: the lags grow until the ledger answers)", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	next := map[string]time.Time{}
	for site := range j.latest {
		next[site] = time.Time{} // seen before: no rows, unless read now
	}
	for site, t := range got {
		next[site] = t
	}
	j.latest = next
	return nil
}

// Lags is each site's ingestion lag, as of now and the last read. It is
// empty before the first read, and for a Ledger that is not an Ingestion.
// For a metric, and an alert on Behind.
func (j *Job) Lags() map[string]SiteLag {
	now := j.now()
	allowance := cmp.Or(j.Allowance, DefaultAllowance)
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]SiteLag, len(j.latest))
	for site, t := range j.latest {
		if t.IsZero() {
			out[site] = SiteLag{NoRows: true, Behind: true}
			continue
		}
		lag := now.Sub(t)
		out[site] = SiteLag{Latest: t, Lag: lag, Behind: lag > allowance}
	}
	return out
}

func (j *Job) stepSpend(ctx context.Context) error {
	now := j.now()
	loc := cmp.Or(j.Location, time.UTC)
	since := now.Add(-cmp.Or(j.Allowance, DefaultAllowance)).Truncate(time.Minute)
	cur := period.Of(now, loc)
	// A boundary still in the previous period counts nothing of this one.
	// This period's spend is zero until the boundary passes its start.
	if period.Of(since, loc) != cur {
		j.set(&Result{Period: cur, Since: since, Totals: noSpend()})
		return nil
	}
	s, err := j.Ledger.Spend(ctx, cur, since)
	if err != nil {
		j.forgetOtherThan(cur)
		if r, ok := j.Current(); ok && r.Age(now) > j.MaxAge() {
			return fmt.Errorf("known spend for %s: %w (the last result is kept, but stale: its boundary %s is older than %v, so gateways miss every call since it; alert)",
				cur, err, r.Since.UTC().Format(time.RFC3339), j.MaxAge())
		}
		return fmt.Errorf("known spend for %s: %w (the last result is kept)", cur, err)
	}
	j.set(&Result{Period: cur, Since: since, Totals: s})
	return nil
}

func (j *Job) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Job) set(r *Result) {
	j.mu.Lock()
	j.last = r
	j.mu.Unlock()
}

// forgetOtherThan drops the last result unless it is of period p. An older
// result only undercounts its own period, but says nothing of another.
func (j *Job) forgetOtherThan(p string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last != nil && j.last.Period != p {
		j.last = nil
	}
}

// Current is the latest result, or false if there is none yet. Without one
// a snapshot carries no known spend. A stale result is still current.
func (j *Job) Current() (Result, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last == nil {
		return Result{}, false
	}
	return *j.last, true
}

// Age is the current result's age, or false if there is none. For a metric.
func (j *Job) Age() (time.Duration, bool) {
	r, ok := j.Current()
	if !ok {
		return 0, false
	}
	return r.Age(j.now()), true
}

// Stale reports whether the current result's boundary is older than
// MaxAge. The gateways' known spend then misses every call since the
// boundary. For an alert.
func (j *Job) Stale() bool {
	age, ok := j.Age()
	return ok && age > j.MaxAge()
}

// ClickHouse is the ledger over ClickHouse's HTTP interface. Values are
// bound as query parameters, never written into the query's text.
type ClickHouse struct {
	URL            string // e.g. https://clickhouse.golem.svc:8443
	User, Password string
	Database       string // default golem (tests use their own)
	Client         *http.Client
}

// spendQuery sums, by scope, the period's events that ended before the
// boundary. golem.spend_by_event (deploy/clickhouse/spend.sql) keeps one
// row per event_id once merged. FINAL makes that exact at read time,
// within the one partition the period names.
const spendQuery = `SELECT scope_id, toString(sum(cost_micros)) AS spend
FROM {db}.spend_by_event FINAL
WHERE period = {period:Date} AND ended_at < {before:DateTime64(3, 'UTC')}
GROUP BY scope_id
FORMAT JSONEachRow`

// Spend runs spendQuery for the period named p.
func (c ClickHouse) Spend(ctx context.Context, p string, boundary time.Time) (Totals, error) {
	q := url.Values{
		"param_period": {period.FirstDay(p)},
		"param_before": {boundary.UTC().Format("2006-01-02 15:04:05.000")},
	}
	var out Totals
	err := c.query(ctx, spendQuery, q, func(r io.Reader) (err error) { out, err = readSpend(r); return err })
	return out, err
}

// latestQuery is each site's latest ingested ended_at.
// golem.ingest_by_site (deploy/clickhouse/spend.sql) keeps one row per
// site once merged. max over the unmerged parts makes it exact at read time.
const latestQuery = `SELECT site, toString(max(latest)) AS latest
FROM {db}.ingest_by_site
GROUP BY site
FORMAT JSONEachRow`

// Latest runs latestQuery, for Ingestion.
func (c ClickHouse) Latest(ctx context.Context) (map[string]time.Time, error) {
	var out map[string]time.Time
	err := c.query(ctx, latestQuery, url.Values{}, func(r io.Reader) (err error) { out, err = readLatest(r); return err })
	return out, err
}

// query runs query in c's database, its values bound in params, and hands
// the answer to read.
func (c ClickHouse) query(ctx context.Context, query string, params url.Values, read func(io.Reader) error) error {
	query = strings.Replace(query, "{db}", cmp.Or(c.Database, "golem"), 1)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+"/?"+params.Encode(), strings.NewReader(query))
	if err != nil {
		return err
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
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("ClickHouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return read(io.LimitReader(resp.Body, 64<<20))
}

// readLatest reads latestQuery's rows: site to its latest ended_at.
func readLatest(r io.Reader) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := eachRow(r, func(row struct {
		Site   string `json:"site"`
		Latest string `json:"latest"` // DateTime64(3, 'UTC') as text
	}) error {
		t, err := time.Parse("2006-01-02 15:04:05.000", row.Latest)
		out[row.Site] = t.UTC()
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readSpend reads the query's rows, each a scope's spend.
func readSpend(r io.Reader) (Totals, error) {
	out := noSpend()
	err := eachRow(r, func(row struct {
		Scope string `json:"scope_id"`
		Spend string `json:"spend"` // a string: Int64 sums are quoted in JSON by default
	}) error {
		n, err := strconv.ParseInt(row.Spend, 10, 64)
		if err != nil {
			return err
		}
		out.Scopes[row.Scope] += n
		return nil
	})
	if err != nil {
		return Totals{}, err
	}
	return out, nil
}

// eachRow decodes r's JSONEachRow rows, skipping blank lines, and gives
// each to use. An error names its row.
func eachRow[Row any](r io.Reader, use func(Row) error) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var row Row
		err := json.Unmarshal(sc.Bytes(), &row)
		if err == nil {
			err = use(row)
		}
		if err != nil {
			return fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
	}
	return sc.Err()
}

var (
	_ Ledger    = ClickHouse{}
	_ Ingestion = ClickHouse{}
)
