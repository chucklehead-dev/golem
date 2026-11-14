package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

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

// TestUnpricedScope: a budgeted scope over an unpriced model, written to
// the state, does not stop publication: every pool is published, the
// scope without that alias (it fails closed) and reported, the other
// scopes as they are. A config edit that leaves a budgeted scope unpriced
// is refused as a config, and the last good one renewed.
func TestUnpricedScope(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	edge := *r1.Publishers["internal"]
	edge.Pool = "edge"
	r1.Publishers["edge"] = &edge
	pricedConfig(w, false)
	w.lead("r1")
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	if _, err := r1.Elector.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{
			"budgeted": {Models: []string{"priced", "unpriced"}, BudgetMicros: 5_000_000},
			"free":     {Models: []string{"priced", "unpriced"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := w.step("r1")
	if !p || !errors.Is(err, ErrUnpriced) || !strings.Contains(err.Error(), `scope budgeted: models: alias "unpriced" reaches model "local-large"`) {
		t.Fatalf("published %v, %v: want every pool published and the unpriced alias reported", p, err)
	}
	if u := r1.Unpriced(); len(u) != 1 || u[0].Object != "scope budgeted" {
		t.Fatalf("status %v: want the budgeted scope's alias", u)
	}
	if m := w.scopeModels(); m["budgeted"] != "priced" || m["free"] != "priced,unpriced" {
		t.Fatalf("published scopes %v: want the budgeted scope without its unpriced alias, the free one whole", m)
	}
	for _, pool := range []string{"internal", "edge"} {
		raw, _, err := w.bucket.Get(t.Context(), snapshot.PointerKey(pool), "")
		ptr, verr := snapshot.OpenPointer(raw, w.signer.Verifier())
		if err != nil || verr != nil || ptr.PublishSeq != 2 {
			t.Fatalf("pool %s: pointer %+v %v %v; want publication 2", pool, ptr, err, verr)
		}
	}
	if p, err := w.step("r1"); p || !errors.Is(err, ErrUnpriced) {
		t.Fatalf("a step with nothing due: published %v, %v; want the scope still reported", p, err)
	}

	// The model is priced: the alias is published again.
	pricedConfig(w, true)
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("after pricing the model: %v %v", p, err)
	}
	if m := w.scopeModels(); m["budgeted"] != "priced,unpriced" || len(r1.Unpriced()) != 0 {
		t.Fatalf("published scopes %v, status %v", m, r1.Unpriced())
	}
	// An edit taking the price away again is the config's mistake: refused,
	// and the last good config renewed with the alias.
	pricedConfig(w, false)
	r1.clock = r1.clock.Add(61 * time.Second)
	w.lead("r1")
	p, err = w.step("r1")
	if !p || err == nil || !strings.Contains(err.Error(), "last good one is published") || errors.Is(err, ErrUnpriced) {
		t.Fatalf("published %v, %v: want the renewal and the config refused", p, err)
	}
	if m := w.scopeModels(); m["budgeted"] != "priced,unpriced" {
		t.Fatalf("published scopes %v: the refused edit was published", m)
	}

	// The control API's check (service.Service.Priced): against the good
	// config, which prices both; on a replica with none compiled, against
	// the directory, which does not.
	d := r1.Elector.State.Current()
	d.Scopes["new"] = state.Scope{Models: []string{"priced", "unpriced"}, BudgetMicros: 1}
	if err := r1.CheckScope(d, "new"); err != nil {
		t.Fatalf("the good config prices both: %v", err)
	}
	other := &Controller{Dir: w.dir, Lease: time.Hour}
	if err := other.CheckScope(d, "new"); err == nil || !strings.Contains(err.Error(), `"unpriced" reaches model "local-large"`) {
		t.Fatalf("checked against the directory: %v", err)
	}
	if err := other.CheckScope(d, "free"); err != nil {
		t.Fatalf("a scope with no budget: %v", err)
	}
}
