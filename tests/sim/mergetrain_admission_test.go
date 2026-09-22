package sim

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file covers #1821's fresh-batch admission gate (ADR-1821): a Queued member whose
// OWN PR CI is confirmed red is kept out of the batch and routed off Queued, while every
// ambiguous outcome (pending, zero check runs, a FetchCheckRuns error) admits exactly as
// before — the inverse polarity of the singleton fast path (mergetrain_singleton_fastpath_test.go).
// Scenarios use ValidateWaitForCI, the gate's precondition, except where noted.

var errInjectedCheckRunsFault = errors.New("simgh: injected FetchCheckRuns fault")

// memberHeadSHA returns issue num's linked PR head SHA.
func memberHeadSHA(t *testing.T, env *Env, num int) string {
	t.Helper()
	pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR(#%d): %v", num, err)
	}
	return pr.HeadSHA
}

// seedMemberCI seeds check runs on num's own PR head.
func seedMemberCI(t *testing.T, env *Env, num int, runs ...gh.CheckRun) {
	t.Helper()
	sha := memberHeadSHA(t, env, num)
	for _, r := range runs {
		env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha, r)
	}
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seedMemberCI(#%d): %v", num, err)
	}
}

// trialPRsContain reports whether any merge-train trial/landing PR ever opened carries
// "Closes #num" — i.e. whether num was ever assembled into a trial.
func trialPRsContain(t *testing.T, env *Env, num int) bool {
	t.Helper()
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
			continue
		}
		for _, n := range parseClosesNumbers(pr.Body) {
			if n == num {
				return true
			}
		}
	}
	return false
}

func admissionEnv(t *testing.T) *Env {
	t.Helper()
	return mergeTrainEnv(t, mergeTrainEnvOptions{ValidateWaitForCI: true})
}

func assertDeferredNotPaused(t *testing.T, env *Env, num int) {
	t.Helper()
	WaitForProjectStatus(t, env, num, "Validate", 20)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("deferred #%d must not be paused, labels: %v", num, labels)
	}
	if projectItem(t, env, num).IsClosed {
		t.Errorf("deferred #%d must not land", num)
	}
	if !hasCommentContaining(t, env, num, "deferred (own CI failing)") {
		t.Errorf("expected the deferral comment on #%d", num)
	}
	if !hasCommentContaining(t, env, num, "not a merge-train interaction") {
		t.Errorf("expected the comment on #%d to attribute the failure to the PR itself", num)
	}
	if hasCommentContaining(t, env, num, "unresolved review-thread finding") || hasCommentContaining(t, env, num, "merge-train — ejected") {
		t.Errorf("deferral on #%d must not reuse ejection/review wording", num)
	}
}

// R1/R4/R5/R6 + mixed batch: only the red member is deferred, the green one lands, and
// the red one never appears in any trial.
func TestMergeTrainAdmission_MixedBatch_OnlyRedDeferred(t *testing.T) {
	t.Parallel()
	env := admissionEnv(t)

	green, _ := QueueMember(t, env, "adm-green", map[string]string{"g.txt": "g\n"})
	red, _ := QueueMember(t, env, "adm-red", map[string]string{"r.txt": "r\n"})
	seedMemberCI(t, env, green, greenCheckRun(""))
	seedMemberCI(t, env, red, redCheckRun("e2e"))
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	WaitForProjectStatus(t, env, green, "Done", 20)
	WaitForIssueClosed(t, env, green, 5)
	assertDeferredNotPaused(t, env, red)
	if !hasCommentContaining(t, env, red, "**e2e**") {
		t.Error("expected the deferral comment to name the failing check")
	}
	if trialPRsContain(t, env, red) {
		t.Error("the deferred red member must never appear in any trial or landing PR")
	}
}

// R8: every member red — the worker ends cleanly with nothing assembled.
func TestMergeTrainAdmission_AllRed_NothingAssembled(t *testing.T) {
	t.Parallel()
	env := admissionEnv(t)

	a, _ := QueueMember(t, env, "adm-all-a", map[string]string{"a.txt": "a\n"})
	b, _ := QueueMember(t, env, "adm-all-b", map[string]string{"b.txt": "b\n"})
	seedMemberCI(t, env, a, redCheckRun(""))
	seedMemberCI(t, env, b, redCheckRun(""))
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	assertDeferredNotPaused(t, env, a)
	assertDeferredNotPaused(t, env, b)
	if got := draftPRCount(env); got != 0 {
		t.Errorf("expected no trial to be assembled when every member is deferred, got %d CreateDraftPR call(s)", got)
	}
	if got := len(env.Sim.Log().ByMethod("MergePR")); got != 0 {
		t.Errorf("expected no merge, got %d", got)
	}
}

// R2: each ambiguity admits and the member lands exactly as it did before the gate.
func TestMergeTrainAdmission_AmbiguityAdmits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, env *Env, num int)
	}{
		{"pending", func(t *testing.T, env *Env, num int) {
			seedMemberCI(t, env, num, gh.CheckRun{Name: "ci", Status: "in_progress"})
		}},
		{"zero-check-runs", func(t *testing.T, env *Env, num int) {}},
		{"fetch-error", func(t *testing.T, env *Env, num int) {
			sha := memberHeadSHA(t, env, num)
			seedMemberCI(t, env, num, redCheckRun("")) // red is there, but unreadable
			env.Sim.Faults().FailWhen("FetchCheckRuns",
				func(a simgh.Args) bool { return a.SHA == sha }, 1, errInjectedCheckRunsFault)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := admissionEnv(t)
			num, _ := QueueMember(t, env, "adm-amb-"+tc.name, map[string]string{"amb.txt": "amb\n"})
			tc.setup(t, env, num)
			startTrialVerdictSeeder(t, env, allGreenVerdict)

			RunPoll(t, env)

			WaitForProjectStatus(t, env, num, "Done", 20)
			WaitForIssueClosed(t, env, num, 5)
			if hasCommentContaining(t, env, num, "deferred (own CI failing)") {
				t.Errorf("an ambiguous read must admit, never defer")
			}
		})
	}
}

