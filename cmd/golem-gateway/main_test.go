package main

import "testing"

// TestReleaseGate: the production-publication gate (golem#5). The gateway
// serves only standalone, on loopback, until M1's gates pass.
func TestReleaseGate(t *testing.T) {
	for _, tc := range []struct {
		standalone, listen string
		ok                 bool
	}{
		{"", "127.0.0.1:8080", false}, // no published snapshots yet
		{"./config", "127.0.0.1:8080", true},
		{"./config", "[::1]:8080", true},
		{"./config", "localhost:8080", true},
		{"./config", "0.0.0.0:8080", false},
		{"./config", ":8080", false},
		{"./config", "10.0.0.5:8080", false},
	} {
		if err := releaseGate(tc.standalone, tc.listen); (err == nil) != tc.ok {
			t.Errorf("releaseGate(%q, %q) = %v, want ok %v", tc.standalone, tc.listen, err, tc.ok)
		}
	}
}
