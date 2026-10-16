package sim

import (
	"errors"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twins for #2342: every ordinary landing write names the head the gates
// judged, and a push that lands between the gate and the merge request defers
// the landing silently; the new head lands on its own merits.
//
// The window is injected with FaultTable.BeforeNthCall: the push happens on the
// calling goroutine immediately before the engine's Nth landing call reaches the
// model, i.e. after every gate has cleared and before GitHub sees the request.
// A scenario that could only push between polls would pass against unpinned
// code, because the next poll's gates would simply read the new head.

// latePush records the heads around an injected push.
type latePush struct{ oldHead, newHead string }

func driveToValidateComplete(t *testing.T, env *Env, title string) int {
	t.Helper()
	num := FileIssue(t, env, title, "Prove that no unjudged head is merged (#2342).", "Specify", "fabrik:yolo")
	return num
}

// validateRuns records, at every call to a landing method, how many times Validate
// had run. A head pushed after Validate completed may land only once Validate has
// re-run on it, so a successful landing write seen at a count of 1 is an unvalidated
// head (#2342, Validate-SHA check at the landing decision).
type validateRuns struct {
	mu     sync.Mutex
	counts []int
}

func recordValidateRuns(env *Env, method string) *validateRuns {
	v := &validateRuns{}
	env.Sim.Faults().BeforeCall(method, func(simgh.Args) {
		n := env.Claude.StageCallCount("Validate")
		v.mu.Lock()
		v.counts = append(v.counts, n)
		v.mu.Unlock()
	})
	return v
}

// at returns the Validate run count at the i-th call (0 when the call is unknown).
func (v *validateRuns) at(i int) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if i < len(v.counts) {
		return v.counts[i]
	}
	return 0
}

// assertNoDeferralSideEffects fails if the deferral produced a pause, a rebase
// request or a failure label — a head mismatch is not a failure.
func assertNoDeferralSideEffects(t *testing.T, env *Env, num int) {
	t.Helper()
	for _, e := range env.Sim.Log().ByMethod("AddLabelToIssue") {
		if e.Args.Number != num {
			continue
		}
		switch e.Args.Label {
		case "fabrik:paused", "fabrik:awaiting-input", "fabrik:rebase-needed":
			t.Errorf("a head mismatch must not label the item %q", e.Args.Label)
		}
		if strings.HasSuffix(e.Args.Label, ":failed") {
			t.Errorf("a head mismatch must not label the item %q", e.Args.Label)
		}
	}
}

// TestLandingHeadPin_AutoMerge_PushAfterGate_DefersThenNewHeadLands: native
// auto-merge enablement is pinned; a push between the gate and the enable call
// is refused by the model with gh.ErrHeadMoved, the engine defers silently, and
// the next landing decision enables auto-merge on the new head.
func TestLandingHeadPin_AutoMerge_PushAfterGate_DefersThenNewHeadLands(t *testing.T) {
	t.Parallel()
	calls, lp, runs := runAutoMergeLatePush(t, false)

	if len(calls) < 2 {
		t.Fatalf("want a refused enable then a successful one, got %d call(s)", len(calls))
	}
	if first := calls[0]; !errors.Is(first.Err, gh.ErrHeadMoved) || first.Args.SHA != lp.oldHead {
		t.Errorf("first enable = (err %v, pinned %s), want ErrHeadMoved pinned to the judged head %s", first.Err, first.Args.SHA, lp.oldHead)
	}
	// Whatever lands is a head Validate re-ran on: the late push is never enabled as
	// is (the Validate-SHA check re-runs Validate, which commits its own report, so
	// the landed head is the post-Validate one, not lp.newHead).
	landed := false
	for i, c := range calls {
		if c.Err != nil {
			continue
		}
		landed = true
		if c.Args.SHA == lp.oldHead {
			t.Errorf("auto-merge was enabled on the superseded head %s", lp.oldHead)
		}
		if n := runs.at(i); n < 2 {
			t.Errorf("auto-merge enabled on %s with Validate run %d time(s): the head was not validated", c.Args.SHA, n)
		}
	}
	if !landed {
		t.Error("no successful enable after the deferral")
	}
}

