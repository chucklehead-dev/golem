// Package key is golem's API key format (contracts.md §5, ADR 0009):
//
//	glm_live_<49 base62 characters>   a production installation's
//	glm_dev_<49 base62 characters>    standalone and make dev-up's
//
// The characters encode 32 random bytes and their CRC32, so a mistyped or
// truncated key is refused before any lookup, and secret scanners can match
// the prefix with few false positives. golem stores only a key's SHA-256 and
// a display prefix; the key itself is shown once, at minting.
package key

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math/big"
	"strings"
)

// Env is the installation a key belongs to.
type Env string

// The installations: a production one, and standalone or development.
const (
	Live Env = "live"
	Dev  Env = "dev"
)

const (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	width    = 49 // base62 digits of 36 bytes
	// DisplayLen is how much of a key golem shows and logs (key_id's prefix).
	DisplayLen = 16
)

// Parse's errors: a string that is not a golem key, and one whose checksum
// fails.
var (
	ErrFormat   = errors.New("key: not a golem key (glm_live_ or glm_dev_ and 49 base62 characters)")
	ErrChecksum = errors.New("key: the checksum does not match: the key is mistyped or truncated")
)

// New mints a key for env.
func New(env Env) string {
	var b [36]byte
	rand.Read(b[:32])
	binary.BigEndian.PutUint32(b[32:], crc32.ChecksumIEEE(b[:32]))
	return env.prefix() + encode(b[:])
}

// prefix is what a key of env starts with: "glm_live_" or "glm_dev_".
func (env Env) prefix() string { return "glm_" + string(env) + "_" }

// Parse checks a key's shape and checksum and returns its installation.
func Parse(s string) (Env, error) {
	for _, env := range []Env{Live, Dev} {
		body, ok := strings.CutPrefix(s, env.prefix())
		if !ok {
			continue
		}
		b, ok := decode(body)
		if !ok {
			return "", ErrFormat
		}
		if crc32.ChecksumIEEE(b[:32]) != binary.BigEndian.Uint32(b[32:]) {
			return "", ErrChecksum
		}
		return env, nil
	}
	return "", ErrFormat
}

// Hash is what golem stores and looks a key up by.
func Hash(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// Display is the part of a key that may be shown and logged.
func Display(s string) string { return s[:min(len(s), DisplayLen)] }

var sixtyTwo = big.NewInt(62)

// encode writes b, a key's 36 bytes, as width base62 digits, most
// significant first.
func encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		var r big.Int
		n.DivMod(n, sixtyTwo, &r)
		out[i] = alphabet[r.Int64()]
	}
	return string(out)
}

// decode is encode's inverse; false for a body of the wrong length, a
// character outside the alphabet, or a value over 36 bytes.
func decode(s string) ([36]byte, bool) {
	var b [36]byte
	if len(s) != width {
		return b, false
	}
	n := new(big.Int)
	for i := range len(s) {
		d := strings.IndexByte(alphabet, s[i])
		if d < 0 {
			return b, false
		}
		n.Mul(n, sixtyTwo)
		n.Add(n, big.NewInt(int64(d)))
	}
	if n.BitLen() > 36*8 {
		return b, false
	}
	n.FillBytes(b[:])
	return b, true
}
