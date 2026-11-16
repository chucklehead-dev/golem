package compile

import (
	"strings"
	"testing"
)

// TestPoolWeight: a pool member's weight unset is 1; 0, which reads as
// taking the deployment out of the pool but did not (weights are not used
// in M1), is refused saying what to do instead; a negative one is refused.
func TestPoolWeight(t *testing.T) {
	member := func(extra string) string {
		return strings.Replace(good, "pool: [{backend: anthropic, deployment: sonnet}]", "pool: [{backend: anthropic, deployment: sonnet"+extra+"}]", 1)
	}
	for _, c := range []struct{ name, extra, want string }{
		{"unset", "", ""},
		{"one", ", weight: 1", ""},
		{"zero", ", weight: 0", "spec.pool[0].weight: weight 0 does not take anthropic/sonnet out of the pool: weights are not used in M1"},
		{"negative", ", weight: -1", "spec.pool[0].weight: must not be negative"},
	} {
		s, err := Compile(objects(t, map[string]string{"golem.yaml": member(c.extra)}), published)
		switch {
		case c.want == "" && (err != nil || s.Routes[0].Pool[0].Weight != 1):
			t.Errorf("%s: %v, want it compiled with weight 1", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
