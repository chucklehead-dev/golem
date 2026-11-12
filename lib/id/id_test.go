package id

import (
	"regexp"
	"testing"
	"time"
)

var form = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDv7(t *testing.T) {
	a, b := At(time.UnixMilli(1000)), At(time.UnixMilli(2000))
	if !form.MatchString(a.String()) {
		t.Fatalf("%s is not a UUIDv7", a)
	}
	if a.String() >= b.String() {
		t.Fatalf("%s sorts after %s: UUIDv7s must sort by time", a, b)
	}
	if New() == New() {
		t.Fatal("two identical ids")
	}
}
