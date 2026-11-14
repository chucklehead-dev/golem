// Package state is golem-control's store (control-plane.md §3, ADR 0006):
// one document in the bucket, read from memory and refreshed by a
// conditional get, written only by Update, which applies a change to a
// copy, checks the store's invariants, increments state_version and writes
// it with If-Match, re-reading and retrying when another writer won.
//
// It holds golem-control's leadership fence (control/lead keeps the lease
// itself in an object of its own), each pool's publication record
// (control/publish numbers and fences by it), the entitlements
// (entitlements.go, restored by restore.go) and the audit outbox
// (control/audit ships it). The rest of the outline in §3.1 joins it as it
// is built. The encoding is JSON, decoded strictly; the design's protobuf
// and zstd replace it behind Encode and Decode.
package state

import (
	"context"
	"crypto/rand"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// Schema is the document's format; a reader refuses any other.
const Schema = 1

// DocumentKey is the document's object key.
const DocumentKey = "state/document.json"

// Record is a pool's publication record: the last publication's sequence,
// and the leadership epoch and state version it was made under
// (model/publication.md's PubRecord).
type Record struct {
	Epoch  uint64 `json:"epoch"`
	Seq    uint64 `json:"seq"`
	Source uint64 `json:"source"`
}

// Fence is a leadership: the replica that leads, and at which epoch. The
// epoch increases whenever leadership changes hands (control/lead).
type Fence struct {
	Holder string `json:"holder"`
	Epoch  uint64 `json:"epoch"`
}

// Document is the state.
type Document struct {
	Schema       int    `json:"schema"`
	StateVersion uint64 `json:"stateVersion"`
	// Fence is golem-control's leadership as the document knows it: who
	// leads, at which epoch (ADR 0049). The lease itself is an object of
	// its own (control/lead); a replica leads once it has claimed the fence
	// here, and every leader-only write checks it (FencedUpdate).
	Fence *Fence `json:"fence,omitempty"`
	// Write names the update that wrote this version, so a writer whose
	// compare-and-swap reply was lost can tell its own write from
	// another's (Update).
	Write        string            `json:"write,omitempty"`
	Publications map[string]Record `json:"publications,omitempty"`

	Principals  map[string]Principal `json:"principals,omitempty"` // by principal ID
	Scopes      map[string]Scope     `json:"scopes,omitempty"`     // by scope ID
	Keys        map[string]Key       `json:"keys,omitempty"`       // by hex SHA-256 of the key
	Revocations []Revocation         `json:"revocations,omitempty"`
	Roles       []RoleBinding        `json:"roles,omitempty"`
	// Audit is the outbox (§3): a write's events go in with it; the leader
	// ships them and trims only what is durably archived.
	Audit    []AuditEvent `json:"audit,omitempty"`
	AuditSeq uint64       `json:"auditSeq,omitempty"` // the last event's number
}

// number gives each new audit event (Seq 0) the next number.
func (d *Document) number() {
	for i := range d.Audit {
		if d.Audit[i].Seq == 0 {
			d.AuditSeq++
			d.Audit[i].Seq = d.AuditSeq
		}
	}
}

func (d *Document) clone() *Document {
	c := *d
	c.Publications = maps.Clone(d.Publications)
	c.Principals = maps.Clone(d.Principals) // values are copied; their slices are replaced, never edited in place
	c.Scopes = maps.Clone(d.Scopes)
	c.Keys = maps.Clone(d.Keys)
	c.Revocations = slices.Clone(d.Revocations)
	c.Roles = slices.Clone(d.Roles)
	c.Audit = slices.Clone(d.Audit)
	if d.Fence != nil {
		f := *d.Fence
		c.Fence = &f
	}
	return &c
}

// Errors.
var (
	// ErrInvariant: a change the store refuses to write.
	ErrInvariant = errors.New("state: the change breaks an invariant")
	// ErrContended: other writers won every attempt.
	ErrContended = errors.New("state: the document changed under every attempt; retry")
	// ErrRegressed: the bucket holds an older version than this store has
	// seen, or none: an out-of-band restore or a deletion. The store adopts
	// neither (adopting would drop every later revocation, the naive
	// restore model/publication.md shows unsafe) nor writes over it.
	ErrRegressed = errors.New("state: the document in the bucket went back")
	// ErrFenced: the document's leadership is not the writer's (it has lost
	// the lease, or never held it at that epoch). Nothing was written.
	ErrFenced = errors.New("state: fenced: this replica does not lead at that epoch")
	// ErrMissing: there is no document, but the bucket shows golem-control
	// has run on it (one of Store.Installed exists): the document was
	// deleted. A store adopts no empty document then; only Bootstrap
	// creates one.
	ErrMissing = errors.New("state: the document is missing from a bucket golem-control has published from")
)

// Store is the document in a bucket. The zero Store is not ready: Load it.
type Store struct {
	Bucket bucket.Bucket
	// Attempts bounds an Update's retries after losing to another writer
	// (default 8).
	Attempts int
	// Installed are objects whose presence shows golem-control has run on
	// this bucket (the lease object, the pools' checkpoints and pointers):
	// a first read that finds no document but one of these is ErrMissing,
	// not the empty document. Without them a missing document is a first
	// start.
	Installed []string

	mu  sync.Mutex
	cur *Document
	tag string // the document's entity tag, or bucket.Absent before the first write
}

// Load reads the document; none yet is the empty document at version 0,
// unless the bucket shows golem-control has run on it (Installed), which
// is ErrMissing.
func (s *Store) Load(ctx context.Context) error { return s.refresh(ctx, true) }

// Refresh re-reads the document if it changed (one conditional get).
func (s *Store) Refresh(ctx context.Context) error { return s.refresh(ctx, false) }

func (s *Store) refresh(ctx context.Context, full bool) error {
	s.mu.Lock()
	tag := s.tag
	s.mu.Unlock()
	if full || tag == bucket.Absent {
		tag = ""
	}
	body, newTag, err := s.Bucket.Get(ctx, DocumentKey, tag)
	switch {
	case errors.Is(err, bucket.ErrNotModified):
		return nil
	case errors.Is(err, bucket.ErrNotFound):
		had, ok := s.version()
		if ok && had > 0 {
			return regressed(had, "none (deleted)")
		}
		// A store that has read nothing yet cannot tell a first start from a
		// deleted document by memory: the bucket's other objects tell it.
		if !ok {
			evidence, err := s.installed(ctx)
			if err != nil {
				return err
			}
			if evidence != "" {
				return missing(evidence)
			}
		}
		s.set(&Document{Schema: Schema}, bucket.Absent)
		return nil
	case err != nil:
		return fmt.Errorf("state %s: %w", DocumentKey, err)
	}
	d, err := Decode(body)
	if err != nil {
		return err
	}
	if had, ok := s.version(); ok && d.StateVersion < had {
		return regressed(had, fmt.Sprintf("version %d", d.StateVersion))
	}
	s.set(d, newTag)
	return nil
}

// regressed is ErrRegressed, saying what to do.
func regressed(had uint64, now string) error {
	return fmt.Errorf("%w: from version %d to %s. A restore goes through golem-control restore, which keeps every later revocation and writes above the current version; restore that way, or put the newer version back (bucket versioning). Until then golem-control serves what it last read and writes nothing", ErrRegressed, had, now)
}

// missing is ErrMissing, naming the object that shows golem-control ran
// here, and what to do.
func missing(evidence string) error {
	return fmt.Errorf("%w: there is no %s, but %s exists. Starting from an empty document would publish snapshots with no principals or keys (every caller refused). Put the newest version of %s back from the bucket's versions (or its Object Lock replica) and start again; or, to start over with no entitlements on purpose, run golem-control bootstrap", ErrMissing, DocumentKey, evidence, DocumentKey)
}

// installed is the first of Installed that exists in the bucket, or "".
func (s *Store) installed(ctx context.Context) (string, error) {
	for _, key := range s.Installed {
		switch _, _, err := s.Bucket.Get(ctx, key, ""); {
		case err == nil:
			return key, nil
		case !errors.Is(err, bucket.ErrNotFound):
			return "", fmt.Errorf("state: checking for %s, to tell a first start from a deleted document: %w", key, err)
		}
	}
	return "", nil
}

// version is the state version the store last read or wrote; ok is false
// before it has read or written any.
func (s *Store) version() (v uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return 0, false
	}
	return s.cur.StateVersion, true
}

