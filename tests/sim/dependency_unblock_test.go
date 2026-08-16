package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Gap 1 (#1592): dependency blocking and unblocking (engine/observers.go,
// engine/dependencies.go). Two issues interacting across polls — exactly
// what this package exists to drive, and untouched by any existing scenario
// before this file.
//
// PushUnblockObserver (engine/observers.go) fires on two distinct paths, and
// this file's two tests pin the asymmetry between them (R2):
//
//   - Path 1 (StateChanged): a blocker closes; the observer scans the store
//     for dependents listing it and removes fabrik:blocked once every
//     blocker is resolved.
//   - Path 2 (BlockedByChanged): the dependent's own BlockedBy is
//     re-populated by a deep-fetch; the observer inspects only that item.
//     It returns early when the new list is EMPTY (observers.go:299) —
//     deliberate, per ADR-1419: a stale-empty cache must never be trusted
//     as "no blockers" on its own.
//
// TestDependencyUnblock_OnBlockerClose (AC1) drives the close-and-unblock
// sequence. TestDependencyUnblock_EmptyEdgeListDoesNotUnblockViaObserver
// (AC2) proves the empty-list guard holds, and that recovery instead falls
// to the "dep-blocked" cooldown-gated pull path in checkDependencies
// (engine/dependencies.go) — the behaviour #1453's chain work hit in
// practice.
//
// # Why AC1's blocker is never placed on the project board
//
// tests/sim's Engine is built via NewWithDeps, which wires the bare
// GitHubAdapter as readClient (deliberately — see NewWithDeps's own doc
// comment), never boardcache.CacheImpl. Production's per-poll store refresh
// for EVERY board item, including ones outside normal dispatch admission
// (runProbeAndDeepFetch, engine/terminal.go), is CacheImpl-only machinery
// tests/sim never runs. Without it, e.store's per-item IsClosed field is
// written ONLY by itemstate.ItemDeepFetched, and ONLY for items
// itemMayNeedWork actually admits — which excludes a closed item outright
// unless it is either at a cleanup stage (which itself skips
// FetchItemDetails — see selectDeepFetchCandidates) or already carries
// stage:<itsStage>:complete for a non-cleanup stage it hasn't yet advanced
// away from. In practice a completed, yolo-advanced item leaves that window
// within the very same dispatch goroutine that completed it (confirmed by
// running the scenario both ways), so there is no poll at which a
// board-resident blocker is simultaneously closed, admitted, and freshly
// deep-fetched.
//
// Anything short of that leaves the blocker's store entry either absent
// (harmless — checkDependencies and PushUnblockObserver.allBlockersClosed
// both fall back to the dependency edge's own State field, which
// resolveDependenciesLocked always resolves correctly straight from the
// repo's issue record, independent of project-board membership — see
// tests/sim/simgh/board.go) or, worse, MATERIALIZED WRONG: the very first
// mutation any settle scan or cleanup dispatch applies to a
// previously-untracked item (e.g. SelfWriteObserved from
// settleClosedItemsToDone, or a plain LabelAdded from handleCleanupStage)
// creates a zero-value store entry via Store.getOrCreate whose IsClosed
// defaults to false — durably wrong, since nothing later corrects a field
// neither mutation type touches. checkDependencies's Store-preferred check
// then trusts that wrong entry over the correct dep.State fallback,
// re-blocking the dependent on its very next dispatch attempt. This has no
// production consequence — CacheImpl's probe refresh always keeps every
// board item's IsClosed current before any settle scan runs, so the race
// this harness exposes cannot occur there — but it is very much reachable
// here, and was hit and traced while writing this scenario.
//
// The clean way to route around it, matching how a blocker frequently is
// NOT itself a Fabrik-managed item in practice (an upstream issue, a
// human-owned issue, an issue in an unmanaged repo): file the blocker as a
// plain repo issue via Sim.SeedIssue with no Status, so it is never added
// to any project board at all. It never enters e.store, is never touched by
// any settle scan, and its true open/closed state is always available to
// the dependent's own BlockedBy resolution regardless. See
// TestDependencyUnblock_EmptyEdgeListDoesNotUnblockViaObserver's own doc
// comment for why its blocker does not need this treatment.
func dependencyUnblockStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Backlog", Order: 1, Unmanaged: true},
		{Name: "Specify", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// TestDependencyUnblock_OnBlockerClose is gap 1's first direction (AC1): a
// blocker issue closes, PushUnblockObserver clears fabrik:blocked from the
// dependent, and the dependent proceeds — actually dispatches to Specify,
// not merely loses the label.
//
// See dependencyUnblockStages's doc comment for why the blocker is filed as
// a board-less repo issue rather than a project item.
//
// Non-vacuity (R5): hasLabel(..., "stage:Specify:complete") is checked
// false on the dependent immediately after the first poll (still blocked),
// and the Precedes assertion below requires the label removal to genuinely
// precede the dependent's own dispatch, not merely both eventually being
// true — an implementation that dispatched Specify regardless of
// fabrik:blocked (dropping the gate entirely, engine/item.go:542) would
// still pass a terminal-state-only check but fails Precedes here, and fails
// the "still blocked" check above even sooner.
func TestDependencyUnblock_OnBlockerClose(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages()})
	// PushUnblockObserver is not registered by default — see NewEnv's own
	// doc comment (ADR-1592). Both scenarios in this file specifically need it.
	t.Cleanup(env.Engine.RegisterObservers())

	const blocker = 1000
	env.Sim.Sim().SeedIssue(env.OwnerRepo, simgh.IssueSeed{
		Number: blocker,
		Title:  "blocker",
		Body:   "This issue must close before the dependent below is allowed to proceed. Deliberately not placed on any project board — see dependencyUnblockStages's doc comment.",
	})
	dependent := FileIssue(t, env, "dependent",
		"Blocked on the blocker issue; must not dispatch to Specify until it closes.",
		"Specify", "fabrik:blocked")
	env.Sim.Sim().SeedBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// First poll: the dependent's alreadyBlocked live re-read
	// (engine/dependencies.go) finds the blocker still open — its label
	// survives and Specify is never dispatched. Confirms the scenario
	// actually starts blocked, not merely labelled so.
	RunPoll(t, env)
	if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("dependent lost fabrik:blocked before the blocker even closed")
	}
	if hasLabel(IssueLabels(t, env, dependent), "stage:Specify:complete") {
		t.Fatal("dependent's Specify stage completed while still blocked")
	}

	if err := env.Sim.CloseIssue(env.Owner, env.Repo, blocker); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	// PushUnblockObserver removes fabrik:blocked (o.Remove runs in its own
	// goroutine — see observers.go) once allBlockersClosed holds.
	WaitForLabelAbsent(t, env, dependent, "fabrik:blocked", 10)

	// "…and it proceeds": the dependent actually dispatches once unblocked.
	WaitForIssueLabel(t, env, dependent, "stage:Specify:complete", 10)

	// R1: a sequence-shaped claim — the unblock must genuinely precede the
	// dispatch it enables, not merely both end up true. Only the mutation
	// log (not a terminal-label read) can distinguish "unblocked, then
	// dispatched" from "dispatched regardless, unblocked separately".
	log := env.Sim.Log()
	unblock := simgh.LabelRemoved(dependent, "fabrik:blocked")
	dispatched := simgh.LabelAdded(dependent, "stage:Specify:complete")
	if before, err := log.Precedes(unblock, dispatched); err != nil {
		t.Fatalf("Precedes(fabrik:blocked removed, stage:Specify:complete added): %v", err)
	} else if !before {
		t.Error("fabrik:blocked must clear before Specify dispatches — the dependent proceeded before (or without) actually being unblocked")
	}
}

