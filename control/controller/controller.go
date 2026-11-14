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
	"strings"
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

	synced  time.Time // when the group sync last ran
	mu      sync.Mutex
	good    v1.Objects // the last config that compiled; written under mu (CheckScope reads it from the API's goroutines)
	goodSum [32]byte
	// unpriced is what the last publication left out of scopes, by
	// Options.Unpriced (under mu): each scope's aliases that reach a model
	// no PriceList prices.
	unpriced []compile.Problem
	// renewing is each pool's publication as found in the bucket, renewed
	// (compile.Renew) while no config has compiled since this replica
	// started; nil once one has.
	renewing  map[string]*snapshot.Snapshot
	published struct { // the last publication that reached every pool
		sum   [32]byte // its config's
		ents  [32]byte // the state's entitlements published
		epoch uint64
		at    time.Time
	}
	// status is what the loop has done, for its metrics (control/metrics),
	// under mu: written by the loop, read at a collection.
	status status
}

// status is the controller's record of its own work, for its metrics.
type status struct {
	pools        map[string]Publication // each pool's last successful publication by this replica
	failures     map[Failure]int64      // publication attempts failed or degraded
	configBroken bool                   // the last step found the directory not what is published
	synced       time.Time              // the last group sync that succeeded
	guardTrips   int64                  // group syncs refused by their removal guard
	shipped      time.Time              // the last audit step that succeeded
}

// Publication is a pool's last successful publication by this replica:
// when it was made (by the controller's clock), its sequence, and the
// authorization lease it issued (the snapshot's ValidUntil).
type Publication struct {
	At         time.Time
	Seq        uint64
	ValidUntil time.Time
}

// Failure is a kind of publication failure of a pool: Reason is one of
// the Reason constants.
type Failure struct{ Pool, Reason string }

// The reasons a publication attempt failed or was degraded (Failures).
const (
	// ReasonTampered: the pool's checkpoint does not verify, or is missing
	// (publish.ErrTampered); the pool is not published until a person looks.
	ReasonTampered = "tampered"
	// ReasonRegressed: the state document went back (publish.ErrRegressed,
	// state.ErrRegressed, state.ErrMissing).
	ReasonRegressed = "regressed"
	// ReasonFenced: this replica no longer leads at its epoch
	// (publish.ErrFenced, state.ErrFenced).
	ReasonFenced = "fenced"
	// ReasonConflict: another writer won the record or the document
	// (publish.ErrConflict, state.ErrContended).
	ReasonConflict = "conflict"
	// ReasonUnpriced: the publication left aliases out of scopes their
	// budgets could not be charged for (ErrUnpriced).
	ReasonUnpriced = "unpriced"
	// ReasonConfigRenewing: the config directory does not compile, or an
	// edit was refused: the last good config (or the bucket's) is renewed
	// instead, or nothing can be.
	ReasonConfigRenewing = "config_renewing"
	// ReasonOther: anything else (the bucket refusing a write, the signer
	// failing, a build error).
	ReasonOther = "other"
)

// Reasons are every Reason, in a fixed order.
var Reasons = []string{ReasonTampered, ReasonRegressed, ReasonFenced, ReasonConflict, ReasonUnpriced, ReasonConfigRenewing, ReasonOther} // read-only

// FailureReason classifies a publication's error by the sentinel errors it
// wraps.
func FailureReason(err error) string {
	switch {
	case errors.Is(err, publish.ErrTampered):
		return ReasonTampered
	case errors.Is(err, publish.ErrRegressed), errors.Is(err, state.ErrRegressed), errors.Is(err, state.ErrMissing):
		return ReasonRegressed
	case errors.Is(err, publish.ErrFenced), errors.Is(err, state.ErrFenced):
		return ReasonFenced
	case errors.Is(err, publish.ErrConflict), errors.Is(err, state.ErrContended):
		return ReasonConflict
	case errors.Is(err, ErrUnpriced):
		return ReasonUnpriced
	}
	return ReasonOther
}

// count adds a failure of reason to each of pools (every pool if none is
// named).
func (c *Controller) count(reason string, pools ...string) {
	if len(pools) == 0 {
		pools = slices.Collect(maps.Keys(c.Publishers))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.failures == nil {
		c.status.failures = map[Failure]int64{}
	}
	for _, p := range pools {
		c.status.failures[Failure{p, reason}]++
	}
}

// degraded counts what made a publication attempt less than the
// directory's config with every scope whole: a config not applied
// (cfgErr), and aliases left out of scopes.
func (c *Controller) degraded(cfgErr error) {
	if cfgErr != nil {
		c.count(ReasonConfigRenewing)
	}
	if len(c.Unpriced()) > 0 {
		c.count(ReasonUnpriced)
	}
}

// setConfigBroken records whether the directory is what is published.
func (c *Controller) setConfigBroken(broken bool) {
	c.mu.Lock()
	c.status.configBroken = broken
	c.mu.Unlock()
}

// Publications is each pool's last successful publication by this
// replica; a pool it has not published to is absent. For a metric.
func (c *Controller) Publications() map[string]Publication {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.status.pools)
}

