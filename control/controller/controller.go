// Package controller is golem-control's publishing loop (control-plane.md
// §4, §6): while this replica leads, it compiles the config and publishes
// every pool's snapshot when the config changes, when it becomes leader,
// and every renewal interval to extend the snapshots' authorization lease
// (ADR 0030).
//
// This slice reads config CRDs from a directory and gives every pool the
// same snapshot, its entitlements the state document's (principals,
// scopes, keys and revocations); a change to them is published at the next
// step. Pool selectors and the CRD watch come later.
package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/control/audit"
	"github.com/chucklehead-dev/golem/control/entrasync"
	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/control/state"
	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/compile"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Controller publishes the config in Dir to every pool in Publishers.
type Controller struct {
	Dir        string // config CRD YAML
	Elector    *lead.Elector
	Publishers map[string]*publish.Publisher // by pool; they share the elector's state store
	// Lease is the published snapshots' authorization (ADR 0030, 0032: four
	// hours for internal pools); Renew how often they are republished to
	// extend it (default a minute).
	Lease, Renew time.Duration
	Now          func() time.Time
	Log          *slog.Logger
	// Audit, if set, ships the state's audit outbox while this replica
	// leads (control/audit).
	Audit *audit.Shipper
	// Spend, if set, is the known-spend job: each publication carries every
	// scope's spend this period from the ledger, to a boundary (ADR 0048).
	// It is read whenever a publication is due.
	Spend *spend.Job
	// Sync, if set, reconciles the bound Entra groups' members while this
	// replica leads, every SyncEvery (default five minutes; ADR 0047).
	Sync      *entrasync.Sync
	SyncEvery time.Duration
	synced    time.Time

	good      v1.Objects // the last config that compiled
	goodSum   [32]byte
	published struct {
		sum   [32]byte
		ents  [32]byte // the state's entitlements published
		epoch uint64
		at    time.Time
	}
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// lease is the snapshots' authorization lease: four hours unless set.
func (c *Controller) lease() time.Duration { return cmp.Or(c.Lease, 4*time.Hour) }

func (c *Controller) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Step publishes if this replica leads and a publication is due: a config
// change, a new epoch, or the renewal interval. A config that does not
// compile is reported, and the last one that did is renewed, so a broken
// edit cannot let every gateway's authorization lapse. It reports whether
// it published.
func (c *Controller) Step(ctx context.Context) (bool, error) {
	epoch := c.Elector.Epoch()
	if epoch == 0 {
		return false, nil
	}
	cfgErr := c.load()
	if c.goodSum == ([32]byte{}) {
		return false, fmt.Errorf("no config has compiled yet: %w", cfgErr)
	}
	// The state as of now: a revocation is published within a step of its
	// write (the store's own loop refreshes it too).
	if err := c.Elector.State.Refresh(ctx); err != nil {
		return false, errors.Join(err, cfgErr)
	}
	doc := c.Elector.State.Current()
	ents, err := entitlementsSum(doc)
	if err != nil {
		return false, err
	}
	renew := cmp.Or(c.Renew, time.Minute)
	due := c.goodSum != c.published.sum || ents != c.published.ents || epoch != c.published.epoch ||
		!c.now().Before(c.published.at.Add(renew))
	if !due {
		return false, cfgErr
	}
	source := doc.StateVersion
	var spendErr error
	if c.Spend != nil {
		spendErr = c.Spend.Step(ctx) // on failure, the last result for this period
	}
	entitlements := func(aliases []string) snapshot.Entitlements {
		e := doc.Entitlements(aliases)
		if c.Spend != nil {
			if k, ok := c.Spend.Current(); ok {
				for i := range e.Scopes {
					e.Scopes[i].Period, e.Scopes[i].Since = k.Period, k.Since
					e.Scopes[i].KnownSpend = k.Spend[e.Scopes[i].ID]
				}
			}
		}
		return e
	}
	var pubs []*publish.Publisher
	for _, pool := range slices.Sorted(maps.Keys(c.Publishers)) {
		pubs = append(pubs, c.Publishers[pool])
	}
	// Every pool's record in one state write (ADR 0049).
	ptrs, perrs := publish.PublishAll(ctx, c.Elector.State, publish.Lease{Holder: c.Elector.ID, Epoch: epoch}, source, pubs,
		func(_ *publish.Publisher, seq uint64) (*snapshot.Snapshot, error) {
			return compile.Compile(c.good, compile.Options{PublishSeq: seq, Now: c.now().UTC(), Lease: c.lease(),
				Entitlements: entitlements})
		})
	var errs []error
	for _, pool := range slices.Sorted(maps.Keys(c.Publishers)) {
		if err := perrs[pool]; err != nil {
			errs = append(errs, fmt.Errorf("pool %s: %w", pool, err))
			continue
		}
		ptr := ptrs[pool]
		c.log().Info("published", "pool", pool, "seq", ptr.PublishSeq, "epoch", epoch, "hash", ptr.Hash[:12])
	}
	if len(errs) > 0 {
		return false, errors.Join(append(errs, cfgErr, spendErr)...)
	}
	c.published.sum, c.published.ents, c.published.epoch, c.published.at = c.goodSum, ents, epoch, c.now()
	return true, errors.Join(cfgErr, spendErr)
}

// load reads the directory, keeping it as the good config if it compiles
// (a trial compile with no entitlements). An unchanged directory costs a
// content hash.
func (c *Controller) load() error {
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return fmt.Errorf("config directory: %w", err)
	}
	defer root.Close()
	fsys := root.FS()
	sum, err := v1.ContentHash(fsys)
	if err != nil {
		return err
	}
	if sum == c.goodSum {
		return nil
	}
	objs, err := v1.DecodeFS(fsys)
	if err != nil {
		return fmt.Errorf("config not applied, the last good one is published: %w", err)
	}
	if _, err := compile.Compile(objs, compile.Options{PublishSeq: 1, Now: c.now().UTC(), Lease: c.lease()}); err != nil {
		return fmt.Errorf("config not applied, the last good one is published: %w", err)
	}
	c.good, c.goodSum = objs, sum
	return nil
}

