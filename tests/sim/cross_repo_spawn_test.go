package sim

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// This file ports tests/e2e/cross_repo_spawn_test.go's TestCrossRepoSpawn:
// a parent issue's Plan output declares a FABRIK_SPAWN_CHILD_BEGIN/END block
// targeting a second repo; the parent is gated (fabrik:blocked) until the
// spawned child closes; the child flows through its own pipeline
// independently; the parent then resumes and completes.
//
// Divergence from the live suite (R2): the live test relies on a real Claude
// Plan invocation actually deciding to decompose the work (a "beta must land
// first" constraint in the issue body). This port seeds the parent directly
// at the Implement column with stage:Plan:complete already applied and its
// Plan-stage comment already containing the FABRIK_SPAWN_CHILD_BEGIN/END
// block (via SeedComment) — the same zero-Claude-cost direct-seed precedent
// review_gate_helpers.go/paused_merged_pr_recovery_test.go use elsewhere in
// this package — rather than scripting a live Plan invocation to produce it.
// preImplement (engine/spawn.go) only reads the Plan comment's text; it has
// no way to tell whether Claude wrote it or a scenario seeded it directly.
//
// R5 fidelity note: an earlier draft of this port scripted the Plan stage's
// own Claude invocation directly (mirroring how every other custom-scripted
// stage in this package works) and hit a reproducible git-worktree
// corruption specific to a *non-default* (non-committing) Plan-stage script
// running under EnvOptions.SecondRepo's multi-repo Env — "could not fetch
// origin", eventually a bare-repo path resolving to simgh's own internal
// storage directory instead of the WorktreeManager's registered origin.
// Root cause not identified within this port's time budget; recorded in
// FIDELITY.md and the coverage matrix. Seeding the Plan comment directly
// sidesteps it entirely (Plan's own dispatch — and the worktree operations
// around it — never runs at all for the parent), and is arguably a closer
// match to this package's own idioms besides.
//
// The live test also adds fabrik:yolo to the parent explicitly, needed
// because the live bed's engine instance is not globally in yolo mode. This
// Env defaults cfg.Yolo to true (EnvOptions.Yolo's own default), so every
// issue in this package already auto-advances without a per-issue label.
//
// Requires EnvOptions.SecondRepo (an opt-in, additive Env extension — see
// env.go) so the one engine instance legitimately serves both repos,
// mirroring spawnTargetServedByThisInstance's cfg.Repo == "" branch (ADR-1419),
// and the engine.RegisterWorktreeManagerForTest seam (engine/engine.go) this
// port added so a multi-repo Env can wire a real, locally-backed
// WorktreeManager per repo instead of falling through to production's
// dynamic ensureBareClone-over-the-network path.

// crossRepoSpawnStages is smokeStages() under another name: the full
// Specify->Research->Plan->Implement->Review->Validate->Done pipeline is
// needed here (unlike most of this package's minimal pipelines) because
// advanceValidateTerminalItem — the mechanism that merges the PR and
// advances an issue to Done — only ever fires for a stage literally named
// "Validate" (engine/pr_terminal_advance.go); both the parent and the child
// need to actually reach Done and close.
func crossRepoSpawnStages() []*stages.Stage {
	return smokeStages()
}

// spawnPlanCommentBody builds a Plan-stage engine comment (matching the
// "🏭 **Fabrik — stage: Plan**" prefix findStageComment, engine/context.go,
// requires) whose body contains a well-formed FABRIK_SPAWN_CHILD_BEGIN/END
// block targeting childRepo.
func spawnPlanCommentBody(childRepo, childTitle, childBody string) string {
	return fmt.Sprintf("🏭 **Fabrik — stage: Plan**\n\nDecompose into %s first.\n\nFABRIK_SPAWN_CHILD_BEGIN %s\nTITLE: %s\n%s\nFABRIK_SPAWN_CHILD_END\n",
		childRepo, childRepo, childTitle, childBody)
}

// seedSpawnReadyParent files a parent issue directly at the Implement
// column with stage:Plan:complete already applied and its Plan-stage
// comment already seeded with a spawn block targeting childRepo — see the
// file-level doc comment for why this sidesteps a live Plan invocation
// entirely.
func seedSpawnReadyParent(t *testing.T, env *Env, childRepo, childTitle, childBody string) int {
	t.Helper()
	num := FileIssue(t, env, "sim cross-repo spawn (parent)",
		"Prove cross-repo decomposition: Plan declares a child in the beta repo; the parent gates on it.",
		"Implement", "stage:Plan:complete")
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "fabrik-sim-bot", spawnPlanCommentBody(childRepo, childTitle, childBody))
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seedSpawnReadyParent: %v", err)
	}
	return num
}

// projectItemInRepo/waitForIssueLabelInRepo/waitForIssueClosedInRepo are the
// repo-parameterized counterparts of assertions.go's projectItem/
// WaitForIssueLabel/WaitForIssueClosed — needed only here, since this is the
// one scenario in the package observing an issue outside env.OwnerRepo (the
// spawned child, in env.OwnerRepoBeta). Kept local rather than folded into
// assertions.go: every other scenario in the package is single-repo and
// would gain nothing from a repo parameter it always passes as env.OwnerRepo.
func projectItemInRepo(t *testing.T, env *Env, ownerRepo string, issueNumber int) gh.ProjectItem {
	t.Helper()
	owner, repo, _ := strings.Cut(ownerRepo, "/")
	item, err := env.Sim.FetchProjectItem(owner, repo, issueNumber)
	if err != nil {
		t.Fatalf("fetch %s#%d: %v", ownerRepo, issueNumber, err)
	}
	if item == nil {
		t.Fatalf("%s#%d is not on the project board", ownerRepo, issueNumber)
	}
	return *item
}

