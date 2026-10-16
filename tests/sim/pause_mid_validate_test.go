package sim

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of #2341 (ADR 2341): an operator pauses a yolo item while its Validate
// run is in progress, and the engine must not merge when Validate completes —
// fabrik:paused is the operator's stop button and wins over the landing decision.
// Once the pause is lifted the same item lands on a later poll (R4).
//
// Shape: wait_for_ci: false (smokeStages, via gateEnv). The merge decision is made
// synchronously in handleStageComplete on the dispatch-time snapshot, which carries
// no pause; only the live label refresh sees it. That is the discriminating shape:
// with the pause guard neutralised the engine merges and the negative test fails
// (TestPauseMidValidate_NonVacuity pins it). Under wait_for_ci: true the pause is
// already honoured by the poll's paused-item filter, which is not the subject here.
//
// The pause is lifted by removing the pause pair directly: the sim has no human
// comment helper that drives the unblock path, and the unblock itself is ADR 1813's
// subject, not this twin's.

type pauseMidValidateProbe struct {
	mu     sync.Mutex
	calls  int
	paused bool
}

func (p *pauseMidValidateProbe) snapshot() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.paused
}

// runPauseMidValidate drives one yolo issue to Validate-complete; the Validate worker
// applies the pause pair mid-run when pause is set.
func runPauseMidValidate(t *testing.T, env *Env, pause bool) (int, *pauseMidValidateProbe) {
	t.Helper()
	probe := &pauseMidValidateProbe{}
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		probe.mu.Lock()
		probe.calls++
		probe.mu.Unlock()
		if pause {
			for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
				if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, issue.Number, l); err != nil {
					return "", false, engine.TokenUsage{}, err
				}
			}
			probe.mu.Lock()
			probe.paused = true
			probe.mu.Unlock()
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})
	num := FileIssue(t, env, "paused mid-Validate (#2341)", "body", "Specify", "fabrik:yolo")
	return num, probe
}

func assertNotLanded(t *testing.T, env *Env, num int) {
	t.Helper()
	for _, m := range []string{"MergePR", "MergePRAtHeadSHA", "EnablePullRequestAutoMerge"} {
		if n := len(env.Sim.Log().ByMethod(m)); n != 0 {
			t.Errorf("%s called %d time(s) for a paused item", m, n)
		}
	}
	if labelAdds(env, num, "fabrik:auto-merge-enabled") != 0 {
		t.Error("fabrik:auto-merge-enabled applied to a paused item")
	}
	if linkedPRMerged(env, num) {
		t.Error("linked PR merged while the item is paused")
	}
	if got := projectItem(t, env, num).Status; got != "Validate" {
		t.Errorf("board Status = %q, want Validate (no advance)", got)
	}
	if n := queuedMoves(env); n != 0 {
		t.Errorf("%d move(s) into Queued, want 0", n)
	}
	if projectItem(t, env, num).IsClosed {
		t.Error("issue closed, want open")
	}
}

func TestPauseMidValidate_BlocksLanding_ThenLandsAfterPauseLifted(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	num, probe := runPauseMidValidate(t, env, true)

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	for i := 0; i < 4; i++ {
		env.Clock.Advance(time.Second)
		RunPolls(t, env, 3)
	}

	if calls, paused := probe.snapshot(); calls != 1 || !paused {
		t.Fatalf("Validate worker calls=%d paused=%v, want 1/true — the scenario is vacuous", calls, paused)
	}
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") {
		t.Fatalf("pause label missing; labels = %v", labels)
	}
	assertNotLanded(t, env, num)
	// R2: the engine adds no label of its own beyond the completion label.
	if hasLabel(labels, "fabrik:blocked") || hasLabel(labels, "stage:Validate:failed") {
		t.Errorf("a refusal must not add state labels; labels = %v", labels)
	}

	// R4: the pause is lifted; a later poll lands the item.
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		// The engine already clears an orphaned fabrik:awaiting-input at completion.
		if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, l); err != nil && !errors.Is(err, gh.ErrNotFound) {
			t.Fatalf("lifting the pause: %v", err)
		}
	}
	WaitForIssueClosed(t, env, num, 200)
	if !linkedPRMerged(env, num) {
		t.Fatal("the item closed but its PR is not merged")
	}
	if n := len(env.Sim.Log().ByMethod("MergePR")); n != 1 {
		t.Errorf("MergePR called %d time(s) after the pause was lifted, want 1", n)
	}
}

// Positive control: the identical setup with no pause lands, so the negative case
// above is not a bed that cannot land at all.
func TestPauseMidValidate_PositiveControl(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	num, probe := runPauseMidValidate(t, env, false)

	WaitForIssueClosed(t, env, num, 200)

	if calls, paused := probe.snapshot(); calls != 1 || paused {
		t.Fatalf("control worker calls=%d paused=%v, want 1/false", calls, paused)
	}
	if !linkedPRMerged(env, num) {
		t.Fatal("issue closed but the PR is not merged")
	}
}

// Non-vacuity: with the pause guard neutralised the paused item is merged, which
// is what the negative case above fails on.
func TestPauseMidValidate_NonVacuity(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	env.Engine.SetPauseGuardDisabledForTest(true)
	num, _ := runPauseMidValidate(t, env, true)

	WaitForIssueClosed(t, env, num, 200)

	if !linkedPRMerged(env, num) {
		t.Fatal("with the pause guard disabled the paused item should have been merged")
	}
	if n := len(env.Sim.Log().ByMethod("MergePR")); n != 1 {
		t.Errorf("MergePR called %d time(s), want 1", n)
	}
}
