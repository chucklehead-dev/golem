package contract

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// The request headers golem reads as one value, and those it reads as a
// list. A header golem reads must be in one of them (TestHeadersDeclared):
// a single-valued header sent twice is ambiguous (golem would read the
// first, a proxy in front of it or the provider behind it perhaps the
// last), so it is refused rather than one value silently chosen.
var (
	// SingleHeaders may appear at most once. Of these, NoListHeaders may
	// not hold a comma either: an intermediary may join two of them into
	// one comma-separated line (RFC 9110), and none of their values
	// has a comma of its own. read-only
	SingleHeaders = []string{"Authorization", "X-Api-Key", "Content-Type",
		HeaderScope, HeaderSession, HeaderEndUser, "Anthropic-Version", "Openai-Organization"}
	NoListHeaders = []string{"X-Api-Key", HeaderScope, "Anthropic-Version", "Openai-Organization"} // read-only
	// ListHeaders may repeat, each value a comma-separated list. read-only
	ListHeaders = []string{"Anthropic-Beta", HeaderTags}
)

// CheckSingleHeaders reports the first single-valued header h holds more
// than once, or as a list, with what to do: nil if there is none.
func CheckSingleHeaders(h http.Header) error {
	for _, name := range SingleHeaders {
		v := h.Values(name)
		switch {
		case len(v) > 1:
			return fmt.Errorf("the %s header appears %d times: send it once", name, len(v))
		case len(v) == 1 && strings.Contains(v[0], ",") && isNoList(name):
			return fmt.Errorf("the %s header holds a list (%q): send one value", name, v[0])
		}
	}
	return nil
}

func isNoList(name string) bool { return slices.Contains(NoListHeaders, name) }
