package sim

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// This file is the sim twin of the merge-train singleton catch-up (#2044, ADR-2044): a
// Queued singleton whose own branch is BEHIND the pinned base is caught up on its own
// branch (a merge commit, pushed) and landed through the singleton fast path once its
// own CI is green — no trial branch, no draft integration PR. The unit tests in
// engine/merge_train_catchup*_test.go cover the decision logic against mocks and the git
// half against real repos; this file proves the end-to-end behaviour once, against the
// sim's git-backed origin, including the review-churn recognition (R3).
//
// Neutralisation (each scenario fails with the named branch removed):
//   - LandsWithoutTrial*       : remove the trySingletonCatchUp call in runMergeTrainWorker
//     (the member then builds a trial: draftPRCount >= 1, MergePR instead of MergePRAtHeadSHA).
//   - PureCatchUpReview*       : remove dropCatchUpFeedback from queuedReviewFindings (the
//     settle scan flags the member and the worker ejects it to Implement).
//   - ConflictEditedReview*    : make catchUpSuppresses ignore Pure (the review is muted and
//     the member lands instead of being ejected).
//   - Off*                     : remove the singletonCatchUpEnabled guard (no trial is built).

// catchUpEnvOptions: catch-up on (the default) and cfg.User set to the sim's own actor
// login so the engine recognises the marker comment it posts as its own.
func catchUpEnvOptions() mergeTrainEnvOptions {
	return mergeTrainEnvOptions{ConfigureCfg: func(c *engine.Config) { c.User = "simgh-bot" }}
}

// catchUpCISeeder plays CI and the review bot for a member's own PR. It watches the
// member branch and, the moment its head moves off origHead (the catch-up push), seeds
// the checks for that new head — immediately, or only once release is closed — and,
// when review is true, a Pruefer-shaped COMMENTED review plus an inline thread comment
// made against that head.
type catchUpCISeeder struct {
	headCh  chan string
	release chan struct{}
	stop    func()
}

func startCatchUpCISeeder(t *testing.T, env *Env, branch string, prNum int, origHead string, review bool, hold bool, checks []gh.CheckRun) *catchUpCISeeder {
	t.Helper()
	s := &catchUpCISeeder{headCh: make(chan string, 1), release: make(chan struct{})}
	if !hold {
		close(s.release)
	}
	done := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
			head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
			if err != nil || head == origHead {
				continue
			}
			if review {
				env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{
					Author: "handarbeit-pruefer[bot]", State: "COMMENTED", Body: "Review of the pushed head.",
					DatabaseID: 7001, CommitID: head,
				})
				env.Sim.Sim().SeedReviewThreadCommentAt(env.OwnerRepo, prNum, head, "handarbeit-pruefer[bot]", "nit: rename this", "a.txt", 1)
			}
			s.headCh <- head
			select {
			case <-done:
				return
			case <-s.release:
			}
			for _, cr := range checks {
				env.Sim.Sim().SeedCheckRun(env.OwnerRepo, head, cr)
			}
			return
		}
	}()
	s.stop = func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
	t.Cleanup(s.stop)
	return s
}

// pollWithoutWaiting runs one poll without RunPoll's wait for worker quiescence — a
// scenario that holds the worker mid-CI-wait cannot use RunPoll.
func pollWithoutWaiting(t *testing.T, env *Env) {
	t.Helper()
	if env.PollInterval > 0 {
		env.Clock.Advance(env.PollInterval)
	}
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func waitForCatchUpMarker(t *testing.T, env *Env, prNum int, pure bool) {
	t.Helper()
	want := fmt.Sprintf("pure=%t -->", pure)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, prNum)
		if err == nil {
			for _, c := range comments {
				if strings.Contains(c.Body, "fabrik:train-catch-up") && strings.Contains(c.Body, want) {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no catch-up marker (pure=%t) appeared on PR #%d\n\n%s", pure, prNum, diagnostics(env))
}

func memberBranch(num int) string { return fmt.Sprintf("fabrik/issue-%d", num) }

func advanceMain(t *testing.T, env *Env, files map[string]string) {
	t.Helper()
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, "main", "main", files, "external direct push advancing main")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("advancing main: %v", err)
	}
}

func TestMergeTrainSingletonCatchUp_BehindMember_LandsWithoutTrial(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, catchUpEnvOptions())
	num, prNum := QueueMember(t, env, "catchup-lands", map[string]string{"a.txt": "a\n"})
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, false, false, []gh.CheckRun{greenCheckRun("")})

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)

	if got := draftPRCount(env); got != 0 {
		t.Errorf("expected no draft CI PR (no trial), got %d", got)
	}
	if got := len(env.Sim.Log().ByMethod("CreatePR")); got != 0 {
		t.Errorf("expected no landing PR, got %d CreatePR call(s)", got)
	}
	if got := len(env.Sim.Log().ByMethod("MergePR")); got != 0 {
		t.Errorf("expected the unpinned MergePR never called, got %d", got)
	}
	newHead, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if newHead == orig {
		t.Fatal("the member branch was not caught up")
	}
	merges := env.Sim.Log().ByMethod("MergePRAtHeadSHA")
	if len(merges) != 1 || merges[0].Args.Number != prNum || len(merges[0].Args.Values) != 1 || merges[0].Args.Values[0] != newHead {
		t.Fatalf("want exactly one MergePRAtHeadSHA on the member's own PR #%d pinned to the caught-up head %s, got %+v", prNum, newHead, merges)
	}
	if !hasCommentContaining(t, env, prNum, "fabrik:train-catch-up head="+newHead) {
		t.Errorf("the catch-up marker comment is missing on PR #%d", prNum)
	}
	if !hasCommentContaining(t, env, prNum, "after a catch-up") {
		t.Errorf("the landed comment does not mention the catch-up: %v", commentsOn(t, env, prNum))
	}
}

