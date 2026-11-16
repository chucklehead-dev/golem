// Package ledger tests the ledger's whole path (contracts.md, ADR
// 0043): the gateway's telemetry exports rows over OTLP to the collector
// configured by deploy/otel/collector.yaml, which queues them on disk and
// writes them to ClickHouse, whose views (deploy/clickhouse/spend.sql) make
// golem.spend's rows of them; control/spend's ClickHouse reads the known
// spend back.
//
// With Docker it starts ClickHouse and the collector itself (the images
// below, GOLEM_E2E_CLICKHOUSE_IMAGE and GOLEM_E2E_COLLECTOR_IMAGE to
// change them; the collector on the host's network, so its receiver is
// the configuration's loopback one). Without Docker, it runs against
// GOLEM_E2E_CLICKHOUSE_URL (ClickHouse's HTTP interface) and
// GOLEM_E2E_OTLP_ENDPOINT (a collector running collector.yaml, host:port,
// writing to that ClickHouse's GOLEM_E2E_CLICKHOUSE_DATABASE, default
// golem), and skips the restart step, which needs to control the
// collector. Otherwise, and with -short, it skips.
package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"uuid"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/otlp"
)

// The images the test runs: the collector's version is the gateway chart's
// (otel/opentelemetry-collector-contrib, from GitHub's registry: Docker
// Hub's rate limit); ClickHouse's is a current LTS, from a mirror of Docker
// Hub's official image.
const (
	collectorImage  = "ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.115.1"
	clickhouseImage = "public.ecr.aws/docker/library/clickhouse:26.8"
)

