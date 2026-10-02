//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Focused auth tests (#1975, ADR-1975).
//
// The sparse gate runs every live test once, in the baseline cell, and re-runs in
// the other auth mode only the tests the registry marks auth: sensitive. These
// three exist so that set stays small: each seeds the minimum state and checks
// ONE identity or permission behaviour, instead of the long pipeline scenario it
// was extracted from re-running in a second auth mode just to repeat one
// assertion.
//
//   - TestAuthLockLabelShape      — the lock label the engine holds (ADR-1893);
//     new coverage, no source scenario.
//   - TestAuthEnginePRAuthorIdentity — the author of a PR the engine creates, and so
//     the credentialed push that precedes it (ADR-1846). Extracted from
//     TestConjunctiveCIReviewGate's AssertPRAuthorIsEngineIdentity call.
//   - TestAuthReviewGateIdentity  — the review gate engaging for a harness-authored
//     PR with a distinct requested reviewer. Extracted from the
//     AssertPRAuthorIsExpectedIdentity calls in review_authority_test.go and
//     expected_reviewers_test.go.
//
// All three are mode-agnostic: they detect the running leg (detectAuthLeg) and
// assert the shape that leg must produce, so each runs in whichever auth cells the
// sparse plan gives it (app/on baseline, pat/on).

// TestAuthLockLabelShape: while the engine holds an item it labels it
// fabrik:locked:<id>, where <id> is the PAT's login under a PAT and
// "<slug>-<6 hex>" under App auth (never the operator login).
//
// The lock is taken after checkDependencies and before the stage worker starts, so
// one real Specify dispatch is unavoidable; the label is held for its whole
// duration, so a short poll sees it. The issue asks Specify to short-circuit with
// FABRIK_NO_WORK_NEEDED to keep the cost minimal.
//
// Wall-clock: ~2-5 min. Cost: ~$0.10.
func TestAuthLockLabelShape(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	leg := detectAuthLeg(t, env)
	repo := env.RepoAlpha

	num := FileIssue(t, env, repo, fmt.Sprintf("e2e auth lock-label shape (%s)", time.Now().UTC().Format("150405.000")),
		`## Goal

Verify the lock label the engine holds while it works this issue.

## Trivial change

This is an identity probe. No implementation required. If you (the Specify agent) are reading this, emit FABRIK_NO_WORK_NEEDED to short-circuit.`)
	itemID := AddIssueToProject(t, env, repo, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify on the %s leg (engine identity %q)", repo, num, leg.Mode, leg.Login)

	deadline := time.Now().Add(15 * time.Minute)
	for {
		labels, err := restIssueLabels(env, repo, num)
		if err == nil {
			var locks []string
			for _, l := range labels {
				if strings.HasPrefix(l, lockLabelPrefix) {
					locks = append(locks, l)
				}
			}
			if len(locks) > 1 {
				t.Fatalf("%s#%d carries %d lock labels %v, want exactly one", repo, num, len(locks), locks)
			}
			if len(locks) == 1 {
				if p := lockLabelProblem(leg, locks[0]); p != "" {
					t.Fatalf("%s#%d: %s", repo, num, p)
				}
				t.Logf("lock label %q has the %s-leg shape", locks[0], leg.Mode)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 15m: the engine never labelled %s#%d with a %s* lock (last read error: %v) — the item was never dispatched",
				repo, num, lockLabelPrefix, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// TestAuthEnginePRAuthorIdentity: the PR the engine creates is authored by the
// engine's own identity — the PAT's login under a PAT, "<slug>[bot]" under App
// auth. The PR existing at all proves the credentialed push (the git credential
// helper under App auth, ADR-1846; the PAT otherwise) and the PR-create write
// worked under this mode's grants.
//
// Seeding: the earlier stages are marked complete and the card is placed at
// Implement, so the engine dispatches one real Implement invocation against an
// issue whose entire spec is "add one file". There is no Plan comment to
// consume; Implement works from the issue body. That is the minimum state that
// makes the ENGINE (not the harness) author a PR, and it costs one Implement run.
//
// Wall-clock: ~8-20 min. Cost: ~$0.30.
func TestAuthEnginePRAuthorIdentity(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	leg := detectAuthLeg(t, env)
	repo := env.RepoAlpha

	stamp := time.Now().UTC().Format("150405.000")
	num := FileIssue(t, env, repo, fmt.Sprintf("e2e auth engine PR author (%s)", stamp),
		fmt.Sprintf(`## Goal

Add one file so the engine opens a pull request.

## Requirements

Create exactly one new file, `+"`e2e/auth-focused/engine-pr-%d.md`"+`, containing the single line `+"`auth focused test`"+`. Change nothing else. No tests or docs are needed.`, time.Now().UnixNano()%1_000_000))
	itemID := AddIssueToProject(t, env, repo, num)
	for _, stage := range []string{"Specify", "Research", "Plan"} {
		label := "stage:" + stage + ":complete"
		AddLabel(t, env, repo, num, label)
		AwaitLabelVisible(t, env, repo, num, label, awaitSeedTimeout)
	}
	SetIssueStatus(t, env, itemID, "Implement")
	AwaitBoardItemVisible(t, env, repo, num, awaitSeedTimeout)
	t.Logf("seeded %s#%d at Status=Implement on the %s leg; waiting for the engine's PR", repo, num, leg.Mode)

	deadline := time.Now().Add(45 * time.Minute)
	prNum := 0
	for prNum == 0 {
		if n, err := tryLinkedPRNumber(env, repo, num); err == nil && n > 0 {
			prNum = n
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 45m: the engine never opened a PR for %s#%d", repo, num)
		}
		pollSleep(pollBase())
	}
	AssertPRAuthorIsEngineIdentity(t, env, repo, prNum)
	t.Logf("PR #%d on %s was authored by the engine's own identity on the %s leg (%q)", prNum, repo, leg.Mode, leg.Login)
}

// TestAuthReviewGateIdentity: with a harness-authored PR and a distinct
// requested reviewer, the review gate engages and the engine labels the item
// fabrik:awaiting-review — which exercises, on this mode's grants, the engine's
// reads of review requests and its first label write on a PR-bearing item. The
// PR author is checked against the harness token first (a shadowed FABRIK_TOKEN
// would author it as someone else and silently break RequestPRReviewer, #925).
//
// No Claude cost: the item is seeded past Review, so only the gate runs.
// Wall-clock: ~2-5 min.
func TestAuthReviewGateIdentity(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	leg := detectAuthLeg(t, env)
	repo := env.RepoAlpha

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required to request a review from a non-author identity")
	}
	reviewerLogin := TokenLogin(t, reviewerToken)

	num, prNum, _ := seedReviewGateItem(t, env, repo, "main", "Review", "auth-review-gate-identity")
	AssertPRAuthorIsExpectedIdentity(t, env, repo, prNum)
	if author := TokenLogin(t, env.GHToken); reviewerLogin == author {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the PR author — set it to a distinct account's PAT", reviewerLogin)
	}
	RequestPRReviewer(t, env, repo, prNum, reviewerLogin)
	WaitForIssueLabel(t, env, repo, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("review gate engaged on %s#%d (PR #%d, reviewer %s) on the %s leg", repo, num, prNum, reviewerLogin, leg.Mode)
}
