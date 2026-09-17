package sim

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

// This file closes #1751's fidelity gap: tests/sim could not previously
// construct an Engine in GitHub App-auth mode at all, so it could never
// exercise the App-auth dispatch-admission path (resolveRepoAccess →
// resolveAppRepoAccess, engine/startup.go) — the exact seam whose
// pre-#1750 behavior (unconditionally reading permissions.push, which
// GitHub reports all-false for an installation token) gated out every
// item under App auth and shipped green through a 209s sim run. See
// engine.SetGitHubAppModeForTest's doc comment for the mechanism these
// tests use, and README.md's "GitHub App auth: dispatch-admission
// coverage (#1751)" section for what is and isn't covered here.

// githubAppAuthStages is a minimal single-stage pipeline — these scenarios
// only care about whether the first stage dispatches at all, not a full
// pipeline traversal (mirrors failure_shapes_test.go's failureShapeStages).
func githubAppAuthStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// TestGitHubAppAuth_DispatchAdmission_ListedRepoDispatches is R3/AC1/AC2: a
// plain item in a repo the App installation covers still dispatches at the
// first stage. This is a genuine regression test for the class of bug #1750
// was, not merely a happy-path assertion (AC2): env.OwnerRepo is also
// seeded, via the pre-#1750 PAT-shaped SeedRepoAccess seam, with the exact
// all-false shape that caused #1750 (gh.RepoAccess{CanPush: false,
// AllowAutoMerge: false}) — the closest sim-expressible equivalent of a
// permissions object with every field false. resolveAppRepoAccess never
// reads this value in the current (fixed) engine, so the item dispatches;
// a regression back to #1750's pre-fix behavior (unconditionally calling
// FetchRepoAccess, ignoring e.ghAppAuth) would see this poisoned value
// instead and fail the item's dispatch, flipping this test's outcome.
func TestGitHubAppAuth_DispatchAdmission_ListedRepoDispatches(t *testing.T) {
	t.Parallel() // touches no shared/global state — see the negative scenario below for the one that can't.
	env := NewEnv(t, EnvOptions{Stages: githubAppAuthStages()})

	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{CanPush: false, AllowAutoMerge: false})
	env.Engine.SetGitHubAppModeForTest(map[string]bool{env.OwnerRepo: true}, false)

	num := FileIssue(t, env, "App-auth listed repo", "body", "Specify")

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)
}

// TestGitHubAppAuth_DispatchAdmission_ExcludedRepoBlocksAndWarns is R4/AC1/
// AC3: the inverse case — a repo genuinely excluded from the App
// installation's accessible-repo list must never be silently processed into
// nothing. It must instead surface via ADR-1750's second escalation tier: a
// persistent warnings.Record entry naming the repo and the installation
// (the only tier reachable from tests/sim — the first tier, a hard startup
// refusal on zero accessible repos, lives in New(), which NewWithDeps/
// tests/sim never call; see README.md's blind-spot section). The scenario
// asserts both halves of "not silently swallowed": the item never reaches
// Specify, AND a warnings entry is actually recorded — either alone could
// be true for unrelated reasons (AC2/AC3's non-vacuity concern).
//
// Deliberately no t.Parallel(): asserting a real warnings.Load() read
// requires pointing the package-level warnings.WarningsPathOverride at a
// t.TempDir() path, and every scenario in this package's first poll for a
// new repo already calls warnings.Record/warnings.Clear against whatever
// that global currently holds (resolveRepoAccess, checkAllowAutoMerge) —
// running this test in parallel with any t.Parallel() sibling would be a
// live, -race-flagged data race on the global itself, not merely a flaky
// assertion. Go runs every serial (non-t.Parallel()) top-level test in a
// package to completion before starting the paused parallel batch, so
// omitting t.Parallel() here is sufficient — do not "fix" this back in.
func TestGitHubAppAuth_DispatchAdmission_ExcludedRepoBlocksAndWarns(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })

	const installationID = 4242
	env := NewEnv(t, EnvOptions{
		Stages: githubAppAuthStages(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.GitHubAppInstallationID = installationID
		},
	})

	// The installation covers some other repo, but not env.OwnerRepo — a
	// confirmed exclusion, not an ambiguous/never-fetched answer.
	env.Engine.SetGitHubAppModeForTest(map[string]bool{"acme/other-repo": true}, false)

	num := FileIssue(t, env, "App-auth excluded repo", "body", "Specify")

	RunPolls(t, env, 5)

	if got := env.Claude.StageCallCount("Specify"); got != 0 {
		t.Errorf("Specify was invoked %d times for a repo excluded from the App installation — must never dispatch", got)
	}
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Specify:in_progress") || hasLabel(labels, "stage:Specify:complete") {
		t.Errorf("issue #%d dispatched despite its repo being excluded from the App installation's accessible-repo list: labels=%v", num, labels)
	}

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("warnings.Load: %v", err)
	}
	wantKey := "repo_access:" + env.OwnerRepo
	var found *warnings.Entry
	for i := range entries {
		if entries[i].Key == wantKey {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no warnings entry recorded for key %q; entries=%v", wantKey, entries)
	}
	if found.Type != "repo_access" {
		t.Errorf("warnings entry Type = %q, want %q", found.Type, "repo_access")
	}
	if !strings.Contains(found.Detail, fmt.Sprintf("%d", installationID)) {
		t.Errorf("warnings entry Detail = %q, want it to name installation %d", found.Detail, installationID)
	}
}
