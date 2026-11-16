// Package doccheck keeps Go comments pointing at text that still exists. A
// comment may not cite an ADR that is now a stub (superseded, or
// consolidated into another), nor a doc's section number, which moves
// whenever the doc is edited. docs/contributing.md has the rule.
package doccheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const root = "../.."

// checked are the trees whose comments are checked.
var checked = []string{"cmd", "control", "gateway", "internal", "lib", "test"} // read-only

// excluded are trees not yet held to the rule.
var excluded = []string{ // read-only
}

// adrList matches an ADR's citation, "ADR" and its number, and lists such
// as "ADRs" and two numbers joined by "and", or numbers after commas.
var adrList = regexp.MustCompile(`\bADRs?\s+(\d{4}(?:(?:\s*,\s*|\s+and\s+|\s+or\s+)\d{4})*)`)

var adrNumber = regexp.MustCompile(`\d{4}`)

// stubs are the ADRs whose status line says another record holds the
// decision now, by number, each with its file's name.
func stubs(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "docs", "adr", "[0-9][0-9][0-9][0-9]-*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no ADRs under docs/adr (%v)", err)
	}
	out := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(b)) {
			if !strings.HasPrefix(line, "Status:") {
				continue
			}
			if status := strings.TrimSpace(strings.TrimPrefix(line, "Status:")); strings.HasPrefix(status, "Superseded") || strings.HasPrefix(status, "Consolidated into") {
				out[filepath.Base(p)[:4]] = filepath.Base(p)
			}
			break
		}
	}
	return out
}

// problems lists what is wrong with one comment's text.
func problems(text string, stubs map[string]string) []string {
	var out []string
	if strings.Contains(text, "§") {
		out = append(out, "cites a section number (§): name the doc, or the section's title, instead")
	}
	for _, m := range adrList.FindAllStringSubmatch(text, -1) {
		for _, n := range adrNumber.FindAllString(m[1], -1) {
			if status, ok := stubs[n]; ok {
				out = append(out, fmt.Sprintf("cites ADR %s, a stub (docs/adr/%s): cite the record its status line names", n, status))
			}
		}
	}
	return out
}

// generated reports whether f is generated code, by Go's convention.
func generated(f *ast.File) bool {
	for _, g := range f.Comments {
		if g.Pos() > f.Package {
			break
		}
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "// Code generated ") && strings.HasSuffix(c.Text, " DO NOT EDIT.") {
				return true
			}
		}
	}
	return false
}

// TestCommentsCiteLiveText fails on a non-generated Go comment that cites a
// stub ADR or a section number.
func TestCommentsCiteLiveText(t *testing.T) {
	st := stubs(t)
	fset := token.NewFileSet()
	files := 0
	for _, tree := range checked {
		if slices.Contains(excluded, tree) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			if generated(f) {
				return nil
			}
			files++
			for _, g := range f.Comments {
				for _, c := range g.List {
					for _, p := range problems(c.Text, st) {
						rel, _ := filepath.Rel(root, path)
						t.Errorf("%s:%d: %s", rel, fset.Position(c.Pos()).Line, p)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files == 0 {
		t.Fatal("no Go files checked")
	}
}

// TestProblems checks the rule on single comments, so that the test above
// cannot pass by matching nothing.
func TestProblems(t *testing.T) {
	st := map[string]string{"0048": "Status: Consolidated into 0031", "0049": "Status: Consolidated into 0053"}
	for text, want := range map[string]int{
		"// as ADR 0031 says":                 0,
		"// known spend (ADR 0048)":           1,
		"// the fence (ADRs 0045 and 0049)":   1,
		"// the lease (ADR 0030, 0048, 0049)": 2,
		"// see control-plane.md §4":          1,
		"// 0048 alone is not a citation":     0,
	} {
		if got := problems(text, st); len(got) != want {
			t.Errorf("%q: %v, want %d problems", text, got, want)
		}
	}
}
