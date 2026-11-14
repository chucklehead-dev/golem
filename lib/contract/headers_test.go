package contract

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestCheckSingleHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
		want string // "" = accepted
	}{
		{"one of each", http.Header{"Authorization": {"Bearer k"}, "Golem-Scope": {"a"}, "Anthropic-Version": {"2023-06-01"}}, ""},
		{"lists where lists are allowed", http.Header{"Anthropic-Beta": {"a,b", "c"}, "Golem-Tags": {"x,y"}}, ""},
		{"a comma where a value may hold one", http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Golem-End-User": {"Doe, Jane"}}, ""},
		{"two Authorization headers", http.Header{"Authorization": {"Bearer good", "Bearer other"}}, "the Authorization header appears 2 times"},
		{"two keys", http.Header{"X-Api-Key": {"a", "b"}}, "the X-Api-Key header appears 2 times"},
		{"two keys joined by a proxy", http.Header{"X-Api-Key": {"a, b"}}, "the X-Api-Key header holds a list"},
		{"two scopes joined", http.Header{"Golem-Scope": {"a,b"}}, "the Golem-Scope header holds a list"},
		{"two versions", http.Header{"Anthropic-Version": {"2023-06-01", "2099-01-01"}}, "the Anthropic-Version header appears 2 times"},
		{"two versions joined", http.Header{"Anthropic-Version": {"2023-06-01, 2026-10-21"}}, "the Anthropic-Version header holds a list"},
		{"two organizations", http.Header{"Openai-Organization": {"o1", "o2"}}, "the Openai-Organization header appears 2 times"},
		{"two sessions", http.Header{"Golem-Session": {"s1", "s2"}}, "the Golem-Session header appears 2 times"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSingleHeaders(tc.h)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("%v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestHeadersDeclared: every request header the code reads by name is
// declared single-valued or a list, so a new one cannot skip the check.
// It finds the reads by their spelling (a hyphenated header name, or a
// contract.Header constant, passed to Get or Values); response headers
// (a provider's resp, the caller's w) are not requests and are left out.
func TestHeadersDeclared(t *testing.T) {
	declared := append(slices.Clone(SingleHeaders), ListHeaders...)
	byConst := map[string]string{"HeaderScope": HeaderScope, "HeaderSession": HeaderSession, "HeaderEndUser": HeaderEndUser,
		"HeaderTags": HeaderTags, "HeaderRequestID": HeaderRequestID, "HeaderWarning": HeaderWarning,
		"HeaderRoute": HeaderRoute, "HeaderTranslated": HeaderTranslated}
	literal := regexp.MustCompile(`\b(?:Get|Values)\("([A-Z][A-Za-z0-9]*(?:-[A-Za-z0-9]+)+)"\)`)
	constant := regexp.MustCompile(`\b(?:Get|Values)\(contract\.(Header\w+)\)`)
	found := 0
	for _, dir := range []string{"../../gateway", "../../control", "../../cmd", "../../lib"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(src), "\n") {
				if strings.Contains(line, "resp.") || strings.Contains(line, "w.Header") || strings.Contains(line, "res.Header") {
					continue
				}
				var names []string
				for _, m := range literal.FindAllStringSubmatch(line, -1) {
					names = append(names, http.CanonicalHeaderKey(m[1]))
				}
				for _, m := range constant.FindAllStringSubmatch(line, -1) {
					names = append(names, byConst[m[1]])
				}
				for _, n := range names {
					found++
					if !slices.Contains(declared, n) {
						t.Errorf("%s:%d reads the %s header, which is in neither SingleHeaders nor ListHeaders (lib/contract/headers.go): declare it", path, i+1, n)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found < 5 {
		t.Fatalf("found %d header reads: the scan is broken", found)
	}
}