// TestLandingHeadPin_ValidateSHACheckDisabled_LandsTheUnvalidatedHead is the
// non-vacuity twin: with the landing decision's Validate-SHA check neutralised, the
// late push is enabled for auto-merge before Validate ever ran on it.
func TestLandingHeadPin_ValidateSHACheckDisabled_LandsTheUnvalidatedHead(t *testing.T) {
	t.Parallel()
	calls, lp, runs := runAutoMergeLatePush(t, true)

	unvalidated := false
	for i, c := range calls {
		if c.Err == nil && c.Args.SHA == lp.newHead && runs.at(i) < 2 {
			unvalidated = true
		}
	}
	if !unvalidated {
		t.Errorf("with the check off the late push %s should have been enabled before Validate re-ran on it; calls=%+v", lp.newHead, calls)
	}
}

// runAutoMergeLatePush drives a yolo item to the landing decision, pushes a commit
// right before the first auto-merge enable, waits until auto-merge is enabled and
// returns the enable calls, the push, and the Validate run counts per call.
func runAutoMergeLatePush(t *testing.T, disableValidateSHACheck bool) ([]simgh.Entry, *latePush, *validateRuns) {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	if disableValidateSHACheck {
		defer env.Engine.SetLandingValidateSHACheckDisabledForTest()()
	}
	num := driveToValidateComplete(t, env, "sim landing head pin: auto-merge")
	runs := recordValidateRuns(env, "EnablePullRequestAutoMerge")

	lp := &latePush{}
	env.Sim.Faults().BeforeNthCall("EnablePullRequestAutoMerge", 1, func() {
		old, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
		lp.oldHead = old
		env.Sim.Sim().SeedCommit(env.OwnerRepo, memberBranch(num), map[string]string{"late.txt": "after the gate\n"}, "late push")
		lp.newHead, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	})

	WaitForIssueLabel(t, env, num, "fabrik:auto-merge-enabled", 200)
	if lp.newHead == "" || lp.newHead == lp.oldHead {
		t.Fatalf("the late push did not happen (old=%q new=%q)", lp.oldHead, lp.newHead)
	}
	assertNoDeferralSideEffects(t, env, num)
	return env.Sim.Log().ByMethod("EnablePullRequestAutoMerge"), lp, runs
}

// TestLandingHeadPin_DirectMergeFallback_PushAfterGate_DefersThenNewHeadLands:
// when auto-merge is refused as already clean, the direct merge is pinned too.
func TestLandingHeadPin_DirectMergeFallback_PushAfterGate_DefersThenNewHeadLands(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	// Native auto-merge unavailable: the engine falls back to a direct merge.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	num := driveToValidateComplete(t, env, "sim landing head pin: direct merge")

	runs := recordValidateRuns(env, "MergePRAtHeadSHA")
	var lp latePush
	env.Sim.Faults().BeforeNthCall("MergePRAtHeadSHA", 1, func() {
		old, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
		lp.oldHead = old
		env.Sim.Sim().SeedCommit(env.OwnerRepo, memberBranch(num), map[string]string{"late.txt": "after the gate\n"}, "late push")
		lp.newHead, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	})

	WaitForIssueClosed(t, env, num, 200)
	if lp.newHead == "" || lp.newHead == lp.oldHead {
		t.Fatalf("the late push did not happen (old=%q new=%q)", lp.oldHead, lp.newHead)
	}
	merges := env.Sim.Log().ByMethod("MergePRAtHeadSHA")
	if len(merges) < 2 {
		t.Fatalf("want a refused merge then a successful one, got %d call(s)", len(merges))
	}
	if first := merges[0]; !errors.Is(first.Err, gh.ErrHeadMoved) || first.Args.Values[0] != lp.oldHead {
		t.Errorf("first merge = (err %v, pinned %v), want ErrHeadMoved pinned to the judged head %s", first.Err, first.Args.Values, lp.oldHead)
	}
	for i, m := range merges {
		if m.Err != nil {
			continue
		}
		if m.Args.Values[0] == lp.oldHead {
			t.Errorf("the superseded head %s was merged", lp.oldHead)
		}
		if n := runs.at(i); n < 2 {
			t.Errorf("merged %s with Validate run %d time(s): the head was not validated", m.Args.Values[0], n)
		}
	}
	assertNoDeferralSideEffects(t, env, num)
}

