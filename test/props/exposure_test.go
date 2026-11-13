package props

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/qhegel"
	"github.com/chucklehead-dev/gotogether/qobs"
	"github.com/chucklehead-dev/gotogether/qtest"
	"hegel.dev/go/hegel"

	"github.com/chucklehead-dev/golem/gateway/limit"
)

// Hegel drives the real limiter (one per gateway over one shared store),
// a provider that bills cancelled calls up to the worst case, a ledger that
// ingests late and in any order, golem-control publishing watermarked
// snapshots that each gateway applies on its own, Valkey outages with
// per-gateway recovery and grace, and failovers; model/exposure.md judges
// every run. The connect test in gateway/limit covers the other direction.

var (
	requests = []string{"a1", "a2", "b1", "b2", "c1"}
	owners   = map[string]string{"a1": "p1", "a2": "p1", "b1": "p2", "b2": "p2", "c1": "p3"}
	limitGws = []string{"g1", "g2"}
)

type limitMutation int

const (
	limitFaithful limitMutation = iota
	noScopeLimit                // per-principal limits only
)

type row struct {
	id   string
	cost int64
	seq  int64
}

type limMachine struct {
	ctx       context.Context
	mut       limitMutation
	store     *limit.Mem
	gws       map[string]*limit.Limiter
	phase     map[string]string // idle, inflight, cancelled, done
	tickets   map[string]*limit.Ticket
	seen      map[string]int64
	rows      []row // numbered, not yet ingested
	ledger    []row
	latest    limit.Known
	applied   map[string]limit.Known
	down      bool
	recovered map[string]bool // since the last outage; grace may end once all have
	graced    map[string]bool
}

var (
	inPeriod = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	period   = "2026-11"
)

func newLimMachine(ctx context.Context, mut limitMutation) *limMachine {
	st := limit.NewMem()
	m := &limMachine{ctx: ctx, mut: mut, store: st, gws: map[string]*limit.Limiter{}, latest: limit.Known{Period: period},
		phase: map[string]string{}, tickets: map[string]*limit.Ticket{}, seen: map[string]int64{},
		recovered: map[string]bool{}, graced: map[string]bool{}, applied: map[string]limit.Known{}}
	for _, g := range limitGws {
		l := limit.New(st)
		l.Now = func() time.Time { return inPeriod } // the model has one budget period
		l.OnNumbered = func(n limit.Numbered) { m.rows = append(m.rows, row{n.ID, n.Cost, n.Seq}) }
		m.gws[g] = l
		m.recovered[g] = true
		m.applied[g] = limit.Known{Period: period} // the model's initial snapshot: nothing, in its one period
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
	c := limit.Call{ID: r, Principal: owners[r], Scope: "s", Budget: 4, PrincipalLimit: 1, ScopeLimit: 2}
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

// RuleDeliver: the ledger ingests a numbered row, late and in any order.
func (m *limMachine) RuleDeliver(tc hegel.TestCase) {
	if len(m.rows) == 0 {
		return
	}
	i := hegel.Draw(tc, hegel.Integers(0, len(m.rows)-1))
	r := m.rows[i]
	m.rows = slices.Delete(m.rows, i, i+1)
	m.ledger = append(m.ledger, r)
	qobs.Record(m.ctx, "deliver", nil, "id", r.id, "cost", r.cost, "seq", r.seq)
}

// RulePublish: golem-control's snapshot: the ledger's contiguous
// watermark and the spend of the rows up to it (ADR 0042).
func (m *limMachine) RulePublish(hegel.TestCase) {
	have := map[int64]bool{}
	for _, r := range m.ledger {
		have[r.seq] = true
	}
	var w int64
	for have[w+1] {
		w++
	}
	k := limit.Known{Period: period, Watermark: w}
	for _, r := range m.ledger {
		if r.seq <= w {
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
	m.gws[g].ApplySnapshot(m.ctx, "s", m.latest)
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
		e, ok, err := m.gws[g].ReplayOne(m.ctx)
		if err != nil {
			tc.Errorf("replay at %s: %v", g, err) // aborts the case (Hegel)
		}
		if !ok {
			break
		}
		qobs.Record(m.ctx, "replayOne", nil, "gateway", g, "id", e.ID, "cost", e.Cost)
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

// RuleFailover: Valkey loses its counter's tail.
func (m *limMachine) RuleFailover(tc hegel.TestCase) {
	n := len(m.store.Counter("s", period))
	if m.down || n == 0 {
		return
	}
	keep := hegel.Draw(tc, hegel.Integers(0, n-1))
	m.store.Failover("s", period, keep)
	qobs.Record(m.ctx, "failover", nil, "keep", keep)
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
