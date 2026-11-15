//go:build hegel

package snapshot

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/qhegel"
	"github.com/chucklehead-dev/gotogether/qobs"
	"github.com/chucklehead-dev/gotogether/qtest"
	"hegel.dev/go/hegel"
)

// Hegel drives the real gateway (Holder) with a stand-in publisher and an
// attacker that replays any old publication, and model/publication.md
// judges every step the gateway took. This is the direction model-driven
// tests cannot cover: a gateway that applies or serves what the model
// forbids. (The connect test, publication_mbt_test.go, covers the other: a
// gateway refusing what the model allows.)

const (
	lease   = 3 // the design instance's LEASE
	maxTime = 8 // and MAX_TIME
)

// The clock is publication_mbt_test.go's (epoch, at).
var (
	gateways = []string{"g1", "g2"}
	keys     = []string{"k1", "k2"}
)

// fullPub is a publication in full: the model's Pub with its epoch, source
// and revocations (publication_mbt_test.go's pub keeps what its driver
// checks).
type fullPub struct {
	Seq, Epoch, Source int
	Revoked            []string
	ValidUntil         int
}

// pubMutation plants a gateway bug, to show the test catches it.
type pubMutation int

const (
	pubFaithful pubMutation = iota
	noFloor                 // a gateway that ignores its floor
	noExpiry                // a gateway that serves past its lease
)

type pubMachine struct {
	ctx context.Context
	mut pubMutation

	// The stand-in publisher: replica r1, always the leader at epoch 1, as
	// the model's publish defines it (golem-control replaces it).
	clock        int
	revoked      []string
	stateVersion int
	view         struct {
		Version, LoadedAt int
		Revoked           []string
	}
	record    struct{ Seq, Source int }
	pointer   fullPub
	published []fullPub

	gws       map[string]*Holder
	revokedAt map[uint64][]string // what each publication's entitlements revoke
}

func newPubMachine(ctx context.Context, mut pubMutation) *pubMachine {
	m := &pubMachine{ctx: ctx, mut: mut, gws: map[string]*Holder{}, revokedAt: map[uint64][]string{}}
	genesis := fullPub{Seq: 0, Epoch: 1, ValidUntil: lease}
	m.pointer, m.published = genesis, []fullPub{genesis}
	for _, g := range gateways {
		m.gws[g] = m.holder()
		m.apply(g, genesis)
	}
	return m
}

func (m *pubMachine) holder() *Holder {
	return &Holder{Now: func() time.Time { return at(m.clock) }}
}

// apply is the gateway's poll: the real Holder decides.
func (m *pubMachine) apply(g string, p fullPub) error {
	s := publication(pub{Seq: p.Seq, ValidUntil: p.ValidUntil})
	err := m.gws[g].Apply(s, fmt.Sprint(p.Seq), nil)
	if err != nil && m.mut == noFloor && strings.Contains(err.Error(), "below the floor") {
		// The planted bug: a gateway without the floor takes it anyway.
		h := m.holder()
		err = h.Apply(s, fmt.Sprint(p.Seq), nil)
		m.gws[g] = h
	}
	if err == nil {
		m.revokedAt[uint64(p.Seq)] = p.Revoked
	}
	return err
}

func (m *pubMachine) RuleRevoke(tc hegel.TestCase) {
	k := hegel.Draw(tc, hegel.SampledFrom(keys))
	if slices.Contains(m.revoked, k) {
		return
	}
	m.revoked = append(slices.Clone(m.revoked), k)
	slices.Sort(m.revoked)
	m.stateVersion++
	qobs.Record(m.ctx, "revoke", nil, "key", k)
}

func (m *pubMachine) RuleLoadState(hegel.TestCase) {
	m.view.Version, m.view.Revoked, m.view.LoadedAt = m.stateVersion, m.revoked, m.clock
	qobs.Record(m.ctx, "loadState", nil, "replica", "r1")
}

