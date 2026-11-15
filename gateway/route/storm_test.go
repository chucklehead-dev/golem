package route

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/qload"
)

// Retry storms (m1-plan.md step 2; testing.md §3.2): an SDK's retries on
// top of the gateway's, against a provider whose capacity drops for a
// while (a brownout). The gateway's retries are route.Health's, run on
// qload's virtual time: each client attempt is a gateway call (Called),
// and the gateway's second try takes a retry from the route's budget
// (Retry). The contrast is a gateway that always retries once (no budget).
//
// qload's retries cannot tell a refusal from a timeout, and the gateway
// retries only refusals (an uncertain attempt ends the call), so here it
// retries both: the storm it adds is at most this one.

const (
	sdkRetries   = 3 // the Anthropic and OpenAI SDKs' default
	gatewayTries = 2 // a route's default maxAttempts
)

// sdkBackoff is the SDKs' default: 0.5 s doubling to at most 8 s, with
// jitter.
func sdkBackoff(n int, r *rand.Rand) time.Duration {
	d := min(500*time.Millisecond<<(n-1), 8*time.Second)
	return time.Duration(float64(d) * (0.75 + 0.25*r.Float64()))
}

// layers is an SDK's policy with the gateway's tries inside each of its
// attempts. Retry n is the gateway's (at once) when it is the second try
// of a client attempt and the gateway retries; otherwise the client's,
// after its backoff, up to sdkRetries. Without per-request state (qload's
// Retrier has none), a client attempt whose gateway retry the budget
// refused counts its next retry as the client's too: the gateway's retries
// are undercounted there, never its client's.
type layers struct {
	rnd     *rand.Rand
	h       *Health // nil: the gateway always retries once
	alias   string
	gateway int // retries the gateway made
	calls   int // gateway calls: client attempts
}

func (l *layers) Request(time.Time) { l.call() }

func (l *layers) call() {
	l.calls++
	if l.h != nil {
		l.h.Called(l.alias)
	}
}

func (l *layers) Retry(n int, _ time.Time) (time.Duration, bool) {
	if n%gatewayTries == 1 && (l.h == nil || l.h.Retry(l.alias)) {
		l.gateway++
		return 0, true
	}
	if n/gatewayTries+1 > sdkRetries+1 {
		return 0, false
	}
	l.call()
	return sdkBackoff(n/gatewayTries+1, l.rnd), true
}

// stormConfig is a provider of 4 workers at 3 calls/s each (12/s),
// offered lambda calls/s, with the SDK's timeout of 10 s.
func stormConfig(lambda float64, policy func(*rand.Rand) qload.Retrier) qload.SimConfig {
	return qload.SimConfig{
		Server: qload.ServerConfig{Name: "provider", Workers: 4, ServiceRate: 3, QueueCap: 100},
		Client: qload.ClientConfig{Name: "sdk", Rate: qload.Constant(lambda), Timeout: 10 * time.Second,
			Retries: gatewayTries*(sdkRetries+1) - 1, Retrier: policy},
	}
}

// brownout is the provider at a third of its speed for two minutes.
var brownout = qload.Trigger{Slowdown: 3, Duration: 2 * time.Minute}

// TestRetryBudgetBoundsTheStorm: through a brownout, the gateway's retries
// stay within its budget's share of the calls it was given (RetryRatio, a
// fifth, with its burst), where a gateway that always retries once adds a
// retry to about half of them; with the budget, every run recovers after
// the brownout (a provider whose queue drains within the SDK's timeout).
//
// What the gateway's retries do not decide: on a provider whose queue
// outlasts the timeout (300 at 12 calls/s), the SDK's own retries keep
// every policy down, the SDK's alone included; near the tipping point (a
// queue of 120) recovery is a coin toss for all three, with no ordering
// between them over six seeds. So the claim is the amplification, which
// holds everywhere, not a recovery the gateway's layer does not control.
func TestRetryBudgetBoundsTheStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("each policy runs several simulations of ~10 minutes")
	}
	const seeds, lambda = 6, 9
	run := func(h func() *Health) (recovered int, ratio float64) {
		var gateway, calls int
		for seed := range uint64(seeds) {
			var l *layers
			cfg := stormConfig(lambda, func(r *rand.Rand) qload.Retrier {
				l = &layers{rnd: r, alias: "a", h: h()}
				return l
			})
			cfg.Seed = seed + 1
			r := qload.Recovers(cfg, lambda, brownout, 5*time.Minute)
			if r.Err != nil {
				t.Fatalf("seed %d: %v", seed, r.Err)
			}
			if r.Recovered {
				recovered++
			}
			gateway, calls = gateway+l.gateway, calls+l.calls
		}
		return recovered, float64(gateway) / float64(max(calls, 1))
	}
	budgetRecovered, budget := run(func() *Health { return &Health{} })
	_, fixed := run(func() *Health { return nil })
	t.Logf("budgeted: %d/%d recover, %.2f gateway retries a call; unbudgeted: %.2f a call", budgetRecovered, seeds, budget, fixed)
	if budget > 0.2+0.05 {
		t.Errorf("the budgeted gateway retried %.2f of its calls: want at most its ratio, 0.2, and its burst", budget)
	}
	if fixed < 2*budget {
		t.Errorf("the unbudgeted gateway retried %.2f of its calls, the budgeted %.2f: want it well above (else the brownout made no failures)", fixed, budget)
	}
	if budgetRecovered != seeds {
		t.Errorf("%d of %d runs recover with the budget: want all", budgetRecovered, seeds)
	}
}
