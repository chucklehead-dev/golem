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
	"errors"
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

// Known is the ledger's spend for one period of the calls that ended
// before Since, by account (org-model.md §4.2): micro-units by scope ID,
// by member in a scope, and by key (its display ID, the ledger's key_id).
type Known struct {
	Period  string    // "2026-11"
	Since   time.Time // B: a whole minute
	Spend   map[string]int64
	Members map[Member]int64
	Keys    map[string]int64
}

// Member is a principal in a scope: a member's account.
type Member struct{ Scope, Principal string }

// Totals is a ledger read's spend by account, as Known holds it.
type Totals struct {
	Scopes  map[string]int64
	Members map[Member]int64
	Keys    map[string]int64
}

// Ledger reads the deduplicated ledger.
type Ledger interface {
	// Spend sums each account's cost in period over the calls that ended
	// before before.
	Spend(ctx context.Context, period string, before time.Time) (Totals, error)
}

// Ingestion is a Ledger that also says how far each site's rows have
// arrived: the boundary assumes every call that ended before it is in the
// ledger, which holds only while a site's rows arrive within the
// allowance. A Ledger without it reports no lag (Job.Lags is empty).
type Ingestion interface {
	// Latest is, by site, the latest ended_at of any row ingested from it.
	Latest(ctx context.Context) (map[string]time.Time, error)
}

// SiteLag is one site's ingestion lag as of the job's clock.
type SiteLag struct {
	// Latest is the latest ended_at ingested from the site; zero with
	// NoRows.
	Latest time.Time
	// NoRows: the site was in an earlier read and is in the ledger no
	// more (the table rebuilt or truncated, or another ClickHouse).
	NoRows bool
	// Lag is now − Latest; zero with NoRows.
	Lag time.Duration
	// Behind: Lag exceeds the allowance, or NoRows. The boundary then lies
	// past the site's latest row, so any of its calls that ended between
	// the two and have not arrived are in neither the snapshot's spend
	// nor the gateways' minutes (ADR 0048): known spend undercounts them
	// until a later boundary passes their rows. A site that served nothing
	// lately is behind too: the ledger cannot tell idle from late.
	Behind bool
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
	// Retention - Lease - Margin old (MaxAge) is stale: the spend between
	// its boundary and the retention's start is in neither its spend nor
	// the minutes read. It is still published: a gateway's known spend is
	// never below its counter's period total, so a stale result counts at
	// least what none would (this site's calls), and its spend adds the
	// other sites' calls before its boundary. Stale reports it, for an
	// alert.

	// Retention is the gateways' counter retention (default six hours,
	// limit.Mem's and limit.Valkey's Retain).
	Retention time.Duration
	// Lease is the snapshots' authorization lease (default four hours,
	// golem-control's -lease).
	Lease time.Duration
	// Margin covers the publication's path to the gateways and their
	// clocks (default ten minutes).
	Margin time.Duration

	mu     sync.Mutex
	last   *Known
	latest map[string]time.Time // by site, as last read; zero: no rows any more
}

// Defaults of the Job's durations.
const (
	DefaultAllowance = 5 * time.Minute
	DefaultRetention = 6 * time.Hour
	DefaultLease     = 4 * time.Hour
	DefaultMargin    = 10 * time.Minute
)

// MaxAge is how old a result's boundary may be before it is stale:
// Retention - Lease - Margin.
func (j *Job) MaxAge() time.Duration {
	return cmp.Or(j.Retention, DefaultRetention) - cmp.Or(j.Lease, DefaultLease) - cmp.Or(j.Margin, DefaultMargin)
}

// Validate reports a configuration under which every result is stale: a
// fresh one is an allowance and a minute old, so a MaxAge no longer than
// that would alert from the first publication, and gateways at the end of
// their lease would read past their retention even then. For golem-control
// to check at start.
func (j *Job) Validate() error {
	allowance := cmp.Or(j.Allowance, DefaultAllowance)
	if maxAge := j.MaxAge(); maxAge <= allowance+time.Minute {
		return fmt.Errorf("known spend: the retention %v less the lease %v and the margin %v leaves %v, no more than the allowance %v and a minute: every result would be stale; lengthen the gateways' retention or shorten the lease",
			cmp.Or(j.Retention, DefaultRetention), cmp.Or(j.Lease, DefaultLease), cmp.Or(j.Margin, DefaultMargin), maxAge, allowance)
	}
	return nil
}

// Step reads the ledger's spend at a new boundary, and, from an Ingestion
// ledger, each site's latest row (Lags). On a failure the last result is
// kept for its own period (a new period's spend is not the old one's), and
// the error returned, saying whether it is stale; Current says what to
// publish. The boundary is never moved for a lagging site: Lags reports
// it, for an alert.
func (j *Job) Step(ctx context.Context) error {
	return errors.Join(j.stepSpend(ctx), j.stepLatest(ctx))
}