func (m *pubMachine) RulePublish(hegel.TestCase) {
	p := fullPub{Seq: m.record.Seq + 1, Epoch: 1, Source: m.view.Version, Revoked: m.view.Revoked,
		ValidUntil: m.view.LoadedAt + lease} // the lease counts from the source read (ADR 0030)
	m.record.Seq, m.record.Source = p.Seq, p.Source
	m.pointer = p
	m.published = append(m.published, p)
	qobs.Record(m.ctx, "publish", nil, "replica", "r1")
}

// RuleReplay is the attacker: any old publication back in the pointer.
func (m *pubMachine) RuleReplay(tc hegel.TestCase) {
	p := m.published[hegel.Draw(tc, hegel.Integers(0, len(m.published)-1))]
	m.pointer = p
	qobs.Record(m.ctx, "replayPointer", nil, "seq", p.Seq, "epoch", p.Epoch, "source", p.Source,
		"revoked", slices.Clone(p.Revoked), "validUntil", p.ValidUntil)
}

func (m *pubMachine) RulePoll(tc hegel.TestCase) {
	g := hegel.Draw(tc, hegel.SampledFrom(gateways))
	if m.apply(g, m.pointer) == nil {
		qobs.Record(m.ctx, "poll", nil, "gateway", g, "obs.seq", m.pointer.Seq)
	}
}

func (m *pubMachine) RuleRestart(tc hegel.TestCase) {
	g := hegel.Draw(tc, hegel.SampledFrom(gateways))
	h := m.holder()
	h.RaiseFloor(uint64(m.record.Seq)) // the trusted publisher checkpoint
	m.gws[g] = h
	qobs.Record(m.ctx, "restartEmpty", nil, "gateway", g)
}

func (m *pubMachine) RuleTick(hegel.TestCase) {
	if m.clock < maxTime {
		m.clock++
		qobs.Record(m.ctx, "tick", nil)
	}
}

func (m *pubMachine) RuleServe(tc hegel.TestCase) {
	g := hegel.Draw(tc, hegel.SampledFrom(gateways))
	k := hegel.Draw(tc, hegel.SampledFrom(keys))
	c, err := m.gws[g].Current()
	if err == ErrExpired && m.mut == noExpiry {
		err = nil // the planted bug
	}
	if err != nil || slices.Contains(m.revokedAt[c.Snapshot.PublishSeq], k) {
		return // refused
	}
	qobs.Record(m.ctx, "serve", nil, "gateway", g, "key", k)
}

func pubBody(mut pubMutation) func(hegel.TestCase, *qhegel.Case) {
	return func(ht hegel.TestCase, c *qhegel.Case) {
		ctx := qobs.WithRecorder(context.Background(), c.Recorder)
		hegel.RunStateful(ht, newPubMachine(ctx, mut), hegel.WithStatefulStepCount(25))
	}
}

func TestGatewayAgainstPublicationModel(t *testing.T) {
	qtest.RequireQuint(t)
	qhegel.Test(t, qhegel.Config{Binding: "publication.binding.yaml", Mode: qhegel.Batched, AllowEmpty: true}, pubBody(pubFaithful),
		hegel.WithTestCases(60), hegel.WithDatabase(""))
}

func TestNoFloorCaught(t *testing.T) {
	qtest.RequireQuint(t)
	if testing.Short() {
		t.Skip("shrinks against the model")
	}
	qhegel.FindViolation(t, qhegel.ViolationConfig{Binding: "publication.binding.yaml"}, pubBody(noFloor),
		hegel.WithTestCases(100), hegel.WithSeed(1), hegel.WithDatabase("")).Expect(t, qtest.Rejects("poll"))
}

func TestNoExpiryCaught(t *testing.T) {
	qtest.RequireQuint(t)
	if testing.Short() {
		t.Skip("shrinks against the model")
	}
	qhegel.FindViolation(t, qhegel.ViolationConfig{Binding: "publication.binding.yaml"}, pubBody(noExpiry),
		hegel.WithTestCases(100), hegel.WithSeed(1), hegel.WithDatabase("")).Expect(t, qtest.Rejects("serve"))
}
