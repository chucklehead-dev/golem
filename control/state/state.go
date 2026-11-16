// Package state is golem-control's store: one document in the bucket
// (ADR 0006). Reads come from memory, refreshed by a conditional get.
//
// Only Update writes. It applies a change to a copy, checks the store's
// invariants, increments state_version and writes with If-Match. When
// another writer won, it reads again and retries.
//
// The document holds the fence, each pool's publication record, the
// entitlements and the audit outbox. It is JSON, decoded strictly.
// TODO(ADR 0006): protobuf and zstd, behind the same Encode and Decode.
package state

import (
	"cmp"
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
// epoch increases whenever leadership changes hands (Claim).
type Fence struct {
	Holder string `json:"holder"`
	Epoch  uint64 `json:"epoch"`
}

// Document is the state.
type Document struct {
	Schema       int    `json:"schema"`
	StateVersion uint64 `json:"stateVersion"`
	// Fence is the replica that may write state, and its epoch (ADR 0053).
	// golem-control claims it at start, and every leader-only write checks
	// it.
	Fence *Fence `json:"fence,omitempty"`
	// Writes names the updates that wrote the last RecentWrites versions,
	// newest last, so a writer whose compare-and-swap reply was lost can
	// tell its own write from another's even after others have written
	// over it (Update).
	Writes       []string          `json:"writes,omitempty"`
	Publications map[string]Record `json:"publications,omitempty"`

	Principals  map[string]Principal `json:"principals,omitempty"` // by principal ID
	Scopes      map[string]Scope     `json:"scopes,omitempty"`     // by scope ID
	Keys        map[string]Key       `json:"keys,omitempty"`       // by hex SHA-256 of the key
	Revocations []Revocation         `json:"revocations,omitempty"`
	Roles       []RoleBinding        `json:"roles,omitempty"`
	// Audit is the outbox. A write's events go in with it, and the leader
	// trims only what is durably archived. It is bounded by
	// Store.MaxOutbox, its last 20% kept for restrictive changes.
	Audit    []AuditEvent `json:"audit,omitempty"`
	AuditSeq uint64       `json:"auditSeq,omitempty"` // the last event's number
}

// RecentWrites is how many write IDs a document keeps (Writes): a writer
// whose reply was lost recognises its write while fewer than this many
// others have landed since, far more than land in the few round trips its
// retry takes.
const RecentWrites = 32

// wrote records write id as the newest of d's Writes, keeping the last
// RecentWrites. The slice is new: d's may be shared with the document it
// was cloned from.
func (d *Document) wrote(id string) {
	keep := d.Writes[max(0, len(d.Writes)-(RecentWrites-1)):]
	d.Writes = append(append(make([]string, 0, len(keep)+1), keep...), id)
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
	// ErrInvariant means the store refuses to write a change.
	ErrInvariant = errors.New("state: the change breaks an invariant")
	// ErrContended means other writers won every attempt.
	ErrContended = errors.New("state: the document changed under every attempt; retry")
	// ErrRegressed means the bucket holds an older version than this store
	// has seen, or none, after an out-of-band restore or a deletion. The
	// store neither adopts it nor writes over it. Adopting would drop every
	// later revocation.
	ErrRegressed = errors.New("state: the document in the bucket went back")
	// ErrFenced means the document's fence is not the writer's. A later
	// replica has claimed it, or the writer never held it at that epoch.
	// Nothing was written.
	ErrFenced = errors.New("state: fenced: this replica does not lead at that epoch")
	// ErrMissing means there is no document, though the bucket shows
	// golem-control has run on it. The document was deleted. Only Bootstrap
	// creates one then.
	ErrMissing = errors.New("state: the document is missing from a bucket golem-control has published from")
)

// Store is the document in a bucket. The zero Store is not ready: Load it.
type Store struct {
	Bucket bucket.Bucket
	// Attempts bounds an Update's retries after losing to another writer
	// (default 8).
	Attempts int
	// Installed are objects whose presence shows golem-control has run on
	// this bucket, such as the pools' checkpoints. A first read that finds
	// one of these but no document is ErrMissing, not a first start.
	Installed []string
	// MaxOutbox bounds the audit outbox (outbox.go; default
	// DefaultMaxOutbox): past 80% of it a write that grants or widens
	// access is refused, and at it every write that adds an audit event.
	MaxOutbox int
	// Timeout bounds each of the store's bucket calls: a read (Load,
	// Refresh) and each write of an Update (default 15 seconds). A bucket
	// that accepts a request and never answers must not hold the
	// publication loop, the refresh or an API call for good.
	Timeout time.Duration

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
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
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

// timeout is Timeout or its default.
func (s *Store) timeout() time.Duration { return cmp.Or(s.Timeout, 15*time.Second) }

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
	var ids []string // one write ID an attempt: any of them found written is this update landed
	for range attempts {
		s.mu.Lock()
		base, tag := s.cur, s.tag
		s.mu.Unlock()
		next, err := apply(base, change, s.OutboxMax())
		if err != nil {
			return nil, err
		}
		next.Schema, next.StateVersion = Schema, base.StateVersion+1
		id := rand.Text()
		ids = append(ids, id)
		next.wrote(id)
		body, err := next.Encode()
		if err != nil {
			return nil, err
		}
		pctx, cancel := context.WithTimeout(ctx, s.timeout())
		newTag, err := s.Bucket.Put(pctx, DocumentKey, body, tag)
		cancel()
		// A put that ran out of its own time may have landed, its reply
		// lost: it is judged as a lost race is.
		timedOut := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		if errors.Is(err, bucket.ErrPrecondition) || timedOut {
			// Another writer won, or this one did and its reply was lost (a
			// retried conditional put fails its own condition): read what
			// is there, and if one of this update's writes is among its
			// recent ones, it succeeded, whether or not others have written
			// since. Applying the change again would apply it twice (a
			// second audit event; a removal that landed answered NotFound).
			if err := s.Load(ctx); err != nil {
				return nil, err
			}
			if cur := s.Current(); slices.ContainsFunc(cur.Writes, func(w string) bool { return slices.Contains(ids, w) }) {
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

// FencedUpdate is Update for a leader-only write, such as a publication or
// the group sync. change is applied only if the document's fence is still
// f, checked in the same compare-and-swap. So a leader deposed while it
// worked writes nothing.
//
//quint:enforces publicationsOrdered
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

// Claim takes the fence for holder at one above the document's epoch.
// golem-control's one replica claims it at start (ADR 0053). Its later
// leader-only writes are fenced by the returned fence. A predecessor still
// running is refused at its next fenced write. A claim whose reply was lost
// is recognised by its write ID, and not made twice.
func (s *Store) Claim(ctx context.Context, holder string) (Fence, error) {
	if holder == "" {
		return Fence{}, errors.New("state: a fence's holder is required: the replica's name")
	}
	var f Fence
	_, err := s.Update(ctx, func(d *Document) error {
		f = Fence{Holder: holder, Epoch: 1}
		if d.Fence != nil {
			f.Epoch = d.Fence.Epoch + 1
		}
		claimed := f
		d.Fence = &claimed
		return nil
	})
	if err != nil {
		return Fence{}, fmt.Errorf("claiming the fence for %s: %w", holder, err)
	}
	return f, nil
}

// Leads reports whether f is the document's fence as last read or
// written: false once another has claimed a later epoch, which f can never
// regain (checkFence).
func (s *Store) Leads(f Fence) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && s.cur.Fence != nil && *s.cur.Fence == f
}

// Validate applies change to a copy of the current document and checks the
// invariants, writing nothing. It serves the control API's validate_only.
func (s *Store) Validate(ctx context.Context, change func(*Document) error) error {
	if err := s.loaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	base := s.cur
	s.mu.Unlock()
	_, err := apply(base, change, s.OutboxMax())
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
// base, its new audit events numbered, the invariants checked, the outbox
// within outbox (its bound). base is read, never changed: the documents a
// store holds are replaced, never edited in place.
func apply(base *Document, change func(*Document) error, outbox int) (*Document, error) {
	next := base.clone()
	if err := change(next); err != nil {
		return nil, err
	}
	next.number()
	if err := check(base, next, outbox); err != nil {
		return nil, err
	}
	return next, nil
}

// broken is an ErrInvariant naming what the change breaks.
func broken(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvariant}, args...)...)
}

// check is what every write must keep: the fence's and the publication
// records' invariants, the entitlements' (checkEntitlements), and last the
// audit outbox's bound, outbox (checkOutbox), so a change that breaks an
// invariant says so whatever the outbox holds.
func check(old, next *Document, outbox int) error {
	if err := checkFence(old, next); err != nil {
		return err
	}
	if err := checkEntitlements(old, next); err != nil {
		return err
	}
	if err := checkPublications(old, next); err != nil {
		return err
	}
	return checkOutbox(old, next, outbox)
}

// checkFence refuses a change that drops the fence or moves its epoch
// back, so a fenced epoch stays fenced.
func checkFence(old, next *Document) error {
	if old.Fence != nil && (next.Fence == nil || next.Fence.Epoch < old.Fence.Epoch) {
		return broken("the fence's epoch goes back")
	}
	return nil
}

// checkPublications refuses a change that removes a pool's publication
// record or moves its sequence, epoch or source back. So a publisher fenced
// by a record stays fenced.
//
//quint:enforces publicationsOrdered
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

// Run refreshes the document every 500 ms until ctx ends, logging each
// failure once until it changes.
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