// Run tries for the lease and publishes when due, every second, until
// ctx ends. It returns once the elector's loop has too, so a Release after
// it cannot be undone by a renewal still in flight.
func (c *Controller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { c.Elector.Run(ctx) })
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := c.Step(ctx)
		if c.Audit != nil && c.Elector.Epoch() != 0 {
			if _, aerr := c.Audit.Step(ctx); aerr != nil {
				err = errors.Join(err, fmt.Errorf("audit: %w", aerr))
			}
		}
		if serr := c.syncStep(ctx); serr != nil {
			err = errors.Join(err, fmt.Errorf("entra sync: %w", serr))
		}
		switch {
		case err == nil:
			last = ""
		case err.Error() != last:
			c.log().Error("golem-control", "err", err)
			last = err.Error()
		}
	}
}

// syncStep runs the group sync if this replica leads and it is due. A
// failed step is retried at the next interval, not every second.
func (c *Controller) syncStep(ctx context.Context) error {
	epoch := c.Elector.Epoch()
	if c.Sync == nil || epoch == 0 || c.now().Before(c.synced.Add(cmp.Or(c.SyncEvery, 5*time.Minute))) {
		return nil
	}
	c.synced = c.now()
	res, err := c.Sync.Step(ctx, state.Fence{Holder: c.Elector.ID, Epoch: epoch})
	if err == nil && res != (entrasync.Result{}) {
		c.log().Info("entra sync", "created", res.Created, "added", res.Added, "removed", res.Removed, "keysRevoked", res.Revoked)
	}
	return err
}

// entitlementsSum hashes the state's entitlements: not its version, which
// the publisher's own record update moves at every publication.
func entitlementsSum(d *state.Document) ([32]byte, error) {
	b, err := json.Marshal(struct {
		P map[string]state.Principal
		S map[string]state.Scope
		K map[string]state.Key
		R []state.Revocation
	}{d.Principals, d.Scopes, d.Keys, d.Revocations}) // synced memberships are the principals'
	return sha256.Sum256(b), err
}
