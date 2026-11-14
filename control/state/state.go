// Package state is golem-control's store (control-plane.md §3, ADR 0006):
// one document in the bucket, read from memory and refreshed by a
// conditional get, written only by Update, which applies a change to a
// copy, checks the store's invariants, increments state_version and writes
// it with If-Match, re-reading and retrying when another writer won.
//
// This slice holds what publication needs: each pool's publication record
// (control/publish numbers and fences by it). The rest of the outline in
// §3.1 joins it as it is built. The encoding is JSON, decoded strictly; the
// design's protobuf and zstd replace it behind Encode and Decode.
package state

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// Schema is the document's format; a reader refuses any other.
const Schema = 1

// Key is the document's object key.
const Key = "state/document.json"

// Record is a pool's publication record: the last publication's sequence,
// and the leadership epoch and state version it was made under
// (model/publication.md's PubRecord).
type Record struct {
	Epoch  uint64 `json:"epoch"`
	Seq    uint64 `json:"seq"`
	Source uint64 `json:"source"`
}

// Lease is golem-control's leadership (control/lead): who holds it, its
// epoch (which increases whenever it changes hands, and fences
// publication), and when its holder last renewed it.
type Lease struct {
	Holder    string    `json:"holder"`
	Epoch     uint64    `json:"epoch"`
	RenewedAt time.Time `json:"renewedAt"`
	// DurationMillis is how long a renewal holds it (encoding/json/v2 has
	// no default form for a time.Duration).
	DurationMillis int64 `json:"durationMillis"`
}

// Expires is when the lease lapses unless renewed.
func (l *Lease) Expires() time.Time {
	return l.RenewedAt.Add(time.Duration(l.DurationMillis) * time.Millisecond)
}

// Document is the state.
type Document struct {
	Schema       int               `json:"schema"`
	StateVersion uint64            `json:"stateVersion"`
	Leader       *Lease            `json:"leader,omitempty"`
	Publications map[string]Record `json:"publications,omitempty"`
}

func (d *Document) clone() *Document {
	c := *d
	c.Publications = maps.Clone(d.Publications)
	if d.Leader != nil {
		l := *d.Leader
		c.Leader = &l
	}
	return &c
}

// Errors.
var (
	// ErrInvariant: a change the store refuses to write.
	ErrInvariant = errors.New("state: the change breaks an invariant")
	// ErrContended: other writers won every attempt.
	ErrContended = errors.New("state: the document changed under every attempt; retry")
)

// Store is the document in a bucket. The zero Store is not ready: Load it.
type Store struct {
	Bucket bucket.Bucket
	// Attempts bounds an Update's retries after losing to another writer
	// (default 8).
	Attempts int

	mu  sync.Mutex
	cur *Document
	tag string // the document's entity tag, or bucket.Absent before the first write
}

// Load reads the document; none yet is the empty document at version 0.
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
	body, newTag, err := s.Bucket.Get(ctx, Key, tag)
	switch {
	case errors.Is(err, bucket.ErrNotModified):
		return nil
	case errors.Is(err, bucket.ErrNotFound):
		s.set(&Document{Schema: Schema}, bucket.Absent)
		return nil
	case err != nil:
		return fmt.Errorf("state %s: %w", Key, err)
	}
	d, err := Decode(body)
	if err != nil {
		return err
	}
	s.set(d, newTag)
	return nil
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

// Update applies change to a copy of the document and writes it if it
// keeps the invariants, retrying from a fresh read when another writer
// won. change may run more than once; an error from it ends the update
// and is returned as it is. It returns the document written.
func (s *Store) Update(ctx context.Context, change func(*Document) error) (*Document, error) {
	s.mu.Lock()
	loaded := s.cur != nil
	s.mu.Unlock()
	if !loaded {
		if err := s.Load(ctx); err != nil {
			return nil, err
		}
	}
	attempts := s.Attempts
	if attempts <= 0 {
		attempts = 8
	}
	for range attempts {
		s.mu.Lock()
		base, tag := s.cur.clone(), s.tag
		s.mu.Unlock()
		next := base.clone()
		if err := change(next); err != nil {
			return nil, err
		}
		if err := check(base, next); err != nil {
			return nil, err
		}
		next.Schema, next.StateVersion = Schema, base.StateVersion+1
		body, err := next.Encode()
		if err != nil {
			return nil, err
		}
		newTag, err := s.Bucket.Put(ctx, Key, body, tag)
		if errors.Is(err, bucket.ErrPrecondition) {
			if err := s.Load(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("state %s: %w", Key, err)
		}
		s.set(next, newTag)
		return next.clone(), nil
	}
	return nil, ErrContended
}

// check is what every write must keep: the lease's epoch never goes back
// and is never dropped, and no pool's publication record is removed or
// goes back in sequence, epoch or source (a publisher fenced by them must
// stay fenced; model/publication.md).
func check(old, next *Document) error {
	if old.Leader != nil && (next.Leader == nil || next.Leader.Epoch < old.Leader.Epoch) {
		return fmt.Errorf("%w: the lease's epoch goes back", ErrInvariant)
	}
	for pool, r := range old.Publications {
		n, ok := next.Publications[pool]
		if !ok {
			return fmt.Errorf("%w: pool %s's publication record removed", ErrInvariant, pool)
		}
		if n.Seq < r.Seq || n.Epoch < r.Epoch || n.Source < r.Source {
			return fmt.Errorf("%w: pool %s's publication record goes back, %+v to %+v", ErrInvariant, pool, r, n)
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
		return nil, fmt.Errorf("state %s: %w", Key, err)
	}
	if d.Schema != Schema {
		return nil, fmt.Errorf("state %s: schema %d, this reader reads %d", Key, d.Schema, Schema)
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
