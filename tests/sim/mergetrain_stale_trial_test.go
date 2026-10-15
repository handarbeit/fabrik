package sim

import (
	"strings"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// This file is the sim twin of the merge-train stale-trial rebuild (#2298, ADR 2298): a
// trial whose base branch advances conflictingly while its CI runs reads mergeable_state
// "dirty" — a fact about the trial's age, not the members — and must be rebuilt on the new
// base, uncharged, within the shared max_train_rebase_cycles budget, instead of being
// reported as the members' red (singleton → Validate / pause; batch → bisection).
//
// It depends on simgh reading a conflicting DRAFT PR as dirty (FIDELITY.md, precedence
// dirty > draft); without that the stale branch is unreachable here.
//
// Neutralisation: every scenario below fails with engine.SetTrainStaleAsRedForTest(true)
// (dirty mapped back to TrainCIRed unconditionally) — the singleton is sent to Implement
// and paused / auto-repaired, the batch bisects — which TestMergeTrainStaleTrial_
// NeutralisedDirtyIsRed demonstrates for the singleton scenario.

const staleBlame = "combined Validate is failing"

// staleVerdicts holds the first greenFrom-1 trials pending (no check runs), then greens
// every later trial. A conflicting base advance flips a held trial to dirty.
func staleVerdicts(greenFrom int) func([]int) []gh.CheckRun {
	var mu sync.Mutex
	n := 0
	return func([]int) []gh.CheckRun {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n < greenFrom {
			return nil
		}
		return []gh.CheckRun{greenCheckRun("")}
	}
}

// pollUntilTrial runs polls until a trial PR other than notPR is open and returns it.
func pollUntilTrial(t *testing.T, env *Env, notPR int) int {
	t.Helper()
	ph := pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool {
		return len(p) == 1 && p[0].TrialPR != 0 && p[0].TrialPR != notPR
	}, 30)
	return ph[0].TrialPR
}

// conflictBase advances main with a change that conflicts with the members' index.md.
func conflictBase(t *testing.T, env *Env, n int) {
	t.Helper()
	advanceMain(t, env, map[string]string{"index.md": strings.Repeat("base row\n", n)})
}

func assertNoBlame(t *testing.T, env *Env, nums ...int) {
	t.Helper()
	for _, n := range nums {
		for _, c := range commentsOn(t, env, n) {
			if strings.Contains(c.Body, staleBlame) || strings.Contains(c.Body, "auto-repair") || strings.Contains(c.Body, "ejected") {
				t.Errorf("#%d got a member-blaming comment for a stale trial:\n%s", n, c.Body)
			}
		}
		for _, l := range IssueLabels(t, env, n) {
			if l == "fabrik:paused" || l == "fabrik:revalidate" {
				t.Errorf("#%d carries %s after a stale trial", n, l)
			}
		}
	}
}

func staleEnv(t *testing.T, cycles int) *Env {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{AsyncTrain: true, ConfigureCfg: func(c *engine.Config) {
		c.MaxTrainRebaseCycles = cycles
	}})
	env.Claude.ForStageComments("Queued", claudeResolveConflict("index.md", "resolved row\n"))
	return env
}

func TestMergeTrainStaleTrial_SingletonIsRebuiltAndLands(t *testing.T) {
	t.Parallel()
	env := staleEnv(t, 3)
	num, _ := QueueMember(t, env, "stale-single", map[string]string{"index.md": "member row\n"})
	startTrialVerdictSeeder(t, env, staleVerdicts(2))

	first := pollUntilTrial(t, env, 0)
	conflictBase(t, env, 1)
	second := pollUntilTrial(t, env, first)
	if second == first {
		t.Fatal("no new trial was assembled on the new base")
	}

	WaitForProjectStatus(t, env, num, "Done", 30)
	assertNoBlame(t, env, num)
	if got := draftPRCount(env); got != 2 {
		t.Errorf("draft trial PRs = %d, want 2 (the stale one and its rebuild)", got)
	}
	if env.Claude.CommentCallCount("Queued") == 0 {
		t.Error("the rebuild must have resolved the conflict on the moved base through the agent")
	}
	if ph := env.Engine.TrainRunPhases(); len(ph) != 0 {
		t.Errorf("run still open after landing: %+v", ph)
	}
}

