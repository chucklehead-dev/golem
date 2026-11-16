package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
)

// The gateway's state instruments (operations.md, Alerts).
const (
	MetricSnapshotAge     = "golem.snapshot.age"
	MetricLeaseRemaining  = "golem.snapshot.lease_remaining"
	MetricSnapshotSeq     = "golem.snapshot.seq"
	MetricPointerOverdue  = "golem.snapshot.pointer_overdue"
	MetricServingLastGood = "golem.snapshot.last_good"
	MetricLimiterDown     = "golem.limit.down"
	MetricWaivers         = "golem.limit.waivers"
	MetricDenylistEntries = "golem.denylist.entries"
)

// State is what the gateway's state gauges read: each is read only when
// the meter collects (every minute over OTLP), never on a request.
type State struct {
	Holder *snapshot.Holder
	// Source, if set, is the pool's bucket source (nil standalone): its
	// pointer's overdue flag and whether the last-good copy is served.
	Source *snapshot.Bucket
	// Limiter, if set, reports its outage and waivers.
	Limiter *limit.Limiter
	// Deny, if set, is the site's denylist file.
	Deny *auth.DenyFile
	// Now is the clock the snapshot's age and lease are measured by
	// (default time.Now).
	Now func() time.Time
}

// ObserveState registers the gateway's state instruments on mp: the
// applied snapshot's age (now − IssuedAt) and lease remaining
// (ValidUntil − now) in seconds, its publication sequence, the pool's
// pointer overdue and the last-good copy served (0/1), the limiter down
// (0/1), its waivers, and the denylist's entries. None is attributed beyond the resource's site and pool.
func ObserveState(mp metric.MeterProvider, s State) error {
	m := mp.Meter("golem/gateway") // the request path's meter (New)
	now := s.Now
	if now == nil {
		now = time.Now
	}
	var errs []error
	fg := func(name, unit, desc string) metric.Float64ObservableGauge {
		g, err := m.Float64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return g
	}
	ig := func(name, unit, desc string) metric.Int64ObservableGauge {
		g, err := m.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return g
	}
	ic := func(name, unit, desc string) metric.Int64ObservableCounter {
		g, err := m.Int64ObservableCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return g
	}
	age := fg(MetricSnapshotAge, "s", "How long ago the applied snapshot was issued (now - IssuedAt)")
	lease := fg(MetricLeaseRemaining, "s", "The applied snapshot's authorization lease remaining (ValidUntil - now); negative once it has run out")
	seq := ig(MetricSnapshotSeq, "{count}", "The applied snapshot's publication sequence")
	insts := []metric.Observable{age, lease, seq}
	var overdue, lastGood metric.Int64ObservableGauge
	if s.Source != nil {
		overdue = ig(MetricPointerOverdue, "1", "1 while the pool's newest pointer seen is past its Expires: no publication since")
		lastGood = ig(MetricServingLastGood, "1", "1 while the snapshot served is the last-good copy from disk, not one read from the bucket")
		insts = append(insts, overdue, lastGood)
	}
	var down metric.Int64ObservableGauge
	var waivers metric.Int64ObservableCounter
	if s.Limiter != nil {
		down = ig(MetricLimiterDown, "1", "1 while the limiter believes the site store unreachable: admissions are local, as waivers")
		waivers = ic(MetricWaivers, "{count}", "Calls admitted as waivers of the budget's bound (an outage, or the grace after it)")
		insts = append(insts, down, waivers)
	}
	var deny metric.Int64ObservableGauge
	if s.Deny != nil {
		deny = ig(MetricDenylistEntries, "{count}", "Entries in the site's emergency denylist in force")
		insts = append(insts, deny)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	_, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if c := s.Holder.Applied(); c != nil {
			t := now()
			o.ObserveFloat64(age, t.Sub(c.Snapshot.IssuedAt).Seconds())
			if v := c.Snapshot.ValidUntil; !v.IsZero() { // standalone's has no lease
				o.ObserveFloat64(lease, v.Sub(t).Seconds())
			}
			o.ObserveInt64(seq, int64(c.Snapshot.PublishSeq))
		}
		if s.Source != nil {
			o.ObserveInt64(overdue, b2i(s.Source.Overdue()))
			o.ObserveInt64(lastGood, b2i(s.Source.ServingLastGood()))
		}
		if l := s.Limiter; l != nil {
			o.ObserveInt64(down, b2i(l.Down()))
			o.ObserveInt64(waivers, l.Waived.Load())
		}
		if s.Deny != nil {
			o.ObserveInt64(deny, int64(s.Deny.Current().Len()))
		}
		return nil
	}, insts...)
	return err
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
