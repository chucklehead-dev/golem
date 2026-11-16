package controller

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// policy writes scope's ScopePolicy to the config directory: spec in flow
// style.
func (w *world) policy(scope, spec string) {
	w.write("policy-"+scope+".yaml", "apiVersion: golem.dev/v1alpha1\nkind: ScopePolicy\nmetadata: {name: "+scope+"}\nspec: "+spec+"\n")
}

// pricedConfig writes a backend with two deployments, a route to each, and
// a PriceList pricing the first's model only, or both.
func pricedConfig(w *world, both bool) {
	w.write("local.yaml", `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: local}
spec:
  dialect: openai.chat
  auth: {header: authorization, secretRef: {name: local, key: token}}
  deployments:
  - {name: small, url: "https://models.example/v1/chat/completions", model: local-small}
  - {name: large, url: "https://models.example/v1/chat/completions", model: local-large}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: priced}
spec: {alias: priced, pool: [{backend: local, deployment: small}]}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: unpriced}
spec: {alias: unpriced, pool: [{backend: local, deployment: large}]}
`)
	prices := "  - {model: local-small, inputPerMTok: 1000000, outputPerMTok: 2000000}\n"
	if both {
		prices += "  - {model: local-large, inputPerMTok: 1000000, outputPerMTok: 2000000}\n"
	}
	w.write("prices.yaml", "apiVersion: golem.dev/v1alpha1\nkind: PriceList\nmetadata: {name: prices}\nspec:\n  currency: USD\n  prices:\n"+prices)
}

// scopeModels is each scope's models in the gateway's snapshot.
func (w *world) scopeModels() map[string]string {
	c, err := w.gateway.Holder.Current()
	if err != nil {
		w.t.Fatal(err)
	}
	m := map[string]string{}
	for _, s := range c.Snapshot.Entitlements.Scopes {
		m[s.ID] = strings.Join(s.Models, ",")
	}
	return m
}

