package sim

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

var errInjectedFeedbackRead = errors.New("simgh: injected 502 on the feedback gate's live read")

// #2498 sim twin: a single transient live-read failure at the advance holds the
// item with the short feedback-read-failed cooldown, so the next poll retries
// and the item advances; with the change neutralised the hold records the long
// feedback-pending cooldown and the item waits for the periodic re-evaluation.
func TestFeedbackGate_TransientReadFailureRetriedOnNextPoll(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, neutralise bool) (advancedAfter int) {
		env := newFeedbackGateEnv(t, nil)
		env.Engine.SetFeedbackReadFailShortCooldownDisabledForTest(neutralise)
		var armed atomic.Bool
		var num int
		// The Implement worker finishes, then the engine runs the advance gate; arm
		// the one-shot failure so it lands on that gate's live read.
		env.Claude.ForStage("Implement", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
			if armed.CompareAndSwap(false, true) {
				// The first read after the run is verifyAndHealLinkage's; the second is the
				// advance gate's own live read.
				var seen atomic.Int32
				env.Sim.Faults().FailWhen("FetchItemDetails",
					func(a simgh.Args) bool { return a.Number == issue.Number && seen.Add(1) == 2 }, 1, errInjectedFeedbackRead)
			}
			return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
		})
		num = FileIssue(t, env, "feedback gate transient read failure", "body", "Implement", "fabrik:yolo")

		failedAt := 0
		for i := 1; i <= 60; i++ {
			RunPoll(t, env)
			if failedAt == 0 && env.Sim.Faults().FiredCount("FetchItemDetails") == 1 {
				failedAt = i
			}
			if hasLabel(IssueLabels(t, env, num), "stage:Implement:complete") && projectItem(t, env, num).Status == "Review" {
				if failedAt == 0 {
					t.Fatal("the injected fault never fired")
				}
				return i - failedAt
			}
		}
		t.Fatal("item never advanced to Review")
		return 0
	}
	short := run(t, false)
	long := run(t, true)
	t.Logf("advanced after %d poll(s) with the short cooldown, %d neutralised", short, long)
	if short > 3 {
		t.Errorf("short cooldown: advanced after %d polls, want within about one poll of the failure", short)
	}
	if long <= short+3 {
		t.Errorf("neutralised: advanced after %d polls (short: %d); expected to wait for the periodic re-evaluation", long, short)
	}
}
