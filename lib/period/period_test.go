package period

import (
	"testing"
	"time"
)

func TestOf(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	late := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC) // still November in New York
	if got := Of(late, time.UTC); got != "2026-12" {
		t.Errorf("UTC: %s", got)
	}
	if got := Of(late, ny); got != "2026-11" || FirstDay(got) != "2026-11-01" {
		t.Errorf("New York: %s", got)
	}
}