func (s *Store) set(d *Document, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A read never moves the document back: a slow get that returns an
	// older version than one this store wrote is ignored.
	if s.cur != nil && d.StateVersion < s.cur.StateVersion {
		return
	}
	s.cur, s.tag = d, tag
}

// Current is a copy of the document as last read or written.
func (s *Store) Current() *Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return &Document{Schema: Schema}
	}
	return s.cur.clone()
}

// OutboxLen is the number of audit events in the document as last read or
// written, waiting to be shipped (control/audit): for a metric, without
// copying the document.
func (s *Store) OutboxLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return 0
	}
	return len(s.cur.Audit)
}

// Update applies change to a copy of the document and writes it if it
// keeps the invariants, retrying from a fresh read when another writer
// won. change may run more than once; an error from it ends the update
// and is returned as it is. It returns the document written.
func (s *Store) Update(ctx context.Context, change func(*Document) error) (*Document, error) {
	if err := s.loaded(ctx); err != nil {
		return nil, err
	}
	attempts := s.Attempts
	if attempts <= 0 {
		attempts = 8
	}
	for range attempts {
		s.mu.Lock()
		base, tag := s.cur, s.tag
		s.mu.Unlock()
		next, err := apply(base, change)
		if err != nil {
			return nil, err
		}
		next.Schema, next.StateVersion = Schema, base.StateVersion+1
		next.Write = rand.Text()
		body, err := next.Encode()
		if err != nil {
			return nil, err
		}
		newTag, err := s.Bucket.Put(ctx, DocumentKey, body, tag)
		if errors.Is(err, bucket.ErrPrecondition) {
			// Another writer won, or this one did and its reply was lost (a
			// retried conditional put fails its own condition): read what
			// is there, and if it is this write, it succeeded.
			if err := s.Load(ctx); err != nil {
				return nil, err
			}
			if cur := s.Current(); cur.Write == next.Write {
				return cur, nil
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("state %s: %w", DocumentKey, err)
		}
		s.set(next, newTag)
		return next.clone(), nil
	}
	return nil, ErrContended
}

// FencedUpdate is Update for a leader-only write (publication, the group
// sync, the period and expiry jobs): change is applied only if the
// document's leadership is still f, checked in the same compare-and-swap,
// so a leader deposed while it worked writes nothing (model/leadership.md,
// JOBS "doc").
func (s *Store) FencedUpdate(ctx context.Context, f Fence, change func(*Document) error) (*Document, error) {
	return s.Update(ctx, func(d *Document) error {
		if l := d.Fence; l == nil || *l != f {
			have := "no leader"
			if l != nil {
				have = fmt.Sprintf("%s leads at epoch %d", l.Holder, l.Epoch)
			}
			return fmt.Errorf("%w: %s; this writer is %s at epoch %d", ErrFenced, have, f.Holder, f.Epoch)
		}
		return change(d)
	})
}

// Validate applies change to a copy of the current document and checks the
// invariants, writing nothing: validate_only (control-plane.md §2).
func (s *Store) Validate(ctx context.Context, change func(*Document) error) error {
	if err := s.loaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	base := s.cur
	s.mu.Unlock()
	_, err := apply(base, change)
	return err
}

// loaded loads the document unless the store has read or written it.
func (s *Store) loaded(ctx context.Context) error {
	if _, ok := s.version(); ok {
		return nil
	}
	return s.Load(ctx)
}

// apply is the part of a write that touches no bucket: change on a copy of
// base, its new audit events numbered, the invariants checked. base is
// read, never changed: the documents a store holds are replaced, never
// edited in place.
func apply(base *Document, change func(*Document) error) (*Document, error) {
	next := base.clone()
	if err := change(next); err != nil {
		return nil, err
	}
	next.number()
	if err := check(base, next); err != nil {
		return nil, err
	}
	return next, nil
}

// broken is an ErrInvariant naming what the change breaks.
func broken(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvariant}, args...)...)
}