// R4: a failed reroute keeps the confirmed-red member out of the batch, posts nothing, and
// the next poll retries the whole operation.
func TestMergeTrainAdmission_FailedReroute_RetriedNextPoll(t *testing.T) {
	t.Parallel()
	env := admissionEnv(t)

	green, _ := QueueMember(t, env, "adm-fr-green", map[string]string{"g.txt": "g\n"})
	red, _ := QueueMember(t, env, "adm-fr-red", map[string]string{"r.txt": "r\n"})
	seedMemberCI(t, env, green, greenCheckRun(""))
	seedMemberCI(t, env, red, redCheckRun(""))
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	itemID := projectItem(t, env, red).ItemID
	env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
		func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
		1, errInjectedRerouteFault)

	RunPoll(t, env)

	WaitForProjectStatus(t, env, green, "Done", 20)
	if got := projectItem(t, env, red).Status; got != "Queued" {
		t.Fatalf("expected #%d to remain in Queued after a failed reroute, got %q", red, got)
	}
	if hasCommentContaining(t, env, red, "deferred (own CI failing)") {
		t.Error("no comment may be posted when the reroute fails")
	}
	if trialPRsContain(t, env, red) {
		t.Error("a confirmed-red member must be excluded from the batch even when its reroute fails")
	}

	RunPoll(t, env)
	assertDeferredNotPaused(t, env, red)
}

// Without wait_for_ci on the reroute target nothing would re-detect a deferred member, so
// the gate is skipped and a red member behaves exactly as before #1821 (rides a trial).
func TestMergeTrainAdmission_NoWaitForCI_GateSkipped(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, _ := QueueMember(t, env, "adm-nowait", map[string]string{"n.txt": "n\n"})
	seedMemberCI(t, env, num, redCheckRun(""))
	startTrialVerdictSeeder(t, env, poisonVerdict(num))

	RunPoll(t, env)

	WaitForProjectStatus(t, env, num, "Validate", 20)
	if hasCommentContaining(t, env, num, "deferred (own CI failing)") {
		t.Error("the gate must be skipped without wait_for_ci")
	}
	if got := draftPRCount(env); got == 0 {
		t.Error("expected the member to be assembled into a trial exactly as before the gate existed")
	}
}

// R7: the restart-reconstruction paths never consult the gate — a member whose own CI is
// red still resumes/completes its already-decided train.
func TestMergeTrainAdmission_RestartResume_NotGated(t *testing.T) {
	t.Parallel()
	env := admissionEnv(t)

	num, _ := QueueMember(t, env, "adm-restart-resume", map[string]string{"resume.txt": "resume\n"})
	seedMemberCI(t, env, num, redCheckRun(""))

	const trialBranch = mergeTrainBranchPrefix + "merge-train-main-crashed-adm"
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, trialBranch, "main",
		map[string]string{"resume.txt": "resume\n"}, "trial assembly (simulated crash)")
	body := fmt.Sprintf("🏭 **Fabrik merge-train trial integration for #%d**\n\nCloses #%d\n\n%s", num, num, mergeTrainBatchMarker)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Head: trialBranch, Base: "main", Title: fmt.Sprintf("[merge-train] batch: #%d", num), Body: body, Draft: true,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	restarted := restartMergeTrainEnv(t, env)
	startTrialVerdictSeeder(t, restarted, allGreenVerdict)

	RunPoll(t, restarted)

	WaitForProjectStatus(t, restarted, num, "Done", 20)
	WaitForIssueClosed(t, restarted, num, 5)
	if hasCommentContaining(t, restarted, num, "deferred (own CI failing)") {
		t.Error("resumeTrain must never route through the admission gate")
	}
}

func TestMergeTrainAdmission_RestartDeferredLanding_NotGated(t *testing.T) {
	t.Parallel()
	env := admissionEnv(t)

	num, _ := QueueMember(t, env, "adm-restart-landing", map[string]string{"deferred.txt": "deferred\n"})
	seedMemberCI(t, env, num, redCheckRun(""))

	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, "side-base", "main",
		map[string]string{"side-base-marker.txt": "x\n"}, "throwaway non-default base")
	const trialBranch = mergeTrainBranchPrefix + "merge-train-main-crashed-adm-landing"
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, trialBranch, "side-base",
		map[string]string{"deferred.txt": "deferred\n"}, "trial assembly for the deferred-landing PR")
	body := fmt.Sprintf("🏭 **Fabrik merge-train landing PR**\n\nThis PR lands the following Queued issues via the internal merge train:\n\n"+
		"- #%d — member\n\nCloses #%d\n\n%s", num, num, mergeTrainBatchMarker)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Head: trialBranch, Base: "side-base", Title: fmt.Sprintf("[merge-train] batch: #%d", num), Body: body, Merged: true,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	restarted := restartMergeTrainEnv(t, env)
	RunPoll(t, restarted)

	WaitForProjectStatus(t, restarted, num, "Done", 20)
	WaitForIssueClosed(t, restarted, num, 5)
	if hasCommentContaining(t, restarted, num, "deferred (own CI failing)") {
		t.Error("completeDeferredLanding must never route through the admission gate")
	}
}
