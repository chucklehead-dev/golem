// Package metrics is golem-control's operational metrics (operations.md,
// Alerts): observable instruments read from the controller's state at
// each collection, so a stalled control plane is seen long before the
// gateways' authorization lease runs out. Nothing here is on a write
// path: each callback copies what it reads under the components' own
// short locks, and none holds a lock across I/O.
//
// Times are Unix seconds, so "seconds since" and "lease remaining" are
// the collector's subtraction from its own clock; attributes are bounded
// (pool, reason, period).
package metrics

import (
	"context"
	"maps"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/chucklehead-dev/golem/control/controller"
)

// Instrument names, with the gateway's golem. prefix.
const (
	PublishTime       = "golem.control.publish.time"
	PublishSeq        = "golem.control.publish.seq"
	PublishValidUntil = "golem.control.publish.valid_until"
	PublishFailures   = "golem.control.publish.failures"
	LeaderEpoch       = "golem.control.leader.epoch"
	Leader            = "golem.control.leader"
	Unpriced          = "golem.control.unpriced"
	ConfigRenewing    = "golem.control.config.renewing"
	SpendAge          = "golem.control.spend.age"
	SpendStale        = "golem.control.spend.stale"
	IngestionLag      = "golem.control.spend.ingestion_lag"
	IngestionBehind   = "golem.control.spend.ingestion_behind"
	SyncTime          = "golem.control.entra_sync.time"
	SyncGuardTrips    = "golem.control.entra_sync.guard_trips"
	AuditOutbox       = "golem.control.audit.outbox"
	AuditShippedTime  = "golem.control.audit.shipped.time"
)