// TestUnmatchedScopes: the state's scopes and the config's ScopePolicies
// change apart, and a mismatch does not stop publication: every pool is
// published, a scope with no policy able to call nothing, a policy of no
// scope left out, both reported (ErrUnmatched, Unmatched, the failure
// count) until they match; the matched scope as its policy says.
func TestUnmatchedScopes(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	edge := *r1.Publishers["internal"]
	edge.Pool = "edge"
	r1.Publishers["edge"] = &edge
	w.policy("team-a", "{models: [claude]}")
	w.policy("team-later", "{models: [claude]}")
	w.lead("r1")
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {}, "team-b": {}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := w.step("r1")
	if !p || !errors.Is(err, ErrUnmatched) || !strings.Contains(err.Error(), `scope team-b: ScopePolicy: no ScopePolicy named "team-b"`) ||
		!strings.Contains(err.Error(), `metadata.name: the state has no scope "team-later"`) {
		t.Fatalf("published %v, %v: want every pool published and both mismatches reported", p, err)
	}
	if u := r1.Unmatched(); len(u) != 2 {
		t.Fatalf("status %v: want the scope with no policy and the policy with no scope", u)
	}
	if m := w.scopeModels(); !slices.Equal(slices.Sorted(maps.Keys(m)), []string{"team-a", "team-b"}) || m["team-a"] != "claude" || m["team-b"] != "" {
		t.Fatalf("published scopes %v: want team-a's policy and team-b able to call nothing", m)
	}
	for _, pool := range []string{"internal", "edge"} {
		raw, _, err := w.bucket.Get(t.Context(), snapshot.PointerKey(pool), "")
		ptr, verr := snapshot.OpenPointer(raw, w.signer.Verifier())
		if err != nil || verr != nil || ptr.PublishSeq != 1 {
			t.Fatalf("pool %s: pointer %+v %v %v; want publication 1", pool, ptr, err, verr)
		}
	}
	if n := r1.Failures()[Failure{"internal", ReasonUnmatched}]; n != 1 {
		t.Fatalf("failures %v: want the degraded publication counted", r1.Failures())
	}
	if p, err := w.step("r1"); p || !errors.Is(err, ErrUnmatched) {
		t.Fatalf("a step with nothing due: published %v, %v; want the mismatch still reported", p, err)
	}

	// Both fixed: team-b's policy merged, team-later created.
	w.policy("team-b", "{models: [claude], budgetMicros: 0}")
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes["team-later"] = state.Scope{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p, err := w.step("r1"); !p || err != nil || len(r1.Unmatched()) != 0 {
		t.Fatalf("matched: %v %v, status %v", p, err, r1.Unmatched())
	}
	if m := w.scopeModels(); m["team-b"] != "claude" || m["team-later"] != "claude" {
		t.Fatalf("published scopes %v", m)
	}
}

// TestUnpricedPolicy: a ScopePolicy whose budget reaches a model no
// PriceList prices is the config's mistake, both being config: the edit
// is refused, and the last good config renewed as it was.
func TestUnpricedPolicy(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	pricedConfig(w, true)
	w.policy("budgeted", "{models: [priced, unpriced], budgetMicros: 5000000}")
	w.lead("r1")
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"budgeted": {}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	pricedConfig(w, false)
	r1.clock = r1.clock.Add(61 * time.Second)
	w.lead("r1")
	p, err := w.step("r1")
	if !p || err == nil || !strings.Contains(err.Error(), "last good one is published") ||
		!strings.Contains(err.Error(), `ScopePolicy/budgeted (policy-budgeted.yaml#1): spec.models: alias "unpriced" reaches model "local-large", which no PriceList prices`) {
		t.Fatalf("published %v, %v: want the renewal and the config refused", p, err)
	}
	if m := w.scopeModels(); m["budgeted"] != "priced,unpriced" {
		t.Fatalf("published scopes %v: the refused edit was published", m)
	}
}

// TestScopeModels: the control API's check of a key's models
// (service.Service.ScopeModels) reads a scope's ScopePolicy from the good
// config or, on a replica with none compiled, the directory; a scope with
// no policy allows none; a directory that does not compile cannot tell.
func TestScopeModels(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	w.policy("team-a", "{models: [claude]}")
	w.lead("r1")
	w.step("r1")
	if m, ok := r1.ScopeModels("team-a"); !ok || !slices.Equal(m, []string{"claude"}) {
		t.Fatalf("from the good config: %v %v", m, ok)
	}
	if m, ok := r1.ScopeModels("team-z"); !ok || m != nil {
		t.Fatalf("a scope with no policy: %v %v", m, ok)
	}
	other := &Controller{Dir: w.dir, Lease: time.Hour}
	if m, ok := other.ScopeModels("team-a"); !ok || !slices.Equal(m, []string{"claude"}) {
		t.Fatalf("from the directory: %v %v", m, ok)
	}
	w.policy("team-a", "{models: [nowhere]}")
	if _, ok := other.ScopeModels("team-a"); ok {
		t.Fatal("a directory that does not compile told")
	}
}

// TestRemovedPolicyRefused: a config edit that deletes a live scope's
// ScopePolicy is refused (the last good config stays published, the scope
// keeps its models); deleting a policy of a scope the state does not have
// is not; and models: [] is the way to stop a live scope.
func TestRemovedPolicyRefused(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	w.lead("r1")
	w.policy("team-a", "{models: [claude]}")
	w.policy("team-z", "{models: [claude]}") // no such scope in the state
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p, _ := w.step("r1"); !p {
		t.Fatal("not published")
	}
	models := func() string {
		c, _ := w.gateway.Holder.Current()
		for _, s := range c.Snapshot.Entitlements.Scopes {
			if s.ID == "team-a" {
				return strings.Join(s.Models, ",")
			}
		}
		return "absent"
	}
	os.Remove(filepath.Join(w.dir, "policy-team-a.yaml"))
	r1.clock = r1.clock.Add(61 * time.Second)
	w.lead("r1")
	if _, err := w.step("r1"); err == nil || !strings.Contains(err.Error(), "ScopePolicy of live scope team-a removed") {
		t.Fatalf("%v: want the edit refused, naming team-a", err)
	}
	if m := models(); m != "claude" {
		t.Fatalf("team-a's models %q after a refused edit: want the last good config's", m)
	}
	w.policy("team-a", "{models: []}")
	os.Remove(filepath.Join(w.dir, "policy-team-z.yaml"))
	r1.clock = r1.clock.Add(61 * time.Second)
	w.lead("r1")
	if _, err := w.step("r1"); err != nil && strings.Contains(err.Error(), "removed") {
		t.Fatalf("%v: models: [] and a policy of no live scope removed are accepted", err)
	}
	if m := models(); m != "" {
		t.Fatalf("team-a's models %q: want none", m)
	}
}
