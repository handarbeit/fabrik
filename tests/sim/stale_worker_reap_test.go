package sim

import (
	"testing"

	"github.com/handarbeit/fabrik/stages"
)

// Gap 3 (#1592): stale-worker-label reaping (engine/worker_liveness.go).
// forEachStaleUnworkedItem reaps orphaned fabrik:editing/fabrik:locked:<user>
// labels left behind when a worker dies mid-dispatch and the label survives
// on GitHub with no in-memory Worker record to match it. Sequence-shaped:
// the worker dies, a fresh process discovers the label N polls later —
// directly adjacent to the restart-recovery scenarios #1451 already built
// (tests/sim/restart.go's RestartEnv), reused here for exactly the genuine
// process-state boundary it exists to provide.
//
// Driven through Engine.RunStartupCleanup (ADR-1592) — the seam this issue
// adds, mirroring RegisterObservers/PollWithBackoff above: the five startup
// scans it wraps are unexported and called only from Run(), so gap 3 is
// otherwise 100% unreachable from tests/sim.
func staleWorkerReapStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// TestStaleWorkerReap_OrphanedLockAndEditingLabels is AC4: an orphaned
// fabrik:locked:<user> and a separate orphaned fabrik:editing label are each
// left on an issue, as if the worker holding it died before releasing it;
// RunStartupCleanup, run once a fresh process's store has been populated,
// reaps both.
//
// Both issues also carry stage:Specify:complete from the moment they're
// seeded — itemNeedsWork treats a stage-complete item as having no
// dispatchable work left (fabrik:editing is separately excluded from
// dispatch outright), so neither label can be touched by any ordinary
// dispatch path on the restarted env's own first poll. This isolates
// RunStartupCleanup as the only mechanism in the sequence that could
// possibly remove either label — the discriminating property a
// terminal-state check on its own could not establish.
//
// The lock label uses NewEnv's own hardcoded cfg.User ("fabrik-sim-bot" —
// see env.go's Config literal), matching what a real fabrik:locked:<user>
// label would read for this Env's configured identity.
//
// Non-vacuity (R5): both post-cleanup checks fail if RunStartupCleanup no
// longer reaps its target label. Confirmed by temporarily neutralizing
// forEachStaleUnworkedItem (returning immediately without ever calling fn)
// and observing both assertions fail together — and, run separately,
// confirmed each label's own removal call
// (removeLockLabel/removeEditingLabel) is what RunStartupCleanup actually
// depends on by tracing runStartupCleanup's two forEachStaleUnworkedItem
// passes directly.
func TestStaleWorkerReap_OrphanedLockAndEditingLabels(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: staleWorkerReapStages()})

	lockedNum := FileIssue(t, env, "orphaned lock",
		"Simulates a worker that died holding fabrik:locked:<user> — no active Worker record survives a restart.",
		"Specify", "fabrik:locked:fabrik-sim-bot", "stage:Specify:complete")
	editingNum := FileIssue(t, env, "orphaned editing",
		"Simulates a comment-processing worker that died holding fabrik:editing.",
		"Specify", "fabrik:editing", "stage:Specify:complete")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Restart: a genuine process-state boundary. RestartEnv discards the
	// engine and its store — Worker() becomes nil for everything — while
	// simgh's model, and both labels with it, survives unchanged. No poll
	// runs on the pre-restart env at all: gap 3's story begins with a fresh
	// process discovering labels a dead predecessor left behind, not with
	// this process's own dispatch history.
	restarted := RestartEnv(t, env)

	// One poll populates the restarted engine's store — RunStartupCleanup's
	// own documented precondition ("must be called after the store is
	// populated by the first poll cycle").
	RunPoll(t, restarted)
	if !hasLabel(IssueLabels(t, restarted, lockedNum), "fabrik:locked:fabrik-sim-bot") {
		t.Fatal("orphaned lock label vanished before RunStartupCleanup ran — seeding/timing assumption broken")
	}
	if !hasLabel(IssueLabels(t, restarted, editingNum), "fabrik:editing") {
		t.Fatal("orphaned editing label vanished before RunStartupCleanup ran — seeding/timing assumption broken")
	}

	restarted.Engine.RunStartupCleanup()

	if hasLabel(IssueLabels(t, restarted, lockedNum), "fabrik:locked:fabrik-sim-bot") {
		t.Error("RunStartupCleanup did not reap the orphaned fabrik:locked:<user> label")
	}
	if hasLabel(IssueLabels(t, restarted, editingNum), "fabrik:editing") {
		t.Error("RunStartupCleanup did not reap the orphaned fabrik:editing label")
	}
}
