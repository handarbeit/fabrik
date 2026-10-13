package sim

import (
	"strings"
	"testing"
)

// This file is the sim twin for #2090 (ADR 2090): a parent carrying
// base:<branch> that spawns children in the same repo must give each child
// the same base:<branch> — applied before the child's board Status placement,
// so its first dispatch forks from, and its PR targets, the parent's base
// rather than the repository default.
//
// Like cross_repo_spawn_test.go, the parent is seeded at Implement with
// stage:Plan:complete and a Plan comment already holding the spawn block, so
// no scripted Plan invocation is needed; preImplement only reads that text.

// seedBaseSpawnParent files a parent at Implement with extraLabels and a Plan
// comment whose spawn block targets env.OwnerRepo (a same-repo child).
func seedBaseSpawnParent(t *testing.T, env *Env, extraLabels ...string) int {
	t.Helper()
	labels := append([]string{"stage:Plan:complete"}, extraLabels...)
	num := FileIssue(t, env, "sim spawn base inheritance (parent)",
		"Plan declares a same-repo child; the child must inherit the parent's base.",
		"Implement", labels...)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "fabrik-sim-bot",
		spawnPlanCommentBody(env.OwnerRepo, "sim spawn base inheritance (child)", "Do the child-side work."))
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seedBaseSpawnParent: %v", err)
	}
	return num
}

// spawnedChildNumber waits for the spawn step to run and returns the one
// child's issue number from the parent's blockedBy edge.
func spawnedChildNumber(t *testing.T, env *Env, parent int) int {
	t.Helper()
	WaitForIssueLabel(t, env, parent, "fabrik:children-spawned", 40)
	item := projectItem(t, env, parent)
	if len(item.BlockedBy) != 1 {
		t.Fatalf("expected exactly 1 blockedBy dependency after spawn, got %d: %+v", len(item.BlockedBy), item.BlockedBy)
	}
	// A same-repo dependency may be reported with an empty Repo; only a
	// different, non-empty repo would mean the child went elsewhere.
	if r := item.BlockedBy[0].Repo; r != "" && r != env.OwnerRepo {
		t.Fatalf("expected a same-repo child in %s, got %q", env.OwnerRepo, r)
	}
	return item.BlockedBy[0].Number
}

// TestSpawnChildInheritsParentBase: a base:<branch> parent spawns a same-repo
// child; the child carries base:<branch> — written before its Status
// placement — and its PR targets the parent's base, not the default branch.
func TestSpawnChildInheritsParentBase(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: baseBranchStages(), Yolo: boolPtr(false)})

	const branchName = "e2e-spawn-base-sim"
	env.Sim.Sim().SeedBranch(env.OwnerRepo, branchName, "")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedBranch: %v", err)
	}

	// fabrik:cruise drives auto-advance (Yolo is off), and is inherited by the
	// child, so the child runs its pipeline to a PR without manual moves.
	parent := seedBaseSpawnParent(t, env, "base:"+branchName, "fabrik:cruise")
	child := spawnedChildNumber(t, env, parent)

	if labels := IssueLabels(t, env, child); !hasLabel(labels, "base:"+branchName) {
		t.Fatalf("child #%d labels %v lack base:%s", child, labels, branchName)
	}

	// Ordering (R2): the child's base: write precedes its Status placement.
	// The first UpdateProjectItemStatus after the child's CreateIssue is that
	// child's placement (the parent was seeded in place, never moved by the
	// engine).
	entries := env.Sim.Log().Entries()
	createSeq, baseSeq, statusSeq := -1, -1, -1
	for _, e := range entries {
		switch {
		case e.Method == "CreateIssue" && !e.Failed() && createSeq < 0:
			createSeq = e.Seq
		case e.Method == "AddLabelToIssue" && !e.Failed() && e.Args.Number == child && e.Args.Label == "base:"+branchName && baseSeq < 0:
			baseSeq = e.Seq
		case e.Method == "UpdateProjectItemStatus" && !e.Failed() && createSeq >= 0 && statusSeq < 0:
			statusSeq = e.Seq
		}
	}
	if createSeq < 0 || baseSeq < 0 || statusSeq < 0 {
		t.Fatalf("missing log entries (create=%d base=%d status=%d); log:\n%s", createSeq, baseSeq, statusSeq, env.Sim.Log().Dump(80))
	}
	if baseSeq > statusSeq {
		t.Fatalf("base: label write (seq %d) must precede the child's Status placement (seq %d)", baseSeq, statusSeq)
	}

	// The child's worktree is forked from, and its PR opened against, the
	// parent's base.
	WaitForIssueLabel(t, env, child, "stage:Implement:complete", 200)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, child)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR for child #%d after Implement", child)
	}
	if pr.BaseRef != branchName {
		t.Fatalf("child PR #%d base ref = %q, want %q (inheritance missing: fell back to default?)", pr.Number, pr.BaseRef, branchName)
	}
	t.Logf("%s#%d spawned child #%d with base:%s, PR #%d targets %q", env.OwnerRepo, parent, child, branchName, pr.Number, pr.BaseRef)
}

// TestSpawnChildInheritsParentBase_NonVacuous is the control: a parent without
// a base: label spawns a child that gets none and targets the default branch,
// so the assertions above discriminate on the inheritance itself.
func TestSpawnChildInheritsParentBase_NonVacuous(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: baseBranchStages(), Yolo: boolPtr(false)})

	parent := seedBaseSpawnParent(t, env, "fabrik:cruise")
	child := spawnedChildNumber(t, env, parent)

	for _, l := range IssueLabels(t, env, child) {
		if strings.HasPrefix(l, "base:") {
			t.Fatalf("child #%d must not carry a base: label, got %q", child, l)
		}
	}

	WaitForIssueLabel(t, env, child, "stage:Implement:complete", 200)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, child)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR for child #%d", child)
	}
	if pr.BaseRef == "" || pr.BaseRef == "e2e-spawn-base-sim" {
		t.Fatalf("child PR #%d base ref = %q, want the repo default branch", pr.Number, pr.BaseRef)
	}
}