// Failures counts this replica's publication attempts that failed or were
// degraded, by pool and reason: one for each attempt (a due step, or a
// step that could not read the state, every second while it cannot). For
// a metric.
func (c *Controller) Failures() map[Failure]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.status.failures)
}

// ConfigRenewing reports whether the last step, while leading, found the
// config directory not what is published: it does not compile, or an edit
// was refused, and the last good config (or the bucket's) is renewed.
func (c *Controller) ConfigRenewing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.configBroken
}

// SyncedAt is when the group sync last succeeded on this replica; zero if
// it has not.
func (c *Controller) SyncedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.synced
}

// GuardTrips counts the group syncs refused by their removal guard
// (entrasync.ErrTooManyRemovals) on this replica.
func (c *Controller) GuardTrips() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.guardTrips
}

// ShippedAt is when an audit step last succeeded on this replica (the
// outbox shipped up to its batch, or found empty); zero if none has.
func (c *Controller) ShippedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.shipped
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
// edit cannot let every gateway's authorization lapse. A scope of the
// state's whose budget reaches an unpriced model is published without
// that model's alias and reported (ErrUnpriced), every step until it is
// fixed; the other scopes and pools are published as usual. It reports
// whether it published.
func (c *Controller) Step(ctx context.Context) (bool, error) {
	fence, leads := c.Elector.Fence()
	if !leads {
		return false, nil
	}
	// The state as of now: a revocation is published within a step of its
	// write (the store's own loop refreshes it too). The config is
	// trial-compiled against it.
	if err := c.Elector.State.Refresh(ctx); err != nil {
		c.count(FailureReason(err))
		return false, err
	}
	doc := c.Elector.State.Current()
	cfgErr := c.load(doc)
	if c.goodSum == ([32]byte{}) {
		// No config has compiled since this replica started: renew what is
		// published (it compiled when it was), so a restart over a broken
		// edit cannot let every gateway's authorization lapse either.
		if err := c.loadPublished(ctx); err != nil {
			c.setConfigBroken(true)
			c.count(ReasonConfigRenewing)
			return false, fmt.Errorf("no config has compiled yet, and nothing published can be renewed: fix the config directory: %w", errors.Join(cfgErr, err))
		}
		cfgErr = fmt.Errorf("renewing the published config: the config directory does not compile, and none has since golem-control started; fix it: %w", cfgErr)
	} else {
		c.renewing = nil
	}
	c.setConfigBroken(cfgErr != nil)
	ents, err := entitlementsSum(doc)
	if err != nil {
		c.count(FailureReason(err))
		return false, err
	}
	if !c.due(ents, fence.Epoch) {
		return false, errors.Join(cfgErr, c.unpricedErr())
	}
	var spendErr error
	if c.Spend != nil {
		spendErr = c.Spend.Step(ctx) // on failure, the last result for this period
	}
	errs := c.publish(ctx, fence, doc) // counts each pool's failure
	c.degraded(cfgErr)
	if len(errs) > 0 {
		return false, errors.Join(append(errs, cfgErr, spendErr, c.unpricedErr())...)
	}
	c.published.sum, c.published.ents, c.published.epoch, c.published.at = c.goodSum, ents, fence.Epoch, c.now()
	return true, errors.Join(cfgErr, spendErr, c.unpricedErr())
}

// ErrUnpriced: a scope of the state's with a budget can reach a model no
// PriceList prices, so it is published without that alias (it fails
// closed) while every other scope and pool is published as usual.
var ErrUnpriced = errors.New("scopes published without the aliases their budgets could not be charged for")

// unpricedErr is ErrUnpriced naming each alias the last publication left
// out, and what to do; nil if it left none out.
func (c *Controller) unpricedErr() error {
	ps := c.Unpriced()
	if len(ps) == 0 {
		return nil
	}
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return fmt.Errorf("%w; their members cannot call them until a PriceList prices the model or the alias is removed from the scope:\n  %s",
		ErrUnpriced, strings.Join(lines, "\n  "))
}

// Unpriced is what the last publication left out of scopes because their
// budgets could not be charged for it: each scope and alias, and the
// unpriced model it reaches. The operator's status; Step reports it too.
func (c *Controller) Unpriced() []compile.Problem {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.unpriced)
}

