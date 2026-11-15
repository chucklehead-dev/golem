//go:build hegel

package limit

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/qhegel"
	"github.com/chucklehead-dev/gotogether/qobs"
	"github.com/chucklehead-dev/gotogether/qtest"
	"hegel.dev/go/hegel"
)

// Hegel drives the real limiter (one per gateway over one shared store),
// a provider that bills cancelled calls up to the worst case, a clock in
// minutes, a ledger that ingests late and in any order within its
// allowance, golem-control publishing snapshots with a boundary the
// allowance behind, which each gateway applies on its own, Valkey outages
// with per-gateway recovery and grace, and failovers; model/exposure.md
// judges every run. The connect test (exposure_mbt_test.go) covers the
// other direction; its inPeriod, period and call (a request's Call) are
// this test's too.

var (
	requests = []string{"a1", "a2", "b1", "b2", "c1"}
	limitGws = []string{"g1", "g2"}
)

type limitMutation int

const (
	limitFaithful limitMutation = iota
	noScopeLimit                // per-principal limits only
)

// row is a ledger row: its call, cost and the minute the call ended.
type row struct {
	id   string
	cost int64
	at   int64
}

type limMachine struct {
	ctx       context.Context
	mut       limitMutation
	store     *Mem
	gws       map[string]*Limiter
	phase     map[string]string // idle, inflight, cancelled, done
	tickets   map[string]*Ticket
	seen      map[string]int64
	clock     int64 // the minute
	rows      []row // written, not yet ingested
	ledger    []row
	latest    Known
	applied   map[string]Known
	down      bool
	recovered map[string]bool // since the last outage; grace may end once all have
	graced    map[string]bool
}

// allowance is the ledger's ingestion allowance in minutes: the model's
// ALLOWANCE.
const allowance = 1

func (m *limMachine) now() time.Time { return inPeriod.Add(time.Duration(m.clock) * time.Minute) }

func newLimMachine(ctx context.Context, mut limitMutation) *limMachine {
	st := NewMem()
	st.Retain = 1000 * 24 * time.Hour // inPeriod is long past by the store's clock
	m := &limMachine{ctx: ctx, mut: mut, store: st, gws: map[string]*Limiter{}, latest: Known{Period: period, Since: inPeriod},
		phase: map[string]string{}, tickets: map[string]*Ticket{}, seen: map[string]int64{},
		recovered: map[string]bool{}, graced: map[string]bool{}, applied: map[string]Known{}}
	for _, g := range limitGws {
		l := New(st)
		l.Now = m.now                   // the model has one budget period
		l.Grace = 1000 * 24 * time.Hour // the model ends a grace by graceEnds
		m.gws[g] = l
		m.recovered[g] = true
		// The model's initial snapshot: nothing, its boundary minute 0 (a
		// gateway's zero Known counts the same: every minute).
		m.applied[g] = Known{Period: period, Since: inPeriod}
	}
	for _, r := range requests {
		m.phase[r] = "idle"
	}
	return m
}

func (m *limMachine) inPhase(p string) []string {
	var rs []string
	for _, r := range requests {
		if m.phase[r] == p {
			rs = append(rs, r)
		}
	}
	return rs
}

func (m *limMachine) RuleAdmit(tc hegel.TestCase) {
	r := hegel.Draw(tc, hegel.SampledFrom(requests))
	g := hegel.Draw(tc, hegel.SampledFrom(limitGws))
	m.admit(r, g)
}

func (m *limMachine) admit(r, g string) {
	if m.phase[r] != "idle" {
		return // one call per request id
	}
	c := call(r)
	if m.mut == noScopeLimit {
		c.ScopeLimit = 0
	}
	t, err := m.gws[g].Admit(m.ctx, c)
	if err != nil {
		return // refused: no model step
	}
	m.phase[r], m.tickets[r] = "inflight", t
	qobs.Record(m.ctx, "admit", nil, "request", r, "gateway", g)
}

