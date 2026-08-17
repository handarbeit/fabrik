package sim

import (
	"context"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/stages"
)

// Gap 2 (#1592): GraphQL rate-limit backoff and the REST hard gate
// (engine/backoff.go) across successive polls. Both are driven exclusively
// through Engine.PollWithBackoff (ADR-1592) — the seam this issue adds,
// mirroring RegisterObservers above and PollOnce/ADR-1449: backoff.go's five
// pure functions have no call site outside Run()'s own closure, so without
// this seam gap 2 is 100% unreachable from tests/sim regardless of what a
// scenario would want to assert.
//
// Control surface (R3, confirmed during Research against backoff.go's
// actual call sites): SeedRateLimits/tests/sim/simgh's static rate-limit
// budgets, not fault injection — RateLimitStats is documented in
// simgh/FIDELITY.md as the one interface method fault injection cannot
// fail, and backoff.go consumes budget *ratios*, not a failing call.

// backoffStages is deliberately a single, never-dispatched stage: both
// scenarios below never file an issue at all (an empty board), since
// PollWithBackoff's rate-limit bookkeeping is entirely independent of board
// content — only NewEnv's own Stages requirement needs satisfying.
func backoffStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
	}
}

// runPollWithBackoff drives exactly one Engine.PollWithBackoff call — the
// gap-2 analogue of poll.go's own RunPoll, which drives PollOnce instead.
// Advances env.Clock by env.PollInterval first, mirroring RunPoll's own
// reasoning (CooldownAt-based gates must not see a frozen clock), then waits
// for worker quiescence exactly as RunPoll does — no scenario in this file
// dispatches a worker, but a future one reusing this helper safely might.
func runPollWithBackoff(t *testing.T, env *Env, configuredInterval time.Duration) engine.PollBackoffResult {
	t.Helper()
	if env.PollInterval > 0 {
		env.Clock.Advance(env.PollInterval)
	}
	result, err := env.Engine.PollWithBackoff(context.Background(), configuredInterval)
	if err != nil {
		t.Fatalf("PollWithBackoff: %v", err)
	}
	if env.Engine.HasInFlightWorker() {
		waitForWorkerQuiescence(t, env)
	} else {
		time.Sleep(workerYield)
	}
	return result
}

// TestBackoff_GraphQLRateLimitIntervalEscalatesAndRecovers is AC3's primary
// scenario: seeded GraphQL rate-limit ratios, read fresh on each
// PollWithBackoff call via SeedRateLimits, drive computeEffectiveInterval's
// full escalation schedule (>=10%: 2x, >=5%: 4x, >=1%: 6x, <1%: 10x) across
// successive polls, then the two-threshold hysteresis (nextRateLimitLow:
// activate <20%, clear only >50% — the sticky zone in between holds the
// backoff active) on the way back down.
//
// Non-vacuity (R5): the sticky-zone poll (30% remaining, asserted at exactly
// 2x rather than 1x) is the discriminating step — an implementation that
// dropped the hysteresis (cleared backoff the instant ratio rose above the
// 20% activation threshold, rather than requiring 50%) would compute 1x
// there instead of 2x, and every other step in this sequence would still
// pass. Confirmed by temporarily neutralizing nextRateLimitLow's hysteresis
// (forcing it to clear whenever ratio >= rateLimitBackoffThreshold instead
// of requiring rateLimitHealthyThreshold) and observing this assertion fail
// while the rest of the sequence kept passing.
func TestBackoff_GraphQLRateLimitIntervalEscalatesAndRecovers(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: backoffStages()})
	configuredInterval := env.PollInterval

	// Baseline: full budget, no backoff.
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 5000)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != configuredInterval {
		t.Fatalf("baseline: NextInterval = %v, want configuredInterval %v", r.NextInterval, configuredInterval)
	}

	// Activate: 18% remaining (< 20% threshold), falls in the >=10% bracket (2x).
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 900)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != 2*configuredInterval {
		t.Fatalf("18%% remaining: NextInterval = %v, want 2x configuredInterval (%v)", r.NextInterval, 2*configuredInterval)
	}

	// 8% remaining: >=5% and <10% bracket (4x).
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 400)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != 4*configuredInterval {
		t.Fatalf("8%% remaining: NextInterval = %v, want 4x configuredInterval (%v)", r.NextInterval, 4*configuredInterval)
	}

	// 4% remaining: >=1% and <5% bracket (6x).
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 200)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != 6*configuredInterval {
		t.Fatalf("4%% remaining: NextInterval = %v, want 6x configuredInterval (%v)", r.NextInterval, 6*configuredInterval)
	}

	// 0.8% remaining: <1% bracket, the max multiplier (10x).
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 40)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != 10*configuredInterval {
		t.Fatalf("0.8%% remaining: NextInterval = %v, want 10x configuredInterval (%v)", r.NextInterval, 10*configuredInterval)
	}

	// 30% remaining: ABOVE the 20% activation threshold, but backoff is
	// already active and 30% is still below the 50% healthy threshold — the
	// hysteresis's sticky zone. Must stay active (2x, the >=10% bracket for
	// the current 30% ratio), not clear to 1x. This is the discriminating
	// assertion (see the doc comment above).
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 1500)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != 2*configuredInterval {
		t.Fatalf("30%% remaining (sticky zone): NextInterval = %v, want 2x configuredInterval (%v) — hysteresis should still hold backoff active", r.NextInterval, 2*configuredInterval)
	}

	// 60% remaining: above the 50% healthy threshold — clears.
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 3000)
	if r := runPollWithBackoff(t, env, configuredInterval); r.NextInterval != configuredInterval {
		t.Fatalf("60%% remaining: NextInterval = %v, want configuredInterval %v (fully recovered)", r.NextInterval, configuredInterval)
	}
}

