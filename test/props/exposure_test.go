package props

import (
	"context"
	"slices"
	"testing"

	"github.com/chucklehead-dev/gotogether/qhegel"
	"github.com/chucklehead-dev/gotogether/qobs"
	"github.com/chucklehead-dev/gotogether/qtest"
	"hegel.dev/go/hegel"

	"github.com/chucklehead-dev/golem/gateway/limit"
)

// Hegel drives the real limiter (one per gateway over one shared store),
// a provider that bills cancelled calls up to the worst case, a ledger that
// ingests late and in any order, snapshots, Valkey outages and failovers;
// model/exposure.md judges every run. The connect test in gateway/limit
// covers the other direction.

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

type limMachine struct {
	ctx      context.Context
	mut      limitMutation
	store    *limit.Mem
	gws      map[string]*limit.Limiter
	phase    map[string]string // idle, inflight, cancelled, done
	tickets  map[string]*limit.Ticket
	seen     map[string]int64
	observed []limit.Event // emitted, in order
	ledger   map[string]int64
	snap     map[string]bool
	down     bool
}

func newLimMachine(ctx context.Context, mut limitMutation) *limMachine {
	st := limit.NewMem()
	m := &limMachine{ctx: ctx, mut: mut, store: st, gws: map[string]*limit.Limiter{},
		phase: map[string]string{}, tickets: map[string]*limit.Ticket{}, seen: map[string]int64{},
		ledger: map[string]int64{}, snap: map[string]bool{}}
	for _, g := range limitGws {
		m.gws[g] = limit.New(st)
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
	r := hegel.Draw(tc, hegel.SampledFrom(rs))
	cost := int64(hegel.Draw(tc, hegel.Integers(1, 3)))
	m.tickets[r].Complete(m.ctx, cost)
	m.phase[r] = "done"
	m.observed = append(m.observed, limit.Event{ID: r, Cost: cost})
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
	m.observed = append(m.observed, limit.Event{ID: r, Cost: seen})
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

// RuleDeliver: the ledger ingests an event, late and in any order.
func (m *limMachine) RuleDeliver(tc hegel.TestCase) {
	var todo []limit.Event
	for _, e := range m.observed {
		if _, ok := m.ledger[e.ID]; !ok {
			todo = append(todo, e)
		}
	}
	if len(todo) == 0 {
		return
	}
	e := todo[hegel.Draw(tc, hegel.Integers(0, len(todo)-1))]
	m.ledger[e.ID] = e.Cost
	qobs.Record(m.ctx, "deliver", nil, "id", e.ID, "cost", e.Cost)
}

// RulePublish: golem-control's renewal carries the ledger's balance.
func (m *limMachine) RulePublish(hegel.TestCase) {
	k := limit.Known{Included: map[string]bool{}}
	for id, c := range m.ledger {
		k.Spend += c
		k.Included[id] = true
	}
	m.snap = k.Included
	for _, g := range limitGws {
		m.gws[g].ApplySnapshot(m.ctx, "s", k)
	}
	qobs.Record(m.ctx, "publishSnapshot", nil)
}

func (m *limMachine) RuleValkeyDown(hegel.TestCase) {
	if m.down {
		return
	}
	m.down = true
	m.store.SetDown(true)
	qobs.Record(m.ctx, "valkeyDown", nil)
}

func (m *limMachine) RuleValkeyBack(tc hegel.TestCase) {
	if !m.down {
		return
	}
	m.down = false
	m.store.SetDown(false)
	for _, g := range limitGws {
		if err := m.gws[g].Recover(m.ctx); err != nil {
			tc.Errorf("recover %s: %v", g, err) // aborts the case (Hegel)
		}
	}
	qobs.Record(m.ctx, "valkeyBack", nil)
}

func (m *limMachine) RuleFailover(hegel.TestCase) {
	if m.down {
		return
	}
	es, _ := m.store.Events(m.ctx, "s")
	if !slices.ContainsFunc(es, func(e limit.Event) bool { return !m.snap[e.ID] }) {
		return // nothing to lose: the model's failover is not enabled
	}
	m.store.Failover("s", func(id string) bool { return m.snap[id] })
	qobs.Record(m.ctx, "failover", nil)
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
func (m *limMachine) admitScripted(r, g string) {
	c := limit.Call{ID: r, Principal: owners[r], Scope: "s", Budget: 4, PrincipalLimit: 1, ScopeLimit: 2}
	t, err := m.gws[g].Admit(m.ctx, c)
	if err != nil {
		return
	}
	m.phase[r], m.tickets[r] = "inflight", t
	qobs.Record(m.ctx, "admit", nil, "request", r, "gateway", g)
}

func (m *limMachine) completeScripted(r string, cost int64) {
	m.tickets[r].Complete(m.ctx, cost)
	m.phase[r] = "done"
	m.observed = append(m.observed, limit.Event{ID: r, Cost: cost})
	qobs.Record(m.ctx, "complete", nil, "request", r, "cost", cost)
}
