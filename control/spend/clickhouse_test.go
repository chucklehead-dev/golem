package spend

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestClickHouseLedger runs deploy/clickhouse/spend.sql on a real
// ClickHouse (GOLEM_CLICKHOUSE_URL, e.g. http://127.0.0.1:8123, with
// GOLEM_CLICKHOUSE_USER and GOLEM_CLICKHOUSE_PASSWORD if needed), in a
// database of its own, and checks the known-spend query against it:
//   - an event delivered twice, in separate inserts and not yet merged, is
//     counted once (golem.spend_by_event's FINAL);
//   - only the period's rows that ended before the boundary count;
//   - the sum equals golem.spend_dedup's, the view the ledger's readers use.
//
// Without FINAL in the query, the duplicate counts twice: the test's first
// expectation fails.
func TestClickHouseLedger(t *testing.T) {
	base := os.Getenv("GOLEM_CLICKHOUSE_URL")
	if base == "" {
		t.Skip("no ClickHouse: set GOLEM_CLICKHOUSE_URL (e.g. http://127.0.0.1:8123) to run the ledger's DDL and query")
	}
	ch := chClient{base: base, user: os.Getenv("GOLEM_CLICKHOUSE_USER"), password: os.Getenv("GOLEM_CLICKHOUSE_PASSWORD")}
	db := ch.ledger(t)
	row := func(event, scope, ended string, cost int64, period string) string {
		return fmt.Sprintf(`{"schema_version":2,"row_kind":"attempt","request_id":%q,"attempt_id":%q,"event_id":%q,"started_at":%q,"ended_at":%q,"period":%q,"scope_id":%q,"cost_micros":%d,"currency":"USD"}`,
			event, event, event, ended, ended, period, scope, cost)
	}
	e := func(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }
	insert := func(rows ...string) {
		t.Helper()
		if err := ch.exec(t.Context(), "INSERT INTO "+db+".spend FORMAT JSONEachRow\n"+strings.Join(rows, "\n")); err != nil {
			t.Fatal(err)
		}
	}
	// The boundary is 09:55. Each insert is its own part: no merge yet.
	insert(row(e(1), "team-a", "2026-11-14 09:00:00.000", 100, "2026-11-01"), row(e(3), "team-b", "2026-11-14 09:54:59.999", 7, "2026-11-01"))
	insert(row(e(1), "team-a", "2026-11-14 09:00:00.000", 100, "2026-11-01"))  // the duplicate, redelivered
	insert(row(e(2), "team-a", "2026-11-14 09:55:00.000", 50, "2026-11-01"))   // at the boundary: the site counter's
	insert(row(e(4), "team-a", "2026-10-31 23:00:00.000", 1000, "2026-10-01")) // last month

	c := ClickHouse{URL: base, User: ch.user, Password: ch.password, Database: db}
	before := time.Date(2026, 11, 14, 9, 55, 0, 0, time.UTC)
	got, err := c.Spend(t.Context(), "2026-11", before)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scopes["team-a"] != 100 || got.Scopes["team-b"] != 7 || len(got.Scopes) != 2 {
		t.Fatalf("known spend %v; want team-a 100 (the duplicate once, the boundary's row excluded, October's excluded), team-b 7", got.Scopes)
	}
	// The same sum from the readers' view.
	out, err := ch.query(t.Context(), "SELECT scope_id, sum(cost_micros) FROM "+db+".spend_dedup WHERE period = '2026-11-01' AND ended_at < '2026-11-14 09:55:00.000' GROUP BY scope_id ORDER BY scope_id FORMAT TSV")
	if err != nil || strings.TrimSpace(out) != "team-a\t100\nteam-b\t7" {
		t.Fatalf("golem.spend_dedup says %q (%v): the two disagree", out, err)
	}
}

