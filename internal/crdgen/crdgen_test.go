package crdgen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCRDsCurrent: the chart's CRDs are what the types make
// (GOLEM_UPDATE_CRDS=1 rewrites them).
func TestCRDsCurrent(t *testing.T) {
	docs, err := Docs("../../lib/api/v1alpha1")
	if err != nil {
		t.Fatal(err)
	}
	dir := "../../deploy/charts/golem-control/crds"
	for _, k := range Kinds {
		got, err := CRD(k.Kind, k.Plural, k.Spec, docs)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, strings.ToLower(k.Kind)+".yaml")
		if os.Getenv("GOLEM_UPDATE_CRDS") == "1" {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is not what lib/api/v1alpha1 makes (%v): GOLEM_UPDATE_CRDS=1 go test ./internal/crdgen/", path, err)
		}
	}
}

// TestChartDDLCurrent: the golem-control chart's copy of the ledger's DDL
// (its DDL Job's) is deploy/clickhouse/spend.sql.
func TestChartDDLCurrent(t *testing.T) {
	want, err := os.ReadFile("../../deploy/clickhouse/spend.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile("../../deploy/charts/golem-control/files/spend.sql"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("deploy/charts/golem-control/files/spend.sql differs from deploy/clickhouse/spend.sql (%v): copy it", err)
	}
}

// TestChartCollectorCurrent: the golem-gateway chart's copy of the ledger's
// collector configuration (its sidecar's) is deploy/otel/collector.yaml.
func TestChartCollectorCurrent(t *testing.T) {
	want, err := os.ReadFile("../../deploy/otel/collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile("../../deploy/charts/golem-gateway/files/otel-collector.yaml"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("deploy/charts/golem-gateway/files/otel-collector.yaml differs from deploy/otel/collector.yaml (%v): copy it", err)
	}
}