func waitForIssueClosedInRepo(t *testing.T, env *Env, ownerRepo string, issueNumber int, maxPolls int) {
	t.Helper()
	AdvanceUntil(t, env, func(env *Env) bool {
		return projectItemInRepo(t, env, ownerRepo, issueNumber).IsClosed
	}, maxPolls)
}

// TestCrossRepoSpawn ports the live test of the same name: a parent in
// env.OwnerRepo spawns a child in env.OwnerRepoBeta, the parent blocks until
// the child closes, the child runs its own full pipeline, and the parent
// resumes and closes.
func TestCrossRepoSpawn(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:     crossRepoSpawnStages(),
		SecondRepo: &SecondRepoOptions{}, // defaults: acme/gadgets alongside acme/widgets
	})
	// Direct-merge fallback (matches smoke_test.go) for BOTH repos — each
	// pipeline (parent's and child's) needs to reach a merged, closed Done
	// state on its own.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepoBeta, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	parent := seedSpawnReadyParent(t, env, env.OwnerRepoBeta, "sim cross-repo spawn (child)", "Do the beta-side work the parent depends on.")
	t.Logf("filed parent: %s#%d (Plan already complete, spawn block pre-seeded)", env.OwnerRepo, parent)

	WaitForIssueLabel(t, env, parent, "fabrik:children-spawned", 40)
	t.Logf("parent has fabrik:children-spawned — spawn step ran")

	item := projectItem(t, env, parent)
	if len(item.BlockedBy) != 1 {
		t.Fatalf("expected exactly 1 blockedBy dependency after spawn, got %d: %+v", len(item.BlockedBy), item.BlockedBy)
	}
	dep := item.BlockedBy[0]
	if dep.Repo != env.OwnerRepoBeta {
		t.Fatalf("expected the spawned child in %s, got %q", env.OwnerRepoBeta, dep.Repo)
	}
	child := dep.Number
	t.Logf("blockedBy linkage confirmed via API: child is %s#%d", env.OwnerRepoBeta, child)

	// The parent must be blocked while the child is open.
	WaitForIssueLabel(t, env, parent, "fabrik:blocked", 20)
	t.Logf("parent %s#%d carries fabrik:blocked", env.OwnerRepo, parent)

	// Child runs its own full pipeline (Specify..Validate, merge, close) —
	// same mechanics as smoke_test.go, just in the second repo.
	waitForIssueClosedInRepo(t, env, env.OwnerRepoBeta, child, 300)
	t.Logf("child %s#%d closed — parent should resume", env.OwnerRepoBeta, child)

	// Parent unblocks and completes its own remaining stages (Implement,
	// Review, Validate — Specify/Research/Plan were pre-seeded complete).
	WaitForLabelAbsent(t, env, parent, "fabrik:blocked", 80)
	WaitForIssueClosed(t, env, parent, 200)
	t.Logf("parent %s#%d closed — cross-repo flow complete", env.OwnerRepo, parent)
}

// TestCrossRepoSpawn_RefusesUnservedTarget is this port's AC3 non-vacuity
// proof — the ADR-1419 board-servability companion to the positive path
// above: a repo:-scoped instance (no EnvOptions.SecondRepo, so cfg.Repo is
// the primary repo's own name, not "") must refuse to spawn into a target it
// does not serve, failing loud (fabrik:paused, an explanatory comment, no
// child created) rather than silently registering the child onto its own
// board. Directly proves spawnTargetServedByThisInstance's cfg.Repo == ""
// branch in TestCrossRepoSpawn above is load-bearing, not a no-op — without
// it, this test's target repo would be accepted too.
func TestCrossRepoSpawn_RefusesUnservedTarget(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages: crossRepoSpawnStages(),
		// No SecondRepo: this instance is scoped to its own single repo
		// (cfg.Repo != ""), so any other target is unservable.
	})

	const unservedTarget = "acme/unserved-elsewhere"
	parent := seedSpawnReadyParent(t, env, unservedTarget, "should never be created", "should never be created")

	WaitForIssueLabel(t, env, parent, "fabrik:paused", 80)

	labels := IssueLabels(t, env, parent)
	if hasLabel(labels, "fabrik:children-spawned") {
		t.Fatal("fabrik:children-spawned applied despite an unservable spawn target — the board-servability guard did not fire")
	}
	t.Logf("parent %s#%d paused, fabrik:children-spawned absent — unservable target refused", env.OwnerRepo, parent)

	// The explanatory comment is engine-authored and captured in the
	// mutation log's own record of the AddComment call (Args.Body).
	found := false
	for _, e := range env.Sim.Log().ByMethod("AddComment") {
		if e.Args.Number == parent && strings.Contains(e.Args.Body, unservedTarget) && strings.Contains(e.Args.Body, "not served by this Fabrik instance") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected an AddComment call on #%d naming %q as not served by this instance; log:\n%s", parent, unservedTarget, env.Sim.Log().Dump(50))
	}
	t.Logf("AC3 verified: explanatory comment names the unservable target %q", unservedTarget)
}
