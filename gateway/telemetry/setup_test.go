package telemetry

import "testing"

func TestPlaintextOK(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.1": true, "127.1.2.3": true, "::1": true,
		"localhost":         false, // a name is resolved when dialed: not held to this machine
		"collector.example": false, "10.0.0.5": false, "localhost.example": false} {
		if plaintextOK(host) != want {
			t.Errorf("plaintextOK(%q) = %v, want %v", host, !want, want)
		}
	}
}
