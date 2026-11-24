package sim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Sim twin of #2615 (ADR 2615): the singleton catch-up used to defer a Queued member for
// ever while its worktree held only Fabrik scratch files or a stale REBASE_HEAD, and gave
// up nothing: the deferral is uncharged, so no retry cap, ejection cap or runaway guard
// ever fired, and health said `idle_reason: unknown`.
//
// Neutralisation (each scenario fails with the named branch removed):
//   - ScratchOnly*          : engine.SetCatchUpScratchExemptDisabledForTest(true)
//   - StaleRebaseHead*      : engine.SetStaleRefCleanupDisabledForTest(true)
//   - PersistentlyBusy*     : engine.SetCatchUpBusyBoundDisabledForTest(true) (stays Queued)
//     (DeferredMember*: SetCatchUpDeferralVisibilityDisabledForTest(true) leaves idle_reason "unknown").

// busyMemberEnv queues a lone member that is behind main, creates its worktree and returns
// the worktree directory so a scenario can leave something in it.
func busyMemberEnv(t *testing.T, marker string) (env *Env, num, prNum int, orig, wtDir string) {
	t.Helper()
	// BoardCache: the health read serves from the item Store, which the cache populates.
	opts := catchUpEnvOptions()
	opts.BoardCache = true
	env = mergeTrainEnv(t, opts)
	num, prNum = QueueMember(t, env, marker, map[string]string{"a.txt": "a\n"})
	orig, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	dir, err := env.WM.EnsureWorktree(num, "main", true)
	if err != nil {
		t.Fatalf("creating the member worktree: %v", err)
	}
	return env, num, prNum, orig, dir
}

func partitionIdleReason(t *testing.T, env *Env) string {
	t.Helper()
	h, err := env.Engine.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.MergeTrain) != 1 {
		t.Fatalf("partitions = %+v", h.MergeTrain)
	}
	return h.MergeTrain[0].IdleReason
}

func TestMergeTrainSingletonCatchUp_ScratchOnlyWorktree_IsCaughtUp(t *testing.T) {
	t.Parallel()
	env, num, prNum, orig, wtDir := busyMemberEnv(t, "busy-scratch")
	for _, f := range []string{".fabrik/scratch/validate.log", ".fabrik/scratch/two.log"} {
		p := filepath.Join(wtDir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("log\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, false, false, []gh.CheckRun{greenCheckRun("")})

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	if head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num)); head == orig {
		t.Fatal("the member branch was not caught up despite only scratch files in its worktree")
	}
	if got := draftPRCount(env); got != 0 {
		t.Errorf("no trial expected, got %d draft PR(s)", got)
	}
}

func TestMergeTrainSingletonCatchUp_StaleRebaseHead_IsRemovedAndCaughtUp(t *testing.T) {
	t.Parallel()
	env, num, prNum, orig, wtDir := busyMemberEnv(t, "busy-stale-ref")
	if out, err := exec.Command("git", "-C", wtDir, "update-ref", "REBASE_HEAD", orig).CombinedOutput(); err != nil {
		t.Fatalf("planting REBASE_HEAD: %v: %s", err, out)
	}
	startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, false, false, []gh.CheckRun{greenCheckRun("")})

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	if out, err := exec.Command("git", "-C", wtDir, "rev-parse", "-q", "--verify", "REBASE_HEAD").CombinedOutput(); err == nil {
		t.Errorf("the stale REBASE_HEAD was not removed: %s", out)
	}
}

