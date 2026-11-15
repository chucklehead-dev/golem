package state

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// Bootstrap is the deliberate start of a document where the bucket already
// holds golem-control's publications (golem-control bootstrap). floors is
// each pool's highest publication the bucket proves was made (its verified
// checkpoint and pointer: publish.Publisher.Floor); each pool's record is
// written at its floor, so the publisher numbers above what gateways trust
// rather than halting on a record below it (publish.ErrRegressed).
//
// With no document, it creates one, empty of entitlements, create-only
// (If-None-Match: *), so it never replaces one another writer put there
// meanwhile: every caller is refused until entitlements are created again.
// With a document, it refuses unless adopt, which accepts the document as
// it is (an older version put back when the newest cannot be recovered)
// and raises each record below its floor to it, by Update. Either is
// audited (state.bootstrap, state.adopt) by actor. It returns the document
// written.
func (s *Store) Bootstrap(ctx context.Context, floors map[string]uint64, actor string, adopt bool) (*Document, error) {
	if actor == "" {
		return nil, errors.New("state: a bootstrap names who runs it, for the audit")
	}
	_, _, err := s.Bucket.Get(ctx, DocumentKey, "")
	switch {
	case errors.Is(err, bucket.ErrNotFound):
		return s.create(ctx, floors, actor)
	case err != nil:
		return nil, fmt.Errorf("state %s: %w", DocumentKey, err)
	case !adopt:
		return nil, fmt.Errorf("state: %s exists; nothing written. A bootstrap creates a missing document; to accept the existing one as it is (an older version put back), and number publication above what the bucket proves, add -adopt", DocumentKey)
	}
	if err := s.Load(ctx); err != nil {
		return nil, err
	}
	at := time.Now().UTC()
	return s.Update(ctx, func(d *Document) error {
		raised := map[string]string{}
		for _, pool := range slices.Sorted(maps.Keys(floors)) {
			r, f := d.Publications[pool], floors[pool]
			if r.Seq >= f {
				continue
			}
			raised[pool] = fmt.Sprintf("%d to %d", r.Seq, f)
			r.Seq = f
			if d.Publications == nil {
				d.Publications = map[string]Record{}
			}
			d.Publications[pool] = r
		}
		if len(raised) == 0 {
			return errors.New("state: every pool's record is at or above what the bucket proves; nothing to adopt, nothing written")
		}
		d.Audit = append(d.Audit, AuditEvent{At: at, Actor: actor, Action: "state.adopt", Target: "document", Detail: raised})
		return nil
	})
}

// create writes the first document of a bootstrap, its records at floors.
func (s *Store) create(ctx context.Context, floors map[string]uint64, actor string) (*Document, error) {
	detail := map[string]string{}
	next, err := apply(&Document{Schema: Schema}, func(d *Document) error {
		for pool, f := range floors {
			if f == 0 {
				continue
			}
			if d.Publications == nil {
				d.Publications = map[string]Record{}
			}
			d.Publications[pool] = Record{Seq: f}
			detail[pool] = strconv.FormatUint(f, 10)
		}
		d.Audit = append(d.Audit, AuditEvent{At: time.Now().UTC(), Actor: actor, Action: "state.bootstrap", Target: "document", Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	next.StateVersion = 1
	next.wrote(rand.Text())
	body, err := next.Encode()
	if err != nil {
		return nil, err
	}
	tag, err := s.Bucket.Put(ctx, DocumentKey, body, bucket.Absent)
	if errors.Is(err, bucket.ErrPrecondition) {
		return nil, fmt.Errorf("state: %s was written while the bootstrap ran; nothing written: look at it before running again", DocumentKey)
	}
	if err != nil {
		return nil, fmt.Errorf("state %s: %w", DocumentKey, err)
	}
	s.set(next, tag)
	return next.clone(), nil
}
