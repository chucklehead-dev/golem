package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/internal/flagusage"
)

// TestUsageGroupsEveryFlag checks that -help prints every flag under a
// group, and CLICKHOUSE_PASSWORD with the known-spend flags.
func TestUsageGroupsEveryFlag(t *testing.T) {
	fs := flag.NewFlagSet("golem-control", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	if _, err := parseFlagsInto(fs, []string{"-help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-help: %v", err)
	}
	if missing := flagusage.Ungrouped(fs, flagGroups); len(missing) > 0 {
		t.Errorf("flags in no group of flagGroups: %v", missing)
	}
	help := out.String()
	if spend, pw := strings.Index(help, "Known spend:"), strings.Index(help, "$CLICKHOUSE_PASSWORD\n"); spend < 0 || pw < spend {
		t.Errorf("$CLICKHOUSE_PASSWORD is not listed with the known-spend flags:\n%s", help)
	}
	if strings.Contains(help, "Other:") {
		t.Errorf("a flag is in no group:\n%s", help)
	}
}
