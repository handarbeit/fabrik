package sim

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestMilestoneCaptured pins #1967 R10 against the real engine: a board read
// through the sim carries each issue's milestone, and the engine's store ends
// up holding it — set, and known-none for an unmilestoned issue — without any
// extra GitHub call. Unmilestoned and milestoned items must be distinguishable
// (none vs. unknown), which is the whole point of MilestoneKnown.
func TestMilestoneCaptured(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})

	withMS := FileIssue(t, env, "has milestone", "body", "Specify")
	without := FileIssue(t, env, "no milestone", "body", "Specify")
	env.Sim.Sim().SeedIssueMilestone(env.OwnerRepo, withMS, &gh.Milestone{Title: "v1.2", Number: 7})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The simulated board read itself carries the field.
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	var sawSet, sawNone bool
	for _, it := range board.Items {
		switch it.Number {
		case withMS:
			sawSet = it.MilestoneKnown && it.Milestone != nil && it.Milestone.Title == "v1.2" && it.Milestone.Number == 7
		case without:
			sawNone = it.MilestoneKnown && it.Milestone == nil
		}
	}
	if !sawSet || !sawNone {
		t.Fatalf("board did not carry milestone: set=%v none=%v", sawSet, sawNone)
	}

	RunPoll(t, env)

	ms, known, inStore := env.Engine.ItemMilestoneForTest(env.OwnerRepo, withMS)
	if !inStore || !known || ms == nil || ms.Title != "v1.2" || ms.Number != 7 {
		t.Fatalf("engine store milestone for #%d = %+v known=%v inStore=%v, want v1.2 #7", withMS, ms, known, inStore)
	}
	ms, known, inStore = env.Engine.ItemMilestoneForTest(env.OwnerRepo, without)
	if !inStore || !known || ms != nil {
		t.Fatalf("engine store milestone for #%d = %+v known=%v inStore=%v, want known-none", without, ms, known, inStore)
	}

	// A later change on GitHub is NOT asserted here: the sim deliberately never
	// wires boardcache.CacheImpl in (see engine.NewWithDeps), and Reconcile —
	// the only path that refreshes a shallow field on an already-cached item —
	// lives there. boardcache's TestReconcileCapturesMilestone... and the
	// milestoned/demilestoned delta tests cover that half.
}
