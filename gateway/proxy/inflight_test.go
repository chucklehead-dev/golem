package proxy

import (
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestSweepSparesANewerGrant: a sweep that read the keys before a snapshot
// was applied does not end a call that authenticated under that snapshot
// (a key minted a moment ago, unknown to the keys the sweep read): the keys
// in force when it would end the call must refuse it too. A key the newer
// snapshot also refuses is still ended.
func TestSweepSparesANewerGrant(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	minted, other, gone := key.New(key.Live), key.New(key.Live), key.New(key.Live)
	ents := func(keys ...string) snapshot.Entitlements {
		e := snapshot.Entitlements{
			Scopes:    []snapshot.Scope{{ID: "team-a", Models: []string{"claude"}}},
			KeyOwners: []snapshot.Owner{{ID: "alice", Kind: "user", Scopes: []string{"team-a"}}},
		}
		for _, k := range keys {
			e.Keys = append(e.Keys, snapshot.Key{Hash: hashHex(k), Principal: "alice", PrincipalKind: "user", Scope: "team-a"})
		}
		return e
	}
	clock := func() time.Time { return now }
	ancient := auth.Compile(ents(gone), key.Live, clock, nil)        // gone was a key then
	older := auth.Compile(ents(other), key.Live, clock, nil)         // gone removed; before the mint
	newer := auth.Compile(ents(other, minted), key.Live, clock, nil) // the mint applied
	var g, gOther auth.Grant
	if _, no := newer.AuthenticateGrant(http.Header{"X-Api-Key": {minted}}, &g); no != nil {
		t.Fatal(no.Msg)
	}
	if _, no := ancient.AuthenticateGrant(http.Header{"X-Api-Key": {gone}}, &gOther); no != nil {
		t.Fatal(no.Msg)
	}
	reads := 0
	f := &InFlight{Keys: func() *auth.Keys {
		reads++
		if reads == 1 {
			return older // what the sweep read as it began
		}
		return newer // applied while it ran
	}}
	var ended, endedOther bool
	var c, cOther inFlightCall
	f.add(&c, &g, func(error) { ended = true }, 0, f.generation())
	f.add(&cOther, &gOther, func(error) { endedOther = true }, 1, f.generation())
	f.Sweep()
	if ended {
		t.Fatal("a call the snapshot applied since honours was ended by keys read before it")
	}
	if !endedOther {
		t.Fatal("a call both the keys read and the newer ones refuse was not ended")
	}
}

func hashHex(k string) string { h := key.Hash(k); return hex.EncodeToString(h[:]) }
