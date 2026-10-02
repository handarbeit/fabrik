//go:build e2e

package e2e

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/seedspec"
)

// TestConvergenceRace is the deterministic provocation test for the
// post-Validate auto-merge race covered by handarbeit/fabrik#829 (Story 2 /
// SC-002).
//
// The test bed's CI workflow has a required "slow-gate" job that sleeps for
// ~10 minutes when the PR body contains the literal string "slow-ci-required".
// We seed two yolo issues whose PRs BOTH add the same new file with different
// content and BOTH carry the marker. The arrangement makes the race deterministic:
//
//  1. Both issues are seeded at Validate-complete (#1992, ADR-1992) — the
//     Specify → Implement traversal was set-up, not subject.
//  2. Both PRs open against main from the same base SHA.
//  3. Validate is complete on both (after both slow-gates are green); engine enables GitHub native auto-merge on
//     both PRs (fabrik:auto-merge-enabled is applied).
//  4. Both PRs wait on the 10-minute slow-gate check (before being exposed).
//  5. Whichever merges first lands atomically via GitHub auto-merge. Main
//     now has the marker file.
//  6. The OTHER PR is now "behind main" AND has a true add/add conflict on
//     the same path — mergeable=CONFLICTING. Fabrik must observe this and
//     dispatch a single rebase reinvoke (NOT a CI-fix reinvoke, NOT a stream
//     of spurious cycles bounded by MaxRebaseCycles/MaxCiFixCycles).
//  7. Claude resolves the conflict (keeps both markers), pushes; auto-merge
//     re-enables; the slow-gate runs again; the second PR merges; the issue
//     closes.
//
// The above (steps 3-7) is the train mode "off" contract. Under train mode
// "on" (ADR-059), Validate completion for a yolo item diverts to
// advanceToQueued before the fabrik:auto-merge-enabled label site is ever
// reached (engine/merge_gate.go:230 — by design, non-queue repos never get
// that label under the train), so the two conflicting PRs never race a
// native GitHub auto-merge. Instead both members land via the train's own
// batch/singleton landing path, and any textual conflict between them is
// resolved inline while assembling the trial branch (ADR-059 D3) rather
// than via a rebase-reinvoke. The pass criteria under "on" is therefore
// evidence that the train's own contention path doesn't stall — both
// issues still land within budget, neither ends fabrik:paused, and
// fabrik:auto-merge-enabled is never applied — not a literal rebase-
// reinvoke assertion.
//
// Pass criteria:
//   - Both issues close within the wall-clock budget.
//   - Train mode "off": both had fabrik:auto-merge-enabled applied (FR-004).
//     Train mode "on": fabrik:auto-merge-enabled is never applied to either
//     (merge_gate.go:230).
//   - Neither ends in fabrik:paused (FR-013 was NOT triggered — convergence
//     succeeded within budget). True in both modes.
//
// This is the regression test for the production failure on
// example-org/example-repo#82 (spurious "CI fix cycle limit reached" on a
// post-Validate yolo PR whose only real problem was that main moved during
// its CI run). Before #829, this test would fail with the second PR stuck
// in fabrik:paused.
//
// Non-vacuity: neutralise the post-Validate convergence handling (the rebase
// reinvoke) and the second PR stays CONFLICTING, so it never closes within the budget.
// Under train mode "on" neutralise the trial-branch inline conflict resolution.
//
// Wall-clock: ~30-45 min (two slow-gate waits overlap; the rebase reinvoke is the one
// real Claude run). Cost: ~$0.30-1.
func TestConvergenceRace(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	trainMode := resolveTrainMode(t, env)
	t.Logf("bed train mode: %s", trainMode)

	stamp := time.Now().UTC().Format("20060102-150405")

	// Seeded at Validate-complete (#1992): the subject is the post-Validate auto-merge
	// race, so the Specify → Review traversal of both issues was set-up. Two yolo
	// issues each get a ready harness PR that adds the SAME new file with DIFFERENT
	// content — an add/add conflict (the merge-train conflict scenarios' precedent;
	// the Contents API cannot edit README.md's existing blob without its SHA) — and
	// whose body carries slow-ci-required, widening the merge window. They are seeded
	// without a Status, and exposed to the engine together once both slow-gates have
	// passed: Validate-complete only ever follows a green CI gate in a real traversal.
	// expected-reviewers:none keeps the review gate out of the way (review is not this
	// test's subject, and harness PRs opt out of Pruefer).
	type issuePair struct {
		title   string
		body    string
		content string
	}
	sharedPath := fmt.Sprintf("e2e/convergence/race-%s.md", stamp)
	pairs := []issuePair{
		{
			title:   fmt.Sprintf("e2e convergence-race A (%s)", stamp),
			body:    fmt.Sprintf(convergenceBodyTemplate, "A", stamp),
			content: fmt.Sprintf("# convergence race\n\n<!-- convergence-race-A-%s -->\n", stamp),
		},
		{
			title:   fmt.Sprintf("e2e convergence-race B (%s)", stamp),
			body:    fmt.Sprintf(convergenceBodyTemplate, "B", stamp),
			content: fmt.Sprintf("# convergence race\n\n<!-- convergence-race-B-%s -->\n", stamp),
		},
	}

	nums := make([]int, len(pairs))
	prs := make([]int, len(pairs))
	items := make([]string, len(pairs))
	var fileWg sync.WaitGroup
	for i := range pairs {
		fileWg.Add(1)
		go func(i int) {
			defer fileWg.Done()
			nums[i], prs[i], items[i] = seedAtStage(t, env, env.RepoAlpha, seedspec.Spec{
				Column:       "Validate",
				Title:        pairs[i].title,
				IssueBody:    pairs[i].body,
				ExtraLabels:  []string{"fabrik:yolo", expectedReviewersNoneLabel},
				Path:         sharedPath,
				PathMode:     seedspec.PathExact,
				Content:      pairs[i].content,
				PRBodySuffix: "\nslow-ci-required\n",
				DeferStatus:  true,
			})
		}(i)
	}
	fileWg.Wait()
	// A t.Fatalf inside the setup goroutines above ends only that goroutine,
	// not the test: before this check a GitHub blip during setup left a zero
	// issue number and the test then waited 90 minutes for "#0" to close
	// (0.0.83 gate run 13). Fail fast instead.
	for i, n := range nums {
		if n == 0 {
			// Deliberately a Fatalf, not Inconclusive (#1973): a setup ERROR is not a
			// precondition that "never arose" — it can equally be a permanent harness
			// bug, which an automatic retry would turn into "uncovered" where a FAIL is
			// the better signal. The underlying error is logged above.
			t.Fatalf("setup failed for contention issue %d (%q) — see the setup error above; not an engine regression, re-run", i, pairs[i].title)
		}
	}
	t.Logf("seeded contention pair: %s#%d (PR #%d) and %s#%d (PR #%d)", env.RepoAlpha, nums[0], prs[0], env.RepoAlpha, nums[1], prs[1])

	// Both slow-gates must be green before either item is visible to the engine, so
	// the two landings are contended from the first poll (the race this test exists
	// for) rather than staggered by CI start times.
	for _, pr := range prs {
		WaitForCheckConclusion(t, env, env.RepoAlpha, pr, "slow-gate", "success", 30*time.Minute)
	}
	for i := range nums {
		SetIssueStatus(t, env, items[i], "Validate")
	}
	for _, num := range nums {
		AwaitBoardItemVisible(t, env, env.RepoAlpha, num, awaitSeedTimeout)
	}
	t.Logf("both slow-gates green; both items exposed at Validate")

	// Both must reach closed (merged). Wait in parallel — one will close
	// well before the other; the second is the interesting one because it
	// had to rebase through a conflict.
	var closeWg sync.WaitGroup
	for _, num := range nums {
		closeWg.Add(1)
		go func(num int) {
			defer closeWg.Done()
			WaitForIssueClosedWithReviewCheck(t, env, env.RepoAlpha, num, 90*time.Minute)
			t.Logf("%s#%d closed", env.RepoAlpha, num)
		}(num)
	}
	closeWg.Wait()

	// The fabrik:auto-merge-enabled contract diverges by mode: under "off",
	// attemptMergeOnValidate enables GitHub's native auto-merge and applies
	// the label to both PRs (FR-004). Under "on", the same Validate
	// completion diverts to advanceToQueued before that label site is ever
	// reached, and the non-queue-repo path in merge_gate.go never applies it,
	// by design (engine/merge_gate.go:230) — see TestYoloAutoMergeLabel for
	// the reference pattern this mirrors.
	t.Run(fmt.Sprintf("train-mode=%s", trainMode), func(t *testing.T) {
		if trainMode == "on" {
			for _, num := range nums {
				AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
			}
			t.Logf("fabrik:auto-merge-enabled was never applied to either issue — train-on contract verified " +
				"(the conflicting pair resolved via the train's own inline trial-branch conflict resolution, " +
				"ADR-059 D3, rather than a native auto-merge rebase-reinvoke)")
			return
		}
		for _, num := range nums {
			AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
		}
		t.Logf("both issues had fabrik:auto-merge-enabled applied — FR-004 verified for both")
	})

	// Neither issue should have ended in fabrik:paused — that would mean
	// either the convergence budget exhausted (FR-013) OR a legacy cycle
	// limit fired (the bug #829 fixes). Either way, the test fails.
	for _, num := range nums {
		for _, l := range IssueLabels(t, env, env.RepoAlpha, num) {
			if l == "fabrik:paused" {
				t.Fatalf("%s#%d ended in fabrik:paused — convergence failed (regression of #829)",
					env.RepoAlpha, num)
			}
		}
	}
	t.Logf("neither issue ended paused — bounded convergence verified (SC-002)")
}