// TestClickHouseIngestion runs deploy/clickhouse/spend.sql as
// TestClickHouseLedger does and checks Latest against it: each site's
// latest ended_at, over inserts not yet merged and in any order (an older
// row arriving last does not lower it); a site with no rows is not listed.
// Then a Job over it reports the lagging site behind.
func TestClickHouseIngestion(t *testing.T) {
	base := os.Getenv("GOLEM_CLICKHOUSE_URL")
	if base == "" {
		t.Skip("no ClickHouse: set GOLEM_CLICKHOUSE_URL (e.g. http://127.0.0.1:8123) to run the ledger's DDL and query")
	}
	ch := chClient{base: base, user: os.Getenv("GOLEM_CLICKHOUSE_USER"), password: os.Getenv("GOLEM_CLICKHOUSE_PASSWORD")}
	db := ch.ledger(t)
	c := ClickHouse{URL: base, User: ch.user, Password: ch.password, Database: db}
	if got, err := c.Latest(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("an empty ledger: %v %v", got, err)
	}
	n := 0
	insert := func(site, ended string) {
		t.Helper()
		n++
		e := fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
		row := fmt.Sprintf(`{"schema_version":2,"row_kind":"attempt","request_id":%q,"attempt_id":%q,"event_id":%q,"started_at":%q,"ended_at":%q,"period":"2026-11-01","site":%q,"scope_id":"team-a","cost_micros":1,"currency":"USD"}`,
			e, e, e, ended, ended, site)
		if err := ch.exec(t.Context(), "INSERT INTO "+db+".spend FORMAT JSONEachRow\n"+row); err != nil {
			t.Fatal(err)
		}
	}
	// Each insert its own part: no merge yet.
	insert("eu-1", "2026-11-14 09:58:00.000")
	insert("eu-1", "2026-11-14 09:59:30.250")
	insert("eu-1", "2026-11-14 09:10:00.000") // an older row, arriving last
	insert("us-1", "2026-11-14 09:53:00.000")
	got, err := c.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"eu-1": time.Date(2026, 11, 14, 9, 59, 30, 250e6, time.UTC), "us-1": time.Date(2026, 11, 14, 9, 53, 0, 0, time.UTC)}
	if len(got) != len(want) || !got["eu-1"].Equal(want["eu-1"]) || !got["us-1"].Equal(want["us-1"]) {
		t.Fatalf("latest %v, want %v", got, want)
	}
	now := time.Date(2026, 11, 14, 10, 0, 30, 0, time.UTC)
	j := &Job{Ledger: c, Now: func() time.Time { return now }}
	if err := j.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lags := j.Lags(); lags["eu-1"].Behind || !lags["us-1"].Behind || lags["us-1"].Lag != 7*time.Minute+30*time.Second {
		t.Fatalf("lags %+v: want us-1 behind by 7m30s, eu-1 not", lags)
	}
}

// ledger runs deploy/clickhouse/spend.sql in a database of its own, which
// it drops when the test ends, and returns the database's name.
func (c chClient) ledger(t *testing.T) string {
	t.Helper()
	db := "golem_test_" + strings.ToLower(rand.Text()[:10])
	ddl, err := os.ReadFile("../../deploy/clickhouse/spend.sql")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.exec(context.Background(), "DROP DATABASE IF EXISTS "+db) })
	for _, stmt := range statements(inDatabase(string(ddl), db)) {
		if err := c.exec(t.Context(), stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	return db
}

// inDatabase is the DDL with its database golem renamed db: each table
// golem.x, not a string such as the event name 'golem.spend'.
func inDatabase(ddl, db string) string {
	ddl = strings.Replace(ddl, "CREATE DATABASE IF NOT EXISTS golem;", "CREATE DATABASE IF NOT EXISTS "+db+";", 1)
	return tableRef.ReplaceAllString(ddl, "${1}"+db+".")
}

// tableRef is golem. outside a quoted string's start.
var tableRef = regexp.MustCompile(`(^|[^'\w])golem\.`)

// statements splits the DDL file into statements, comments dropped.
func statements(ddl string) []string {
	var b strings.Builder
	for line := range strings.Lines(ddl) {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line)
		}
	}
	var out []string
	for _, s := range strings.Split(b.String(), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// chClient runs statements over ClickHouse's HTTP interface.
type chClient struct{ base, user, password string }

func (c chClient) exec(ctx context.Context, stmt string) error {
	_, err := c.query(ctx, stmt)
	return err
}

func (c chClient) query(ctx context.Context, stmt string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base, "/")+"/?"+url.Values{"input_format_skip_unknown_fields": {"1"}}.Encode(), strings.NewReader(stmt))
	if err != nil {
		return "", err
	}
	if c.user != "" {
		req.Header.Set("X-ClickHouse-User", c.user)
		req.Header.Set("X-ClickHouse-Key", c.password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ClickHouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}
