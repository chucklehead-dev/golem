// Package audit ships the state document's audit outbox. The leader
// archives each event, emits it as an OTel log record for ClickHouse, and
// only then trims it from the outbox.
//
// The archive is the durability boundary, not the collector: each event is
// written once, create-only, as its own object (audit/<seq>.json) in an
// Object Lock bucket in a separate account, so a retry after a crash finds
// it there and goes on, and an event archived with other contents stops
// the shipping (it is never trimmed over a mismatch). The log record is
// emitted at least once (a crash between it and the trim emits it again;
// ClickHouse keeps one row per seq), and a gap in ClickHouse is filled
// from the archive.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// Key is an event's archive object.
func Key(seq uint64) string { return fmt.Sprintf("audit/%020d.json", seq) }

// Shipper ships the outbox's oldest events.
type Shipper struct {
	State   *state.Store
	Archive bucket.Bucket  // an Object Lock bucket, golem-control's role writing only
	Logger  otellog.Logger // the collector's, for ClickHouse; nil: archive only
	Batch   int            // events a step (default 100)
}

// ErrMismatch means an event's archived object is not the event. Shipping
// stops until a person looks, and the outbox keeps the event.
var ErrMismatch = errors.New("audit: an archived event differs from the outbox's")

// Step archives, emits and trims up to Batch events, as the leader at
// fence, and returns how many it trimmed. The trim is a leader-only write,
// fenced like the others (state.FencedUpdate): a shipper deposed while it
// archived writes nothing (state.ErrFenced). What it archived and emitted
// stands; the new leader finds the objects identical and goes on.
func (s *Shipper) Step(ctx context.Context, fence state.Fence) (int, error) {
	if err := s.State.Refresh(ctx); err != nil {
		return 0, err
	}
	events := s.State.Current().Audit
	batch := s.Batch
	if batch <= 0 {
		batch = 100 // a zero or negative size is the default, never a slice panic
	}
	events = events[:min(len(events), batch)]
	if len(events) == 0 {
		return 0, nil
	}
	for _, e := range events {
		if err := s.archive(ctx, e); err != nil {
			return 0, err
		}
	}
	for _, e := range events {
		s.emit(ctx, e)
	}
	last := events[len(events)-1].Seq
	if _, err := s.State.FencedUpdate(ctx, fence, func(d *state.Document) error {
		// d is the store's copy: its outbox may be edited in place.
		d.Audit = slices.DeleteFunc(d.Audit, func(e state.AuditEvent) bool { return e.Seq <= last })
		return nil
	}); err != nil {
		return 0, fmt.Errorf("audit: trimming the outbox to after %d: %w", last, err)
	}
	return len(events), nil
}

func (s *Shipper) archive(ctx context.Context, e state.AuditEvent) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.Archive.Put(ctx, Key(e.Seq), body, bucket.Absent)
	if !errors.Is(err, bucket.ErrPrecondition) {
		return err
	}
	// Archived before (a step that did not get to its trim): it must be
	// this event.
	got, _, err := s.Archive.Get(ctx, Key(e.Seq), "")
	if err != nil {
		return err
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("%w: seq %d at %s", ErrMismatch, e.Seq, Key(e.Seq))
	}
	return nil
}

func (s *Shipper) emit(ctx context.Context, e state.AuditEvent) {
	if s.Logger == nil {
		return
	}
	var r otellog.Record
	r.SetEventName("golem.audit")
	r.SetTimestamp(e.At)
	r.SetBody(attribute.StringValue(e.Action))
	attrs := []attribute.KeyValue{attribute.Int64("golem.audit.seq", int64(e.Seq)), attribute.String("golem.audit.actor", e.Actor),
		attribute.String("golem.audit.action", e.Action), attribute.String("golem.audit.target", e.Target)}
	for _, k := range slices.Sorted(maps.Keys(e.Detail)) {
		attrs = append(attrs, attribute.String("golem.audit.detail."+k, e.Detail[k]))
	}
	r.AddAttributes(attrs...)
	s.Logger.Emit(ctx, r)
}
