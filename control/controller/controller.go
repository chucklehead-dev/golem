// Package controller is golem-control's publishing loop (control-plane.md
// §4, §6): while this replica leads, it compiles the config and publishes
// every pool's snapshot when the config changes, when it claims the fence,
// and every renewal interval to extend the snapshots' authorization lease
// (ADR 0030).
//
// This slice reads config CRDs from a directory and gives every pool the
// same snapshot, its entitlements the state document's (principals,
// scopes, keys and revocations), each scope with its ScopePolicy's policy
// from the config (ADR 0050); a change to either is published at the next
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
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/control/audit"
	"github.com/chucklehead-dev/golem/control/entrasync"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/control/state"
	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/compile"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Controller publishes the config in Dir to every pool in Publishers.
//
// It is golem-control's one replica (ADR 0053): it claims the state
// document's fence once, at start (Claim, state.Store.Claim), and leads
// from then on; every leader-only write it makes is fenced by that claim.
// A replica claimed over (a predecessor paused or not yet gone when its
// successor started) is refused at its next fenced write, stops leading,
// and Run returns ErrDeposed.
type Controller struct {
	Dir string // config CRD YAML
	// Checkout, if set, is the git checkout Dir is in (git-sync's, ADR
	// 0054): its commit is published as each snapshot's
	// Source.ConfigRevision, and its age bounds the renewals.
	Checkout *Checkout
	// MaxConfigAge, with Checkout's Fetched, is how old the checkout may
	// be (since its last fetch) before leases stop being renewed: a
	// stalled git-sync must not have its last checkout renewed for ever,
	// as it may miss a restriction merged since. Past it no pool is
	// renewed; a change of the entitlements or the epoch is still
	// published, with the lease the pool's last publication gave (never
	// later), so a revocation still reaches the gateways and their leases
	// run out on time. 0: no bound.
	MaxConfigAge time.Duration
	// State is the document; ID this replica's name, the fence's holder.
	State      *state.Store
	ID         string
	Publishers map[string]*publish.Publisher // by pool; they share State
	// Lease is the published snapshots' authorization (ADR 0030, 0032: four
	// hours for internal pools); Renew how often they are republished to
	// extend it (default a minute).
	Lease, Renew time.Duration
	// PeriodZone is the budget periods' time zone (an IANA name), stated
	// in every snapshot so a gateway of another zone refuses it
	// (snapshot.PeriodZone); "": none stated.
	PeriodZone string
	// Retry is how soon a pool whose publication failed is tried again
	// (default two seconds), doubling at each further failure up to Renew,
	// jittered; a change to publish (the config, the entitlements, the
	// epoch) tries it at once.
	Retry time.Duration
	Now   func() time.Time
	Log   *slog.Logger
	// Audit, if set, ships the state's audit outbox while this replica
	// leads (control/audit).
	Audit *audit.Shipper
	// Spend, if set, is the known-spend job: each publication carries every
	// scope's spend this period from the ledger, to a boundary (ADR
	// 0048; withKnownSpend). Run reads it every SpendEvery, apart from
	// the publications, which take its latest result.
	Spend *spend.Job
	// SpendEvery is how often known spend is read (default 30 seconds),
	// on its own goroutine: a publication takes its latest result.
	SpendEvery time.Duration
	// Sync, if set, reconciles the bound Entra groups' members while this
	// replica leads, every SyncEvery (default five minutes; ADR 0047).
	Sync      *entrasync.Sync
	SyncEvery time.Duration

	syncTried time.Time // when the group sync last ran, succeeding or not (status.synced: last succeeded)
	mu        sync.Mutex
	// fence is this replica's claim (Claim), claimed whether it was made;
	// deposed is set once the document's fence is another's, for good.
	// Under mu.
	fence   state.Fence
	claimed bool
	deposed bool
	good    v1.Objects // the last config that compiled; written under mu (ScopeModels reads it from the API's goroutines)
	goodSum [32]byte
	goodRev snapshot.Revision // the checkout's commit good was read at (under mu)
	// unmatched is what the last publication found of scopes and
	// ScopePolicies that do not match (under mu, by
	// compile.Options.Unmatched): a scope of the state's with no policy,
	// published able to call nothing, and a policy of no scope.
	unmatched []compile.Problem
	// renewing is each pool's publication as found in the bucket, renewed
	// (compile.Renew) while no config has compiled since this replica
	// started; nil once one has.
	renewing map[string]*snapshot.Snapshot
	// pools is each pool's publishing state: what it last published and
	// when, and its failed attempts since, so each pool is due on its own
	// and a failing one is retried with backoff while the others publish at
	// their own cadence. Only the loop touches it.
	pools map[string]*poolState
	// status is what the loop has done, for its metrics (control/metrics),
	// under mu: written by the loop, read at a collection.
	status status
}

