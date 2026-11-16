package key

import (
	"strings"
	"testing"
)

func TestNewParse(t *testing.T) {
	for _, env := range []Env{Live, Dev} {
		k := New(env)
		if got, err := Parse(k); err != nil || got != env {
			t.Fatalf("Parse(%s) = %v, %v", k, got, err)
		}
		if !strings.HasPrefix(k, "glm_"+string(env)+"_") || len(k) != len("glm_"+string(env)+"_")+width {
			t.Fatalf("shape of %s", k)
		}
	}
	if a, b := New(Live), New(Live); a == b {
		t.Fatal("two equal keys")
	}
}

func TestRefused(t *testing.T) {
	k := New(Live)
	for name, s := range map[string]string{
		"empty":        "",
		"other prefix": "sk-" + k[9:],
		"truncated":    k[:len(k)-1],
		"extended":     k + "0",
		"not base62":   k[:20] + "-" + k[21:],
		"too large":    "glm_live_" + strings.Repeat("z", width),
		"provider key": "sk-ant-api03-abc",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%s: %q parsed", name, s)
		}
	}
}

// FuzzSingleCharError: a one-character change to a valid key is refused.
// The change alters the encoded number, and a different 32 bytes keep the
// same CRC32 about once in 2^32: checked here, not assumed.
func FuzzSingleCharError(f *testing.F) {
	f.Add(uint8(10), uint8(3))
	k := New(Live)
	f.Fuzz(func(t *testing.T, pos, ch uint8) {
		i := len("glm_live_") + int(pos)%width
		c := alphabet[int(ch)%len(alphabet)]
		if k[i] == c {
			return
		}
		bad := k[:i] + string(c) + k[i+1:]
		if _, err := Parse(bad); err == nil {
			t.Fatalf("a one-character change at %d to %q passed the checksum", i, c)
		}
	})
}
