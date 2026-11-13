package jsonscan

import (
	"runtime"
	"strings"
	"testing"
)

// TestNoRetention: scanning a large body leaves no copy of it in the pool.
func TestNoRetention(t *testing.T) {
	body := []byte(`{"model":"x","messages":"` + strings.Repeat("a", 8<<20) + `"}`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := Object(body, func(_ []byte, d *Decoder) error { return d.SkipValue() }); err != nil {
		t.Fatal(err)
	}
	runtime.GC() // the pool's entries survive one GC, as its victims
	runtime.ReadMemStats(&after)
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 1<<20 {
		t.Fatalf("the heap grew %d KiB scanning an 8 MiB body: the pool keeps a copy", grew>>10)
	}
	runtime.KeepAlive(body)
}
