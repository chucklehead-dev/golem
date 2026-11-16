package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/internal/flagusage"
)

// poolArgs are the flags pool mode requires.
var poolArgs = []string{"-pool", "internal", "-bucket", "s3://b/p", "-keys", "/k", "-valkey", "v:6379", "-site", "eu", "-accept-unreleased"} // read-only

// TestModeDefaults checks that -listen and -unready default by mode. A pod
// is unreachable on loopback, and its endpoints take seconds to drop it.
func TestModeDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		args    []string
		listen  string
		unready time.Duration
	}{
		"standalone":           {[]string{"-standalone", "./config"}, "127.0.0.1:8080", 0},
		"pool":                 {poolArgs, ":8080", 5 * time.Second},
		"pool, both set":       {append([]string{"-listen", "10.0.0.1:9000", "-unready", "0"}, poolArgs...), "10.0.0.1:9000", 0},
		"standalone, both set": {[]string{"-standalone", "./config", "-listen", "[::1]:9000", "-unready", "2s"}, "[::1]:9000", 2 * time.Second},
	} {
		o, err := parse(tc.args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if o.listen != tc.listen || o.unready != tc.unready {
			t.Errorf("%s: -listen %q, -unready %v; want %q, %v", name, o.listen, o.unready, tc.listen, tc.unready)
		}
	}
}

// TestUsageGroupsEveryFlag checks that -help prints every flag under a
// group, and VALKEY_PASSWORD with the Valkey flags.
func TestUsageGroupsEveryFlag(t *testing.T) {
	fs := flag.NewFlagSet("golem-gateway", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	_, err := parseInto(fs, []string{"-help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-help: %v", err)
	}
	if missing := flagusage.Ungrouped(fs, flagGroups); len(missing) > 0 {
		t.Errorf("flags in no group of flagGroups: %v", missing)
	}
	help := out.String()
	valkey, guard := strings.Index(help, "-valkey-user"), strings.Index(help, "Guard")
	if pw := strings.Index(help, "$VALKEY_PASSWORD\n"); pw < valkey || pw > guard {
		t.Errorf("$VALKEY_PASSWORD is not listed with the Valkey flags:\n%s", help)
	}
	if strings.Contains(help, "Other:") {
		t.Errorf("a flag is in no group:\n%s", help)
	}
}
