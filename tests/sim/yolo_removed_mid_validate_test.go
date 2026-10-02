package sim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of tests/e2e/mid_stage_label_test.go's
// TestYoloRemovedMidValidateBlocksMerge (ADR-1769): an operator removes
// fabrik:yolo while Validate is running, and the engine must not merge when
// Validate completes — the landing decision re-reads the item's LIVE autonomy
// labels (refreshAutonomyLabels, engine/stages.go), never the snapshot taken at
// dispatch.
//
// Two bed shapes, each with a negative case and a positive control:
//
//   - wait_for_ci: false (smokeStages). The merge decision is made synchronously
//     in handleStageComplete on the dispatch-time item snapshot, which still
//     carries fabrik:yolo. In the sim board.Items is refetched fresh every poll,
//     so this is the shape where the dispatch-snapshot staleness window is real
//     and the twin is DISCRIMINATING: short-circuiting refreshAutonomyLabels
//     makes the engine merge and the negative test fails.
//   - wait_for_ci: true (the live bed's shape). The merge decision moves to
//     runCatchUpPhase2 via settleAwaitingCIScan, which reads a fresh board item,
//     so neutralising refreshAutonomyLabels alone does not flip it — the same
//     limit the live test documents. It guards the end-to-end outcome under the
//     live bed's stage shape (an engine that ignores the removal on the
//     board-derived item still fails it).
//
// The Validate worker removes the label synchronously before returning, so the
// removal always lands after dispatch and before handleStageComplete. The live
// test's REST-poll window proof and INCONCLUSIVE branch therefore have no sim
// analogue. Train-off only: with no autonomy label runCatchUpPhase2 returns
// before the advanceToQueued fork, so the property is mode-invariant.

// yoloMidValidateProbe records what the scripted Validate worker saw.
type yoloMidValidateProbe struct {
	mu          sync.Mutex
	calls       int
	yoloAtStart bool
	removed     bool
}

func (p *yoloMidValidateProbe) snapshot() (calls int, yoloAtStart, removed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.yoloAtStart, p.removed
}

// yoloMidValidateEnv builds the bed. Config-level Yolo is explicitly false: it
// would mask the label removal (yoloActive := cfg.Yolo || hasYoloLabel(item)).
// AllowAutoMerge is false so a landing is the deterministic direct MergePR.
func yoloMidValidateEnv(t *testing.T, waitForCI bool) *Env {
	t.Helper()
	if !waitForCI {
		return gateEnv(t)
	}
	env := newFeedbackGateEnv(t, func(cfg *engine.Config) {
		cfg.MaxCiFixCycles = 5
	})
	return env
}

// runYoloMidValidate drives one issue under fabrik:yolo to Validate-complete.
// The Validate worker removes fabrik:yolo mid-run when removeYolo is set.
func runYoloMidValidate(t *testing.T, env *Env, waitForCI, removeYolo bool) (int, *yoloMidValidateProbe) {
	t.Helper()
	probe := &yoloMidValidateProbe{}
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		probe.mu.Lock()
		probe.calls++
		probe.yoloAtStart = hasLabel(issue.Labels, "fabrik:yolo")
		probe.mu.Unlock()
		if removeYolo {
			if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, issue.Number, "fabrik:yolo"); err != nil {
				return "", false, engine.TokenUsage{}, err
			}
			probe.mu.Lock()
			probe.removed = true
			probe.mu.Unlock()
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})

	first := "Specify"
	if waitForCI {
		first = "Implement"
	}
	num := FileIssue(t, env, "yolo removed mid-Validate (#2006)", "body", first, "fabrik:yolo")

	if waitForCI {
		// Validate completes only once CI is green on the PR head.
		WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 120)
		pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		if err != nil || pr == nil {
			t.Fatalf("FetchLinkedPR: %v", err)
		}
		env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
		if err := env.Sim.Sim().Err(); err != nil {
			t.Fatalf("SeedCheckRun: %v", err)
		}
	}
	return num, probe
}

// labelAdds counts successful label-add calls for label on issue num.
func labelAdds(env *Env, num int, label string) int {
	n := 0
	for _, e := range env.Sim.Log().ByMethod("AddLabelToIssue") {
		if !e.Failed() && e.Args.Number == num && e.Args.Label == label {
			n++
		}
	}
	return n
}