// R4: while a member keeps being deferred as busy, the partition's idle_reason names it and
// the cause. (The bound is neutralised so the member is still waiting when health is read.)
func TestMergeTrainSingletonCatchUp_DeferredMember_IsNamedInIdleReason(t *testing.T) {
	t.Parallel()
	env, num, _, _, wtDir := busyMemberEnv(t, "busy-named")
	env.Engine.SetCatchUpBusyBoundDisabledForTest(true)
	if err := os.WriteFile(filepath.Join(wtDir, "stray.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got string
	AdvanceUntil(t, env, func(env *Env) bool {
		h := healthOf(t, env)
		if len(h.MergeTrain) == 1 && strings.Contains(h.MergeTrain[0].IdleReason, "catch-up deferred") {
			got = h.MergeTrain[0].IdleReason
			return true
		}
		return false
	}, 8)
	if want := "#" + strconv.Itoa(num) + " catch-up deferred: "; !strings.HasPrefix(got, want) || !strings.Contains(got, "uncommitted changes") {
		t.Fatalf("idle_reason = %q, want it to start with %q and name the cause", got, want)
	}
	if st := projectItem(t, env, num).Status; st != "Queued" {
		t.Fatalf("the member must still be Queued while it waits, got %q", st)
	}
}

// R3: a member whose worktree stays busy with nothing working on it is off Queued (paused,
// uncharged) after N formations.
func TestMergeTrainSingletonCatchUp_PersistentlyBusyMember_LeavesQueued(t *testing.T) {
	t.Parallel()
	env, num, _, _, wtDir := busyMemberEnv(t, "busy-persistent")
	// A stray source file is a real uncommitted change that nothing will ever clear.
	if err := os.WriteFile(filepath.Join(wtDir, "stray.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	AdvanceUntil(t, env, func(env *Env) bool { return projectItem(t, env, num).Status != "Queued" }, 10)

	if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Errorf("the member must be paused naming the reason; labels: %v", IssueLabels(t, env, num))
	}
	if !hasCommentContaining(t, env, num, "worktree has uncommitted changes") {
		t.Errorf("no comment names the busy reason: %v", commentsOn(t, env, num))
	}
	if got := len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")) + len(env.Sim.Log().ByMethod("MergePR")); got != 0 {
		t.Errorf("nothing may be merged, got %d call(s)", got)
	}
	for _, l := range IssueLabels(t, env, num) {
		if strings.HasSuffix(l, ":failed") {
			t.Errorf("the bounded deferral must not charge a failure: %s", l)
		}
	}
	// The stray file is the operator's: it is never deleted.
	if _, err := os.Stat(filepath.Join(wtDir, "stray.go")); err != nil {
		t.Errorf("the uncommitted file must survive: %v", err)
	}
}

func TestMergeTrainSingletonCatchUp_PersistentlyBusyMember_NeutralisedStaysQueuedAndUnknown(t *testing.T) {
	t.Parallel()
	env, num, _, _, wtDir := busyMemberEnv(t, "busy-neutralised")
	env.Engine.SetCatchUpBusyBoundDisabledForTest(true)
	env.Engine.SetCatchUpDeferralVisibilityDisabledForTest(true)
	if err := os.WriteFile(filepath.Join(wtDir, "stray.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 6)
	if st := projectItem(t, env, num).Status; st != "Queued" {
		t.Fatalf("with the bound neutralised the member stays Queued, got %q", st)
	}
	if got := partitionIdleReason(t, env); strings.Contains(got, "catch-up deferred") {
		t.Fatalf("with visibility neutralised idle_reason = %q", got)
	}
}

// R3 on a quiet, webhook-healthy board (review finding): a deferral records no admission
// cooldown of its own, so the idle backoff used to put the next formation up to an hour
// away, and the run of deferrals never reached the bound. The deferral now records a short
// cooldown (and a wake at its due), so the poll comes back within about a poll interval
// even though the daemon is as idle as it can be.
//
// Neutralisation: SetCatchUpDeferralCooldownDisabledForTest(true) records nothing and the
// idle daemon sleeps at its idle cadence.
func TestMergeTrainSingletonCatchUp_DeferredMember_KeepsPollingOnAnIdleHealthyBoard(t *testing.T) {
	t.Parallel()
	for _, cooldown := range []bool{true, false} {
		cooldown := cooldown
		name := "cooldown"
		if !cooldown {
			name = "nonvacuity-cooldown-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, num, _, _, wtDir := busyMemberEnv(t, "busy-idle-"+name)
			stub := engine.NewStubTransport()
			stub.SetHealthy(true)
			env.Engine.AttachStubTransportForTest(stub)
			env.Engine.SetCatchUpDeferralCooldownDisabledForTest(!cooldown)
			if err := os.WriteFile(filepath.Join(wtDir, "stray.go"), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Stop at the first deferral: the second would move the member off Queued.
			AdvanceUntil(t, env, func(env *Env) bool {
				h := healthOf(t, env)
				return len(h.MergeTrain) == 1 && strings.Contains(h.MergeTrain[0].IdleReason, "catch-up deferred")
			}, 8)
			if st := projectItem(t, env, num).Status; st != "Queued" {
				t.Fatalf("one deferral must leave the member Queued, got %q", st)
			}
			rec, _ := env.Engine.StoreItemForTest(env.OwnerRepo, num)
			if has := !rec.CooldownAt["catch-up-deferred"].IsZero(); has != cooldown {
				t.Fatalf("catch-up-deferred cooldown recorded = %v, want %v", has, cooldown)
			}

			// A deeply idle poll, taken while the cooldown is unexpired, must not sleep past it.
			env.Engine.SetIdleStartForTest(time.Now().Add(-time.Hour))
			res, err := env.Engine.PollWithBackoff(context.Background(), env.PollInterval)
			if err != nil {
				t.Fatal(err)
			}
			if cooldown && res.NextInterval > time.Minute {
				t.Fatalf("a deferred member must keep the poll coming back soon; next poll in %v", res.NextInterval)
			}
			if !cooldown && res.NextInterval < 10*time.Minute {
				t.Fatalf("neutralised, the idle daemon should sleep at its idle cadence, got %v", res.NextInterval)
			}
		})
	}
}
