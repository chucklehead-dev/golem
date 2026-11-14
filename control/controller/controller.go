// Package controller is golem-control's publishing loop (control-plane.md
// §4, §6): while this replica leads, it compiles the config and publishes
// every pool's snapshot when the config changes, when it becomes leader,
// and every renewal interval to extend the snapshots' authorization lease
// (ADR 0030).
//
// This slice reads config CRDs from a directory and gives every pool the
// same snapshot. Its entitlements are empty until the state document
// carries them (a published snapshot authorizes no one: fail closed);
// pool selectors and the CRD watch come later.
package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
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

	good      v1.Objects // the last config that compiled
	goodSum   [32]byte
	published struct {
		sum   [32]byte
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
	renew := cmp.Or(c.Renew, time.Minute)
	due := c.goodSum != c.published.sum || epoch != c.published.epoch || !c.now().Before(c.published.at.Add(renew))
	if !due {
		return false, cfgErr
	}
	source := c.Elector.State.Current().StateVersion
	var errs []error
	for _, pool := range slices.Sorted(maps.Keys(c.Publishers)) {
		ptr, err := c.Publishers[pool].Publish(ctx, epoch, source, func(seq uint64) (*snapshot.Snapshot, error) {
			return compile.Compile(c.good, compile.Options{PublishSeq: seq, Now: c.now().UTC(), Lease: cmp.Or(c.Lease, 4*time.Hour)})
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("pool %s: %w", pool, err))
			continue
		}
		c.log().Info("published", "pool", pool, "seq", ptr.PublishSeq, "epoch", epoch, "hash", ptr.Hash[:12])
	}
	if len(errs) > 0 {
		return false, errors.Join(append(errs, cfgErr)...)
	}
	c.published.sum, c.published.epoch, c.published.at = c.goodSum, epoch, c.now()
	return true, cfgErr
}

// load reads the directory, keeping it as the good config if it compiles.
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
	if _, err := compile.Compile(objs, compile.Options{PublishSeq: 1, Now: c.now().UTC(), Lease: cmp.Or(c.Lease, 4*time.Hour)}); err != nil {
		return fmt.Errorf("config not applied, the last good one is published: %w", err)
	}
	c.good, c.goodSum = objs, sum
	return nil
}

// Run tries for the lease and publishes when due, every second, until
// ctx ends.
func (c *Controller) Run(ctx context.Context) {
	go c.Elector.Run(ctx) //qenv:ok the elector's loop, ended by ctx
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
		if msg := fmt.Sprint(err); err != nil && msg != last {
			c.log().Error("golem-control", "err", err)
			last = msg
		} else if err == nil {
			last = ""
		}
	}
}
