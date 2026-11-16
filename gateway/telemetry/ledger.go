package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/chucklehead-dev/golem/lib/period"
)

// SchemaVersion is golem.spend's contract version (contracts.md §3): 2,
// rows written at their call's end (ADR 0043), whose ended_at puts them on
// one side of a snapshot's boundary (ADR 0048).
const SchemaVersion = 2

// Ledger writes the access-log records that are golem.spend's rows, one per
// upstream attempt, as OTel log records whose attributes are the table's
// columns; the collector writes them to ClickHouse. A row is written when
// its call ends, whatever the site store's state, so an outage keeps no
// billing rows in memory (ADR 0043).
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

// Ended writes a finished call's row: a golem.spend event (its name also
// the body), stamped with the call's end.
func (l *Ledger) Ended(ctx context.Context, c Call) {
	var row [ledgerColumns]attribute.KeyValue
	l.row(&row, &c)
	var r otellog.Record
	r.SetTimestamp(c.Ended)
	r.SetEventName("golem.spend")
	r.SetBody(attribute.StringValue("golem.spend"))
	r.AddAttributes(row[:]...)
	l.logger.Emit(ctx, r)
}

// row fills dst with c's golem.spend columns, in the DDL's order; a test
// checks them against deploy/clickhouse/spend.sql. The period is the
// limiter's when the call has one, else its start's month in l.Period.
func (l *Ledger) row(dst *[ledgerColumns]attribute.KeyValue, c *Call) {
	p := c.Period
	if p == "" {
		p = period.Of(c.Started, l.Period)
	}
	firstDay := period.FirstDay(p)
	s, i, f := attribute.String, attribute.Int64, attribute.Float64
	*dst = [...]attribute.KeyValue{
		i("schema_version", SchemaVersion),
		s("row_kind", "attempt"),
		s("request_id", c.RequestID),
		s("attempt_id", c.AttemptID()),
		s("event_id", c.AttemptID()), // each attempt its own event (model/attempts.md)
		s("provider_request_id", c.ProviderRequestID),
		s("started_at", c.Started.UTC().Format(time.RFC3339Nano)),
		s("ended_at", c.Ended.UTC().Format(time.RFC3339Nano)),
		s("period", firstDay),
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
		s("envelope", c.Envelope),
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
		attribute.Bool("over_budget", c.OverBudget),
	}
}

// ledgerColumns is golem.spend's column count; the row is built in an
// array of this size, on the stack, not a slice the record copies again.
const ledgerColumns = 40
