package props

import (
	"testing"

	"github.com/chucklehead-dev/gotogether/qobs"
	"github.com/chucklehead-dev/gotogether/qtest"
)

// TestStaleViewDuringOutage: during an outage a gateway admits from the
// counter as it last read it, which may predate other gateways' spend. The
// first version of model/exposure.md checked the true counter there, which
// no gateway can read while Valkey is down, and so rejected this run; the
// model now keeps each gateway's view (exposure.md §3.3), and the run, a
// waiver, conforms.
func TestStaleViewDuringOutage(t *testing.T) {
	qtest.RequireQuint(t)
	run := qtest.Record(t, "exposure.binding.yaml", func(rec *qobs.Recorder) {
		m := newLimMachine(qobs.WithRecorder(t.Context(), rec), limitFaithful)
		admit := func(r, g string) { m.admitScripted(r, g) }
		admit("b1", "g2") // g2 reads the counter: empty
		admit("a1", "g1")
		m.completeScripted("a1", 3)
		admit("a2", "g1")
		m.completeScripted("a2", 3) // the scope has spent 6 of 4
		m.RuleValkeyDown(nil)
		admit("c1", "g2") // g2's last view says 0
	})
	run.Expect(t, qtest.Conforms())
}
