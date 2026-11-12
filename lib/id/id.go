// Package id makes golem's identifiers: UUIDv7 (RFC 9562) for requests and
// events, time-ordered so the ledger and logs sort by them (contracts.md §5).
package id

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// UUID is a UUID's 16 bytes.
type UUID [16]byte

// New returns a UUIDv7 for now.
func New() UUID { return At(time.Now()) }

// At returns a UUIDv7 with the given time and random bits.
func At(t time.Time) UUID {
	var u UUID
	rand.Read(u[6:])
	ms := uint64(t.UnixMilli())
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	u[6] = 0x70 | u[6]&0x0f // version 7
	u[8] = 0x80 | u[8]&0x3f // variant 10
	return u
}

// AppendString appends the canonical 36-character form.
func (u UUID) AppendString(b []byte) []byte {
	var s [36]byte
	hex.Encode(s[0:8], u[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], u[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], u[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], u[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], u[10:16])
	return append(b, s[:]...)
}

func (u UUID) String() string { return string(u.AppendString(nil)) }