// TestLedgerReachesClickHouse sends rows the way the gateway does and
// checks what lands:
//   - a call's row, every column in its type: the cache writes by
//     lifetime, funding and units, the price version and provider account;
//   - a retried call's two rows, each its own event (the retry's ID the
//     UUIDv5 telemetry derives), both counted;
//   - a row delivered twice, in separate exports, counted once;
//   - a malformed row (a request ID that is not a UUID), exported in the
//     same batch as good rows, kept in golem.spend_rejected and counted
//     nowhere, the good rows of its batch landing;
//   - with Docker, a row exported while ClickHouse is unreachable and the
//     collector then killed: it is in the collector's queue on disk, and
//     lands once both are back (ADR 0043);
//
// and spend.ClickHouse's Spend and Latest, golem-control's reads, agree.
func TestLedgerReachesClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the ledger's end-to-end test starts ClickHouse and a collector")
	}
	env := setup(t)
	ctx := t.Context()

	exp, err := otlp.Logs(ctx, env.otlp)
	if err != nil {
		t.Fatal(err)
	}
	p, shutdown := telemetry.SDK(tracetest.NewInMemoryExporter(), sdkmetric.NewManualReader(), exp, resource.Empty())
	t.Cleanup(func() { shutdown(context.Background()) })
	p.Ledger.SetBackoff(200 * time.Millisecond)
	tel, err := telemetry.New(p, "e2e-site", "e2e-pool")
	if err != nil {
		t.Fatal(err)
	}
	flush := func() {
		t.Helper()
		fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := p.Ledger.ForceFlush(fctx); err != nil {
			t.Fatal(err)
		}
		if d, f := p.Ledger.Dropped.Load(), p.Ledger.Failed.Load(); d != 0 || f != 0 {
			t.Fatalf("the gateway lost rows: %d dropped, %d failed", d, f)
		}
	}

	scope := "e2e-" + strings.ToLower(rand.Text()[:8])
	started := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	base := telemetry.Call{Principal: "alice", PrincipalKind: "user", Key: "glm_live_e2e", Scope: scope,
		Frontend: "anthropic.messages", Backend: "anthropic.messages", Alias: "claude", Model: "m-1",
		Deployment: "anthropic/primary", Provider: "anthropic", Started: started, Ended: started.Add(2 * time.Second),
		Currency: "USD", Status: 200, Outcome: "completed"}

	a := base
	a.RequestID = uuid.NewV7().String()
	a.Usage = canon.Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 300, CacheWrite1h: 200, Reasoning: 5}
	a.Cost, a.PriceVersion, a.ProviderAccount = 1000, "default/2026-11", "org-123"
	a.Funding = []telemetry.Funding{{Source: "cc-1", Share: 0.75}, {Source: "cc-2", Share: 0.25}}
	a.Units = map[string]int64{"web_search": 3}
	a.EstimatedFraction, a.Waived = 0.5, true

	b1 := base
	b1.RequestID, b1.Attempt = uuid.NewV7().String(), 1
	b1.Cost, b1.Status, b1.Outcome, b1.ErrorOrigin, b1.ErrorCode = 7, 529, "failed", "provider", "upstream.unavailable"
	b1.Deployment = "anthropic/secondary"
	b2 := base
	b2.RequestID, b2.Attempt, b2.Cost = b1.RequestID, 2, 2000
	b2.Started, b2.Ended = started.Add(time.Second), started.Add(3*time.Second)

	bad := base
	bad.RequestID, bad.Cost = "not-a-uuid", 5_000_000

	// One export: a, the retried call's two attempts, and the malformed row.
	tel.Ended(ctx, a)
	tel.Retried(ctx, b1)
	tel.Ended(ctx, bad)
	tel.Ended(ctx, b2)
	flush()
	// a again, in an export of its own: at-least-once delivery's duplicate.
	tel.Ledger.Ended(ctx, a)
	flush()
	want := a.Cost + b1.Cost + b2.Cost

	env.waitFor(t, "SELECT count() FROM {db}.spend WHERE scope_id = '"+scope+"'", "4")
	env.waitFor(t, "SELECT count() FROM {db}.spend_rejected WHERE LogAttributes['request_id'] = 'not-a-uuid' AND LogAttributes['scope_id'] = '"+scope+"'", "1")
	if got := env.query(t, "SELECT reason FROM {db}.spend_rejected WHERE LogAttributes['scope_id'] = '"+scope+"'"); got != "request_id" {
		t.Fatalf("the malformed row's reason %q, want request_id", got)
	}

	// Each column of a's row in its type.
	got := env.query(t, `SELECT schema_version, row_kind, toString(request_id) = toString(attempt_id) AND attempt_id = event_id,
		started_at, ended_at, period, site, gateway_pool, principal_kind, principal_id, key_id, scope_path,
		toString(funding), backend_dialect, envelope, alias, model, deployment, provider_account,
		tokens_input, tokens_output, tokens_cache_read, tokens_cache_write_5m, tokens_cache_write_1h, tokens_reasoning,
		toString(units), cost_micros, currency, price_version, outcome, error_origin, estimated_fraction, waived, over_budget
		FROM {db}.spend WHERE request_id = toUUID('`+a.RequestID+`') LIMIT 1`)
	wantRow := strings.Join([]string{"2", "attempt", "1",
		"2026-11-14 09:00:00.000", "2026-11-14 09:00:02.000", "2026-11-01", "e2e-site", "e2e-pool", "user", "alice", "glm_live_e2e", scope,
		"[('cc-1',0.75),('cc-2',0.25)]", "anthropic.messages", "", "claude", "m-1", "anthropic/primary", "org-123",
		"10", "20", "30", "100", "200", "5",
		"{'web_search':3}", "1000", "USD", "default/2026-11", "completed", "", "0.5", "true", "false"}, "\t")
	if got != wantRow {
		t.Fatalf("a's row:\n  got  %q\n  want %q", got, wantRow)
	}
	// The retried call: two events, the retry's ID derived from the request.
	got = env.query(t, "SELECT toString(attempt_id), toString(event_id), cost_micros, provider_account, outcome FROM {db}.spend WHERE request_id = toUUID('"+b1.RequestID+"') ORDER BY started_at")
	wantB := b1.RequestID + "\t" + b1.RequestID + "\t7\tanthropic\tfailed\n" +
		telemetry.AttemptUUID(b1.RequestID, 2) + "\t" + telemetry.AttemptUUID(b1.RequestID, 2) + "\t2000\tanthropic\tcompleted"
	if got != wantB {
		t.Fatalf("the retried call's rows:\n%s\nwant\n%s", got, wantB)
	}

	ch := spend.ClickHouse{URL: env.chURL, Database: env.db}
	known := func() int64 {
		t.Helper()
		s, err := ch.Spend(ctx, "2026-11", started.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s.Scopes[scope]
	}
	if got := known(); got != want {
		t.Fatalf("known spend %d, want %d: a once (its duplicate not counted), both attempts of the retried call, not the malformed row", got, want)
	}
	if got := env.query(t, "SELECT sum(cost_micros) FROM {db}.spend_dedup WHERE scope_id = '"+scope+"'"); got != fmt.Sprint(want) {
		t.Fatalf("golem.spend_dedup's sum %s, want %d", got, want)
	}
	latest, err := ch.Latest(ctx)
	if err != nil || latest["e2e-site"].Before(b2.Ended) {
		t.Fatalf("ingestion %v %v: want e2e-site's latest row at least %v", latest, err, b2.Ended)
	}

	if env.collector == "" {
		t.Log("an external collector: the queue's restart step is skipped")
		return
	}
	// ClickHouse unreachable: the collector takes the row into its queue
	// (the gateway's export succeeds), and is then killed. Both back, the
	// row lands from the queue on disk.
	docker(t, "pause", env.clickhouse)
	c := base
	c.RequestID, c.Cost = uuid.NewV7().String(), 40
	tel.Ended(ctx, c)
	flush()
	time.Sleep(2 * time.Second) // past the exporter's batching, into its retries
	docker(t, "kill", env.collector)
	docker(t, "unpause", env.clickhouse)
	if got := env.query(t, "SELECT count() FROM {db}.spend WHERE request_id = toUUID('"+c.RequestID+"')"); got != "0" {
		t.Fatalf("the row landed while ClickHouse was paused (%s): the step proves nothing", got)
	}
	docker(t, "start", env.collector)
	env.waitFor(t, "SELECT count() FROM {db}.spend WHERE request_id = toUUID('"+c.RequestID+"')", "1")
	if got := known(); got != want+c.Cost {
		t.Fatalf("known spend after the restart %d, want %d", got, want+c.Cost)
	}
}

