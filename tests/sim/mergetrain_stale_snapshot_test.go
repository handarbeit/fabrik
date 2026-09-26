package sim

import (
	"strings"
	"testing"
)

// This file covers #1871: right after the merge train lands a member, the next
// poll can dispatch a fresh worker from a board snapshot read BEFORE the landing's
// Done move, so both "resume an interrupted landing" branches (completeDeferredLanding
// via reconstructTrainState, and trySingletonFastPath's already-merged branch) used to
// re-run the whole landing off the stale Queued status. simgh.LagBoardStatus models
// the lagging read model (bulk board reads keep reporting Queued/open) while the
// single-node live reads keep telling the truth — the engine runs unmodified.
//
// Non-vacuity (Acceptance 3): each duplicate scenario is run twice, once with
// the guard disabled via SetMergeTrainLandingGuardDisabledForTest — which must
// reproduce the duplicate — and once with the production default, which must not.

// landedCommentCount counts "Landed via" comments across the member's issue and
// its own PR.
func landedCommentCount(t *testing.T, env *Env, nums ...int) int {
	t.Helper()
	n := 0
	for _, num := range nums {
		for _, c := range commentsOn(t, env, num) {
			if strings.Contains(c.Body, "Landed via") {
				n++
			}
		}
	}
	return n
}

func memberItemID(t *testing.T, env *Env, num int) string {
	t.Helper()
	return projectItem(t, env, num).ItemID
}

// runFastPathDuplicate lands a singleton via the fast path, then lags the board
// back to Queued and polls again, returning the number of "Landed via" comments
// and MergePRAtHeadSHA calls observed.
func runFastPathDuplicate(t *testing.T, guardDisabled bool) (landed, merges int) {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetMergeTrainLandingGuardDisabledForTest(guardDisabled)

	num, prNum := QueueMember(t, env, "stale-fastpath", map[string]string{"stale-fp.txt": "a\n"})
	pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("could not resolve member's linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, greenCheckRun(""))
	itemID := memberItemID(t, env, num)
	preLabels := projectItem(t, env, num).Labels

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)

	// The next poll reads a board taken before the Done move.
	env.Sim.Sim().LagBoardStatus(itemID, "Queued", preLabels)
	RunPoll(t, env)
	RunPoll(t, env)

	return landedCommentCount(t, env, num, prNum), len(env.Sim.Log().ByMethod("MergePRAtHeadSHA"))
}

func TestMergeTrainStaleSnapshot_SingletonFastPath_LandedExactlyOnce(t *testing.T) {
	t.Parallel()
	landed, merges := runFastPathDuplicate(t, false)
	if landed != 1 {
		t.Errorf("expected exactly 1 \"Landed via\" comment, got %d", landed)
	}
	if merges != 1 {
		t.Errorf("expected exactly 1 MergePRAtHeadSHA call, got %d", merges)
	}
}

func TestMergeTrainStaleSnapshot_SingletonFastPath_NonVacuity_GuardDisabledDuplicates(t *testing.T) {
	t.Parallel()
	landed, _ := runFastPathDuplicate(t, true)
	if landed < 2 {
		t.Errorf("with the guard disabled the stale snapshot must reproduce the duplicate (>=2 \"Landed via\" comments), got %d — the scenario proves nothing", landed)
	}
}

// runIntegrationDuplicate lands a member through the ordinary trial + integration-PR
// path (the base moves, so the fast path declines), then replays the stale snapshot.
func runIntegrationDuplicate(t *testing.T, guardDisabled bool) (landed, doneMoves int) {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetMergeTrainLandingGuardDisabledForTest(guardDisabled)

	num, prNum := QueueMember(t, env, "stale-integration", map[string]string{"stale-int.txt": "b\n"})
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, "main", "main",
		map[string]string{"external-push.txt": "external\n"}, "external direct push advancing main")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding external push: %v", err)
	}
	itemID := memberItemID(t, env, num)
	preLabels := projectItem(t, env, num).Labels
	stop := startTrialVerdictSeeder(t, env, allGreenVerdict)
	defer stop()

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)
	if got := draftPRCount(env); got < 1 {
		t.Fatalf("scenario premise: expected the trial path (>=1 draft CI PR), got %d", got)
	}

	env.Sim.Sim().LagBoardStatus(itemID, "Queued", preLabels)
	RunPoll(t, env)
	RunPoll(t, env)

	return landedCommentCount(t, env, num, prNum), len(env.Sim.Log().ByMethod("UpdateProjectItemStatus"))
}

func TestMergeTrainStaleSnapshot_IntegrationPR_LandedExactlyOnce(t *testing.T) {
	t.Parallel()
	landed, _ := runIntegrationDuplicate(t, false)
	if landed != 1 {
		t.Errorf("expected exactly 1 \"Landed via\" comment, got %d", landed)
	}
}

func TestMergeTrainStaleSnapshot_IntegrationPR_NonVacuity_GuardDisabledDuplicates(t *testing.T) {
	t.Parallel()
	landed, _ := runIntegrationDuplicate(t, true)
	if landed < 2 {
		t.Errorf("with the guard disabled the stale snapshot must reproduce the duplicate (>=2 \"Landed via\" comments), got %d — the scenario proves nothing", landed)
	}
}

