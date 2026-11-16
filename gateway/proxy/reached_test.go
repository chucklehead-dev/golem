package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/envelope"
)

// TestReachedProvider: only failures the provider cannot have seen are
// unreached (free, and failed over): a name that did not resolve, a dial
// or TLS handshake that failed, and an envelope's request that never left
// the gateway (envelope.ErrNotSent, however wrapped); anything else, a
// reset or a timeout after the request went out, may have reached it.
func TestReachedProvider(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		reached bool
	}{
		{"the name did not resolve", &net.DNSError{Err: "no such host", Name: "provider.example"}, false},
		{"the dial failed", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		{"the handshake failed", tls.RecordHeaderError{Msg: "not TLS"}, false},
		{"not signed", envelope.NotSent(errors.New("bedrock: AWS credentials: no web identity token")), false},
		{"not signed, wrapped", fmt.Errorf("round trip: %w", envelope.NotSent(errors.New("bedrock: the request body: not a JSON object"))), false},
		{"a reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		{"anything else", errors.New("bedrock: AWS credentials: no web identity token"), true},
	} {
		if got := reachedProvider(c.err); got != c.reached {
			t.Errorf("%s: reached %v, want %v", c.name, got, c.reached)
		}
	}
}