// Register registers golem-control's instruments on mp, read from c (its
// elector, publishers and jobs) at each collection. A job c does not run
// (no Spend, Sync or Audit) reports nothing of its own.
func Register(mp metric.MeterProvider, c *controller.Controller) error {
	m := mp.Meter("golem/control")
	var r regs
	pubTime := r.fgauge(m, PublishTime, "s", "When this replica last published the pool successfully (Unix time)")
	pubSeq := r.igauge(m, PublishSeq, "{count}", "The publication sequence of this replica's last successful publication to the pool")
	validUntil := r.fgauge(m, PublishValidUntil, "s", "The authorization lease's end (ValidUntil, Unix time) of this replica's last publication to the pool")
	failures := r.icounter(m, PublishFailures, "{count}", "Publication attempts that failed or were degraded, by pool and reason")
	epoch := r.igauge(m, LeaderEpoch, "{count}", "The leadership epoch this replica leads at; 0 when it does not lead")
	leader := r.igauge(m, Leader, "1", "1 while this replica leads, else 0")
	unpriced := r.igauge(m, Unpriced, "{count}", "Aliases the last publication left out of scopes whose budgets could not be charged for them")
	renewing := r.igauge(m, ConfigRenewing, "1", "1 while the config directory is not what is published (it does not compile, or an edit was refused): the last good config is renewed")
	insts := []metric.Observable{pubTime, pubSeq, validUntil, failures, epoch, leader, unpriced, renewing}

	var spendAge metric.Float64ObservableGauge
	var spendStale metric.Int64ObservableGauge
	var ingestLag metric.Float64ObservableGauge
	var ingestBehind metric.Int64ObservableGauge
	if c.Spend != nil {
		spendAge = r.fgauge(m, SpendAge, "s", "How far known spend's boundary is behind now (spend.Job.Age), by period")
		spendStale = r.igauge(m, SpendStale, "1", "1 while known spend's result is stale (spend.Job.Stale), by period")
		ingestLag = r.fgauge(m, IngestionLag, "s", "How far each site's ledger rows are behind now (spend.Job.Lags), by site; none for a site with no rows")
		ingestBehind = r.igauge(m, IngestionBehind, "1", "1 while a site's rows are later than the allowance, or gone (spend.Job.Lags), by site")
		insts = append(insts, spendAge, spendStale, ingestLag, ingestBehind)
	}
	var syncTime metric.Float64ObservableGauge
	var guard metric.Int64ObservableCounter
	if c.Sync != nil {
		syncTime = r.fgauge(m, SyncTime, "s", "When the Entra group sync last succeeded on this replica (Unix time)")
		guard = r.icounter(m, SyncGuardTrips, "{count}", "Entra group syncs refused by their removal guard")
		insts = append(insts, syncTime, guard)
	}
	// The outbox grows whether or not it is shipped (no -audit-archive).
	outbox := r.igauge(m, AuditOutbox, "{count}", "Audit events in the state document's outbox, waiting to be archived")
	insts = append(insts, outbox)
	var shipped metric.Float64ObservableGauge
	if c.Audit != nil {
		shipped = r.fgauge(m, AuditShippedTime, "s", "When an audit shipping step last succeeded on this replica (Unix time)")
		insts = append(insts, shipped)
	}
	if r.err != nil {
		return r.err
	}

	pools := slices.Sorted(maps.Keys(c.Publishers))
	poolSets := map[string]metric.ObserveOption{}
	failureSets := map[controller.Failure]metric.ObserveOption{}
	for _, p := range pools {
		poolSets[p] = metric.WithAttributeSet(attribute.NewSet(attribute.String("pool", p)))
		for _, reason := range controller.Reasons {
			failureSets[controller.Failure{Pool: p, Reason: reason}] = metric.WithAttributeSet(attribute.NewSet(
				attribute.String("pool", p), attribute.String("reason", reason)))
		}
	}
	_, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for pool, p := range c.Publications() {
			opt, ok := poolSets[pool]
			if !ok {
				continue
			}
			o.ObserveFloat64(pubTime, unix(p.At), opt)
			o.ObserveInt64(pubSeq, int64(p.Seq), opt)
			if !p.ValidUntil.IsZero() {
				o.ObserveFloat64(validUntil, unix(p.ValidUntil), opt)
			}
		}
		// Every pool and reason, zero included: an alert on an increase
		// sees the first failure.
		counts := c.Failures()
		for f, opt := range failureSets {
			o.ObserveInt64(failures, counts[f], opt)
		}
		e := c.Elector.Epoch()
		o.ObserveInt64(epoch, int64(e))
		o.ObserveInt64(leader, b2i(e != 0))
		o.ObserveInt64(unpriced, int64(len(c.Unpriced())))
		o.ObserveInt64(renewing, b2i(c.ConfigRenewing()))
		if c.Spend != nil {
			if k, ok := c.Spend.Current(); ok {
				opt := metric.WithAttributes(attribute.String("period", k.Period))
				if age, ok := c.Spend.Age(); ok {
					o.ObserveFloat64(spendAge, age.Seconds(), opt)
				}
				o.ObserveInt64(spendStale, b2i(c.Spend.Stale()), opt)
			}
			for site, l := range c.Spend.Lags() {
				opt := metric.WithAttributes(attribute.String("site", site))
				if !l.NoRows {
					o.ObserveFloat64(ingestLag, l.Lag.Seconds(), opt)
				}
				o.ObserveInt64(ingestBehind, b2i(l.Behind), opt)
			}
		}
		if c.Sync != nil {
			if t := c.SyncedAt(); !t.IsZero() {
				o.ObserveFloat64(syncTime, unix(t))
			}
			o.ObserveInt64(guard, c.GuardTrips())
		}
		o.ObserveInt64(outbox, int64(c.Elector.State.OutboxLen()))
		if c.Audit != nil {
			if t := c.ShippedAt(); !t.IsZero() {
				o.ObserveFloat64(shipped, unix(t))
			}
		}
		return nil
	}, insts...)
	return err
}

// regs makes instruments, keeping the first error.
type regs struct{ err error }

func (r *regs) fgauge(m metric.Meter, name, unit, desc string) metric.Float64ObservableGauge {
	g, err := m.Float64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
	r.keep(err)
	return g
}

func (r *regs) igauge(m metric.Meter, name, unit, desc string) metric.Int64ObservableGauge {
	g, err := m.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
	r.keep(err)
	return g
}

func (r *regs) icounter(m metric.Meter, name, unit, desc string) metric.Int64ObservableCounter {
	g, err := m.Int64ObservableCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))
	r.keep(err)
	return g
}

func (r *regs) keep(err error) {
	if r.err == nil {
		r.err = err
	}
}

// unix is t in Unix seconds, to the millisecond.
func unix(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