func TestMergeTrainStaleTrial_BatchIsRebuiltWithoutBisection(t *testing.T) {
	t.Parallel()
	env := staleEnv(t, 3)
	a, _ := QueueMember(t, env, "stale-batch-a", map[string]string{"index.md": "member row\n"})
	b, _ := QueueMember(t, env, "stale-batch-b", map[string]string{"b.txt": "b\n"})
	startTrialVerdictSeeder(t, env, staleVerdicts(2))

	first := pollUntilTrial(t, env, 0)
	conflictBase(t, env, 1)
	pollUntilTrial(t, env, first)

	WaitForProjectStatus(t, env, a, "Done", 30)
	WaitForProjectStatus(t, env, b, "Done", 10)
	assertNoBlame(t, env, a, b)
	if got := draftPRCount(env); got != 2 {
		t.Errorf("draft trial PRs = %d, want 2: a bisection would have opened half-trials", got)
	}
}

// Repeated base moves past max_train_rebase_cycles: the batch dissolves back to Queued
// with no pause and nobody ejected.
func TestMergeTrainStaleTrial_BudgetExhaustedDissolvesToQueued(t *testing.T) {
	t.Parallel()
	env := staleEnv(t, 2)
	num, _ := QueueMember(t, env, "stale-budget", map[string]string{"index.md": "member row\n"})
	startTrialVerdictSeeder(t, env, staleVerdicts(1000)) // CI never finishes: only the base moves

	prev := 0
	for i := 1; i <= 2; i++ {
		prev = pollUntilTrial(t, env, prev)
		conflictBase(t, env, i)
	}
	// The third trial (second rebuild) is made stale too; the next stale verdict finds the budget spent.
	prev = pollUntilTrial(t, env, prev)
	conflictBase(t, env, 3)
	for i := 0; i < 10 && !hasCommentContaining(t, env, num, "batch dissolved"); i++ {
		RunPoll(t, env)
	}

	if !hasCommentContaining(t, env, num, "kept moving under the trial") {
		t.Fatalf("no dissolve comment naming the moving base: %v", commentsOn(t, env, num))
	}
	assertNoBlame(t, env, num)
	if st := projectItem(t, env, num).Status; st != "Queued" {
		t.Errorf("member status = %q, want it still Queued", st)
	}
	if got := draftPRCount(env); got < 3 {
		t.Errorf("draft trial PRs = %d, want at least 3 (initial + 2 rebuilds)", got)
	}
}

// The rebuild count survives a daemon restart: with two cycles allowed, a stale trial
// before the restart and a stale trial after it spend the budget, and the next stale trial
// dissolves — not the fourth, as it would if the restart had reset the count.
func TestMergeTrainStaleTrial_CycleCountSurvivesRestart(t *testing.T) {
	t.Parallel()
	env := staleEnv(t, 2)
	num, _ := QueueMember(t, env, "stale-restart", map[string]string{"index.md": "member row\n"})
	startTrialVerdictSeeder(t, env, staleVerdicts(1000))

	first := pollUntilTrial(t, env, 0)
	conflictBase(t, env, 1)
	second := pollUntilTrial(t, env, first) // cycle 1 spent, persisted with the run

	env2 := restartMergeTrainEnv(t, env)
	startTrialVerdictSeeder(t, env2, staleVerdicts(1000))
	conflictBase(t, env2, 2)
	third := pollUntilTrial(t, env2, second) // cycle 2 spent (adopted run, restored count)
	conflictBase(t, env2, 3)

	for i := 0; i < 10 && !hasCommentContaining(t, env2, num, "batch dissolved"); i++ {
		RunPoll(t, env2)
	}
	if !hasCommentContaining(t, env2, num, "kept moving under the trial") {
		t.Fatalf("the third stale trial (after %d, %d, %d) must dissolve once two cycles are spent: %v", first, second, third, commentsOn(t, env2, num))
	}
	assertNoBlame(t, env2, num)
}

// Non-vacuity: with dirty mapped back to red the singleton scenario's guarantees fail.
func TestMergeTrainStaleTrial_NeutralisedDirtyIsRed(t *testing.T) {
	t.Parallel()
	env := staleEnv(t, 3)
	env.Engine.SetTrainStaleAsRedForTest(true)
	num, _ := QueueMember(t, env, "stale-neutral", map[string]string{"index.md": "member row\n"})
	startTrialVerdictSeeder(t, env, staleVerdicts(2))

	first := pollUntilTrial(t, env, 0)
	conflictBase(t, env, 1)
	for i := 0; i < 6; i++ {
		RunPoll(t, env)
	}
	_ = first
	blamed := false
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, staleBlame) || strings.Contains(c.Body, "auto-repair") {
			blamed = true
		}
	}
	if !blamed && projectItem(t, env, num).Status == "Queued" && draftPRCount(env) >= 2 {
		t.Error("with dirty mapped back to red the member must be rerouted/paused/auto-repaired, but the scenario behaved as if stale")
	}
}