// check is what every write must keep, the fence's and the publication
// records' here and the entitlements' in checkEntitlements.
func check(old, next *Document) error {
	if err := checkFence(old, next); err != nil {
		return err
	}
	if err := checkEntitlements(old, next); err != nil {
		return err
	}
	return checkPublications(old, next)
}

// checkFence: the fence's epoch never goes back and the fence is never
// dropped, so a fenced epoch stays fenced (control/lead).
func checkFence(old, next *Document) error {
	if old.Fence != nil && (next.Fence == nil || next.Fence.Epoch < old.Fence.Epoch) {
		return broken("the fence's epoch goes back")
	}
	return nil
}

// checkPublications: no pool's publication record is removed or goes back
// in sequence, epoch or source, so a publisher fenced by them stays fenced
// (model/publication.md).
func checkPublications(old, next *Document) error {
	for pool, r := range old.Publications {
		n, ok := next.Publications[pool]
		if !ok {
			return broken("pool %s's publication record removed", pool)
		}
		if n.Seq < r.Seq || n.Epoch < r.Epoch || n.Source < r.Source {
			return broken("pool %s's publication record goes back, %+v to %+v", pool, r, n)
		}
	}
	return nil
}

// Encode is the document's stored form.
func (d *Document) Encode() ([]byte, error) { return jsonv2.Marshal(d) }

// Decode reads a stored document, refusing another schema and anything it
// does not know.
func Decode(b []byte) (*Document, error) {
	var d Document
	if err := jsonv2.Unmarshal(b, &d, jsonv2.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("state %s: %w", DocumentKey, err)
	}
	if d.Schema != Schema {
		return nil, fmt.Errorf("state %s: schema %d, this reader reads %d", DocumentKey, d.Schema, Schema)
	}
	return &d, nil
}

// Run refreshes the document every 500 ms (control-plane.md §3) until ctx
// ends, reporting a failure once until it changes.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.Refresh(ctx); err != nil {
			if err.Error() != last {
				slog.Error("state: refresh failed; serving the last read", "err", err)
			}
			last = err.Error()
		} else {
			last = ""
		}
	}
}
