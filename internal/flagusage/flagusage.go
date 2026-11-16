// Package flagusage prints a command's flags in groups, in the order the
// command lists them. flag.PrintDefaults sorts flags alphabetically, which
// scatters related flags, such as pool mode's, across the page.
package flagusage

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Group is a titled list of flags, and the environment variables that
// belong with them.
type Group struct {
	Title string
	Flags []string // flag names, without the dash
	Env   []Env
}

// Env is a setting read from the environment rather than a flag, such as a
// password that a flag would show in the process list.
type Env struct {
	Name, Usage string
}

// Print writes fs's flags to w, group by group. A flag in no group is
// printed last, under "Other", so a new flag is never hidden.
func Print(w io.Writer, fs *flag.FlagSet, groups []Group) {
	seen := map[string]bool{}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s:\n", g.Title)
		for _, name := range g.Flags {
			if f := fs.Lookup(name); f != nil {
				printFlag(w, f)
				seen[name] = true
			}
		}
		for _, e := range g.Env {
			fmt.Fprintf(w, "  $%s\n    \t%s\n", e.Name, e.Usage)
		}
	}
	var other []*flag.Flag
	fs.VisitAll(func(f *flag.Flag) {
		if !seen[f.Name] {
			other = append(other, f)
		}
	})
	if len(other) > 0 {
		fmt.Fprint(w, "\nOther:\n")
		for _, f := range other {
			printFlag(w, f)
		}
	}
}

// Ungrouped lists fs's flags that are in no group, for a test that keeps
// every flag in one.
func Ungrouped(fs *flag.FlagSet, groups []Group) []string {
	var out []string
	fs.VisitAll(func(f *flag.Flag) {
		if !slices.ContainsFunc(groups, func(g Group) bool { return slices.Contains(g.Flags, f.Name) }) {
			out = append(out, f.Name)
		}
	})
	return out
}

// printFlag prints one flag as flag.PrintDefaults does.
func printFlag(w io.Writer, f *flag.Flag) {
	var b strings.Builder
	fmt.Fprintf(&b, "  -%s", f.Name)
	name, usage := flag.UnquoteUsage(f)
	if name != "" {
		b.WriteString(" " + name)
	}
	b.WriteString("\n    \t")
	b.WriteString(strings.ReplaceAll(usage, "\n", "\n    \t"))
	if !zero(f.DefValue) {
		if name == "string" {
			fmt.Fprintf(&b, " (default %q)", f.DefValue)
		} else {
			fmt.Fprintf(&b, " (default %v)", f.DefValue)
		}
	}
	fmt.Fprintln(w, b.String())
}

// zero reports whether a flag's default is its type's zero value, which
// flag.PrintDefaults does not print either.
func zero(v string) bool {
	switch v {
	case "", "0", "0s", "false":
		return true
	}
	return false
}