// content is what a publication carries, as far as its being due goes:
// the good config, the state's entitlements and the epoch it is made under.
type content struct {
	sum   [32]byte // the good config's
	ents  [32]byte // the state's entitlements
	epoch uint64
}

// poolState is one pool's publishing state.
type poolState struct {
	published content   // its last successful publication's
	at        time.Time // when that was, by the controller's clock
	attempted content   // what its last failed attempt tried to publish
	failures  int       // failed attempts in a row since it last published
	retryAt   time.Time // not tried again before this unless the content changes
	err       error     // the last failed attempt's error
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
	// ReasonUnmatched: the publication found a scope of the state's with no
	// ScopePolicy (published able to call nothing), or a ScopePolicy of no
	// scope (ErrUnmatched).
	ReasonUnmatched = "scope_policy_unmatched"
	// ReasonConfigRenewing: the config directory does not compile, or an
	// edit was refused: the last good config (or the bucket's) is renewed
	// instead, or nothing can be.
	ReasonConfigRenewing = "config_renewing"
	// ReasonConfigStale: the config checkout is older than MaxConfigAge,
	// and publishing would have extended the pool's lease (ErrStale).
	ReasonConfigStale = "config_stale"
	// ReasonOther: anything else (the bucket refusing a write, the signer
	// failing, a build error).
	ReasonOther = "other"
)

// Reasons are every Reason, in a fixed order.
var Reasons = []string{ReasonTampered, ReasonRegressed, ReasonFenced, ReasonConflict, ReasonUnmatched, ReasonConfigRenewing, ReasonConfigStale, ReasonOther} // read-only

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
	case errors.Is(err, ErrUnmatched):
		return ReasonUnmatched
	case errors.Is(err, ErrStale):
		return ReasonConfigStale
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

