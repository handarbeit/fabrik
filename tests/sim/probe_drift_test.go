package sim

import (
	"strconv"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// TestProbeDrift_ClosedUnmergedPR_BoundedCallsAndNoWakes is the #2080 sim
// scenario. Two closed items each have a PR that was closed unmerged: GitHub's
// closedByPullRequestsReferences omits it, so the board probe and the deep fetch
// both report no linked PR, while the engine's REST lookup (FetchLinkedPR) finds
// it and writes it into the cache — the standing disagreement behind the
// 8,767-invalidations-a-day loop on handarbeit/fabrik#1655.
//
//   - #1 is closed in Done (a terminal item): it must not even look at linkage.
//   - #2 is open and paused in Review, never flagged terminal: its disagreement must
//     invalidate at most once, then stay quiet.
//
// Over many poll cycles the board is probed once per poll, the deep fetch is
// paid at most once per item, and nothing asks for an early poll.
func TestProbeDrift_ClosedUnmergedPR_BoundedCallsAndNoWakes(t *testing.T) {
	t.Parallel()
	const polls = 30
	probes, fetches, wakes := runProbeDriftScenario(t, polls, nil)
	if probes != polls {
		t.Errorf("ProbeProjectBoard calls over %d polls = %d, want exactly one per poll", polls, probes)
	}
	if fetches > 1 {
		t.Errorf("FetchItemDetails calls over %d polls = %d, want at most 1 (one invalidation for the closed-PR disagreement, none for the terminal item)", polls, fetches)
	}
	if wakes != 0 {
		t.Errorf("the probe loop requested %d early poll(s) over %d cycles, want 0", wakes, polls)
	}
}

// TestProbeDrift_Neutralised_PreFixBehaviorReturns is the non-vacuity check for
// the scenario above: with the convergence ledger and the terminal skip switched
// off, the same world pays a deep fetch per non-terminal item per poll and the
// terminal item invalidates every poll, so the bound above would fail.
func TestProbeDrift_Neutralised_PreFixBehaviorReturns(t *testing.T) {
	t.Parallel()
	const polls = 30
	_, fetches, _ := runProbeDriftScenario(t, polls, func(env *Env) {
		env.Engine.SetProbeDriftNeutralisationForTest(true, true)
	})
	if fetches < polls {
		t.Errorf("with the fixes neutralised FetchItemDetails calls = %d, want at least one per poll (%d): the scenario would pass vacuously", fetches, polls)
	}
}

// runProbeDriftScenario builds the two-closed-items world, runs polls steady-state
// cycles and returns the ProbeProjectBoard and FetchItemDetails call counts and
// the number of early polls requested over those cycles. neutralise, when
// non-nil, runs before the first poll.
func runProbeDriftScenario(t *testing.T, polls int, neutralise func(*Env)) (probes, fetches, wakes int) {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	if neutralise != nil {
		neutralise(env)
	}
	s := env.Sim.Sim()

	const doneItem, reviewItem = 1, 2
	s.SeedIssue(env.OwnerRepo, simgh.IssueSeed{Number: doneItem, Title: "closed not planned", Status: "Done", State: "CLOSED"})
	// #2 is open and paused awaiting input, so the engine leaves it alone while
	// it stays warm in the cache — the non-terminal variant of the loop.
	s.SeedIssue(env.OwnerRepo, simgh.IssueSeed{Number: reviewItem, Title: "paused", Status: "Review",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}})
	for _, n := range []int{doneItem, reviewItem} {
		branch := "fabrik/issue-" + strconv.Itoa(n)
		s.SeedCommit(env.OwnerRepo, branch, map[string]string{"f" + strconv.Itoa(n) + ".txt": "x"}, "work")
		s.SeedPR(env.OwnerRepo, simgh.PRSeed{Number: 100 + n, Title: "pr", Head: branch, State: "closed", IssueNumber: n})
	}
	if err := s.Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Poll 1 bootstraps the cache from the probe, which reports no linked PR;
	// poll 2 deep-fetches the non-terminal item, so its cache is warm before the
	// REST lookup below writes the closed PR into it.
	RunPolls(t, env, 2)

	// The engine's REST path finds the closed PR and records it in the cache,
	// exactly as engine/stages.go and engine/repo.go do through readClient.
	for _, n := range []int{doneItem, reviewItem} {
		pr, err := env.BoardCache.FetchLinkedPR(env.Owner, env.Repo, n)
		if err != nil || pr == nil {
			t.Fatalf("FetchLinkedPR(#%d) = %v, %v; want the closed PR", n, pr, err)
		}
	}
	// Let the first post-seed poll settle whatever one-time work it owes, then
	// measure the steady state.
	RunPoll(t, env)
	for len(env.Wake) > 0 {
		<-env.Wake
	}
	log := env.Sim.Log()
	probesBefore := len(log.ByMethod("ProbeProjectBoard"))
	fetchesBefore := len(log.ByMethod("FetchItemDetails"))

	RunPolls(t, env, polls)

	return len(log.ByMethod("ProbeProjectBoard")) - probesBefore,
		len(log.ByMethod("FetchItemDetails")) - fetchesBefore,
		len(env.Wake)
}
