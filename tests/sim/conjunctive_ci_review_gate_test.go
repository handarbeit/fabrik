package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// conjunctiveGateCheck is the required-context name standing in for the live
// suite's "slow-gate" check.
const conjunctiveGateCheck = "slow-gate"

// conjunctiveGateStages: Validate carries both wait_for_ci and
// wait_for_reviews (ADR-056 D2's conjunction under test). Review is left
// plain (no wait_for_reviews) — the live test's Review-stage gate exists
// purely to dodge a live-bed confound (an incidental bot review engaging
// Review's own gate before the held-out reviewer is requested; see the live
// file's "History" doc comment, #925) that has no sim analogue (simgh never
// synthesizes bot reviews — FIDELITY.md). Reproducing it here would add
// complexity without adding coverage of anything this port is about.
func conjunctiveGateStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 2},
		{Name: "Validate", Order: 3, WaitForCI: &tr, WaitForReviews: &tr},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

// TestConjunctiveCIReviewGate ports tests/e2e/conjunctive_ci_review_gate_test.go's
// TestConjunctiveCIReviewGate (ADR-056 D2, #895) — R1/R2/R4/R5(approval path).
// R3 (a PR comment posted during CI-await still receives its 👀 reaction) is
// NOT ported: it is really a property of itemNeedsWork's general new-comment
// admission, orthogonal to the CI/review conjunction this test exists to
// protect, and simgh's comment model does not distinguish a PR's own
// conversation thread from the linked issue's — porting it faithfully would
// require modelling a distinction that doesn't exist in the harness today.
// Recorded as a partial-port reason in the coverage matrix (R4).
//
// Sequence proven: Validate's dispatch defers stage:Validate:complete behind
// wait_for_ci (fabrik:awaiting-ci applied, R1) while no check run has been
// seeded yet — genuinely held, not vacuously clear, because
// SeedRequiredContexts is registered before anything else (see
// ci_fix_reinvoke_test.go's SeedRequiredContexts comment for why this
// matters). Once CI is seeded green, the CI gate clears and
// wait_for_reviews engages (fabrik:awaiting-review, R2) — the issue does not
// close while that gate holds even though stage:Validate:complete is
// already present (R4; matches the live test's #890 note: CI-gate-clear and
// review-gate-engage are not simultaneous with landing). An APPROVE review
// then clears the review gate too, and (direct-merge fallback) the issue
// closes with stage:Validate:complete present and fabrik:paused never
// applied (R5, approval path).
func TestConjunctiveCIReviewGate(t *testing.T) {
	t.Parallel()
	env := newConjunctiveGateEnv(t, 15*time.Minute, 5) // production defaults; neither must fire during this test

	num := FileIssue(t, env, "sim conjunctive ci+review gate", "Prove ADR-056 D2's conjunctive CI+review gate.", "Implement")

	WaitForIssueLabel(t, env, num, "stage:Implement:complete", 80)
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)

	// R1: fabrik:awaiting-ci applied and genuinely held — no check run has
	// been seeded yet, so stage:Validate:complete must not appear.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	if hasLabel(IssueLabels(t, env, num), "stage:Validate:complete") {
		t.Fatal("stage:Validate:complete present while the CI gate should still be holding")
	}

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR")
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	// R2: CI gate clears, review gate engages.
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-ci", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)

	// R4: the issue must not close while the review gate holds, even though
	// stage:Validate:complete may already be present (current engine
	// behavior, matching the live test's #890 note — not a bug this port
	// should assert against).
	RunPolls(t, env, 10)
	if projectItem(t, env, num).IsClosed {
		t.Fatal("issue closed while the review gate should still be holding")
	}

	// R5 (approval path): an APPROVE review clears the gate; yolo lands the
	// PR via the direct-merge fallback.
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
	WaitForIssueClosed(t, env, num, 80)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") {
		t.Fatal("fabrik:paused applied — review gate should have cleared via approval, not timed out")
	}
	if !hasLabel(labels, "stage:Validate:complete") {
		t.Fatal("stage:Validate:complete missing at close")
	}
	t.Logf("%s#%d: conjunctive CI+review gate held both halves independently, then cleared on approval — R1/R2/R4/R5 verified", env.OwnerRepo, num)
}