// TestMergeTrainStaleSnapshot_FastPathRestartBetweenMergeAndDone: the member's own
// PR merged but the engine died before the Done move — live Status is still Queued,
// so the resume branch must still complete the landing exactly once (Acceptance 2).
func TestMergeTrainStaleSnapshot_FastPathRestartBetweenMergeAndDone(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, prNum := QueueMember(t, env, "stale-fp-restart", map[string]string{"stale-fpr.txt": "c\n"})
	if err := env.Sim.Sim().MergePR(env.Owner, env.Repo, prNum); err != nil {
		t.Fatalf("merging member PR to simulate a crash after merge: %v", err)
	}
	// GitHub auto-closes the issue on a default-branch merge; the production
	// window is the propagation gap before that happens (see the Route 1
	// restart scenario's doc comment), so put the shape back to open+Queued.
	if projectItem(t, env, num).IsClosed {
		if err := env.Sim.Sim().ReopenIssue(env.Owner, env.Repo, num); err != nil {
			t.Fatalf("reopening issue to model the pre-auto-close window: %v", err)
		}
	}
	if got := projectItem(t, env, num).Status; got != "Queued" {
		t.Fatalf("premise: member status = %q, want Queued", got)
	}

	restarted := restartMergeTrainEnv(t, env)
	RunPoll(t, restarted)
	WaitForProjectStatus(t, restarted, num, "Done", 20)
	RunPoll(t, restarted)

	if got := landedCommentCount(t, restarted, num, prNum); got != 1 {
		t.Errorf("expected the interrupted landing completed exactly once (1 \"Landed via\" comment), got %d", got)
	}
	if got := len(restarted.Sim.Log().ByMethod("MergePRAtHeadSHA")) + len(restarted.Sim.Log().ByMethod("MergePR")); got != 0 {
		t.Errorf("an already-merged PR must never be re-merged, got %d merge call(s)", got)
	}
}

// runFreshBatchDuplicate lands two singleton members (A, B) through the fast path,
// then queues a genuinely Queued member C and lags the board so A and B still show
// as open/Queued. The stale snapshot puts all three into a FRESH batch — there is no
// merged integration PR carrying a batch marker, so reconstructTrainState does not
// match and neither resume-branch guard is on this path. It returns the "Landed via"
// comment counts for A, B and C and whether C ended in Done.
func runFreshBatchDuplicate(t *testing.T, guardDisabled bool) (landedA, landedB, landedC int, cDone bool) {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetMergeTrainLandingGuardDisabledForTest(guardDisabled)
	stop := startTrialVerdictSeeder(t, env, allGreenVerdict)
	defer stop()

	landSingleton := func(name, file string) (num, prNum int, itemID string, preLabels []string) {
		num, prNum = QueueMember(t, env, name, map[string]string{file: "x\n"})
		pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
		if err != nil || pr == nil {
			t.Fatalf("could not resolve #%d's linked PR: %v", num, err)
		}
		env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, greenCheckRun(""))
		itemID = memberItemID(t, env, num)
		preLabels = projectItem(t, env, num).Labels
		RunPoll(t, env)
		WaitForProjectStatus(t, env, num, "Done", 20)
		WaitForIssueClosed(t, env, num, 5)
		return num, prNum, itemID, preLabels
	}
	aNum, aPR, aID, aLabels := landSingleton("stale-fresh-a", "stale-fresh-a.txt")
	bNum, bPR, bID, bLabels := landSingleton("stale-fresh-b", "stale-fresh-b.txt")

	cNum, cPR := QueueMember(t, env, "stale-fresh-c", map[string]string{"stale-fresh-c.txt": "x\n"})
	cLinked, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, cNum)
	if err != nil || cLinked == nil {
		t.Fatalf("could not resolve #%d's linked PR: %v", cNum, err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, cLinked.HeadSHA, greenCheckRun(""))

	env.Sim.Sim().LagBoardStatus(aID, "Queued", aLabels)
	env.Sim.Sim().LagBoardStatus(bID, "Queued", bLabels)
	RunPoll(t, env)
	RunPoll(t, env)

	return landedCommentCount(t, env, aNum, aPR), landedCommentCount(t, env, bNum, bPR),
		landedCommentCount(t, env, cNum, cPR), projectItem(t, env, cNum).Status == "Done"
}

// TestMergeTrainStaleSnapshot_FreshBatch_LandedMembersDropped: the stale snapshot
// lists two just-landed members alongside a genuinely Queued one. Only the Queued
// member is landed; neither landed member gets a second "Landed via…" comment.
func TestMergeTrainStaleSnapshot_FreshBatch_LandedMembersDropped(t *testing.T) {
	t.Parallel()
	a, b, c, cDone := runFreshBatchDuplicate(t, false)
	if a != 1 || b != 1 {
		t.Errorf("already-landed members must not be re-landed: \"Landed via\" comments A=%d B=%d, want 1 each", a, b)
	}
	if c != 1 || !cDone {
		t.Errorf("the genuinely Queued member must land exactly once: comments=%d done=%v", c, cDone)
	}
}

func TestMergeTrainStaleSnapshot_FreshBatch_NonVacuity_GuardDisabledDuplicates(t *testing.T) {
	t.Parallel()
	a, b, _, _ := runFreshBatchDuplicate(t, true)
	if a < 2 && b < 2 {
		t.Errorf("with the guard disabled the stale fresh batch must re-land a landed member (A=%d B=%d, want >=2 for at least one) — the scenario proves nothing", a, b)
	}
}
