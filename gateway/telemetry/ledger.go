package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

// SchemaVersion is golem.spend's contract version (contracts.md §3).
const SchemaVersion = 1

// DefaultMaxWaiting bounds the rows waiting for their counter number.
const DefaultMaxWaiting = 100_000

// Ledger writes the access-log records that are golem.spend's rows: one per
// upstream attempt, as OTel log records whose attributes are the table's
// columns, which the collector writes to ClickHouse.
type Ledger struct {
	logger     otellog.Logger
	site, pool string
	// Period is the time zone of budget periods (default UTC).
	Period *time.Location
	// MaxWaiting bounds the rows waiting for their counter number: during a
	// long Valkey outage the oldest are written without one (counter_seq
	// 0), which golem-control treats as never numbered (ADR 0042's gaps).
	MaxWaiting int

	mu      sync.Mutex
	waiting map[string]Call // by event ID, until numbered
	order   []string
	// Unnumbered counts rows written without a number (MaxWaiting).
	Unnumbered atomic.Int64
}

// NewLedger writes rows through logger.
func NewLedger(logger otellog.Logger, site, pool string) *Ledger {
	return &Ledger{logger: logger, site: site, pool: pool, Period: time.UTC, MaxWaiting: DefaultMaxWaiting, waiting: map[string]Call{}}
}

// Ended takes a finished call's row: written now, or, if the limiter counts
// it, when the limiter numbers its event.
func (l *Ledger) Ended(ctx context.Context, c Call) {
	if !c.Counted {
		l.write(ctx, c, 0)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waiting[c.RequestID] = c
	l.order = append(l.order, c.RequestID)
	// Drop the front's entries already numbered, so order stays as long as
	// waiting (plus the numbered ones behind a waiting one).
	for len(l.order) > 0 {
		if _, ok := l.waiting[l.order[0]]; ok {
			break
		}
		l.order = l.order[1:]
	}
	for len(l.waiting) > l.MaxWaiting {
		id := l.order[0]
		l.order = l.order[1:]
		if old, ok := l.waiting[id]; ok {
			delete(l.waiting, id)
			l.Unnumbered.Add(1)
			l.write(ctx, old, 0)
		}
	}
}

// Numbered is the limiter's OnNumbered: the event's row is written with
// its counter number. The event's ID is the call's request ID; its cost is
// the call's (a cancelled call's, what the gateway saw).
func (l *Ledger) Numbered(_ string, eventID string, seq int64) {
	l.mu.Lock()
	c, ok := l.waiting[eventID]
	delete(l.waiting, eventID)
	l.mu.Unlock()
	if ok {
		l.write(context.Background(), c, seq)
	}
	// Its order entry is dropped by a later Ended.
}

// columns are golem.spend's, in the DDL's order; a test checks them
// against deploy/clickhouse/spend.sql.
func (l *Ledger) row(dst *[ledgerColumns]attribute.KeyValue, c *Call, seq int64) {
	start := c.Started.In(l.Period)
	period := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	estimated := 0.0
	if c.Estimated {
		estimated = 1
	}
	s, i, f := attribute.String, attribute.Int64, attribute.Float64
	*dst = [...]attribute.KeyValue{
		i("schema_version", SchemaVersion),
		s("row_kind", "attempt"),
		s("request_id", c.RequestID),
		s("attempt_id", c.RequestID), // one attempt per request until the attempt loop
		s("event_id", c.RequestID),
		i("counter_seq", seq),
		s("provider_request_id", c.ProviderRequestID),
		s("started_at", c.Started.UTC().Format(time.RFC3339Nano)),
		s("ended_at", c.Ended.UTC().Format(time.RFC3339Nano)),
		s("period", period),
		s("site", l.site),
		s("gateway_pool", l.pool),
		s("principal_kind", c.PrincipalKind),
		s("principal_id", c.Principal),
		s("key_id", c.Key),
		s("scope_id", c.Scope),
		s("scope_path", c.Scope),
		attribute.StringSlice("funding", nil),
		s("frontend_dialect", c.Frontend),
		s("backend_dialect", c.Backend),
		s("envelope", ""),
		s("alias", c.Alias),
		s("model", c.Model),
		s("deployment", c.Deployment),
		s("provider_account", c.Provider),
		i("tokens_input", c.Usage.Input),
		i("tokens_output", c.Usage.Output),
		i("tokens_cache_read", c.Usage.CacheRead),
		i("tokens_cache_write_5m", c.Usage.CacheWrite),
		i("tokens_cache_write_1h", 0),
		i("tokens_reasoning", c.Usage.Reasoning),
		s("units", "{}"), // a JSON object: log attributes have no map type
		i("cost_micros", c.Cost),
		s("currency", c.Currency),
		s("price_version", ""),
		s("outcome", c.Outcome),
		s("error_origin", c.ErrorOrigin),
		s("error_code", c.ErrorCode),
		f("estimated_fraction", estimated),
		attribute.Bool("waived", c.Waived),
	}
}

// ledgerColumns is golem.spend's column count; the row is built in an
// array of this size, on the stack, not a slice the record copies again.
const ledgerColumns = 40

func (l *Ledger) write(ctx context.Context, c Call, seq int64) {
	var row [ledgerColumns]attribute.KeyValue
	l.row(&row, &c, seq)
	var r otellog.Record
	r.SetTimestamp(c.Ended)
	r.SetEventName("golem.spend")
	r.SetBody(attribute.StringValue("golem.spend"))
	r.AddAttributes(row[:]...)
	l.logger.Emit(ctx, r)
}
