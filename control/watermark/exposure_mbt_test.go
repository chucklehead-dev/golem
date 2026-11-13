package watermark

import (
	"fmt"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"
)

// The job against model/exposure.md's publishSnapshot: the model's traces
// run as they are, and at each publication the job is given the model's
// ledger (its rows carry their numbers; here they arrive as rows and
// numbering records) and must compute the model's watermark and spend.
// The traces hold out-of-order deliveries, failovers that hand numbers out
// again, and snapshots taken at any point. Every other action is the
// environment.

type mrow struct {
	Event string `itf:",tuple"`
	Cost  int64
	Seq   int64
}

type msnap struct {
	W, Spend int64
}

type jobDriver struct {
	t   testing.TB
	job *Job
}

// gapped counts the publications made with a gap in the ledger (a number
// above the mark ingested): the case a wrong mark would get wrong.
var gapped, published atomic.Int64

var site = Key{"site", "s", "2026-11"}

func (d *jobDriver) Step(s *connect.Step) error {
	skip := connect.Switch{}
	for _, a := range []string{"admit", "complete", "cancel", "providerEnds", "deliver", "applySnapshot",
		"valkeyDown", "valkeyBack", "replayOne", "recover", "graceEnds", "failover"} {
		skip[a] = connect.Skip
	}
	skip["init"] = func(*connect.Step) error {
		d.job = &Job{GapTimeout: 1000 * time.Hour, Now: func() time.Time { return time.Unix(0, 0) }} // the model has no gap timeout
		return nil
	}
	skip["publishSnapshot"] = func(s *connect.Step) error {
		var ledger []mrow
		var latest msnap
		if err := itf.Decode(s.Get("ledger"), &ledger); err != nil {
			return err
		}
		if err := itf.Decode(s.Get("latest"), &latest); err != nil {
			return err
		}
		var rows []Row
		var nums []Numbering
		for _, r := range ledger {
			rows = append(rows, Row{Event: r.Event, Key: site, Cost: r.Cost, Counted: true, At: time.Unix(0, 0)})
			nums = append(nums, Numbering{Event: r.Event, Key: site, Seq: r.Seq})
		}
		res, _, err := d.job.Run(d.t.Context(), rows, nums)
		if err != nil {
			return err
		}
		published.Add(1)
		for _, r := range ledger {
			if r.Seq > latest.W {
				gapped.Add(1)
				break
			}
		}
		got := msnap{}
		if len(res) == 1 {
			got = msnap{res[0].Watermark, res[0].Spend}
		}
		if got != latest {
			return fmt.Errorf("the job's snapshot %+v, the model's %+v (ledger %v)", got, latest, ledger)
		}
		return nil
	}
	return skip.Step(s)
}

// State projects nothing: the check is publishSnapshot's own.
func (d *jobDriver) State() (any, error) { return struct{}{}, nil }

func TestExposureModel(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 500, MaxSteps: 30},
		func(tb testing.TB) connect.Driver { return &jobDriver{t: tb} })
	t.Logf("%d publications, %d with a gap", published.Load(), gapped.Load())
	if gapped.Load() == 0 {
		t.Error("no publication was made with a gap in the ledger: the traces did not exercise the mark")
	}
}