// env is where the test's ClickHouse and collector are.
type env struct {
	chURL, db string
	otlp      string // the collector's OTLP/HTTP host:port
	// clickhouse and collector are the containers the test started; empty
	// for an external one.
	clickhouse, collector string
}

// setup finds or starts ClickHouse and the collector, and runs spend.sql.
func setup(t *testing.T) *env {
	if u, o := os.Getenv("GOLEM_E2E_CLICKHOUSE_URL"), os.Getenv("GOLEM_E2E_OTLP_ENDPOINT"); u != "" && o != "" {
		e := &env{chURL: u, otlp: o, db: os.Getenv("GOLEM_E2E_CLICKHOUSE_DATABASE")}
		if e.db == "" {
			e.db = "golem"
		}
		e.ddl(t)
		return e
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker, and no GOLEM_E2E_CLICKHOUSE_URL and GOLEM_E2E_OTLP_ENDPOINT: the ledger's end-to-end test needs ClickHouse and a collector")
	}
	if out, err := exec.CommandContext(t.Context(), "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker is not running (%v): %s", err, firstLine(out))
	}
	e := &env{db: "golem"}
	chImage, colImage := cmpEnv("GOLEM_E2E_CLICKHOUSE_IMAGE", clickhouseImage), cmpEnv("GOLEM_E2E_COLLECTOR_IMAGE", collectorImage)
	for _, img := range []string{chImage, colImage} {
		if err := exec.CommandContext(t.Context(), "docker", "image", "inspect", img).Run(); err != nil {
			if out, err := exec.CommandContext(t.Context(), "docker", "pull", "-q", img).CombinedOutput(); err != nil {
				t.Skipf("cannot pull %s (%v): %s", img, err, firstLine(out))
			}
		}
	}

	e.clickhouse = docker(t, "run", "-d", "--rm", "-e", "CLICKHOUSE_SKIP_USER_SETUP=1", "-p", "127.0.0.1::8123", "-p", "127.0.0.1::9000", chImage)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", e.clickhouse).Run() })
	e.chURL = "http://" + docker(t, "port", e.clickhouse, "8123/tcp")
	native := docker(t, "port", e.clickhouse, "9000/tcp")
	waitHTTP(t, e.chURL+"/ping", e.clickhouse)
	e.ddl(t)

	// The collector on the host's network: its receiver is on this
	// machine's loopback, as in a gateway's pod, and ClickHouse at its
	// published port.
	health := freePort(t)
	e.otlp = freePort(t)
	queue := t.TempDir()
	if err := os.Chmod(queue, 0o777); err != nil { // the image's user (10001) writes the queue
		t.Fatal(err)
	}
	config, err := filepath.Abs("../../deploy/otel")
	if err != nil {
		t.Fatal(err)
	}
	e.collector = docker(t, "run", "-d", "--network", "host",
		"-e", "CLICKHOUSE_ENDPOINT=tcp://"+native, "-e", "OTELCOL_QUEUE_DIR=/var/lib/otelcol/queue",
		"-e", "GOLEM_OTLP_LISTEN="+e.otlp, "-e", "GOLEM_OTEL_HEALTH_LISTEN="+health,
		"-v", queue+":/var/lib/otelcol/queue", "-v", config+":/etc/otelcol:ro",
		colImage, "--config=/etc/otelcol/collector.yaml")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "40", e.collector).CombinedOutput()
			t.Logf("the collector's log:\n%s", out)
		}
		exec.Command("docker", "rm", "-f", e.collector).Run()
	})
	waitHTTP(t, "http://"+health+"/", e.collector)
	return e
}

