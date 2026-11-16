package limit

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
)

// BenchmarkAdmitComplete is one call's admission and completion, from
// parallel goroutines, over the in-memory store and over Valkey (whose
// client pipelines concurrent calls on one connection): the slots taken
// with the snapshot's known spend checked, then freed.
func BenchmarkAdmitComplete(b *testing.B) {
	for _, name := range []string{"mem", "valkey"} {
		b.Run(name, func(b *testing.B) {
			var st Store = NewMem()
			if name != "mem" {
				st = newValkey(b, valkeyAddr(b))
			}
			l := New(st)
			var n atomic.Int64
			ctx := context.Background()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id := strconv.FormatInt(n.Add(1), 10)
					t, err := l.Admit(ctx, Call{ID: id, Principal: "p" + id[len(id)-1:], Scope: "s", Budget: 1 << 60,
						PrincipalLimit: 100000, ScopeLimit: 100000})
					if err != nil {
						b.Fatal(err)
					}
					t.Complete(ctx, 10)
				}
			})
		})
	}
}