func yoloMidValidateNegative(t *testing.T, waitForCI bool) {
	t.Helper()
	env := yoloMidValidateEnv(t, waitForCI)
	num, probe := runYoloMidValidate(t, env, waitForCI, true)

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)

	// Settle window: later polls (incl. runCatchUpPhase2, which re-runs every
	// poll on a Validate-complete item) must not merge either.
	for i := 0; i < 4; i++ {
		env.Clock.Advance(time.Second)
		RunPolls(t, env, 3)
	}

	// Preconditions — a no-merge result is meaningless without them.
	calls, yoloAtStart, removed := probe.snapshot()
	if calls != 1 {
		t.Fatalf("Validate worker ran %d time(s), want exactly 1", calls)
	}
	if !yoloAtStart {
		t.Fatal("fabrik:yolo was not on the item at Validate dispatch — the scenario is vacuous")
	}
	if !removed {
		t.Fatal("the Validate worker never removed fabrik:yolo — the scenario is vacuous")
	}

	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "stage:Validate:complete") {
		t.Errorf("stage:Validate:complete missing; labels = %v", labels)
	}
	if hasLabel(labels, "fabrik:yolo") || labelAdds(env, num, "fabrik:yolo") > 1 {
		t.Errorf("fabrik:yolo was re-added by the engine; labels = %v, adds = %d", labels, labelAdds(env, num, "fabrik:yolo"))
	}
	if hasLabel(labels, "fabrik:auto-merge-enabled") || labelAdds(env, num, "fabrik:auto-merge-enabled") != 0 {
		t.Errorf("fabrik:auto-merge-enabled was applied after merge authority was withdrawn; labels = %v", labels)
	}
	for _, m := range []string{"MergePR", "MergePRAtHeadSHA", "EnablePullRequestAutoMerge"} {
		if n := len(env.Sim.Log().ByMethod(m)); n != 0 {
			t.Errorf("%s called %d time(s) after fabrik:yolo was removed mid-Validate — the landing used a stale autonomy snapshot (ADR-1769)", m, n)
		}
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr.Merged || strings.EqualFold(pr.State, "closed") {
		t.Errorf("linked PR merged=%v state=%q, want open and unmerged", pr.Merged, pr.State)
	}
	if got := projectItem(t, env, num).Status; got != "Validate" {
		t.Errorf("board Status = %q, want Validate (no advance to Queued/Done)", got)
	}
	if n := queuedMoves(env); n != 0 {
		t.Errorf("%d move(s) into Queued, want 0", n)
	}
	if projectItem(t, env, num).IsClosed {
		t.Error("issue closed, want open")
	}
}

func yoloMidValidatePositiveControl(t *testing.T, waitForCI bool) {
	t.Helper()
	env := yoloMidValidateEnv(t, waitForCI)
	num, probe := runYoloMidValidate(t, env, waitForCI, false)

	WaitForIssueClosed(t, env, num, 200)

	calls, yoloAtStart, removed := probe.snapshot()
	if calls != 1 || !yoloAtStart || removed {
		t.Fatalf("control worker: calls=%d yoloAtStart=%v removed=%v, want 1/true/false", calls, yoloAtStart, removed)
	}
	if !linkedPRMerged(env, num) {
		t.Fatal("issue closed but the PR is not merged — the setup cannot land, so the negative case proves nothing")
	}
	if n := len(env.Sim.Log().ByMethod("MergePR")); n != 1 {
		t.Errorf("MergePR called %d time(s), want 1", n)
	}
}

// TestYoloRemovedMidValidateBlocksMerge_NoWaitForCI is the discriminating twin
// (wait_for_ci: false): the merge decision runs in handleStageComplete on the
// dispatch-time snapshot, so it fails if refreshAutonomyLabels is neutralised.
func TestYoloRemovedMidValidateBlocksMerge_NoWaitForCI(t *testing.T) {
	t.Parallel()
	yoloMidValidateNegative(t, false)
}

// TestYoloRemovedMidValidateBlocksMerge_NoWaitForCI_PositiveControl: identical
// setup with fabrik:yolo retained must merge.
func TestYoloRemovedMidValidateBlocksMerge_NoWaitForCI_PositiveControl(t *testing.T) {
	t.Parallel()
	yoloMidValidatePositiveControl(t, false)
}

// TestYoloRemovedMidValidateBlocksMerge_WaitForCI is the live bed's shape
// (wait_for_ci: true): the landing decision is runCatchUpPhase2's. It guards the
// end-to-end outcome; it is not where neutralising refreshAutonomyLabels alone
// discriminates (see the file comment).
func TestYoloRemovedMidValidateBlocksMerge_WaitForCI(t *testing.T) {
	t.Parallel()
	yoloMidValidateNegative(t, true)
}

// TestYoloRemovedMidValidateBlocksMerge_WaitForCI_PositiveControl: identical
// setup with fabrik:yolo retained must merge.
func TestYoloRemovedMidValidateBlocksMerge_WaitForCI_PositiveControl(t *testing.T) {
	t.Parallel()
	yoloMidValidatePositiveControl(t, true)
}
