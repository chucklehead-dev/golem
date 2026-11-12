package contract

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func section(t *testing.T, heading string) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/contracts.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, heading)
	if i < 0 {
		t.Fatalf("contracts.md has no %q", heading)
	}
	s = s[i+len(heading):]
	if j := strings.Index(s, "\n## "); j >= 0 {
		s = s[:j]
	}
	return s
}

var (
	backticked = regexp.MustCompile("`([a-z_]+\\.[a-z_]+)`")
	statuses   = regexp.MustCompile(`\b([1-5]\d\d)\b`)
)

// TestCodesMatchContracts: every code in contracts.md §1 is in Codes with a
// status its row names, and every code in Codes is in the table.
func TestCodesMatchContracts(t *testing.T) {
	documented := map[string][]int{}
	for line := range strings.Lines(section(t, "## 1. Refusals")) {
		cells := strings.Split(line, "|")
		if len(cells) < 5 || !strings.Contains(cells[3], "`") {
			continue
		}
		var sts []int
		for _, m := range statuses.FindAllStringSubmatch(cells[2], -1) {
			n, _ := strconv.Atoi(m[1])
			sts = append(sts, n)
		}
		if strings.Contains(cells[2], "in the stream") {
			sts = append(sts, 0)
		}
		for _, m := range backticked.FindAllStringSubmatch(cells[3], -1) {
			documented[m[1]] = sts
		}
	}
	for _, c := range Codes {
		sts, ok := documented[c.Name]
		if !ok {
			t.Errorf("code %s is not in contracts.md §1: document it before the gateway may send it", c.Name)
			continue
		}
		if !slices.Contains(sts, c.Status) {
			t.Errorf("code %s has status %d; contracts.md §1 gives %v", c.Name, c.Status, sts)
		}
	}
	for name := range documented {
		if !slices.ContainsFunc(Codes, func(c Code) bool { return c.Name == name }) {
			t.Errorf("contracts.md §1 lists %s, which lib/contract does not define", name)
		}
	}
}

func TestHeadersMatchContracts(t *testing.T) {
	doc := strings.ToLower(section(t, "## 5. Identifiers and headers"))
	for _, h := range Headers {
		if !strings.Contains(doc, "`"+strings.ToLower(h)) {
			t.Errorf("header %s is not in contracts.md §5", h)
		}
	}
}

// TestLedgerDDL: the ClickHouse table has exactly the columns of
// contracts.md §3.
func TestLedgerDDL(t *testing.T) {
	var want []string
	for line := range strings.Lines(section(t, "## 3. The ledger")) {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		for _, m := range regexp.MustCompile("`([a-z0-9_]+)`").FindAllStringSubmatch(cells[1], -1) {
			want = append(want, m[1])
		}
	}
	b, err := os.ReadFile("../../deploy/clickhouse/spend.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := string(b)
	start := strings.Index(ddl, "CREATE TABLE IF NOT EXISTS golem.spend (")
	end := strings.Index(ddl[start:], "\n)")
	var got []string
	for line := range strings.Lines(ddl[start : start+end]) {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && !strings.HasPrefix(f[0], "--") && !strings.HasPrefix(f[0], "CREATE") {
			got = append(got, f[0])
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Fatalf("golem.spend columns differ from contracts.md §3:\n  ddl:  %v\n  doc:  %v", got, want)
	}
}
