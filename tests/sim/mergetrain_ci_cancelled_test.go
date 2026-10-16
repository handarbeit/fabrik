package sim

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// #2296: the merge train never reads a cancelled latest check run as green and
// never charges, ejects or pauses members for one. Sim twin of the train
// dispositions in engine/train_ci_eval_test.go and engine/merge_train_cancelled_test.go.

// A trial whose `test` run is cancelled is re-run once; still cancelled, it is
// abandoned as infrastructure — members stay Queued and nothing is charged.
func TestMergeTrainCICancelled_TrialRerunThenAbandonedUncharged(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)

	numA, _ := QueueMember(t, env, "cancelled-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "cancelled-b", map[string]string{"b.txt": "b\n"})
	var mu sync.Mutex
	trials := 0
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		mu.Lock()
		trials++
		mu.Unlock()
		s := env.Sim.Sim()
		id := int64(9000 + trial)
		s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "cancelled", JobCount: 1})
		// The re-run is cancelled as well.
		s.SeedRerunStep(env.OwnerRepo, id, simgh.RerunStep{SHA: sha, Conclusion: "cancelled",
			CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(id, 1)}}})
		s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(id, 1)})
	})

	RunPoll(t, env)
	RunPolls(t, env, 3)

	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 1 {
		t.Errorf("RerunFailedJobs calls = %d, want exactly 1", n)
	}
	for _, n := range []int{numA, numB} {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d status = %q, want it left in Queued", n, st)
		}
		labels := IssueLabels(t, env, n)
		if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") {
			t.Errorf("#%d was paused over a cancelled run: %v", n, labels)
		}
		if hasCommentContaining(t, env, n, "ejected") || hasCommentContaining(t, env, n, "dissolved") {
			t.Errorf("#%d was charged or dissolved over a cancelled run", n)
		}
	}
	if n := len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")); n != 0 {
		t.Errorf("MergePR calls = %d, want 0 — a cancelled trial must never land", n)
	}
	// Bisection would have built more than one trial PR.
	if got := len(env.Sim.Log().ByMethod("CreateDraftPR")); got != 1 {
		t.Errorf("trial PRs = %d, want 1 — a cancelled trial must not be bisected", got)
	}
	// The partition is on cooldown: further polls form no new trial.
	before := len(env.Sim.Log().ByMethod("CreateDraftPR"))
	RunPolls(t, env, 3)
	if got := len(env.Sim.Log().ByMethod("CreateDraftPR")); got != before {
		t.Errorf("trial PRs %d -> %d: a fresh trial was formed inside the cooldown", before, got)
	}
}

// Cancelled then re-run to success: the trial lands normally.
func TestMergeTrainCICancelled_TrialRerunSuccessLands(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)

	numA, _ := QueueMember(t, env, "cancelled-ok-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "cancelled-ok-b", map[string]string{"b.txt": "b\n"})
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		s := env.Sim.Sim()
		id := int64(9100 + trial)
		s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "cancelled", JobCount: 1})
		s.SeedRerunStep(env.OwnerRepo, id, simgh.RerunStep{SHA: sha,
			CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success", DetailsURL: actionsJobURL(id, 1)}}})
		s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(id, 1)})
	})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 1 {
		t.Errorf("RerunFailedJobs calls = %d, want exactly 1", n)
	}
}

// A singleton whose own head's latest run is cancelled does not take the fast
// path: nothing is merged directly on the cancelled run.
func TestMergeTrainCICancelled_SingletonOwnHeadCancelledSkipsFastPath(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, _ := QueueMember(t, env, "cancelled-fastpath", map[string]string{"fp.txt": "fp\n"})
	pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("could not resolve queued member's own linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(9200, 1)})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	RunPolls(t, env, 4)

	if got := len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")); got != 0 {
		t.Errorf("MergePRAtHeadSHA calls = %d, want 0 — the fast path must decline on a cancelled own run", got)
	}
	if st := projectItem(t, env, num).Status; st == "Done" {
		t.Errorf("#%d reached Done on a cancelled run", num)
	}
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") {
		t.Errorf("#%d was paused over a cancelled run: %v", num, labels)
	}
}