// TestConjunctiveCIReviewGate_ReviewNeverArrives is this port's AC3
// non-vacuity proof for R4, and a narrower claim than the live test's R5
// timeout fallback: it shows the review-gate half of the conjunction is
// independently load-bearing by never satisfying it and confirming the
// issue genuinely never lands over a substantial number of polls — rather
// than chasing the live test's exact fabrik:paused/fabrik:awaiting-input
// escalation, which this port could not reproduce deterministically. See
// FIDELITY.md for why, and the coverage matrix (R4) for this scope note.
//
// What was tried and abandoned: checkAwaitingReviewTimeout compares REAL
// time.Since(appliedAt) against cfg.ReviewWaitTimeout, where appliedAt comes
// from the engine's own label-applied-at write-through cache
// (recordLabelAppliedAtNow, engine/mutate.go) stamped via e.now() — the same
// injected Clock RunPoll advances by a full simulated PollInterval (1s
// default) per poll, against a real per-poll wall-clock cost (workerYield)
// of ~100ms. That mismatch alone is worked around elsewhere in this package
// (freezing PollInterval, or backdating StartTime when nothing else needs
// the clock to behave normally first — see ci_fix_reinvoke_test.go and this
// file's now-removed timeout attempt). Here it was not sufficient: even a
// large bounded real-time budget with the Clock frozen once
// fabrik:awaiting-review was confirmed present did not reach fabrik:paused.
// Whether that is a genuine simgh/harness gap in this specific gate's
// timeout path or a further clock-interaction subtlety not yet root-caused
// is left as an open question in FIDELITY.md rather than guessed at.
func TestConjunctiveCIReviewGate_ReviewNeverArrives(t *testing.T) {
	t.Parallel()
	env := newConjunctiveGateEnv(t, 15*time.Minute, 5) // must not fire during this test — see main test's comment

	num := FileIssue(t, env, "sim conjunctive gate (review never arrives)", "Prove the review-gate half blocks independently.", "Implement")

	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)

	// No reviewer ever responds. Drive a substantial number of further polls
	// and confirm the gate is still genuinely holding — not vacuously, and
	// not because it silently gave up and landed anyway.
	RunPolls(t, env, 50)
	item := projectItem(t, env, num)
	if item.IsClosed {
		t.Fatal("issue closed with no review ever submitted — expected OPEN")
	}
	if !hasLabel(item.Labels, "fabrik:awaiting-review") {
		t.Fatal("fabrik:awaiting-review no longer present after 50 further polls with no review ever submitted")
	}
	t.Logf("%s#%d: still open with fabrik:awaiting-review after 50 further polls, no review ever submitted — proves the review half is independently load-bearing", env.OwnerRepo, num)
}

// newConjunctiveGateEnv is the shared setup for both tests above.
//
// reviewWaitTimeout/maxReviewCycles are passed explicitly rather than left to
// default: like MaxCiFixCycles (see ci_fix_reinvoke_test.go),
// engine.Config.ReviewWaitTimeout and .MaxReviewCycles have NO built-in
// fallback inside the engine package — cmd/root.go's flag resolution is what
// maps an unset 0 to production's defaults (15 minutes, 5 cycles), and
// NewEnv's Config literal bypasses that entirely. Left unset,
// checkAwaitingReviewTimeout's `time.Since(appliedAt) >= 0` and
// reviewGateBlocksLanding's `cycleCount(0) >= 0` are both trivially true
// instantly — an immediate, false pause. The main test needs both
// comfortably large (never fires); the timeout test needs both small enough
// to fire within an ordinary real-time poll budget WITHOUT collapsing back
// to the instant-zero-cycle case (reviewGateBlocksLanding's escalation
// ladder waits out reviewWaitTimeout once per cycle, so the real cost is
// roughly maxReviewCycles × reviewWaitTimeout — both need to be small, not
// just one) — see that test's own doc comment for why StartTime-backdating
// (this package's usual zero-real-wait timeout technique) doesn't work here.
func newConjunctiveGateEnv(t *testing.T, reviewWaitTimeout time.Duration, maxReviewCycles int) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages: conjunctiveGateStages(),
		// See ci_fix_reinvoke_test.go's StartTime comment: real-time-anchored
		// gate branches need StartTime near time.Now(), not the package
		// default (2026-01-01), or they fire instantly and falsely.
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = reviewWaitTimeout
			cfg.MaxReviewCycles = maxReviewCycles
		},
	})
	// Direct-merge fallback (matches smoke_test.go/ci_fix_reinvoke_test.go):
	// deterministic landing once both gates clear.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	// Load-bearing, not cosmetic — see ci_fix_reinvoke_test.go's
	// SeedRequiredContexts comment: without it, the CI gate's mergeable_state
	// derivation is vacuously "clean" before any check run is seeded,
	// defeating R1's "genuinely held" assertion.
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{conjunctiveGateCheck})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}