// TestBackoff_RESTHardGateSkipsPollUntilReset is AC3's second scenario:
// when the REST/core budget is exhausted, PollWithBackoff must skip the
// entire poll work phase (no FetchProjectBoard call at all) until the
// budget is reported healthy again — modelling GitHub's hourly reset, since
// simgh's RateLimitStats always reports Reset relative to the current
// (injected) clock (see tests/sim/simgh/misc.go), not a fixed instant a
// scenario could simply wait out.
//
// Non-vacuity (R5): the discriminating assertion is the FetchProjectBoard
// call count staying flat across the paused poll and only increasing on the
// resumed one — a broken hard gate that let poll() run anyway would still
// leave NextInterval computable (masking a terminal-state-only check) but
// would fail this call-count assertion. Confirmed by temporarily
// neutralizing shouldPauseForRESTRateLimit (forcing it to always return
// false) and observing the "paused poll makes no FetchProjectBoard call"
// assertion fail.
func TestBackoff_RESTHardGateSkipsPollUntilReset(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: backoffStages()})
	configuredInterval := env.PollInterval

	boardFetches := func() int {
		return len(env.Sim.Log().ByMethod("FetchProjectBoard"))
	}

	// Establish a baseline poll so the board-fetch counter has a known
	// starting point.
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 5000)
	runPollWithBackoff(t, env, configuredInterval)
	before := boardFetches()
	if before == 0 {
		t.Fatal("baseline poll made no FetchProjectBoard call — seeding/assumption broken")
	}

	// REST budget exhausted (remaining at 0, well within the near-zero
	// threshold) — the hard gate must skip poll() entirely this cycle.
	env.Sim.Sim().SeedRateLimits(5000, 0, 5000, 5000)
	r := runPollWithBackoff(t, env, configuredInterval)
	if got := boardFetches(); got != before {
		t.Fatalf("FetchProjectBoard call count changed (%d -> %d) while REST-paused — the hard gate should have skipped poll() entirely", before, got)
	}
	// NextInterval must point at (approximately) the REST reset, not the
	// configured interval — SeedRateLimits fixes Reset one hour out from the
	// simulated clock's current instant (see simgh's RateLimitStats).
	if r.NextInterval < 55*time.Minute || r.NextInterval > time.Hour+time.Minute {
		t.Fatalf("NextInterval = %v while REST-paused, want ~1h (time until Reset + rateLimitResetBuffer)", r.NextInterval)
	}

	// The budget "resets" (a fresh SeedRateLimits call, modelling GitHub's
	// hourly rollover — see this test's own doc comment for why the
	// scenario cannot simply wait out simgh's Reset timestamp). Work must
	// resume: poll() runs again, and NextInterval returns to the configured
	// baseline (GraphQL is healthy too, so no rate-limit backoff applies).
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 5000)
	r = runPollWithBackoff(t, env, configuredInterval)
	if got := boardFetches(); got <= before {
		t.Fatalf("FetchProjectBoard call count did not increase (%d -> %d) after the REST budget reset — work should have resumed", before, got)
	}
	if r.NextInterval != configuredInterval {
		t.Fatalf("NextInterval after REST reset = %v, want configuredInterval %v", r.NextInterval, configuredInterval)
	}
}