// ddl runs deploy/clickhouse/spend.sql in e.db (its tables renamed from
// golem, not the event name 'golem.spend').
func (e *env) ddl(t *testing.T) {
	b, err := os.ReadFile("../../deploy/clickhouse/spend.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := string(b)
	if e.db != "golem" {
		ddl = strings.Replace(ddl, "CREATE DATABASE IF NOT EXISTS golem;", "CREATE DATABASE IF NOT EXISTS "+e.db+";", 1)
		ddl = regexp.MustCompile(`(^|[^'\w])golem\.`).ReplaceAllString(ddl, "${1}"+e.db+".")
	}
	var kept strings.Builder
	for line := range strings.Lines(ddl) {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept.WriteString(line)
		}
	}
	for stmt := range strings.SplitSeq(kept.String(), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			if _, err := e.run(t.Context(), stmt); err != nil {
				t.Fatalf("%v\n%s", err, stmt)
			}
		}
	}
}

// query runs q ({db} its database) and returns its answer, tab-separated
// and unescaped, trimmed.
func (e *env) query(t *testing.T, q string) string {
	t.Helper()
	out, err := e.run(t.Context(), strings.ReplaceAll(q, "{db}", e.db)+" FORMAT TSVRaw")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// waitFor polls q until it answers want, for a minute.
func (e *env) waitFor(t *testing.T, q, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if got = e.query(t, q); got == want {
			return
		}
	}
	t.Fatalf("after a minute, %s answers %q, want %q", q, got, want)
}

func (e *env) run(ctx context.Context, stmt string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(e.chURL, "/")+"/?"+url.Values{}.Encode(), strings.NewReader(stmt))
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ClickHouse: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return string(b), nil
}

// docker runs a docker command and returns its output, trimmed.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// waitHTTP waits for url to answer 200, for a minute, else fails with the
// container's log.
func waitHTTP(t *testing.T, url, container string) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		var resp *http.Response
		if resp, err = http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = errors.New(resp.Status)
		}
	}
	out, _ := exec.Command("docker", "logs", "--tail", "40", container).CombinedOutput()
	t.Fatalf("%s did not answer in a minute (%v):\n%s", url, err, out)
}

// freePort is a loopback host:port nothing listens on now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func cmpEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}