func (m *limMachine) RuleComplete(tc hegel.TestCase) {
	rs := m.inPhase("inflight")
	if len(rs) == 0 {
		return
	}
	m.complete(hegel.Draw(tc, hegel.SampledFrom(rs)), int64(hegel.Draw(tc, hegel.Integers(1, 3))))
}

func (m *limMachine) complete(r string, cost int64) {
	m.tickets[r].Complete(m.ctx, cost)
	m.rows = append(m.rows, row{r, cost, m.clock}) // its row, written at its end
	m.phase[r] = "done"
	qobs.Record(m.ctx, "complete", nil, "request", r, "cost", cost)
}

func (m *limMachine) RuleCancel(tc hegel.TestCase) {
	rs := m.inPhase("inflight")
	if len(rs) == 0 {
		return
	}
	r := hegel.Draw(tc, hegel.SampledFrom(rs))
	seen := int64(hegel.Draw(tc, hegel.Integers(1, 3)))
	m.tickets[r].Cancel(m.ctx, seen)
	m.rows = append(m.rows, row{r, seen, m.clock})
	m.phase[r], m.seen[r] = "cancelled", seen
	qobs.Record(m.ctx, "cancel", nil, "request", r, "seen", seen)
}

// RuleProviderEnds: the provider stops working on a cancelled call, having
// billed at least what the gateway saw.
func (m *limMachine) RuleProviderEnds(tc hegel.TestCase) {
	rs := m.inPhase("cancelled")
	if len(rs) == 0 {
		return
	}
	r := hegel.Draw(tc, hegel.SampledFrom(rs))
	cost := int64(hegel.Draw(tc, hegel.Integers(int(m.seen[r]), 3)))
	m.tickets[r].Ended(m.ctx)
	m.phase[r] = "done"
	qobs.Record(m.ctx, "providerEnds", nil, "request", r, "cost", cost)
}

// RuleTick: a minute passes, if every row on its way is within the
// allowance of the next minute (the deployment's assumption).
func (m *limMachine) RuleTick(hegel.TestCase) {
	if slices.ContainsFunc(m.rows, func(r row) bool { return r.at < m.clock+1-allowance }) {
		return
	}
	m.clock++
	qobs.Record(m.ctx, "tick", nil)
}

// RuleDeliver: the ledger ingests a row, late and in any order.
func (m *limMachine) RuleDeliver(tc hegel.TestCase) {
	if len(m.rows) == 0 {
		return
	}
	i := hegel.Draw(tc, hegel.Integers(0, len(m.rows)-1))
	r := m.rows[i]
	m.rows = slices.Delete(m.rows, i, i+1)
	m.ledger = append(m.ledger, r)
	qobs.Record(m.ctx, "deliver", nil, "id", r.id, "cost", r.cost, "at", r.at)
}

// RulePublish: golem-control's snapshot: a boundary the allowance behind
// now, and the spend of the ledger's rows before it (ADR 0048).
func (m *limMachine) RulePublish(hegel.TestCase) {
	b := m.clock - allowance
	k := Known{Period: period, Since: inPeriod.Add(time.Duration(b) * time.Minute)}
	for _, r := range m.ledger {
		if r.at < b {
			k.Spend += r.cost
		}
	}
	m.latest = k
	qobs.Record(m.ctx, "publishSnapshot", nil)
}

func (m *limMachine) RuleApply(tc hegel.TestCase) {
	g := hegel.Draw(tc, hegel.SampledFrom(limitGws))
	if m.applied[g] == m.latest {
		return // nothing new to apply
	}
	m.applied[g] = m.latest
	m.gws[g].ApplySnapshot(m.ctx, scope, m.latest)
	qobs.Record(m.ctx, "applySnapshot", nil, "gateway", g)
}

func (m *limMachine) RuleValkeyDown(hegel.TestCase) {
	if m.down {
		return
	}
	m.down = true
	m.store.SetDown(true)
	for _, g := range limitGws {
		m.gws[g].Renew(m.ctx) // fails: each gateway notices
		m.recovered[g] = false
	}
	qobs.Record(m.ctx, "valkeyDown", nil)
}

