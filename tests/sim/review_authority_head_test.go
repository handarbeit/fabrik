package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Sim twin of the head-aware authoritative review gate (#2465, ADR 2465). Every
// scenario runs with the verdict as shipped and, as its non-vacuity control, with
// engine.SetReviewStandingDisabledForTest(true) (the pre-#2465 head-blind rule):
// the scenarios whose outcome the fix changes must flip.
//
// The reported sequence: a declared bot requests changes on commit A, a fix is
// pushed as commit B, and the bot reviews B with COMMENTED. The sim's
// FetchPRReviews applies the same sticky collapse as production, so the old rule
// sees only the CHANGES_REQUESTED here.

func newHeadAwareEnv(t *testing.T, declared ...string) *Env {
	t.Helper()
	tr := true
	var exp *[]string
	if declared != nil {
		exp = &declared
	}
	return NewEnv(t, EnvOptions{
		Stages: []*stages.Stage{
			{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
			{Name: "Review", Order: 2, WaitForReviews: &tr, ExpectedReviewers: exp},
			{Name: "Done", Order: 3, CleanupWorktree: true},
		},
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = 15 * time.Minute
			cfg.MaxReviewCycles = 5
		},
	})
}

// headOf reads the PR's current head commit.
func headOf(t *testing.T, env *Env, prNum int) string {
	t.Helper()
	d, err := env.Sim.FetchPRDetails(env.Owner, env.Repo, prNum)
	if err != nil || d == nil || d.HeadSHA == "" {
		t.Fatalf("FetchPRDetails(#%d): %v %+v", prNum, d, err)
	}
	return d.HeadSHA
}

// pushFix adds a commit to the PR's branch and returns the new head.
func pushFix(t *testing.T, env *Env, issueNum, prNum int) string {
	t.Helper()
	env.Sim.Sim().SeedCommit(env.OwnerRepo, fmt.Sprintf("fabrik/issue-%d", issueNum),
		map[string]string{fmt.Sprintf("sim/fix-%d.md", issueNum): "fix\n"}, "sim: address review")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("pushFix: %v", err)
	}
	return headOf(t, env, prNum)
}

func seedBareReview(t *testing.T, env *Env, prNum int, author, state, commit string) {
	t.Helper()
	// No body and no database ID: a verdict only, never actionable reinvoke feedback.
	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{Author: author, State: state, CommitID: commit})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
}

// gateCleared drives polls and reports whether the review gate let go of the item
// (no fabrik:awaiting-review). A blocked gate applies that label, so "blocked" is
// positively observed; "cleared" is vouched for by the neutralised control run of
// the same scenario, which must see the label.
func gateCleared(t *testing.T, env *Env, num int) bool {
	t.Helper()
	RunPolls(t, env, 30)
	return !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-review")
}

func TestReviewAuthorityHead_DeclaredBotCleanReReviewClearsTheGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		disabled bool
		want     bool
	}{{"as shipped", false, true}, {"neutralised (pre-#2465 rule)", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newHeadAwareEnv(t, "pruefer-bot")
			env.Engine.SetReviewStandingDisabledForTest(tc.disabled)
			num, prNum := seedReviewGateItem(t, env, "Review", "head-declared", "review-authority:authoritative")
			a := headOf(t, env, prNum)
			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", a)
			b := pushFix(t, env, num, prNum)
			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "COMMENTED", b)
			if got := gateCleared(t, env, num); got != tc.want {
				t.Fatalf("gate cleared = %v, want %v", got, tc.want)
			}
			if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
				t.Fatal("paused: the gate should clear or keep waiting, never pause on its own")
			}
		})
	}
}

func TestReviewAuthorityHead_UndeclaredReviewerStillBlocks(t *testing.T) {
	t.Parallel()
	env := newHeadAwareEnv(t, "pruefer-bot") // carol is not declared
	num, prNum := seedReviewGateItem(t, env, "Review", "head-undeclared", "review-authority:authoritative")
	a := headOf(t, env, prNum)
	seedBareReview(t, env, prNum, "carol", "CHANGES_REQUESTED", a)
	b := pushFix(t, env, num, prNum)
	seedBareReview(t, env, prNum, "carol", "COMMENTED", b)
	if gateCleared(t, env, num) {
		t.Fatal("an undeclared reviewer's COMMENTED must not withdraw their change request")
	}
}

func TestReviewAuthorityHead_VerdictOnOldCommitAwaitsReReview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		disabled bool
		state    string
		want     bool
	}{
		{"change request, as shipped", false, "CHANGES_REQUESTED", false},
		{"approval, as shipped", false, "APPROVED", false},
		{"approval, neutralised (pre-#2465 rule)", true, "APPROVED", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newHeadAwareEnv(t)
			env.Engine.SetReviewStandingDisabledForTest(tc.disabled)
			num, prNum := seedReviewGateItem(t, env, "Review", "head-awaiting", "review-authority:authoritative")
			a := headOf(t, env, prNum)
			seedBareReview(t, env, prNum, "carol", tc.state, a)
			pushFix(t, env, num, prNum)
			if got := gateCleared(t, env, num); got != tc.want {
				t.Fatalf("gate cleared = %v, want %v", got, tc.want)
			}
		})
	}
}

// R4: with a branch-protection requirement GitHub's reviewDecision decides, whatever
// the per-reviewer fallback would say about the stale approval.
func TestReviewAuthorityHead_BranchProtectionDecisionStillWins(t *testing.T) {
	t.Parallel()
	env := newHeadAwareEnv(t)
	env.Sim.Sim().SeedRequiredApprovals(env.OwnerRepo, "main", 1)
	num, prNum := seedReviewGateItem(t, env, "Review", "head-protected", "review-authority:authoritative")
	a := headOf(t, env, prNum)
	seedBareReview(t, env, prNum, "carol", "APPROVED", a)
	pushFix(t, env, num, prNum) // the approval is on an old commit, but GitHub's decision stands
	if !gateCleared(t, env, num) {
		t.Fatal("a non-empty reviewDecision (APPROVED) must decide, not the head-aware fallback")
	}
}

// R5: a verdict that carries no commit cannot be shown stale, so it keeps blocking.
func TestReviewAuthorityHead_ReviewWithoutCommitFailsClosed(t *testing.T) {
	t.Parallel()
	env := newHeadAwareEnv(t, "pruefer-bot")
	num, prNum := seedReviewGateItem(t, env, "Review", "head-nocommit", "review-authority:authoritative")
	seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", "")
	b := pushFix(t, env, num, prNum)
	seedBareReview(t, env, prNum, "pruefer-bot[bot]", "COMMENTED", b)
	if gateCleared(t, env, num) {
		t.Fatal("a change request with no commit must keep blocking")
	}
}
