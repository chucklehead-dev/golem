// Package archtest checks the layering rules of architecture.md §4.2 and
// gateway.md §4 with go list, so a forbidden import fails CI rather than a
// review.
package archtest

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/chucklehead-dev/golem/"

// imports maps each package of the module to its direct imports.
func imports(t *testing.T) map[string][]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", module+"...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	m := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		m[f[0]] = f[1:]
	}
	return m
}

// deps lists every package a binary links.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", module+pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

func TestGatewayLinksNoControlPlane(t *testing.T) {
	forbidden := []string{module + "control/", "github.com/jackc/pgx", "github.com/ClickHouse/clickhouse-go",
		"k8s.io/client-go", "sigs.k8s.io/controller-runtime"}
	for _, d := range deps(t, "cmd/golem-gateway") {
		for _, f := range forbidden {
			if strings.HasPrefix(d, f) {
				t.Errorf("cmd/golem-gateway links %s: the gateway must not depend on %s (architecture.md §4.2)", d, f)
			}
		}
	}
}

func TestGatewayPackages(t *testing.T) {
	for pkg, imps := range imports(t) {
		rel, ok := strings.CutPrefix(pkg, module)
		if !ok {
			continue
		}
		for _, imp := range imps {
			to, ok := strings.CutPrefix(imp, module)
			if !ok {
				continue
			}
			switch {
			case to == "gateway/proxy" && rel != "cmd/golem-gateway":
				t.Errorf("%s imports gateway/proxy: nothing but the gateway's main may (gateway.md §4)", rel)
			case strings.HasPrefix(rel, "gateway/dialect/") && strings.HasPrefix(to, "gateway/") &&
				to != "gateway/canon" && !strings.HasPrefix(to, "gateway/dialect/internal/") && !strings.HasPrefix(to, dialectOf(rel)):
				t.Errorf("%s imports %s: a dialect depends only on canon (gateway.md §4)", rel, to)
			case strings.HasPrefix(rel, "gateway/envelope/") && strings.HasPrefix(to, "gateway/dialect/"):
				t.Errorf("%s imports %s: envelopes depend on no dialect (gateway.md §4)", rel, to)
			case strings.HasPrefix(rel, "lib/") && !strings.HasPrefix(to, "lib/") && !strings.HasPrefix(to, "gen/"):
				t.Errorf("%s imports %s: lib/ imports no component (architecture.md §4.2)", rel, to)
			case strings.HasPrefix(rel, "gateway/") && strings.HasPrefix(to, "control/"):
				t.Errorf("%s imports %s: the gateway imports nothing of the control plane", rel, to)
			}
		}
	}
}

// dialectOf is a dialect package's own tree: gateway/dialect/openai/chat
// may import gateway/dialect/openai/chat/... only.
func dialectOf(pkg string) string {
	parts := strings.SplitN(pkg, "/", 5)
	if len(parts) < 4 {
		return pkg
	}
	return strings.Join(parts[:4], "/")
}
