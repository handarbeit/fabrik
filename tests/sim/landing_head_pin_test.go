package sim

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// Sim twins for #2342: every ordinary landing write names the head the gates
// judged, and a push that lands between the gate and the merge request defers
// the landing silently; the new head lands on its own merits.
//
// The window is injected with FaultTable.BeforeCall: the push happens on the
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
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	num := driveToValidateComplete(t, env, "sim landing head pin: auto-merge")

	var lp latePush
	env.Sim.Faults().BeforeCall("EnablePullRequestAutoMerge", 1, func() {
		old, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
		lp.oldHead = old
		env.Sim.Sim().SeedCommit(env.OwnerRepo, memberBranch(num), map[string]string{"late.txt": "after the gate\n"}, "late push")
		lp.newHead, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	})

	WaitForIssueLabel(t, env, num, "fabrik:auto-merge-enabled", 200)
	if lp.newHead == "" || lp.newHead == lp.oldHead {
		t.Fatalf("the late push did not happen (old=%q new=%q)", lp.oldHead, lp.newHead)
	}

	calls := env.Sim.Log().ByMethod("EnablePullRequestAutoMerge")
	if len(calls) < 2 {
		t.Fatalf("want a refused enable then a successful one, got %d call(s)", len(calls))
	}
	if first := calls[0]; !errors.Is(first.Err, gh.ErrHeadMoved) || first.Args.SHA != lp.oldHead {
		t.Errorf("first enable = (err %v, pinned %s), want ErrHeadMoved pinned to the judged head %s", first.Err, first.Args.SHA, lp.oldHead)
	}
	// The post-landing SHA-invalidation backstop may re-run Validate on the new
	// head and enable again later (a further head), so the assertion is that some
	// enable succeeded pinned to the new head — never that it was the last one.
	pinnedNew := false
	for _, c := range calls {
		if c.Err == nil && c.Args.SHA == lp.newHead {
			pinnedNew = true
		}
	}
	if !pinnedNew {
		t.Errorf("no successful enable pinned to the new head %s", lp.newHead)
	}
	for _, c := range calls {
		if c.Err == nil && c.Args.SHA == lp.oldHead {
			t.Errorf("auto-merge was enabled on the superseded head %s", lp.oldHead)
		}
	}
	assertNoDeferralSideEffects(t, env, num)
}

// TestLandingHeadPin_DirectMergeFallback_PushAfterGate_DefersThenNewHeadLands:
// when auto-merge is refused as already clean, the direct merge is pinned too.
func TestLandingHeadPin_DirectMergeFallback_PushAfterGate_DefersThenNewHeadLands(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	// Native auto-merge unavailable: the engine falls back to a direct merge.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	num := driveToValidateComplete(t, env, "sim landing head pin: direct merge")

	var lp latePush
	env.Sim.Faults().BeforeCall("MergePRAtHeadSHA", 1, func() {
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
	for _, m := range merges {
		if m.Err == nil && m.Args.Values[0] == lp.oldHead {
			t.Errorf("the superseded head %s was merged", lp.oldHead)
		}
	}
	last := merges[len(merges)-1]
	if last.Err != nil || last.Args.Values[0] != lp.newHead {
		t.Errorf("last merge = (err %v, pinned %v), want success pinned to the new head %s", last.Err, last.Args.Values, lp.newHead)
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

	var lp latePush
	env.Sim.Faults().BeforeCall("EnablePullRequestAutoMerge", 1, func() {
		pr, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		env.Sim.Sim().SeedAutoMergeRefusedClean(env.OwnerRepo, pr.Number)
	})
	env.Sim.Faults().BeforeCall("MergePRAtHeadSHA", 1, func() {
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
	for _, m := range merges {
		if m.Err == nil && m.Args.Values[0] != lp.newHead {
			t.Errorf("merged %s, want only the new head %s after its own judgement", m.Args.Values[0], lp.newHead)
		}
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

	var lp latePush
	env.Sim.Faults().BeforeCall("EnqueuePullRequest", 1, func() {
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
	if last := calls[len(calls)-1]; last.Err != nil || last.Args.SHA != lp.newHead {
		t.Errorf("last enqueue = (err %v, pinned %s), want success pinned to the new head %s", last.Err, last.Args.SHA, lp.newHead)
	}
	assertNoDeferralSideEffects(t, env, num)
}