// TestLandingHeadPin_AlreadyClean_PushAfterEnableRefusal: auto-merge is refused
// as already clean (a repo with no required checks) and the head moves before the
// fallback merge — the exact scenario of the issue.
func TestLandingHeadPin_AlreadyClean_PushAfterEnableRefusal_DoesNotMergeUnchecked(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	num := driveToValidateComplete(t, env, "sim landing head pin: already clean")

	runs := recordValidateRuns(env, "MergePRAtHeadSHA")
	var lp latePush
	env.Sim.Faults().BeforeNthCall("EnablePullRequestAutoMerge", 1, func() {
		pr, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		env.Sim.Sim().SeedAutoMergeRefusedClean(env.OwnerRepo, pr.Number)
	})
	env.Sim.Faults().BeforeNthCall("MergePRAtHeadSHA", 1, func() {
		old, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
		lp.oldHead = old
		env.Sim.Sim().SeedCommit(env.OwnerRepo, memberBranch(num), map[string]string{"late.txt": "unchecked\n"}, "late push")
		lp.newHead, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	})

	WaitForIssueClosed(t, env, num, 200)
	merges := env.Sim.Log().ByMethod("MergePRAtHeadSHA")
	if len(merges) == 0 || !errors.Is(merges[0].Err, gh.ErrHeadMoved) {
		t.Fatalf("the fallback merge of the moved head must be refused with ErrHeadMoved, got %+v", merges)
	}
	merged := false
	for i, m := range merges {
		if m.Err != nil {
			continue
		}
		merged = true
		if m.Args.Values[0] == lp.oldHead {
			t.Errorf("merged the unchecked head %s", lp.oldHead)
		}
		if n := runs.at(i); n < 2 {
			t.Errorf("merged %s with Validate run %d time(s): the head was not validated", m.Args.Values[0], n)
		}
	}
	if !merged {
		t.Error("the item closed without a successful pinned merge")
	}
	assertNoDeferralSideEffects(t, env, num)
}

// TestLandingHeadPin_MergeQueueEnqueue_PushAfterGate_DefersThenNewHeadLands: the
// merge-queue enqueue is pinned.
func TestLandingHeadPin_MergeQueueEnqueue_PushAfterGate_DefersThenNewHeadLands(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Sim.Sim().SeedMergeQueueEnabled(env.OwnerRepo, true)
	num := driveToValidateComplete(t, env, "sim landing head pin: enqueue")

	runs := recordValidateRuns(env, "EnqueuePullRequest")
	var lp latePush
	env.Sim.Faults().BeforeNthCall("EnqueuePullRequest", 1, func() {
		old, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
		lp.oldHead = old
		env.Sim.Sim().SeedCommit(env.OwnerRepo, memberBranch(num), map[string]string{"late.txt": "after the gate\n"}, "late push")
		lp.newHead, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	})

	WaitForIssueLabel(t, env, num, "fabrik:auto-merge-enabled", 200)
	calls := env.Sim.Log().ByMethod("EnqueuePullRequest")
	if len(calls) < 2 {
		t.Fatalf("want a refused enqueue then a successful one, got %d call(s)", len(calls))
	}
	if first := calls[0]; !errors.Is(first.Err, gh.ErrHeadMoved) || first.Args.SHA != lp.oldHead {
		t.Errorf("first enqueue = (err %v, pinned %s), want ErrHeadMoved pinned to %s", first.Err, first.Args.SHA, lp.oldHead)
	}
	last := len(calls) - 1
	if c := calls[last]; c.Err != nil || c.Args.SHA == lp.oldHead {
		t.Errorf("last enqueue = (err %v, pinned %s), want a success on a head other than the superseded %s", c.Err, c.Args.SHA, lp.oldHead)
	}
	if n := runs.at(last); n < 2 {
		t.Errorf("enqueued with Validate run %d time(s): the head was not validated", n)
	}
	assertNoDeferralSideEffects(t, env, num)
}
