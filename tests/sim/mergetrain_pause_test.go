package sim

import (
	"errors"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// Sim twin of #2341 R3 (ADR 2341): a merge-train member paused after its batch formed
// is not landed. A batch lands as one integration PR holding every survivor's changes,
// so the landing refuses as a whole; every member stays in Queued with nothing ejected
// or counted. The paused member is already excluded from new batches, so the next poll
// re-forms one without it, and the member lands in its own batch once the pause lifts
// (R4).
//
// The pause is applied from the trial seeder's verdict callback — i.e. after the batch
// formed and its trial PR opened, immediately before the trial is seeded green — which
// is the only window R3 is about.

// pausingVerdict returns a verdictFor that pauses issue `victim` the first time it is
// asked about a trial containing it, then reports every trial green.
func pausingVerdict(env *Env, victim int) func(members []int) []gh.CheckRun {
	var once sync.Once
	return func(members []int) []gh.CheckRun {
		for _, m := range members {
			if m == victim {
				once.Do(func() {
					for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
						if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, victim, l); err != nil {
							panic(err)
						}
					}
				})
			}
		}
		return []gh.CheckRun{greenCheckRun("")}
	}
}

func mergedTrialPRContaining(t *testing.T, env *Env, issue int) bool {
	t.Helper()
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	for _, pr := range prs {
		if !pr.Merged {
			continue
		}
		for _, n := range parseClosesNumbers(pr.Body) {
			if n == issue {
				return true
			}
		}
	}
	return false
}

func TestMergeTrainPause_MemberPausedAfterFormation_NotLanded(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	numA, _ := QueueMember(t, env, "pause-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "pause-b", map[string]string{"b.txt": "b\n"})
	startTrialSeeder(t, env, pausingVerdict(env, numB), nil)

	RunPoll(t, env)
	// The first batch (A+B) must be refused; the next polls re-form without B and land A.
	WaitForProjectStatus(t, env, numA, "Done", 30)

	if mergedTrialPRContaining(t, env, numB) {
		t.Fatalf("a trial PR containing the paused member #%d was merged", numB)
	}
	if got := projectItem(t, env, numB).Status; got != "Queued" {
		t.Errorf("paused #%d status = %q, want Queued", numB, got)
	}
	labels := IssueLabels(t, env, numB)
	if !hasLabel(labels, "fabrik:paused") {
		t.Errorf("pause label lost; labels = %v", labels)
	}
	if hasCommentContaining(t, env, numB, "ejected") || hasCommentContaining(t, env, numB, "merge failure") {
		t.Error("a refusal must not eject the member or post a failure comment")
	}

	// R4: the pause lifts and the member lands in a batch of its own.
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, numB, l); err != nil && !errors.Is(err, gh.ErrNotFound) {
			t.Fatalf("lifting the pause: %v", err)
		}
	}
	WaitForProjectStatus(t, env, numB, "Done", 30)
	if !mergedTrialPRContaining(t, env, numB) {
		t.Error("the member never landed after the pause was lifted")
	}
}

// Non-vacuity: with the pause guard neutralised the first (A+B) batch lands with the
// paused member in it, which is what the test above fails on.
func TestMergeTrainPause_NonVacuity(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetPauseGuardDisabledForTest(true)
	numA, _ := QueueMember(t, env, "pause-nv-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "pause-nv-b", map[string]string{"b.txt": "b\n"})
	startTrialSeeder(t, env, pausingVerdict(env, numB), nil)

	RunPoll(t, env)
	WaitForProjectStatus(t, env, numA, "Done", 30)

	if !mergedTrialPRContaining(t, env, numB) {
		t.Fatal("with the guard disabled the paused member should have been landed in the batch")
	}
}