// degraded counts, against the pools attempted, what made a publication
// attempt less than the directory's config with every scope its policy: a
// config not applied (cfgErr), and scopes and policies unmatched.
func (c *Controller) degraded(cfgErr error, pools ...string) {
	if cfgErr != nil {
		c.count(ReasonConfigRenewing, pools...)
	}
	if len(c.Unmatched()) > 0 {
		c.count(ReasonUnmatched, pools...)
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

// The deadlines of the loop's external calls: a bucket, ClickHouse or
// Graph that accepts a request and never answers must not hold the loop.
// The state's own reads and writes are bounded by its Timeout.
const (
	publishTimeout = time.Minute      // every due pool's publication: the records' write, each pool's objects, the signer
	spendTimeout   = 30 * time.Second // known spend's ClickHouse queries
	auditTimeout   = time.Minute      // one batch of the outbox archived, emitted and trimmed
	syncTimeout    = 2 * time.Minute  // the group sync's Graph reads and its write
)

// bounded runs f under a deadline of d.
func bounded(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
}

// ErrDeposed: another replica has claimed the fence since this one did. It
// leads no more, for good, and Run returns.
var ErrDeposed = errors.New("controller: another golem-control replica has claimed the fence since this one did: this one leads no more, and stops")

// Claim claims the state document's fence for this replica, at one above
// its epoch (state.Store.Claim): it leads from then on, until another
// claims over it. Run claims first unless Claim has been called.
func (c *Controller) Claim(ctx context.Context) error {
	f, err := c.State.Claim(ctx, c.ID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.fence, c.claimed, c.deposed = f, true, false
	c.mu.Unlock()
	c.log().Info("golem-control: claimed the fence", "replica", c.ID, "epoch", f.Epoch)
	return nil
}

// Fence is the leadership this replica acts under, for its leader-only
// writes; ok is false before it has claimed and once it is deposed.
func (c *Controller) Fence() (f state.Fence, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fence, c.claimed && !c.deposed
}

// notice marks this replica deposed if the document, as last read, holds
// another's fence, and reports whether it still leads.
func (c *Controller) notice() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed && !c.deposed && !c.State.Leads(c.fence) {
		c.deposed = true
	}
	return c.claimed && !c.deposed
}

// Step publishes if this replica leads and a publication is due: a config
// change, a new epoch, or the renewal interval. A config that does not
// compile is reported, and the last one that did is renewed, so a broken
// edit cannot let every gateway's authorization lapse. A scope of the
// state's with no ScopePolicy is published able to call nothing, and a
// ScopePolicy of no scope is left out, both reported (ErrUnmatched) every
// step until they match; the other scopes and pools are published as
// usual.
//
// Each pool is due on its own: a pool that fails (a tampered checkpoint, a
// regressed record) is retried with backoff (Retry, doubling up to Renew)
// while the others publish at their own cadence, and its last failure is
// in the error of every step until it publishes. It reports whether it
// published to any pool. A replica that finds itself deposed publishes
// nothing and returns ErrDeposed.
func (c *Controller) Step(ctx context.Context) (bool, error) {
	fence, leads := c.Fence()
	if !leads {
		return false, nil
	}
	// The state as of now: a revocation is published within a step of its
	// write (the store's own loop refreshes it too). The config is
	// trial-compiled against it.
	if err := c.State.Refresh(ctx); err != nil {
		c.count(FailureReason(err))
		return false, err
	}
	if !c.notice() {
		c.count(ReasonFenced)
		return false, ErrDeposed
	}
	doc := c.State.Current()
	cfgErr := c.load(doc)
	stale := c.staleErr()
	if !c.compiled() {
		// No config has compiled since this replica started: renew what is
		// published (it compiled when it was), so a restart over a broken
		// edit cannot let every gateway's authorization lapse either.
		if err := bounded(ctx, publishTimeout, c.loadPublished); err != nil {
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
	cur, now := content{c.goodSum, ents, fence.Epoch}, c.now()
	var due []string
	var waiting []error // pools backing off, not tried this step: their last failure
	for _, pool := range slices.Sorted(maps.Keys(c.Publishers)) {
		switch ps := c.pool(pool); {
		case c.due(ps, cur, now, stale != nil):
			due = append(due, pool)
		case ps.failures > 0:
			waiting = append(waiting, fmt.Errorf("pool %s: %w", pool, ps.err))
		}
	}
	if len(due) == 0 {
		return false, errors.Join(append(waiting, stale, cfgErr, c.unmatchedErr())...)
	}
	pctx, cancel := context.WithTimeout(ctx, publishTimeout)
	perrs := c.publish(pctx, fence, doc, due, stale != nil) // counts each pool's failure
	cancel()
	c.degraded(cfgErr, due...)
	var errs []error
	published := false
	for _, pool := range due {
		ps := c.pool(pool)
		err := perrs[pool]
		if err == nil {
			*ps = poolState{published: cur, at: now}
			published = true
			continue
		}
		ps.failures++
		ps.attempted, ps.err = cur, err
		ps.retryAt = now.Add(c.backoff(ps.failures))
		errs = append(errs, fmt.Errorf("pool %s: %w", pool, err))
	}
	return published, errors.Join(append(append(errs, waiting...), stale, cfgErr, c.unmatchedErr())...)
}

// pool is pool's publishing state, created on first use.
func (c *Controller) pool(pool string) *poolState {
	if c.pools == nil {
		c.pools = map[string]*poolState{}
	}
	ps, ok := c.pools[pool]
	if !ok {
		ps = &poolState{}
		c.pools[pool] = ps
	}
	return ps
}

// renew is the renewal interval: a minute unless set.
func (c *Controller) renew() time.Duration { return cmp.Or(c.Renew, time.Minute) }

// compiled reports whether a config has compiled since this replica
// started (good holds it).
func (c *Controller) compiled() bool { return c.goodSum != [32]byte{} }

// backoff is how long a pool waits after its nth failed attempt in a row:
// Retry (two seconds unless set), doubled at each failure after the first,
// at most the renewal interval, less up to a quarter of jitter. The jitter
// keeps replicas' retries out of step; taking it off, never adding it,
// keeps a failing pool tried at least once a renewal interval, which the
// failure alerts rely on (they count attempts).
func (c *Controller) backoff(n int) time.Duration {
	d, renew := cmp.Or(c.Retry, 2*time.Second), c.renew()
	for i := 1; i < n && d < renew; i++ {
		d *= 2
	}
	d = min(d, renew)
	return d - time.Duration(rand.Int64N(int64(d)/4+1))
}

// ErrUnmatched: the state's scopes and the config's ScopePolicies do not
// match. A scope with no policy is published able to call nothing (it fails
// closed), a policy of no scope is not published, and every other scope and
// pool is published as usual: the two change apart (an admin creates a
// scope, a pull request adds its policy), so neither order stops a
// publication.
var ErrUnmatched = errors.New("scopes and ScopePolicies do not match")

// unmatchedErr is ErrUnmatched naming each mismatch the last publication
// found, and what to do; nil if it found none.
func (c *Controller) unmatchedErr() error {
	ps := c.Unmatched()
	if len(ps) == 0 {
		return nil
	}
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return fmt.Errorf("%w; add a ScopePolicy named after each scope to the config (a pull request), or create the scope a policy names (golemctl scopes create):\n  %s",
		ErrUnmatched, strings.Join(lines, "\n  "))
}

// Unmatched is what the last publication found of the state's scopes and
// the config's ScopePolicies that do not match: a scope with no policy,
// and a policy of no scope. The operator's status; Step reports it too.
func (c *Controller) Unmatched() []compile.Problem {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.unmatched)
}

// ScopeModels is the route aliases scope's ScopePolicy allows, for the
// control API's check that a key only narrows its scope
// (service.Service.ScopeModels): from the last good config, or, on a
// replica where none has compiled (one that does not lead), the
// directory's. A scope with no ScopePolicy allows none. ok is false with
// neither, or a config that does not compile: the API checks nothing, and
// the gateway still intersects a key's models with its scope's.
func (c *Controller) ScopeModels(scope string) (models []string, ok bool) {
	c.mu.Lock()
	objs, good := c.good, c.compiled()
	c.mu.Unlock()
	if !good {
		var err error
		if objs, err = c.decodeDir(); err != nil {
			return nil, false
		}
		if _, err := compile.Compile(objs, compile.Options{PublishSeq: 1, Now: c.now().UTC(), Lease: c.lease()}); err != nil {
			return nil, false
		}
	}
	for _, p := range objs.Policies {
		if p.Object.Metadata.Name == scope {
			return slices.Clone(p.Object.Spec.Models), true
		}
	}
	return nil, true
}

// due reports whether a pool's publication is due at now with content
// cur. A pool whose last attempt failed is due once its backoff has
// passed, or at once if cur differs from what that attempt tried to
// publish: a revocation is not held back by a retry's wait. Otherwise it
// is due when cur differs from its last publication's (the config, the
// entitlements, the epoch), or its renewal interval has passed.
//
// While the config checkout is stale nothing is due for its renewal alone:
// only a change (a revocation, a new epoch) is, published without
// extending the lease (publish).
func (c *Controller) due(ps *poolState, cur content, now time.Time, stale bool) bool {
	if stale && cur == ps.published {
		return false
	}
	if ps.failures > 0 {
		return cur != ps.attempted || !now.Before(ps.retryAt)
	}
	return cur != ps.published || !now.Before(ps.at.Add(c.renew()))
}

// publish publishes the good config with doc's entitlements to pools (in
// sorted order), all their records in one state write (ADR 0049), under
// fence and from doc's version. It returns each failed pool's error. With
// stale (the config checkout past MaxConfigAge) each pool's lease ends
// when its last publication's did, never later; a pool this replica has
// not published, or whose lease has ended, is not published (ErrStale).
func (c *Controller) publish(ctx context.Context, fence state.Fence, doc *state.Document, pools []string, stale bool) map[string]error {
	var pubs []*publish.Publisher
	for _, pool := range pools {
		pubs = append(pubs, c.Publishers[pool])
	}
	built, unmatched := false, map[string]compile.Problem{} // every pool's build finds the same
	validUntil := map[string]time.Time{}                    // each pool's lease, as built
	ptrs, perrs := publish.PublishAll(ctx, c.State, fence, doc.StateVersion, pubs,
		func(p *publish.Publisher, seq uint64) (*snapshot.Snapshot, error) {
			c.mu.Lock()
			rev, last := c.goodRev, c.status.pools[p.Pool].ValidUntil
			c.mu.Unlock()
			now, lease := c.now().UTC(), c.lease()
			if stale {
				if lease = min(lease, last.Sub(now)); lease <= 0 {
					return nil, fmt.Errorf("%w: pool %s's lease would be extended, and it has none left from this replica to keep (its last ended %s)", ErrStale, p.Pool, last.UTC().Format(time.RFC3339))
				}
			}
			o := compile.Options{PublishSeq: seq, Now: now, Lease: lease, PeriodZone: c.PeriodZone, ConfigRevision: rev,
				Entitlements: c.entitlements(doc), Unmatched: func(q compile.Problem) { unmatched[q.String()] = q }}
			var s *snapshot.Snapshot
			var err error
			switch prev, renewing := c.renewing[p.Pool]; {
			case c.compiled():
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
		for _, k := range slices.Sorted(maps.Keys(unmatched)) {
			ps = append(ps, unmatched[k])
		}
		c.mu.Lock()
		c.unmatched = ps
		c.mu.Unlock()
	}
	for _, pool := range pools {
		if err := perrs[pool]; err != nil {
			c.count(FailureReason(err), pool)
			continue
		}
		ptr := ptrs[pool]
		c.mu.Lock()
		if c.status.pools == nil {
			c.status.pools = map[string]Publication{}
		}
		c.status.pools[pool] = Publication{At: c.now(), Seq: ptr.PublishSeq, ValidUntil: validUntil[pool]}
		c.mu.Unlock()
		c.mu.Lock()
		rev := c.goodRev
		c.mu.Unlock()
		c.log().Info("published", "pool", pool, "seq", ptr.PublishSeq, "epoch", fence.Epoch, "hash", ptr.Hash[:12], "configRevision", rev)
	}
	return perrs
}

// entitlements is the snapshot's entitlements for the routes that exist
// (aliases): doc's, each scope with this period's known spend if the
// known-spend job has a result (accounting.md).
func (c *Controller) entitlements(doc *state.Document) func(aliases []string) snapshot.Entitlements {
	return func(aliases []string) snapshot.Entitlements {
		e := doc.Entitlements(aliases)
		if c.Spend != nil {
			if k, ok := c.Spend.Current(); ok {
				withKnownSpend(&e, k)
			}
		}
		return e
	}
}

// withKnownSpend gives e's scopes the period's known spend.
func withKnownSpend(e *snapshot.Entitlements, k spend.Known) {
	for i := range e.Scopes {
		sc := &e.Scopes[i]
		sc.Period, sc.Since, sc.KnownSpend = k.Period, k.Since, k.Scopes[sc.ID]
	}
}

// load reads the directory, keeping it as the good config if it compiles
// with doc's entitlements, as it will be published; one that does not (a
// ScopePolicy's budget over an unpriced model among its problems) is
// refused, and the last good config is renewed. A mismatch of the state's
// scopes and the ScopePolicies is not a reason to refuse it: it is
// published, and reported (compile.Options.Unmatched). An unchanged
// directory costs a content hash.
func (c *Controller) load(doc *state.Document) error {
	dir, rev, err := c.Checkout.resolve(c.Dir)
	if err != nil {
		return fmt.Errorf("config directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
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
		// The same config at another commit (a change elsewhere in the
		// repository): the next publication names it.
		c.mu.Lock()
		c.goodRev = rev
		c.mu.Unlock()
		return nil
	}
	objs, err := v1.DecodeFS(fsys)
	if err != nil {
		return fmt.Errorf("config not applied, the last good one is published: %w", err)
	}
	if _, err := compile.Compile(objs, compile.Options{PublishSeq: 1, Now: c.now().UTC(), Lease: c.lease(),
		Entitlements: c.entitlements(doc)}); err != nil {
		return fmt.Errorf("config not applied, the last good one is published: %w", err)
	}
	c.mu.Lock()
	prev := c.good
	c.mu.Unlock()
	if gone := policiesRemoved(prev, objs, doc); len(gone) > 0 {
		return fmt.Errorf("config not applied, the last good one is published: the ScopePolicy of live scope %s removed: its members would be refused everything; restore it (to stop a scope, set its models to [] in its ScopePolicy)",
			strings.Join(gone, ", "))
	}
	c.mu.Lock()
	c.good, c.goodSum, c.goodRev = objs, sum, rev
	c.mu.Unlock()
	c.log().Info("golem-control: config applied", "configRevision", rev)
	return nil
}

// policiesRemoved lists the scopes of doc's whose ScopePolicy prev had and
// next has not (ADR 0050): such an edit is refused, as a live scope with
// no policy can call nothing. Against this replica's last good config
// only: one that starts over a directory already without it publishes the
// scope with no models, reported as unmatched.
func policiesRemoved(prev, next v1.Objects, doc *state.Document) []string {
	had := map[string]bool{}
	for _, p := range prev.Policies {
		had[p.Object.Metadata.Name] = true
	}
	for _, p := range next.Policies {
		delete(had, p.Object.Metadata.Name)
	}
	var gone []string
	for id := range had {
		if _, live := doc.Scopes[id]; live {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	return gone
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

// decodeDir decodes the config directory.
func (c *Controller) decodeDir() (v1.Objects, error) {
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return v1.Objects{}, fmt.Errorf("config directory: %w", err)
	}
	defer root.Close()
	return v1.DecodeFS(root.FS())
}

// Run claims the fence (unless Claim has been called) and publishes when
// due, every second, until ctx ends (nil) or another replica claims the
// fence over this one (ErrDeposed: its process should exit, as a
// predecessor it has no work left).
//
// Known spend, the audit outbox and the group sync run on goroutines of
// their own, each at its own cadence and under its own deadline, stopped
// before Run returns: a publication (a revocation) never waits behind
// ClickHouse, the archive or Graph. Known spend is read once before the
// first publication, so a restart does not publish a snapshot without it.
func (c *Controller) Run(ctx context.Context) error {
	if _, leads := c.Fence(); !leads {
		if err := c.Claim(ctx); err != nil {
			return err
		}
	}
	jobs, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stop()
	if c.Spend != nil {
		c.report("known spend", c.spendStep(jobs), new(string))
		wg.Go(func() { c.every(jobs, cmp.Or(c.SpendEvery, 30*time.Second), "known spend", c.spendStep) })
	}
	if c.Audit != nil {
		wg.Go(func() { c.every(jobs, time.Second, "audit", c.auditStep) })
	}
	if c.Sync != nil {
		wg.Go(func() { c.every(jobs, time.Second, "entra sync", c.syncStep) })
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var last string // the last error logged: each is logged once until it changes
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		_, err := c.Step(ctx)
		if errors.Is(err, ErrDeposed) {
			return err
		}
		c.report("publication", err, &last)
	}
}

// every runs step at every interval until ctx ends, reporting its failure
// once until it changes.
func (c *Controller) every(ctx context.Context, interval time.Duration, what string, step func(context.Context) error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.report(what, step(ctx), &last)
	}
}

// report logs err unless it is what was last logged (*last).
func (c *Controller) report(what string, err error, last *string) {
	switch {
	case err == nil:
		*last = ""
	case err.Error() != *last:
		c.log().Error("golem-control: "+what, "err", err)
		*last = err.Error()
	}
}

// spendStep reads known spend, under its deadline: publications carry its
// latest result (Spend.Current), which a failure keeps.
func (c *Controller) spendStep(ctx context.Context) error {
	return bounded(ctx, spendTimeout, c.Spend.Step)
}

// auditStep ships a batch of the audit outbox while this replica leads.
func (c *Controller) auditStep(ctx context.Context) error {
	fence, leads := c.Fence()
	if c.Audit == nil || !leads {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, auditTimeout)
	_, err := c.Audit.Step(actx, fence)
	cancel()
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	c.mu.Lock()
	c.status.shipped = c.now()
	c.mu.Unlock()
	return nil
}

// syncStep runs the group sync if this replica leads and it is due. A
// failed step is retried at the next interval, not every second.
func (c *Controller) syncStep(ctx context.Context) error {
	fence, leads := c.Fence()
	if c.Sync == nil || !leads || c.now().Before(c.syncTried.Add(cmp.Or(c.SyncEvery, 5*time.Minute))) {
		return nil
	}
	c.syncTried = c.now()
	sctx, cancel := context.WithTimeout(ctx, syncTimeout)
	res, err := c.Sync.Step(sctx, fence)
	cancel()
	c.mu.Lock()
	switch {
	case err == nil:
		c.status.synced = c.now()
	case errors.Is(err, entrasync.ErrTooManyRemovals):
		c.status.guardTrips++
	}
	c.mu.Unlock()
	if res != (entrasync.Result{}) { // a step its guard refused may still have offboarded
		c.log().Info("entra sync", "created", res.Created, "added", res.Added, "removed", res.Removed, "keysRevoked", res.Revoked, "offboarded", res.Offboarded)
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
