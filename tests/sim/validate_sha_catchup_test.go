package sim

import (
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file is the sim twin of #2158 (ADR-2158): the SHA-invalidation scan must not strip
// stage:Validate:complete from an item that has already landed, nor treat the engine's own
// pure singleton catch-up merge (#2044) as an external push.
//
// Neutralisation (each scenario fails with the named branch removed from
// settleSHAInvalidationScan):
//   - LandedItemKeepsValidateComplete : remove the validateItemLanded exemption — the scan
//     strips the label from the just-landed Done item.
//   - OpenItem*PureCatchUp*           : remove the headIsPureCatchUp exemption.
//
// ValidateCompletedSHA is engine memory and the sim never runs Validate, so each scenario
// seeds it through SeedValidateCompletedSHAForTest — without that the scan skips the item
// (no recorded SHA) and the scenario would pass vacuously.

const validateCompleteLabel = "stage:Validate:complete"

func TestSHAInvalidation_CatchUpLanding_KeepsValidateComplete(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, catchUpEnvOptions())
	num, prNum := QueueMember(t, env, "validate-sha-landed", map[string]string{"a.txt": "a\n"})
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, validateCompleteLabel); err != nil {
		t.Fatalf("labelling #%d: %v", num, err)
	}
	env.Engine.SeedValidateCompletedSHAForTest(env.OwnerRepo, num, orig)
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, false, false, []gh.CheckRun{greenCheckRun("")})

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)
	newHead, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if newHead == orig {
		t.Fatal("the member branch was not caught up — the scenario does not exercise a moved head")
	}
	// In production the moved head reaches the store from the pull_request webhook or
	// the probe, after the landing; the sim has no such feed, so deliver it here.
	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, prNum, newHead)

	// Poll well past the point at which the scan used to fire.
	for i := 0; i < 5; i++ {
		RunPoll(t, env)
	}
	if !hasLabel(IssueLabels(t, env, num), validateCompleteLabel) {
		t.Errorf("landed item lost %s; labels = %v\n\n%s", validateCompleteLabel, IssueLabels(t, env, num), diagnostics(env))
	}
	if got := env.Claude.StageCallCount("Validate"); got != 0 {
		t.Errorf("Validate must not be dispatched for a landed item, got %d call(s)", got)
	}
}

// settledValidateItem files an open item at Validate that completed Validate at its
// original head, with a linked PR on the member-style branch. Returns the issue, PR and
// the original head.
func settledValidateItem(t *testing.T, env *Env, marker string) (num, prNum int, orig string) {
	t.Helper()
	num = FileIssue(t, env, "settled "+marker, "settled at validate. marker="+marker, "Validate", validateCompleteLabel)
	branch := memberBranch(num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "main", map[string]string{"a.txt": "a\n"}, "member commit for #"+fmt.Sprint(num))
	env.issueSeqMu.Lock()
	env.issueSeqNext++
	prNum = env.issueSeqNext
	env.issueSeqMu.Unlock()
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Number: prNum, Head: branch, Base: "main", Title: "settled " + marker,
		Body: fmt.Sprintf("Closes #%d\n", num), IssueNumber: num,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding settled item: %v", err)
	}
	orig, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	// Warm poll: the engine records the item, so the seeds below arrive as store changes
	// (the way a webhook would deliver them) rather than as a first sighting.
	RunPoll(t, env)
	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, prNum, orig)
	env.Engine.SeedValidateCompletedSHAForTest(env.OwnerRepo, num, orig)
	return num, prNum, orig
}

// settledEnv is an Env where a Validate-complete item stays put at Validate: no
// merge train to advance it into Queued, no auto-advance.
func settledEnv(t *testing.T) *Env {
	t.Helper()
	opts := catchUpEnvOptions()
	opts.Mode = "off"
	return mergeTrainEnv(t, opts)
}

