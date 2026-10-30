package sim

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// Sim twin of #2530 (ADR 2530): an item parked at Validate-complete under
// fabrik:cruise, with a clean PR and no fabrik:awaiting-ci, is re-evaluated by the
// periodic safety net every 10 polls. The CI gate used to clear each time and
// re-add stage:Validate:complete, which GitHub answers with no event but a bumped
// updated_at (modelled by simgh since #2530) — a label write per re-evaluation and
// a drift line on the next light reconcile.

func completeLabelAdds(env *Env, num int) int {
	n := 0
	for _, e := range env.Sim.Log().ByMethod("AddLabelToIssue") {
		if e.Args.Number == num && e.Args.Label == "stage:Validate:complete" {
			n++
		}
	}
	return n
}

// parkCompleteUnderCruise drives an item through the real pipeline to
// stage:Validate:complete under cruise (no landing), so the periodic
// re-evaluation stamp is the one a real run leaves.
func parkCompleteUnderCruise(t *testing.T, env *Env, title string) int {
	t.Helper()
	num := FileIssue(t, env, title, "Re-evaluated while waiting.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR, got %+v (err %v)", pr, err)
	}
	seedGreenPrefix(t, env, pr.HeadSHA)
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 40)
	if hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		RunPolls(t, env, 2)
	}
	return num
}

// rewriteRun polls through several periodic re-evaluations (every 10 polls) and
// returns the completion-label adds made and the drift the light reconciles saw.
func rewriteRun(t *testing.T, env *Env, num int) (adds, drift int) {
	t.Helper()
	RunPolls(t, env, 2)
	base := completeLabelAdds(env, num)
	for i := 0; i < 70; i++ {
		RunPoll(t, env)
		env.Clock.Advance(45 * time.Second) // reconcile runs on its own timer, after the 30 s engine-write shield
		d, _, _, err := env.BoardCache.LightReconcile(env.Owner, env.Repo, env.ProjectNum, env.cfg.OwnerType)
		if err != nil {
			t.Fatalf("LightReconcile: %v", err)
		}
		drift += d
	}
	return completeLabelAdds(env, num) - base, drift
}

func newCompleteRewriteEnv(t *testing.T, logOut ...io.Writer) *Env {
	t.Helper()
	var out io.Writer
	if len(logOut) > 0 {
		out = logOut[0]
	}
	env := NewEnv(t, EnvOptions{
		LogOut:     out,
		Stages:     ciFixStages(),
		StartTime:  time.Now(),
		BoardCache: true,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			cfg.PollSeconds = 60
		},
	})
	// The cache path admits items through the reactive mayNeedWork observer.
	t.Cleanup(env.Engine.RegisterObservers())
	env.Claude.ForStageComments("Validate", ciFixCommentScript(env.Sim, env.OwnerRepo, "success"))
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

func TestCompleteLabelRewrite_WaitingItemWritesNothing(t *testing.T) {
	t.Parallel()
	env := newCompleteRewriteEnv(t)
	num := parkCompleteUnderCruise(t, env, "complete label rewrite (#2530)")
	adds, drift := rewriteRun(t, env, num)
	if adds != 0 {
		t.Errorf("a re-evaluated Validate-complete item re-added stage:Validate:complete %d time(s)\n\n%s", adds, diagnostics(env))
	}
	if drift != 0 {
		t.Errorf("light reconcile reported %d drifted item(s) for a waiting, unchanged item\n\n%s", drift, diagnostics(env))
	}
}

// Non-vacuity: with the skip neutralised the repeated add (and its drift) returns.
func TestCompleteLabelRewrite_NeutralisedRestoresTheRepeatedAdd(t *testing.T) {
	t.Parallel()
	env := newCompleteRewriteEnv(t)
	restore := env.Engine.SetCompleteLabelSkipDisabledForTest()
	defer restore()
	num := parkCompleteUnderCruise(t, env, "complete label rewrite neutralised (#2530)")
	adds, drift := rewriteRun(t, env, num)
	if adds < 3 {
		t.Fatalf("non-vacuity: with the skip off the gate re-added the label %d time(s), want >= 3 (drift %d)\n\n%s", adds, drift, diagnostics(env))
	}
	t.Logf("neutralised: %d adds, %d drifted", adds, drift)
}

// lockedBuffer is a goroutine-safe log capture.
type lockedBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

// idleResets drives PollWithBackoff through several periodic re-evaluations of a
// parked item and counts the "activity detected" resets of the idle backoff.
func idleResets(t *testing.T, neutralise bool) int {
	t.Helper()
	var buf lockedBuffer
	env := newCompleteRewriteEnv(t, &buf)
	if neutralise {
		defer env.Engine.SetPeriodicActivityNarrowingDisabledForTest()()
	}
	num := parkCompleteUnderCruise(t, env, "idle backoff (#2530)")
	_ = num
	// Two quiet polls start the idle timer; the resets counted are the ones after.
	for i := 0; i < 3; i++ {
		runPollWithBackoff(t, env, env.PollInterval)
	}
	before := strings.Count(buf.String(), "activity detected")
	for i := 0; i < 70; i++ {
		runPollWithBackoff(t, env, env.PollInterval)
	}
	return strings.Count(buf.String(), "activity detected") - before
}

// A re-evaluation that writes nothing is not activity: a daemon whose only work is
// waiting complete items stays idle (#2530, requirement 4).
func TestCompleteLabelRewrite_PeriodicReEvalDoesNotResetIdleBackoff(t *testing.T) {
	t.Parallel()
	if n := idleResets(t, false); n != 0 {
		t.Errorf("%d idle-backoff reset(s) from periodic re-evaluations of an unchanged item, want 0", n)
	}
}

func TestCompleteLabelRewrite_IdleNarrowingNeutralisedResetsAgain(t *testing.T) {
	t.Parallel()
	if n := idleResets(t, true); n < 2 {
		t.Errorf("non-vacuity: with the narrowing off the re-evaluations reset the idle backoff %d time(s), want >= 2", n)
	}
}
