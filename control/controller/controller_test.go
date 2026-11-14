package controller

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	gwsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
)

const backend = `apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: anthropic}
spec:
  dialect: anthropic.messages
  auth: {header: x-api-key, secretRef: {name: anthropic, key: api-key}}
  deployments:
  - {name: sonnet, url: "https://api.anthropic.com/v1/messages", model: claude-sonnet-4-5}
`

func route(alias string) string {
	return `apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: ` + alias + `}
spec: {alias: ` + alias + `, pool: [{backend: anthropic, deployment: sonnet}]}
`
}

type secrets struct{}

func (secrets) Secret(string, string) (string, error) { return "provider-key", nil }

// replica is a golem-control replica with its own state view and clock.
type replica struct {
	*Controller
	clock time.Time
}

type world struct {
	t        *testing.T
	dir      string // the config CRDs
	bucket   *bucket.Dir
	signer   sign.Ed25519
	replicas map[string]*replica
	gateway  *gwsnap.Bucket
}

func newWorld(t *testing.T) *world {
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	pk, sk, _ := ed25519.GenerateKey(nil)
	w := &world{t: t, dir: t.TempDir(), bucket: b, signer: sign.Ed25519{ID: "k", Key: sk}, replicas: map[string]*replica{}}
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"r1", "r2"} {
		r := &replica{clock: start}
		now := func() time.Time { return r.clock }
		st := &state.Store{Bucket: b}
		r.Controller = &Controller{Dir: w.dir, Lease: 4 * time.Hour, Now: now,
			Elector:    &lead.Elector{State: st, ID: id, Duration: 15 * time.Second, Now: now},
			Publishers: map[string]*publish.Publisher{"internal": {State: st, Bucket: b, Signer: w.signer, Pool: "internal", Now: now}}}
		w.replicas[id] = r
	}
	w.gateway = &gwsnap.Bucket{Bucket: b, Keys: sign.Ed25519Keys{"k": pk}, Pool: "internal",
		Holder: &gwsnap.Holder{Now: func() time.Time { return start }}, Secrets: secrets{}}
	w.write("backend.yaml", backend)
	w.write("claude.yaml", route("claude"))
	return w
}

func (w *world) write(name, body string) {
	if err := os.WriteFile(filepath.Join(w.dir, name), []byte(body), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) lead(id string) uint64 {
	e, err := w.replicas[id].Elector.Try(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	return e
}

// step runs a replica's controller once, and has the gateway poll.
func (w *world) step(id string) (bool, error) {
	published, err := w.replicas[id].Step(w.t.Context())
	if perr := w.gateway.Poll(w.t.Context()); perr != nil && !errors.Is(perr, bucket.ErrNotFound) {
		w.t.Fatalf("gateway poll: %v", perr)
	}
	return published, err
}

func (w *world) applied() (seq uint64, aliases []string, validUntil time.Time) {
	c, err := w.gateway.Holder.Current()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, r := range c.Snapshot.Routes {
		aliases = append(aliases, r.Alias)
	}
	return c.Snapshot.PublishSeq, aliases, c.Snapshot.ValidUntil
}

// TestPublishes: golem-control, end to end with a gateway over one bucket.
func TestPublishes(t *testing.T) {
	w := newWorld(t)
	r1, r2 := w.replicas["r1"], w.replicas["r2"]
	if p, _ := w.step("r1"); p {
		t.Fatal("published without the lease")
	}
	if w.lead("r1") != 1 {
		t.Fatal("r1 leads at epoch 1")
	}
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	if seq, aliases, _ := w.applied(); seq != 1 || strings.Join(aliases, ",") != "claude" {
		t.Fatalf("the gateway holds %d %v", seq, aliases)
	}

	t.Run("a config change reaches the gateway", func(t *testing.T) {
		w.write("haiku.yaml", route("haiku"))
		w.step("r1")
		if seq, aliases, _ := w.applied(); seq != 2 || strings.Join(aliases, ",") != "claude,haiku" {
			t.Fatalf("the gateway holds %d %v", seq, aliases)
		}
		if p, _ := w.step("r1"); p {
			t.Fatal("published again with nothing changed and no renewal due")
		}
	})
	t.Run("the authorization is renewed", func(t *testing.T) {
		_, _, before := w.applied()
		r1.clock = r1.clock.Add(61 * time.Second)
		w.lead("r1")
		if p, _ := w.step("r1"); !p {
			t.Fatal("no renewal after a minute")
		}
		if seq, _, after := w.applied(); seq != 3 || !after.After(before) {
			t.Fatalf("renewal %d: valid until %v, was %v", seq, after, before)
		}
	})
	t.Run("a broken edit renews the last good config", func(t *testing.T) {
		w.write("broken.yaml", "apiVersion: golem.dev/v1alpha1\nkind: ModelRoute\nmetadata: {name: x}\nspec: {alias: x, pool: [{backend: nowhere, deployment: d}]}\n")
		r1.clock = r1.clock.Add(61 * time.Second)
		w.lead("r1")
		p, err := w.step("r1")
		if !p || err == nil || !strings.Contains(err.Error(), "last good one") {
			t.Fatalf("published %v, %v: want the renewal and the config's error", p, err)
		}
		if seq, aliases, _ := w.applied(); seq != 4 || strings.Join(aliases, ",") != "claude,haiku" {
			t.Fatalf("the gateway holds %d %v", seq, aliases)
		}
		os.Remove(filepath.Join(w.dir, "broken.yaml"))
	})
	t.Run("a new leader publishes; the paused one is fenced", func(t *testing.T) {
		// r1 pauses: its clock and belief stand still. r2's passes the lease.
		r2.clock = r1.clock.Add(time.Minute)
		if w.lead("r2") != 2 {
			t.Fatal("r2 takes the lapsed lease at epoch 2")
		}
		if p, err := w.step("r2"); !p || err != nil {
			t.Fatalf("r2's first publication: %v %v", p, err)
		}
		w.write("opus.yaml", route("opus")) // r1, resuming, sees a change to publish
		p, err := w.step("r1")
		if p || !errors.Is(err, publish.ErrFenced) {
			t.Fatalf("the paused leader: published %v, %v; want it fenced", p, err)
		}
		if seq, aliases, _ := w.applied(); seq != 5 || strings.Join(aliases, ",") != "claude,haiku" {
			t.Fatalf("the gateway holds %d %v: the fenced leader's change must not reach it", seq, aliases)
		}
		w.step("r2")
		if seq, aliases, _ := w.applied(); seq != 6 || strings.Join(aliases, ",") != "claude,haiku,opus" {
			t.Fatalf("the gateway holds %d %v: the leader's next publication carries the change", seq, aliases)
		}
	})
}
