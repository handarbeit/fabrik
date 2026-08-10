package sim

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// baseBranchStages mirrors smokeStages' full 7-stage pipeline shape, but adds
// wait_for_reviews: true on Review — TestBaseBranchPipeline's #1050
// assertion (the review gate must clear naturally off a base:<branch> PR's
// review data, not degrade to the timeout path) needs a real gate to clear
// before Validate can start, matching the shipped default pipeline shape
// (stages/examples/review.yaml puts wait_for_reviews on Review, not
// Validate).
func baseBranchStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Research", Order: 2},
		{Name: "Plan", Order: 3},
		{Name: "Implement", Order: 4, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 5, WaitForReviews: &tr},
		{Name: "Validate", Order: 6},
		{Name: "Done", Order: 7, CleanupWorktree: true},
	}
}

// TestBaseBranchPipeline ports tests/e2e/basebranch_test.go's
// TestBaseBranchPipeline — the base:<branch> (non-default base branch)
// pipeline contract, e2e regression coverage for #1046. The live test exists
// because GitHub's GraphQL closingIssuesReferences/closedByPullRequestsReferences
// (and the review data nested inside them) are only populated for PRs
// targeting a repo's *default* branch — any code path trusting those fields
// unconditionally silently breaks for base:<branch> issues. Two fixes shipped
// for #1046: #1047 (issue<->PR linkage verification via
// verifyAndHealLinkageByBody) and #1050 (base-independent review-gate data
// feed via FetchPRReviews/FetchPRReviewRequests).
//
// R5 fidelity finding (see simgh/FIDELITY.md): simgh's PR<->issue linkage
// (FetchLinkedPR, FetchPRClosingIssues) and review data
// (FetchPRReviews/FetchPRReviewRequests) are resolved directly from the
// model — by head-branch convention and by PR number — never through a
// base-branch-conditional GraphQL field the way production's GraphQL query
// is. This means the specific defect class #1046/#1047/#1050 exist to catch
// (a GraphQL field going silently empty for a non-default-base PR) cannot
// occur in sim: every base branch behaves identically to every other. This
// port therefore proves the *mechanism* — a base:<branch> PR is correctly
// targeted, the pipeline does not falsely pause, and the review gate clears
// off a real review — but it is NOT regression coverage for the historical
// GraphQL-field-goes-empty defect itself; the live test remains the only
// coverage of that. See the coverage matrix in README.md.
func TestBaseBranchPipeline(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: baseBranchStages(), Yolo: boolPtr(false)})

	// A throwaway branch off the repo default, standing in for the live
	// test's CreateThrowawayBaseBranch.
	const branchName = "e2e-base-branch-sim"
	env.Sim.Sim().SeedBranch(env.OwnerRepo, branchName, "")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedBranch: %v", err)
	}

	// fabrik:cruise (not fabrik:yolo) drives auto-advance here, exactly as
	// the live test does — Yolo:false above means only the label engages
	// shouldAdvance (engine/stages.go's cruiseActive).
	num := FileIssue(t, env, "sim base-branch pipeline", "Prove the base:<branch> pipeline contract.",
		"Specify", "base:"+branchName, "fabrik:cruise")

	// Core assertion (#1046/#1047): the pipeline must reach end of Implement
	// without a false pause. If verifyAndHealLinkageByBody regressed to
	// trusting an always-empty GraphQL field on a non-default-base PR, the
	// linkage heal would false-fail right here. Waited stage-by-stage
	// (mirroring cruise_test.go) so each transition gets its own maxPolls
	// budget, rather than one shared budget covering four stage completions.
	for _, name := range []string{"Specify", "Research", "Plan", "Implement"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	if labels := IssueLabels(t, env, num); hasLabel(labels, "fabrik:paused") {
		t.Fatalf("fabrik:paused applied by end of Implement — #1046/#1047 regression: labels=%v", labels)
	}

	// Confirm Fabrik targeted the PR at the throwaway branch, not a silent
	// fallback to the repo default (that fallback only fires when the branch
	// is missing on the remote, which it isn't here).
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR after Implement")
	}
	if pr.BaseRef != branchName {
		t.Fatalf("PR #%d base ref = %q, want %q (fell back to default branch?)", pr.Number, pr.BaseRef, branchName)
	}

	// Review-gate assertion (#1050): Review has wait_for_reviews: true, so
	// handleStageComplete optimistically applies fabrik:awaiting-review
	// alongside stage:Review:complete — confirm the gate actually engaged
	// (not vacuously already-clear) before proving it clears off a real
	// review rather than degrading to the timeout/pause path.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)

	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-bot", State: "APPROVED"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}

	// The gate clearing (not a cycle-limit pause) is what proves #1050: this
	// Env's Clock only advances PollInterval per RunPoll (poll.go), so the
	// real ReviewWaitTimeout (15m default) is thousands of polls away —
	// clearing here can only be the natural review-arrival path, never the
	// timeout path, within the maxPolls budgets below.
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-review", 80)

	// With the gate clear, cruise carries the pipeline on to Validate.
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	if labels := IssueLabels(t, env, num); hasLabel(labels, "fabrik:paused") {
		t.Fatalf("fabrik:paused applied — review gate degraded to the timeout path instead of clearing naturally: labels=%v", labels)
	}
	t.Logf("%s#%d: base:<branch> pipeline reached Validate without a false pause, PR #%d targets %q, review gate cleared naturally",
		env.OwnerRepo, num, pr.Number, branchName)
}

// TestBaseBranchPipeline_NonVacuous proves the base-ref assertion above is
// not vacuously true: an issue filed WITHOUT a base:<branch> label targets
// the repo's default branch, not the throwaway branch — so
// TestBaseBranchPipeline's pr.BaseRef check is actually discriminating on
// the label, not just always matching whatever branch happens to be first.
func TestBaseBranchPipeline_NonVacuous(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: baseBranchStages(), Yolo: boolPtr(false)})

	const branchName = "e2e-base-branch-sim-unused"
	env.Sim.Sim().SeedBranch(env.OwnerRepo, branchName, "")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedBranch: %v", err)
	}

	// No base:<branch> label this time — otherwise identical setup.
	num := FileIssue(t, env, "sim base-branch pipeline (no override)", "No base:<branch> label.",
		"Specify", "fabrik:cruise")

	for _, name := range []string{"Specify", "Research", "Plan", "Implement"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR after Implement")
	}
	if pr.BaseRef == branchName {
		t.Fatalf("PR #%d targeted %q with no base:<branch> label present — the label isn't actually driving retargeting", pr.Number, branchName)
	}
	t.Logf("no base:<branch> label -> PR #%d targets %q (the repo default), confirming the label is what redirects targeting", pr.Number, pr.BaseRef)
}