func TestSHAInvalidation_OpenItem_PureCatchUpHead_NotRevalidated(t *testing.T) {
	t.Parallel()
	env := settledEnv(t)
	num, prNum, orig := settledValidateItem(t, env, "pure-catchup")

	// The engine's catch-up: a merge commit on the member branch plus its own marker.
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, memberBranch(num), "main", map[string]string{"a.txt": "a\n"}, "catch-up merge")
	head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if head == orig {
		t.Fatal("head did not move")
	}
	body := "🏭 **Fabrik merge-train** — caught up\n" + formatMarker(head)
	if _, err := env.Sim.Sim().AddComment(env.Owner, env.Repo, prNum, body); err != nil {
		t.Fatalf("posting marker: %v", err)
	}

	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, prNum, head)
	for i := 0; i < 3; i++ {
		RunPoll(t, env)
	}
	if !hasLabel(IssueLabels(t, env, num), validateCompleteLabel) {
		t.Errorf("pure catch-up head must not invalidate Validate; labels = %v", IssueLabels(t, env, num))
	}
	if got := env.Claude.StageCallCount("Validate"); got != 0 {
		t.Errorf("Validate must not re-run after a pure catch-up, got %d call(s)", got)
	}

	// A later external push (no marker) is still caught.
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, memberBranch(num), memberBranch(num), map[string]string{"evil.txt": "x\n"}, "external push")
	pushed, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, prNum, pushed)
	WaitForLabelAbsent(t, env, num, validateCompleteLabel, 5)
}

func TestSHAInvalidation_OpenItem_ExternalPush_StillInvalidates(t *testing.T) {
	t.Parallel()
	env := settledEnv(t)
	num, prNum, orig := settledValidateItem(t, env, "external")

	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, memberBranch(num), memberBranch(num), map[string]string{"evil.txt": "x\n"}, "external push")
	if head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num)); head == orig {
		t.Fatal("head did not move")
	}
	pushed, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, prNum, pushed)
	WaitForLabelAbsent(t, env, num, validateCompleteLabel, 5)
}

func formatMarker(head string) string {
	return fmt.Sprintf("<!-- fabrik:train-catch-up head=%s base=%s pure=true -->", head, "bbbbbbb2222222")
}

// A caught-up member that lands through a trial PR (not the singleton fast path) is
// affected too (#2158 correction: #2114 landed via trial PR with a pure catch-up head
// on its branch). The landed exemption is path-agnostic; this pins it for the trial
// path. No catch-up marker exists here on purpose, so only the landed exemption — not
// the pure-catch-up one — can keep the label.
func TestSHAInvalidation_TrialLanding_OfMovedHeadMember_KeepsValidateComplete(t *testing.T) {
	t.Parallel()
	opts := catchUpEnvOptions()
	opts.ConfigureCfg = func(c *engine.Config) { c.User = "simgh-bot"; c.SingletonCatchUp = "off" }
	env := mergeTrainEnv(t, opts)
	num, prNum := QueueMember(t, env, "validate-sha-trial", map[string]string{"a.txt": "a\n"})
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, validateCompleteLabel); err != nil {
		t.Fatalf("labelling #%d: %v", num, err)
	}
	env.Engine.SeedValidateCompletedSHAForTest(env.OwnerRepo, num, orig)

	// The member's branch moves past the Validate SHA (an earlier catch-up), and main
	// moves on, so the member lands through a trial.
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, memberBranch(num), "main", map[string]string{"a.txt": "a\n", "base0.txt": "b\n"}, "earlier catch-up")
	moved, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if moved == orig {
		t.Fatal("member head did not move")
	}
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)
	if got := draftPRCount(env); got < 1 {
		t.Fatalf("expected a trial (draft CI PR), got %d — the scenario is not a trial landing", got)
	}

	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, prNum, moved)
	for i := 0; i < 5; i++ {
		RunPoll(t, env)
	}
	if !hasLabel(IssueLabels(t, env, num), validateCompleteLabel) {
		t.Errorf("trial-landed item lost %s; labels = %v\n\n%s", validateCompleteLabel, IssueLabels(t, env, num), diagnostics(env))
	}
	if got := env.Claude.StageCallCount("Validate"); got != 0 {
		t.Errorf("Validate must not be dispatched for a landed item, got %d call(s)", got)
	}
}
