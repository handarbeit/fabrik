package sim

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// Sim twins for #2613 (ADR 2613): the merge gate removes a stale
// fabrik:rebase-needed as soon as GitHub reports the PR conflict-free
// (mergeable true, state blocked by pending checks), while still holding the item
// until CI settles. Unknown mergeability keeps the label. Community report,
// mentioned for context: #2608.

const rebaseLabel = "fabrik:rebase-needed"

func newRebaseLabelEnv(t *testing.T) *Env {
	t.Helper()
	return newUnknownMergeEnv(t, time.Now(), 0)
}

func rebaseLabelStatus(t *testing.T, env *Env, num int) *localapi.StatusResult {
	t.Helper()
	st, err := env.Engine.LocalAPIBackend().Status(localapi.StatusParams{Issue: strconv.Itoa(num)})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestMergeGateRebaseLabel_ClearedWhileCIPendingAndStillHeld(t *testing.T) {
	t.Parallel()
	env := newRebaseLabelEnv(t)
	num, _ := seedUnknownMergeItem(t, env, "rebased, ci pending", "in_progress", "", time.Time{}, rebaseLabel, "fabrik:awaiting-ci")

	RunPoll(t, env)
	if hasLabel(IssueLabels(t, env, num), rebaseLabel) {
		t.Fatalf("%s not cleared while mergeable with CI pending\n\n%s", rebaseLabel, diagnostics(env))
	}
	// Still held: not advanced past Validate, not paused, no rebase dispatched.
	st := rebaseLabelStatus(t, env, num)
	if got := st.Attention.Summary; strings.Contains(got, "dispatching a rebase") {
		t.Errorf("attention still claims a rebase dispatch: %q", got)
	}
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "stage:Validate:complete") {
		t.Errorf("item must stay held without pause or completion, labels %v", labels)
	}
	if n := env.Claude.StageCallCount("Validate"); n != 0 {
		t.Errorf("Validate invoked %d times; clearing the label must dispatch nothing", n)
	}
	// Re-evaluated on a later poll after the bypass label is gone (merge-unsettled cooldown).
	env.Clock.Advance(time.Duration(unknownMergePollSeconds) * time.Second)
	RunPoll(t, env)
	if hasLabel(IssueLabels(t, env, num), rebaseLabel) {
		t.Errorf("label re-appeared with no conflict")
	}
}

func TestMergeGateRebaseLabel_KeptWhileMergeabilityUnknown(t *testing.T) {
	t.Parallel()
	env := newRebaseLabelEnv(t)
	t0 := env.Clock.Now()
	num, _ := seedUnknownMergeItem(t, env, "rebased, mergeable unknown", "in_progress", "", t0.Add(10*time.Minute), rebaseLabel, "fabrik:awaiting-ci")

	RunPoll(t, env)
	if !hasLabel(IssueLabels(t, env, num), rebaseLabel) {
		t.Fatalf("%s must be kept while mergeability is unknown\n\n%s", rebaseLabel, diagnostics(env))
	}
}

// Non-vacuity: with the clearing neutralised the label survives the same scenario.
func TestMergeGateRebaseLabel_NeutralisedKeepsLabel(t *testing.T) {
	t.Parallel()
	env := newRebaseLabelEnv(t)
	defer env.Engine.SetRebaseLabelClearOnPendingDisabledForTest()()
	num, _ := seedUnknownMergeItem(t, env, "rebased, neutralised", "in_progress", "", time.Time{}, rebaseLabel, "fabrik:awaiting-ci")

	RunPoll(t, env)
	if !hasLabel(IssueLabels(t, env, num), rebaseLabel) {
		t.Fatalf("neutralised run must keep %s (the twin would be vacuous otherwise)", rebaseLabel)
	}
}
