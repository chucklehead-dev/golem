package telemetry

import (
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestStdoutCountsLostRows: a row the writer refuses is counted, not
// silently dropped; the next is written whole.
func TestStdoutCountsLostRows(t *testing.T) {
	emit := func(p Providers, kv attribute.KeyValue) {
		var r otellog.Record
		r.AddAttributes(kv)
		p.Logger.Logger("t").Emit(t.Context(), r)
	}
	p := Stdout(failWriter{})
	emit(p, attribute.Int64("x", 1))
	if n := p.Logger.(*jsonLogs).l.failed; n != 1 {
		t.Fatalf("%d failures counted, want 1", n)
	}
	var out strings.Builder
	p.Logger.(*jsonLogs).l.w = &out
	emit(p, attribute.Int64("x", 2))
	if n := p.Logger.(*jsonLogs).l.failed; n != 1 || out.String() != `{"x":2}`+"\n" {
		t.Fatalf("%d failures, wrote %q: want the next row written whole", n, out.String())
	}
}
