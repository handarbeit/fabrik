package sim

import (
	"testing"
)

// This file is R6: a multi-issue board driven with MaxConcurrent > 1, run
// under `go test -race` as part of this package's default suite — the
// cheapest continuous race coverage the project can have, per the issue's
// own framing. It exercises the poll loop's worker semaphore
// (engine/poll.go), the processedSet mutex, the in-flight sync.Map, and the
// serialized worktree-creation mutex (engine/worktree.go) all at once,
// against genuinely interleaved mutations from six issues in flight at
// once, rather than one issue at a time the way every other scenario in
// this package runs.
//
// Deliberately failureShapeStages() (Specify -> Done), not a PR-creating
// pipeline: acquireLockAndVerify's lockVerifyDelay (engine/item.go, 2s) is
// a genuine real-wall-clock sleep on *every* dispatch, deliberately not
// folded into this harness's injected Clock (see poll.go's own doc comment
// on why — it exists to let a different Fabrik process observe a competing
// lock), and each issue's own worktree creation is real git-subprocess work
// serialized by a mutex (CLAUDE.md's "Worktree creation serialized by
// mutex"). Two earlier drafts of this scenario — six issues through the
// full 7-stage smokeStages() pipeline under MaxConcurrent=4, then a
// 3-stage Specify->Implement->Validate->Done pipeline under
// MaxConcurrent=6 — both measured at or beyond 100 real wall-clock seconds
// before settling on the single-dispatch pipeline below. R6's actual
// target (genuine concurrent contention across processedSet/the in-flight
// map/the worktree mutex) needs real interleaving, not pipeline depth or a
// merged PR at the end; the identical dispatch/lock/worktree machinery is
// exercised on every single stage dispatch regardless of how many stages a
// pipeline has or what they do.
//
// Non-vacuity (AC8): a regression that broke the worktree-creation mutex or
// the in-flight dedup would not necessarily fail this test's own
// assertions (every issue still eventually reaches Done even with some
// corruption) — the actual value of this scenario is `-race` itself
// catching a genuine data race, which is why it must run as part of the
// package's default (non-opt-in) suite rather than behind a separate flag
// or build tag (see tests/sim/README.md's own "no sim build tag" stance).
// The per-issue distinct-worktree-path assertion below is this scenario's
// own additional, weaker-but-still-real non-vacuity check: independent of
// -race, a severe enough worktree mix-up would show up as two issues
// sharing a marker file's issue-number reference.

// concurrencyIssueCount is deliberately smaller than "many" might suggest —
// six is enough to force real contention against MaxConcurrent (below),
// while keeping this scenario's wall-clock cost bounded (see
// tests/sim/README.md's runtime ceiling and this file's own doc comment on
// why pipeline depth, not issue count, was the actual runtime problem).
const concurrencyIssueCount = 6

// TestConcurrency_MultiIssueConvergence files concurrencyIssueCount issues
// onto one board with MaxConcurrent equal to the issue count (maximum real
// overlap — every issue can be in flight at once, rather than queueing
// behind a smaller worker pool) and asserts every one of them independently
// reaches Done and closes, each via its own genuinely distinct dispatch —
// no cross-issue interference from running under real concurrency.
func TestConcurrency_MultiIssueConvergence(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:        failureShapeStages(),
		MaxConcurrent: concurrencyIssueCount,
	})

	nums := make([]int, concurrencyIssueCount)
	for i := range nums {
		nums[i] = FileIssue(t, env, "sim concurrency", "Prove multi-issue convergence under contention.", "Specify")
	}
	t.Logf("filed %d issues: %v", len(nums), nums)

	// A single shared AdvanceUntil (rather than one per issue in sequence)
	// is what actually drives every issue's work interleaved within the
	// same poll cycles — polling issue-by-issue to convergence would
	// serialize them at the test level even though MaxConcurrent allows
	// real overlap engine-side.
	//
	// Checked via stage:Specify:complete, not IsClosed/Status: for this
	// minimal 2-stage pipeline, closing the issue and archiving its board
	// item off the project board happen in the same settle pass (there is
	// no PR/merge step to separate them in time the way smokeStages()'s
	// full pipeline does) — a race first caught by this exact scenario:
	// projectItem itself fails loudly ("not on the project board") once an
	// item archives, so polling for IsClosed can lose the race against
	// archival entirely. The completion label is applied earlier and
	// persists regardless of board membership, so it is the stable signal
	// this scenario actually needs: "did every issue's own dispatch
	// genuinely complete."
	AdvanceUntil(t, env, func(env *Env) bool {
		for _, num := range nums {
			if !hasLabel(IssueLabels(t, env, num), "stage:Specify:complete") {
				return false
			}
		}
		return true
	}, 200)

	// Non-vacuity: exactly one Specify dispatch total, across all six
	// issues combined (StageCallCount has no per-issue breakdown — it
	// counts every call to the stage regardless of which issue). A
	// processedSet/in-flight-dedup regression severe enough to
	// double-dispatch some issue under real concurrent contention would
	// push this above concurrencyIssueCount, even though every issue still
	// independently reached Done (the corruption this scenario's own doc
	// comment says its terminal-state assertions alone would not catch).
	if got := env.Claude.StageCallCount("Specify"); got != concurrencyIssueCount {
		t.Errorf("StageCallCount(Specify) = %d, want exactly %d — a mismatch means some issue was dispatched more than once despite the real concurrent contention", got, concurrencyIssueCount)
	}
	t.Logf("all %d issues converged to Done via exactly %d total Specify dispatches", len(nums), concurrencyIssueCount)
}