// stepLatest reads each site's latest ingested row. On a failure the last
// read is kept, so the lags grow with the clock: the job cannot see the
// sites' rows arrive, and the alert stays on.
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

// Lags is each site's ingestion lag, as of now and the last read, by
// site: empty before the first read, or for a Ledger that is not an
// Ingestion. For a metric, and an alert on Behind.
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
	period := budget.Of(now, loc)
	// A boundary still in the previous period counts nothing of this one:
	// this period starts from nothing until B passes its start.
	if budget.Of(since, loc) != period {
		j.set(&Known{Period: period, Since: since, Spend: map[string]int64{}, Members: map[Member]int64{}, Keys: map[string]int64{}})
		return nil
	}
	s, err := j.Ledger.Spend(ctx, period, since)
	if err != nil {
		j.forgetOtherThan(period)
		if k, ok := j.Current(); ok && k.Age(now) > j.MaxAge() {
			return fmt.Errorf("known spend for %s: %w (the last result is kept, but stale: its boundary %s is older than %v, so gateways count this site's counter total as a floor and the stale result's spend, and miss other sites' spend since; alert)",
				period, err, k.Since.UTC().Format(time.RFC3339), j.MaxAge())
		}
		return fmt.Errorf("known spend for %s: %w (the last result is kept)", period, err)
	}
	j.set(&Known{Period: period, Since: since, Spend: s.Scopes, Members: s.Members, Keys: s.Keys})
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

// Current is the latest result, or false if there is none yet: a snapshot
// then carries no known spend, and gateways count their site's counter
// alone. A stale result is still current (Stale).
func (j *Job) Current() (Known, bool) {
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

// Stale reports whether the current result's boundary is older than
// MaxAge: the gateways' known spend is then this site's counter total at
// least, and the stale result's spend, but misses other sites' spend
// since the boundary. For an alert.
func (j *Job) Stale() bool {
	age, ok := j.Age()
	return ok && age > j.MaxAge()
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
// the one partition the period names), by scope, principal and key:
// readSpend sums those into each account.
const spendQuery = `SELECT scope_id, principal_id, key_id, toString(sum(cost_micros)) AS spend
FROM {db}.spend_by_event FINAL
WHERE period = {period:Date} AND ended_at < {before:DateTime64(3, 'UTC')}
GROUP BY scope_id, principal_id, key_id
FORMAT JSONEachRow`

// Spend runs the query.
func (c ClickHouse) Spend(ctx context.Context, period string, before time.Time) (Totals, error) {
	q := url.Values{
		"param_period": {budget.FirstDay(period)},
		"param_before": {before.UTC().Format("2006-01-02 15:04:05.000")},
	}
	var out Totals
	err := c.query(ctx, spendQuery, q, func(r io.Reader) (err error) { out, err = readSpend(r); return err })
	return out, err
}

// latestQuery is each site's latest ingested ended_at
// (deploy/clickhouse/spend.sql: golem.ingest_by_site, one row per site
// once merged; max over the unmerged parts makes it exact at read time).
const latestQuery = `SELECT site, toString(max(latest)) AS latest
FROM {db}.ingest_by_site
GROUP BY site
FORMAT JSONEachRow`

// Latest runs latestQuery: the Ingestion half.
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
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var row struct {
			Site   string `json:"site"`
			Latest string `json:"latest"` // DateTime64(3, 'UTC') as text
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
		t, err := time.Parse("2006-01-02 15:04:05.000", row.Latest)
		if err != nil {
			return nil, fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
		out[row.Site] = t.UTC()
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// readSpend reads the query's JSONEachRow rows, each a scope's, a
// principal's and a key's spend, into each account's sum.
func readSpend(r io.Reader) (Totals, error) {
	out := Totals{Scopes: map[string]int64{}, Members: map[Member]int64{}, Keys: map[string]int64{}}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var row struct {
			Scope     string `json:"scope_id"`
			Principal string `json:"principal_id"`
			Key       string `json:"key_id"`
			Spend     string `json:"spend"` // a string: Int64 sums are quoted in JSON by default
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return Totals{}, fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
		n, err := strconv.ParseInt(row.Spend, 10, 64)
		if err != nil {
			return Totals{}, fmt.Errorf("ClickHouse row %q: %w", sc.Text(), err)
		}
		out.Scopes[row.Scope] += n
		if row.Principal != "" {
			out.Members[Member{row.Scope, row.Principal}] += n
		}
		if row.Key != "" {
			out.Keys[row.Key] += n
		}
	}
	if err := sc.Err(); err != nil {
		return Totals{}, err
	}
	return out, nil
}

var (
	_ Ledger    = ClickHouse{}
	_ Ingestion = ClickHouse{}
)