// CheckScope refuses scope id of d if its budget can reach a model the
// config prices not: the control API's check when a scope is created
// (service.Service.Priced), so the mistake is refused where it is made
// rather than published as a scope that fails closed. It checks against
// the last good config, or, on a replica where none has compiled (one
// that does not lead), the directory's. With neither, or a config that
// does not compile, it checks nothing: the publisher's rule still applies.
func (c *Controller) CheckScope(d *state.Document, id string) error {
	c.mu.Lock()
	objs, ok := c.good, c.goodSum != ([32]byte{})
	c.mu.Unlock()
	if !ok {
		var err error
		if objs, err = c.decodeDir(); err != nil {
			return nil
		}
	}
	var bad []string
	if _, err := compile.Compile(objs, compile.Options{PublishSeq: 1, Now: c.now().UTC(), Lease: c.lease(),
		Entitlements: d.Entitlements, Unpriced: func(p compile.Problem) {
			if p.Object == "scope "+id {
				bad = append(bad, p.Msg)
			}
		}}); err != nil {
		return nil
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s; price those models in a PriceList first, or leave the aliases out of the scope", strings.Join(bad, "; "))
	}
	return nil
}

// due reports whether a publication is due: the good config, the
// entitlements (their hash, ents) or the epoch differ from the last
// publication's, or its renewal interval has passed.
func (c *Controller) due(ents [32]byte, epoch uint64) bool {
	renew := cmp.Or(c.Renew, time.Minute)
	return c.goodSum != c.published.sum || ents != c.published.ents || epoch != c.published.epoch ||
		!c.now().Before(c.published.at.Add(renew))
}

// publish publishes the good config with doc's entitlements to every pool,
// all their records in one state write (ADR 0049), under fence and from
// doc's version. It returns each pool's failure, in pool order.
func (c *Controller) publish(ctx context.Context, fence state.Fence, doc *state.Document) []error {
	pools := slices.Sorted(maps.Keys(c.Publishers))
	var pubs []*publish.Publisher
	for _, pool := range pools {
		pubs = append(pubs, c.Publishers[pool])
	}
	built, unpriced := false, map[string]compile.Problem{} // every pool's build leaves out the same
	validUntil := map[string]time.Time{}                   // each pool's lease, as built
	ptrs, perrs := publish.PublishAll(ctx, c.Elector.State, fence, doc.StateVersion, pubs,
		func(p *publish.Publisher, seq uint64) (*snapshot.Snapshot, error) {
			o := compile.Options{PublishSeq: seq, Now: c.now().UTC(), Lease: c.lease(),
				Entitlements: c.entitlements(doc), Unpriced: func(q compile.Problem) { unpriced[q.String()] = q }}
			var s *snapshot.Snapshot
			var err error
			switch prev, renewing := c.renewing[p.Pool]; {
			case c.goodSum != ([32]byte{}):
				s, err = compile.Compile(c.good, o)
			case renewing:
				s, err = compile.Renew(prev, o)
			default:
				err = fmt.Errorf("nothing published to renew, and no config has compiled: fix the config directory")
			}
			built = built || err == nil
			if err == nil {
				validUntil[p.Pool] = s.ValidUntil
			}
			return s, err
		})
	if built {
		var ps []compile.Problem
		for _, k := range slices.Sorted(maps.Keys(unpriced)) {
			ps = append(ps, unpriced[k])
		}
		c.mu.Lock()
		c.unpriced = ps
		c.mu.Unlock()
	}
	var errs []error
	for _, pool := range pools {
		if err := perrs[pool]; err != nil {
			c.count(FailureReason(err), pool)
			errs = append(errs, fmt.Errorf("pool %s: %w", pool, err))
			continue
		}
		ptr := ptrs[pool]
		c.mu.Lock()
		if c.status.pools == nil {
			c.status.pools = map[string]Publication{}
		}
		c.status.pools[pool] = Publication{At: c.now(), Seq: ptr.PublishSeq, ValidUntil: validUntil[pool]}
		c.mu.Unlock()
		c.log().Info("published", "pool", pool, "seq", ptr.PublishSeq, "epoch", fence.Epoch, "hash", ptr.Hash[:12])
	}
	return errs
}