func (m *limMachine) RuleValkeyBack(hegel.TestCase) {
	if !m.down {
		return
	}
	m.down = false
	m.store.SetDown(false)
	qobs.Record(m.ctx, "valkeyBack", nil)
}

// RuleRecover: a gateway notices Valkey is back, replays what it held one
// increment at a time, and recovers.
func (m *limMachine) RuleRecover(tc hegel.TestCase) {
	g := hegel.Draw(tc, hegel.SampledFrom(limitGws))
	if m.down || m.recovered[g] {
		return
	}
	for {
		in, ok, err := m.gws[g].ReplayOne(m.ctx)
		if err != nil {
			tc.Errorf("replay at %s: %v", g, err) // aborts the case (Hegel)
		}
		if !ok {
			break
		}
		at := (in.At.Unix() - inPeriod.Unix()) / 60
		qobs.Record(m.ctx, "replayOne", nil, "gateway", g, "id", in.Call, "cost", in.Cost, "at", at)
	}
	if err := m.gws[g].Recover(m.ctx); err != nil {
		tc.Errorf("recover %s: %v", g, err)
	}
	m.recovered[g], m.graced[g] = true, true
	qobs.Record(m.ctx, "recover", nil, "gateway", g)
}

// RuleGraceEnds: a gateway's grace ends, once every gateway has recovered.
func (m *limMachine) RuleGraceEnds(tc hegel.TestCase) {
	g := hegel.Draw(tc, hegel.SampledFrom(limitGws))
	if !m.graced[g] || slices.ContainsFunc(limitGws, func(h string) bool { return !m.recovered[h] }) {
		return
	}
	m.gws[g].EndGrace()
	m.graced[g] = false
	qobs.Record(m.ctx, "graceEnds", nil, "gateway", g)
}

// RuleFailover: Valkey loses a minute of its counter.
func (m *limMachine) RuleFailover(tc hegel.TestCase) {
	minutes := slices.Sorted(maps.Keys(m.store.Buckets(scope, period)))
	if m.down || len(minutes) == 0 {
		return
	}
	minute := hegel.Draw(tc, hegel.SampledFrom(minutes))
	m.store.Failover(scope, period, minute)
	qobs.Record(m.ctx, "failover", nil, "t", minute-inPeriod.Unix()/60)
}

func limBody(mut limitMutation) func(hegel.TestCase, *qhegel.Case) {
	return func(ht hegel.TestCase, c *qhegel.Case) {
		ctx := qobs.WithRecorder(context.Background(), c.Recorder)
		hegel.RunStateful(ht, newLimMachine(ctx, mut), hegel.WithStatefulStepCount(25))
	}
}

func TestLimiterAgainstExposureModel(t *testing.T) {
	qtest.RequireQuint(t)
	qhegel.Test(t, qhegel.Config{Binding: "exposure.binding.yaml", Mode: qhegel.Batched, AllowEmpty: true}, limBody(limitFaithful),
		hegel.WithTestCases(60), hegel.WithDatabase(""))
}

func TestNoScopeLimitCaught(t *testing.T) {
	qtest.RequireQuint(t)
	if testing.Short() {
		t.Skip("shrinks against the model")
	}
	qhegel.FindViolation(t, qhegel.ViolationConfig{Binding: "exposure.binding.yaml"}, limBody(noScopeLimit),
		hegel.WithTestCases(100), hegel.WithSeed(1), hegel.WithDatabase("")).Expect(t, qtest.Rejects("admit"))
}

// admitScripted and completeScripted are RuleAdmit and RuleComplete with
// their draws given (scripted regressions).
func (m *limMachine) admitScripted(r, g string)             { m.admit(r, g) }
func (m *limMachine) completeScripted(r string, cost int64) { m.complete(r, cost) }
