package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	budget "github.com/chucklehead-dev/golem/lib/period"
)

// SchemaVersion is golem.spend's contract version (contracts.md §3): 2,
// rows written at their call's end (ADR 0043), whose ended_at puts them on
// one side of a snapshot's boundary (ADR 0048).
const SchemaVersion = 2

// Ledger writes the access-log records that are golem.spend's rows, one per
// upstream attempt, as OTel log records whose attributes are the table's
// columns; the collector writes them to ClickHouse. A row is written when
// its call ends, whatever the site counter's state, so an outage of the
// counter keeps no billing rows in memory (ADR 0043).
type Ledger struct {
	logger     otellog.Logger
	site, pool string
	// Period is the time zone of budget periods (default UTC).
	Period *time.Location
}

// NewLedger writes rows through logger.
func NewLedger(logger otellog.Logger, site, pool string) *Ledger {
	return &Ledger{logger: logger, site: site, pool: pool, Period: time.UTC}
}

// Ended writes a finished call's row.
func (l *Ledger) Ended(ctx context.Context, c Call) {
	var row [ledgerColumns]attribute.KeyValue
	l.row(&row, &c)
	l.emit(ctx, c.Ended, "golem.spend", row[:])
}

func (l *Ledger) emit(ctx context.Context, at time.Time, name string, attrs []attribute.KeyValue) {
	var r otellog.Record
	r.SetTimestamp(at)
	r.SetEventName(name)
	r.SetBody(attribute.StringValue(name))
	r.AddAttributes(attrs...)
	l.logger.Emit(ctx, r)
}

// columns are golem.spend's, in the DDL's order; a test checks them
// against deploy/clickhouse/spend.sql.
func (l *Ledger) row(dst *[ledgerColumns]attribute.KeyValue, c *Call) {
	p := c.Period
	if p == "" {
		p = budget.Of(c.Started, l.Period)
	}
	period := budget.FirstDay(p)
	s, i, f := attribute.String, attribute.Int64, attribute.Float64
	*dst = [...]attribute.KeyValue{
		i("schema_version", SchemaVersion),
		s("row_kind", "attempt"),
		s("request_id", c.RequestID),
		s("attempt_id", c.RequestID), // one attempt per request until the attempt loop
		s("event_id", c.RequestID),
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
		f("estimated_fraction", c.EstimatedFraction),
		attribute.Bool("waived", c.Waived),
	}
}

// ledgerColumns is golem.spend's column count; the row is built in an
// array of this size, on the stack, not a slice the record copies again.
const ledgerColumns = 39
