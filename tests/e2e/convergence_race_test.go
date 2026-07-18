//go:build e2e

package e2e

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConvergenceRace is the deterministic provocation test for the
// post-Validate auto-merge race covered by handarbeit/fabrik#829 (Story 2 /
// SC-002).
//
// The test bed's CI workflow has a required "slow-gate" job that sleeps for
// ~10 minutes when the PR body contains the literal string "slow-ci-required".
// We file two yolo issues that BOTH target the same anchor in the same file
// (the first line of README.md) and BOTH carry the marker. The arrangement
// makes the race deterministic:
//
//  1. Both issues flow through Specify → Research → Plan → Implement.
//  2. Both PRs open against main from the same base SHA.
//  3. Validate completes on both; engine enables GitHub native auto-merge on
//     both PRs (fabrik:auto-merge-enabled is applied).
//  4. Both PRs wait on the 10-minute slow-gate check.
//  5. Whichever finishes first merges atomically via GitHub auto-merge. Main
//     now has a new HTML comment line immediately after the first line.
//  6. The OTHER PR is now "behind main" AND has a true textual conflict at
//     the same anchor — mergeable=CONFLICTING. Fabrik must observe this and
//     dispatch a single rebase reinvoke (NOT a CI-fix reinvoke, NOT a stream
//     of spurious cycles bounded by MaxRebaseCycles/MaxCiFixCycles).
//  7. Claude resolves the conflict (keeps both markers), pushes; auto-merge
//     re-enables; the slow-gate runs again; the second PR merges; the issue
//     closes.
//
// Pass criteria:
//   - Both issues close within the wall-clock budget.
//   - Both had fabrik:auto-merge-enabled applied (FR-004).
//   - Neither ends in fabrik:paused (FR-013 was NOT triggered — convergence
//     succeeded within budget).
//
// This is the regression test for the production failure on
// example-org/example-repo#82 (spurious "CI fix cycle limit reached" on a
// post-Validate yolo PR whose only real problem was that main moved during
// its CI run). Before #829, this test would fail with the second PR stuck
// in fabrik:paused.
//
// Wall-clock: ~80-100 min. Cost: ~$2-4.
func TestConvergenceRace(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")

	// Two issues, deliberately conflicting. Filed concurrently so the
	// dispatcher picks them up on the same poll where possible.
	type issuePair struct {
		title string
		body  string
	}
	pairs := []issuePair{
		{
			title: fmt.Sprintf("e2e convergence-race A (%s)", stamp),
			body:  fmt.Sprintf(convergenceBodyTemplate, "A", stamp),
		},
		{
			title: fmt.Sprintf("e2e convergence-race B (%s)", stamp),
			body:  fmt.Sprintf(convergenceBodyTemplate, "B", stamp),
		},
	}

	nums := make([]int, len(pairs))
	var fileWg sync.WaitGroup
	for i := range pairs {
		fileWg.Add(1)
		go func(i int) {
			defer fileWg.Done()
			num := FileIssue(t, env, env.RepoAlpha, pairs[i].title, pairs[i].body, "fabrik:yolo")
			itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
			SetIssueStatus(t, env, itemID, "Specify")
			nums[i] = num
		}(i)
	}
	fileWg.Wait()
	t.Logf("filed contention pair: %s#%d and %s#%d", env.RepoAlpha, nums[0], env.RepoAlpha, nums[1])

	// Both must reach closed (merged). Wait in parallel — one will close
	// well before the other; the second is the interesting one because it
	// had to rebase through a conflict.
	var closeWg sync.WaitGroup
	for _, num := range nums {
		closeWg.Add(1)
		go func(num int) {
			defer closeWg.Done()
			WaitForIssueClosed(t, env, env.RepoAlpha, num, 90*time.Minute)
			t.Logf("%s#%d closed", env.RepoAlpha, num)
		}(num)
	}
	closeWg.Wait()

	// Both PRs must have gone through the GitHub native auto-merge path.
	for _, num := range nums {
		AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
	}
	t.Logf("both issues had fabrik:auto-merge-enabled applied — FR-004 verified for both")

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
// shared between the pair so the resulting README lines are unique per run
// but collide on the same anchor between the two issues.
//
// CRITICAL: both issues MUST instruct Claude to insert the marker at the
// SAME anchor (the line immediately following "# fabrik-test-alpha"). That
// is what makes the textual conflict deterministic. If Claude is allowed
// freedom in where to put the line, the conflict may not materialise.
//
// The PR body MUST also contain the literal string "slow-ci-required". The
// test repo's CI workflow keys on that string to enable the 10-minute
// slow-gate that widens the merge-race window enough for #829's bug to
// manifest reliably.
const convergenceBodyTemplate = `## Goal

Deterministic provocation of the post-Validate auto-merge race covered by
handarbeit/fabrik#829. This issue is one of a deliberately-conflicting
pair filed by the e2e harness.

## The change

Insert exactly one new line into README.md immediately AFTER the line that
says "# fabrik-test-alpha". The new line must be EXACTLY:

    <!-- convergence-race-%s-%s -->

(That is: an HTML comment with the discriminator and a shared timestamp.)

Do NOT insert the line anywhere else. Do NOT modify any other file. The
position is load-bearing: this issue's pair partner inserts a different
discriminator at the same position, and the e2e test depends on the two
inserts producing a true textual conflict during rebase.

If, during rebase, you encounter the other pair member's marker already
present, RESOLVE the conflict by keeping BOTH lines, in either order.

## CI behaviour required

The PR body MUST carry the literal marker below so the test repo's CI
slow-gate fires:

slow-ci-required

This is what gives the auto-merge race a deterministic 10-minute window
during which main can move under us.

## Scope

Single repo. No decomposition. No additional files. Plan should be a
one-line edit and Implement should be a one-line commit.
`
