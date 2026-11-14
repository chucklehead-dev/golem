package bucket_test

import (
	"testing"

	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/bucket/buckettest"
)

func TestDir(t *testing.T) {
	d, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	buckettest.Run(t, d)
	for _, k := range []string{"../x", "/abs", "a/../../x", "a//b", ""} {
		if _, err := d.Put(t.Context(), k, nil, ""); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
}
