package telemetry

import "testing"

func TestPlaintextOK(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true,
		"collector.example": false, "10.0.0.5": false, "localhost.example": false} {
		if plaintextOK(host) != want {
			t.Errorf("plaintextOK(%q) = %v, want %v", host, !want, want)
		}
	}
}