// TestDependencyUnblock_EmptyEdgeListDoesNotUnblockViaObserver is gap 1's
// companion direction (AC2, R2): removing the LAST blockedBy edge by hand
// must NOT unblock via PushUnblockObserver's BlockedByChanged path — the
// empty-list guard at observers.go:299 is a deliberate ADR-1419 protection
// against a stale-empty cache falsely unblocking an issue, and a test
// asserting only TestDependencyUnblock_OnBlockerClose's direction would
// pass against an implementation that dropped this guard entirely.
//
// Unlike the companion test above, this blocker is never closed, so
// dependencyUnblockStages's board-less-blocker concern never applies here:
// it stays on Unmanaged Backlog for the scenario's whole lifetime, purely
// as a real board item SeedBlockedBy/SeedRemoveBlockedBy can reference.
//
// The claim this test can actually make, traced against the real admission
// gate (engine/item.go's itemMayNeedWork "dep-blocked" cooldown): while the
// cooldown set by the item's first checkDependencies call is active, the
// item is not even admitted to deep-fetch, so no BlockedByChanged event can
// fire for it at all — the observer's guard and the admission gate are two
// independent layers protecting the same invariant, and this test proves
// the outer one (no premature re-check) holds across every poll in the
// window, then proves recovery happens only once the window closes and
// checkDependencies's own live re-read (the pull path, not the observer)
// finds the list empty and removes the label directly. See engine/
// dependencies.go's checkDependencies for the pull path and #1453 for where
// exactly this survival-for-minutes-with-no-log-line behaviour was hit in
// practice.
//
// Non-vacuity (R5): the assertion loop below fails immediately (before the
// cooldown boundary) if the guard were dropped and Path 2 fired on any
// intervening poll — the discriminating condition is "still blocked
// throughout the cooldown window", not merely "eventually unblocked", which
// a broken always-unblock-on-empty-list implementation would satisfy
// trivially and too early.
func TestDependencyUnblock_EmptyEdgeListDoesNotUnblockViaObserver(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages()})
	// PushUnblockObserver is not registered by default — see NewEnv's own
	// doc comment (ADR-1592). Both scenarios in this file specifically need it.
	t.Cleanup(env.Engine.RegisterObservers())

	blocker := FileIssue(t, env, "blocker",
		"Blocker issue — deliberately never closed in this scenario; the edge is removed by hand instead.",
		"Backlog")
	dependent := FileIssue(t, env, "dependent",
		"Blocked on the blocker issue via a blockedBy edge that will be removed directly, not resolved by a close.",
		"Specify", "fabrik:blocked")
	env.Sim.Sim().SeedBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// First poll: alreadyBlocked live re-read confirms the blocker is still
	// open and re-stamps the "dep-blocked" cooldown
	// (CooldownAt, PollSeconds*10 — engine/item.go:547).
	RunPoll(t, env)
	if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("dependent lost fabrik:blocked on the very first poll — seeding bug, this scenario needs it to start genuinely blocked")
	}

	// A human deletes the dependency link directly (SeedRemoveBlockedBy —
	// no GitHubClient interface counterpart; production never removes an
	// edge itself). Per #977 this does NOT bump the dependent's updatedAt,
	// so the probe-driven cache refresh has nothing to notice.
	env.Sim.Sim().SeedRemoveBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedRemoveBlockedBy: %v", err)
	}

	// While the dep-blocked cooldown is active the item is not admitted to
	// deep-fetch at all, so no BlockedByChanged can fire — confirm
	// fabrik:blocked survives every poll in this window.
	for i := 0; i < 3; i++ {
		RunPoll(t, env)
		if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
			t.Fatalf("fabrik:blocked cleared during the dep-blocked cooldown window (poll %d) — either the empty-list guard was bypassed, or the cooldown admission gate no longer holds", i+1)
		}
	}

	// Advance past the cooldown boundary (PollSeconds*10) plus a one-second
	// margin so the next poll is admitted for exactly one live re-check.
	env.Clock.Advance(10*env.PollInterval + time.Second)

	// This is the pull path: checkDependencies's own live re-read
	// (engine/dependencies.go) finds an empty blockedBy list and removes
	// the label directly. The very same deep-fetch that feeds it also
	// fires BlockedByChanged with an empty list — PushUnblockObserver's
	// Path 2 sees that event too and, per its own guard, declines to act.
	// Both routes converge on the same underlying RemoveLabelFromIssue
	// call, so the mutation log cannot distinguish which one won; the
	// timing window proven above is what pins the pull path as the actual
	// mechanism.
	WaitForLabelAbsent(t, env, dependent, "fabrik:blocked", 5)
}