// entitlements is the snapshot's entitlements for the routes that exist
// (aliases): doc's, each scope with this period's known spend if the
// known-spend job has a result (accounting.md).
func (c *Controller) entitlements(doc *state.Document) func(aliases []string) snapshot.Entitlements {
	return func(aliases []string) snapshot.Entitlements {
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
}

// load reads the directory, keeping it as the good config if it compiles
// with doc's entitlements, as it will be published. A config that leaves a
// budgeted scope of doc's unpriced where the last good one did not (a
// price removed, a route moved to an unpriced model) is refused as a
// config: the edit is what broke it, and the last good config is renewed.
// What the state alone breaks (a budgeted scope created over an unpriced
// alias) is not the config's: that scope fails closed when published
// (compile.Options.Unpriced). An unchanged directory costs a content hash.
func (c *Controller) load(doc *state.Document) error {
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
	unpriced, err := c.trial(objs, doc)
	if err != nil {
		return fmt.Errorf("config not applied, the last good one is published: %w", err)
	}
	if c.goodSum != ([32]byte{}) {
		before, _ := c.trial(c.good, doc)
		var added []string
		for _, p := range unpriced {
			if !slices.ContainsFunc(before, func(q compile.Problem) bool { return q.String() == p.String() }) {
				added = append(added, p.String())
			}
		}
		if len(added) > 0 {
			return fmt.Errorf("config not applied, the last good one is published: with the state's scopes it leaves %d alias(es) unpriced that the last good config prices; price the models in a PriceList, or remove the aliases from those scopes first:\n  %s",
				len(added), strings.Join(added, "\n  "))
		}
	}
	c.mu.Lock()
	c.good, c.goodSum = objs, sum
	c.mu.Unlock()
	return nil
}

// loadPublished reads each pool's current publication from the bucket,
// verified as a gateway verifies it (publish.Publisher.Published), to be
// renewed while no config has compiled since this replica started. It is
// read once; this replica's renewals keep its config part. It fails only
// if no pool has a publication to renew.
func (c *Controller) loadPublished(ctx context.Context) error {
	if c.renewing != nil {
		return nil
	}
	found := map[string]*snapshot.Snapshot{}
	var errs []error
	for _, pool := range slices.Sorted(maps.Keys(c.Publishers)) {
		s, err := c.Publishers[pool].Published(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("pool %s: %w", pool, err))
			continue
		}
		found[pool] = s
		c.log().Error("golem-control: renewing the published config: the config directory does not compile, and none has since this replica started; fix it",
			"pool", pool, "seq", s.PublishSeq, "configDigest", s.Source.ConfigDigest)
	}
	if len(found) == 0 {
		return errors.Join(errs...)
	}
	c.renewing = found
	return nil
}

// trial compiles objs with doc's entitlements, returning what it would
// leave out of scopes for want of a price.
func (c *Controller) trial(objs v1.Objects, doc *state.Document) ([]compile.Problem, error) {
	var unpriced []compile.Problem
	_, err := compile.Compile(objs, compile.Options{PublishSeq: 1, Now: c.now().UTC(), Lease: c.lease(),
		Entitlements: c.entitlements(doc), Unpriced: func(p compile.Problem) { unpriced = append(unpriced, p) }})
	return unpriced, err
}

// decodeDir decodes the config directory.
func (c *Controller) decodeDir() (v1.Objects, error) {
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return v1.Objects{}, fmt.Errorf("config directory: %w", err)
	}
	defer root.Close()
	return v1.DecodeFS(root.FS())
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
	var last string // the last error logged: each is logged once until it changes
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		switch err := c.tick(ctx); {
		case err == nil:
			last = ""
		case err.Error() != last:
			c.log().Error("golem-control", "err", err)
			last = err.Error()
		}
	}
}

// tick is one second's work: a publication if due, then, while this
// replica leads, the audit outbox shipped and the group sync if due.
func (c *Controller) tick(ctx context.Context) error {
	_, err := c.Step(ctx)
	if c.Audit != nil && c.Elector.Epoch() != 0 {
		if _, aerr := c.Audit.Step(ctx); aerr != nil {
			err = errors.Join(err, fmt.Errorf("audit: %w", aerr))
		} else {
			c.mu.Lock()
			c.status.shipped = c.now()
			c.mu.Unlock()
		}
	}
	if serr := c.syncStep(ctx); serr != nil {
		err = errors.Join(err, fmt.Errorf("entra sync: %w", serr))
	}
	return err
}

// syncStep runs the group sync if this replica leads and it is due. A
// failed step is retried at the next interval, not every second.
func (c *Controller) syncStep(ctx context.Context) error {
	fence, leads := c.Elector.Fence()
	if c.Sync == nil || !leads || c.now().Before(c.synced.Add(cmp.Or(c.SyncEvery, 5*time.Minute))) {
		return nil
	}
	c.synced = c.now()
	res, err := c.Sync.Step(ctx, fence)
	c.mu.Lock()
	switch {
	case err == nil:
		c.status.synced = c.now()
	case errors.Is(err, entrasync.ErrTooManyRemovals):
		c.status.guardTrips++
	}
	c.mu.Unlock()
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