func TestMergeTrainSingletonCatchUp_Off_StillBuildsTrial(t *testing.T) {
	t.Parallel()
	opts := catchUpEnvOptions()
	opts.ConfigureCfg = func(c *engine.Config) { c.User = "simgh-bot"; c.SingletonCatchUp = "off" }
	env := mergeTrainEnv(t, opts)
	num, _ := QueueMember(t, env, "catchup-off", map[string]string{"a.txt": "a\n"})
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)

	if got := draftPRCount(env); got < 1 {
		t.Errorf("singleton_catch_up: off must keep today's trial path, got %d draft PR(s)", got)
	}
	if head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num)); head != orig {
		t.Error("the member's own branch must not be touched with singleton_catch_up: off")
	}
}

// holdWorkerThenPoll runs the first poll (worker starts, catches up, then waits on CI
// held by the seeder), waits for the review and marker to be in place, then runs the
// settle scan once more while the worker is still waiting.
func holdWorkerThenPoll(t *testing.T, env *Env, s *catchUpCISeeder, prNum int, pure bool) {
	t.Helper()
	pollWithoutWaiting(t, env)
	select {
	case <-s.headCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("the member branch was never caught up\n\n%s", diagnostics(env))
	}
	waitForCatchUpMarker(t, env, prNum, pure)
	pollWithoutWaiting(t, env)
}

func TestMergeTrainSingletonCatchUp_PureCatchUpReview_NeitherEjectsNorHolds(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, catchUpEnvOptions())
	num, prNum := QueueMember(t, env, "catchup-pure-review", map[string]string{"a.txt": "a\n"})
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	s := startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, true, true, []gh.CheckRun{greenCheckRun("")})

	holdWorkerThenPoll(t, env, s, prNum, true)

	if st := projectItem(t, env, num).Status; st != "Queued" {
		t.Fatalf("a bot review of a pure catch-up head moved the member to %q — it must stay Queued", st)
	}
	close(s.release)
	WaitForProjectStatus(t, env, num, "Done", 20)
	if got := len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")); got != 1 {
		t.Errorf("want the member landed through the fast path once, got %d MergePRAtHeadSHA call(s)", got)
	}
	if got := draftPRCount(env); got != 0 {
		t.Errorf("no trial expected, got %d draft PR(s)", got)
	}
}

func TestMergeTrainSingletonCatchUp_ConflictEditedReview_IsActionable(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, catchUpEnvOptions())
	env.Claude.ForStageComments("Queued", claudeResolveConflict("shared.txt", "resolved\n"))
	num, prNum := QueueMember(t, env, "catchup-conflict-review", map[string]string{"shared.txt": "from-member\n"})
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	advanceMain(t, env, map[string]string{"shared.txt": "from-main\n"})
	s := startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, true, true, []gh.CheckRun{greenCheckRun("")})

	holdWorkerThenPoll(t, env, s, prNum, false)

	deadline := time.Now().Add(20 * time.Second)
	for projectItem(t, env, num).Status == "Queued" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := projectItem(t, env, num).Status; st == "Queued" || st == "Done" {
		t.Fatalf("a review of a conflict-edited catch-up must eject the member off Queued; status = %q", st)
	}
	close(s.release)
	if got := len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")); got != 0 {
		t.Errorf("an ejected member must not be merged, got %d call(s)", got)
	}
}
