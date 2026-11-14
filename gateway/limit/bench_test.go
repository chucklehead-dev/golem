package limit

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkAdmitComplete is one call's admission and completion, from
// parallel goroutines, over the in-memory store and over Valkey (whose
// client pipelines concurrent calls on one connection), with a counter
// holding every minute of its retention. Admission reads the minutes from
// the snapshot's boundary on (ADR 0048): "current" is a gateway on a
// current snapshot (its boundary three minutes back), "unbounded" one with
// no boundary at all (the first snapshot), which reads the whole retention.
func BenchmarkAdmitComplete(b *testing.B) {
	for _, name := range []string{"mem", "valkey-current", "valkey-unbounded"} {
		b.Run(name, func(b *testing.B) {
			now := time.Now()
			var st Store = NewMem()
			if name != "mem" {
				st = newValkey(b, valkeyAddr(b))
			}
			l := New(st)
			period := l.period(now)
			var incs []Increment
			for m := range 360 {
				incs = append(incs, Increment{Scope: "s", Period: period, Cost: 1, At: now.Add(-time.Duration(m) * time.Minute)})
			}
			if err := st.AddAll(context.Background(), incs); err != nil {
				b.Fatal(err)
			}
			if name != "valkey-unbounded" {
				l.ApplySnapshot(context.Background(), "s", Known{Period: period, Since: now.Add(-3 * time.Minute)})
			}
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