// convergenceBodyTemplate is the issue body for TestConvergenceRace. Two
// %s placeholders: a single-letter discriminator (A or B), and a timestamp
// shared between the pair so the marker lines are unique per run but the two
// issues collide on the same file.
//
// The conflict is deterministic because the harness seeds both PRs adding the SAME
// new file with different content (#1992); nothing depends on where an agent
// chooses to put a line.
//
// The seeded PR bodies carry the literal string "slow-ci-required". The test repo's
// CI workflow keys on that string to enable the 10-minute slow-gate.
const convergenceBodyTemplate = `## Goal

Deterministic provocation of the post-Validate auto-merge race covered by
handarbeit/fabrik#829. This issue is one of a deliberately-conflicting
pair seeded by the e2e harness.

## State of this issue

The e2e harness has already implemented, reviewed and validated this issue: a pull
request is open on this issue's branch that adds a new marker file under
e2e/convergence/ containing the line:

    <!-- convergence-race-%s-%s -->

(An HTML comment with the discriminator and a shared timestamp.) This issue's pair
partner has a PR adding the SAME file with a different discriminator, so the two PRs
conflict textually the moment one of them merges.

If a rebase reinvoke fires because the other pair member's marker is already on the
base branch, RESOLVE the conflict by keeping BOTH marker lines, in either order, and
push.

## CI behaviour

The PR body already carries the literal marker ` + "`slow-ci-required`" + `, which fires
the test repo's CI slow-gate (~10 minutes). Do not edit the PR body.

## Scope

Single repo. No decomposition. No additional files.
`
