package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
)

// Exporters that discard, so the benchmark measures the SDK, not a wire.
type nopLogExp struct{}

func (nopLogExp) Export(context.Context, []sdklog.Record) error { return nil }
func (nopLogExp) Shutdown(context.Context) error                { return nil }
func (nopLogExp) ForceFlush(context.Context) error              { return nil }

type nopSpanExp struct{}

func (nopSpanExp) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (nopSpanExp) Shutdown(context.Context) error                             { return nil }

// BenchmarkProduction is the admitted path as production runs it: the OTel
// SDK's batch span and log processors (exporters that discard) and a meter,
// or standalone's JSON ledger rows. BenchmarkHandler leaves telemetry off.
func BenchmarkProduction(b *testing.B) {
	for _, mode := range []string{"otlp-sdk", "stdout-json"} {
		b.Run(mode, func(b *testing.B) {
			k := key.New(key.Dev)
			events := anthropicStream(200)
			s := &stub{events: events}
			h, _ := standaloneHandler(b, `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes: [{name: team-a, models: [claude]}]
  keys: [{key: `+k+`, principal: alice, scope: team-a}]
`, s)
			var p telemetry.Providers
			if mode == "otlp-sdk" {
				var shutdown func(context.Context) error
				p, shutdown = telemetry.SDK(nopSpanExp{}, sdkmetric.NewManualReader(), nopLogExp{}, nil)
				b.Cleanup(func() { shutdown(context.Background()) })
			} else {
				p = telemetry.Stdout(io.Discard)
			}
			tel, err := telemetry.New(p, "local", "standalone")
			if err != nil {
				b.Fatal(err)
			}
			h.Telemetry = tel
			var br bytes.Reader
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			req.Header.Set("X-Api-Key", k)
			w := &discard{h: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				br.Reset(smallRequest)
				req.Body = io.NopCloser(&br)
				clear(w.h)
				s.events = slices.Clone(events)
				h.ServeHTTP(w, req)
			}
		})
	}
}
