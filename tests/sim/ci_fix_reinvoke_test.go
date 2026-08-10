package sim

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ciFixSentinel is the required-context name standing in for the live
// suite's "ci-fix-sentinel" check.
const ciFixSentinel = "ci-fix-sentinel"

// ciFixStages is a minimal 3-stage pipeline (Implement -> Validate -> Done):
// nothing about the CI-fix reinvoke loop depends on Specify/Research/Plan/
// Review having run first, so this port trims to what the assertions
// actually exercise, mirroring reviewTimeoutStages' precedent
// (tests/sim/timeout_test.go).
func ciFixStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Validate", Order: 2, WaitForCI: &tr},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// gitCommitCount returns the number of commits reachable from to but not
// from, in the bare repo at dir — the sim-side substitute for the live
// harness's PRCommitCount (a `gh pr view --json commits` call): simgh
// exposes no PR-commit-count API of its own (there is nothing to fetch —
// the engine itself never needs one), but the commits are real, so counting
// them directly in the backing git repo is the faithful equivalent.
func gitCommitCount(t *testing.T, dir, from, to string) int {
	t.Helper()
	cmd := exec.Command("git", "rev-list", "--count", from+".."+to)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-list --count %s..%s (dir=%q): %v: %s", from, to, dir, err, out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parsing git rev-list output %q: %v", out, err)
	}
	return n
}

// TestCIFixReinvoke ports tests/e2e/ci_fix_reinvoke_test.go's TestCIFixReinvoke
// — the positive-path regression test for the CI-fix reinvoke loop
// (engine/ci.go, #900). The live test is permanently skipped pending #916 (a
// capable live Claude agent tends to make CI green before the first push, so
// the induced-failure premise never fires there) — sim scripts Claude
// directly, so this constraint doesn't exist here. This is a place sim
// exceeds current live coverage (R2): the positive path is provably
// exercised in sim today, live only once #916 unblocks it.
//
// Sequence proven: Validate's initial dispatch pushes a commit and completes
// (wait_for_ci defers stage:Validate:complete); the sim seeds a FAILING
// required check run for that commit's SHA, which the catch-up loop's
// checkCIGate/dispatchCIFixReinvoke turns into a second Validate dispatch
// (the "CI-fix reinvoke"); that dispatch pushes a genuinely new commit
// (simclaude's commitStageMarker is sequence-numbered — see git.go — so a
// repeated invocation is never a no-op diff); the sim seeds a PASSING check
// run for the new SHA; the gate clears, stage:Validate:complete is granted,
// and (direct-merge fallback, matching smoke_test.go) the issue closes.
//
// R2 divergence from the live assertion, noted rather than silently
// absorbed: the live test's "exactly baseCommits+1" no-rebase-storm guard
// baselines commit count right after Implement, because a live Claude
// Validate dispatch that finds nothing to fix makes no commit at all. In
// sim, DefaultScript commits unconditionally on every invocation (R1 of the
// simclaude package doc comment) — so Validate's own *initial* dispatch
// already adds one commit before any CI-fix involvement. This port
// baselines after that initial dispatch instead (right when
// fabrik:awaiting-ci first appears) and asserts exactly one further commit
// from the reinvoke — the same "no rebase storm" invariant, anchored to
// what sim can actually promise.
func TestCIFixReinvoke(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages: ciFixStages(),
		// engine.Config has no built-in zero-value default for
		// MaxCiFixCycles the way cmd/root.go's flag resolution does
		// (defaulting to 5 there) — NewWithDeps takes the struct literal
		// as-is, so an Env that doesn't set this explicitly gets 0, which
		// makes pauseForCIFixCycleLimit fire on the very first CI failure.
		// This scenario needs room for a genuine reinvoke, so set it
		// explicitly (mirroring production's own default).
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 },
	})
	// Direct-merge fallback (matches smoke_test.go): deterministic Done
	// advancement once the gate clears, without simulating an async native
	// auto-merge convergence (auto_merge_test.go's concern, not this one's).
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "sim ci-fix reinvoke", "Prove the CI-fix reinvoke loop's positive path.", "Implement")

	// Initial Validate dispatch completes; wait_for_ci defers stage:complete
	// and applies fabrik:awaiting-ci unconditionally (before any CI verdict
	// is even known — see CLAUDE.md's fabrik:awaiting-ci entry).
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR")
	}
	sha1 := pr.HeadSHA
	baseCommits := env.Claude.StageCallCount("Validate") // 1, for the log line below only
	t.Logf("PR #%d: initial Validate dispatch (call #%d) landed SHA %s", pr.Number, baseCommits, sha1)

	simBareDir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	baselineSHA := sha1 // commit count baseline anchored post-initial-dispatch — see doc comment

	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha1, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "failure"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	// The CI-fix reinvoke re-dispatches Validate — StageCallCount reaching 2
	// is the sim-observable proxy for "the reinvoke fired" (the live test's
	// WaitForPRCommentContaining a second "stage: Validate" comment has no
	// sim equivalent since sim posts no comments at all; this is a stronger
	// signal in the sense AC10 asks for — it's the dispatch itself, not
	// prose that mentions it).
	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.StageCallCount("Validate") >= 2
	}, 80)

	pr2, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR (after reinvoke): %v", err)
	}
	sha2 := pr2.HeadSHA
	if sha2 == sha1 {
		t.Fatal("CI-fix reinvoke did not push a new commit — head SHA unchanged")
	}
	t.Logf("CI-fix reinvoke (call #2) landed new SHA %s", sha2)

	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha2, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-ci", 80)
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	WaitForIssueClosed(t, env, num, 80)
	WaitForProjectStatus(t, env, num, "Done", 80)

	// No-rebase-storm guard (baselined post-initial-dispatch — see doc
	// comment): exactly one CI-fix commit beyond baselineSHA.
	finalCommits := gitCommitCount(t, simBareDir, baselineSHA, "main")
	if finalCommits != 1 {
		t.Fatalf("expected exactly 1 CI-fix commit beyond the post-initial-dispatch baseline, got %d — possible rebase storm", finalCommits)
	}
	t.Logf("%s#%d closed with exactly 1 CI-fix commit beyond baseline — CI-fix reinvoke positive path verified", env.OwnerRepo, num)
}

// TestCIFixReinvokeCycleLimit ports tests/e2e/ci_fix_reinvoke_test.go's
// TestCIFixReinvokeCycleLimit — the negative-path regression test for the
// CI-fix reinvoke cycle limit (pauseForCIFixCycleLimit, engine/ci.go). The
// sentinel check is permanently failing (every seeded check run — including
// the reinvoke ones — reports failure), forcing the engine to exhaust
// cfg.MaxCiFixCycles and pause.
//
// Carries forward #1320's discriminator (HasPrefix, not Contains, against
// the engine's own cycle-limit comment prefix) and #1323's non-vacuity
// design (each dispatch must land a genuinely new commit, verified via
// gitCommitCount, so the cycle-limit path is reached via genuine repeated
// dispatch rather than a single no-op latch) — see ci_fix_reinvoke_test.go's
// (live) doc comments for the full historical context these carry forward.
func TestCIFixReinvokeCycleLimit(t *testing.T) {
	t.Parallel()
	const maxCycles = 2
	env := NewEnv(t, EnvOptions{
		Stages: ciFixStages(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = maxCycles
		},
	})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "sim ci-fix cycle limit", "Prove the CI-fix cycle limit terminates the loop.", "Implement")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	simBareDir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	baselineSHA := pr.HeadSHA

	// Fail every SHA the pipeline produces, from the initial dispatch through
	// every reinvoke — the sentinel is permanently unfixable.
	seedFailure := func(sha string) {
		env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "failure"})
		if err := env.Sim.Sim().Err(); err != nil {
			t.Fatalf("SeedCheckRun: %v", err)
		}
	}
	seedFailure(baselineSHA)

	seenSHAs := map[string]bool{baselineSHA: true}
	for call := 2; call <= maxCycles+1; call++ {
		AdvanceUntil(t, env, func(env *Env) bool {
			return env.Claude.StageCallCount("Validate") >= call
		}, 80)
		curPR, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		if err != nil {
			t.Fatalf("FetchLinkedPR (call %d): %v", call, err)
		}
		if seenSHAs[curPR.HeadSHA] {
			t.Fatalf("Validate dispatch #%d landed a SHA already seen (%s) — no new commit was pushed", call, curPR.HeadSHA)
		}
		seenSHAs[curPR.HeadSHA] = true
		seedFailure(curPR.HeadSHA)
	}

	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 20)

	if item := projectItem(t, env, num); item.IsClosed {
		t.Fatal("issue closed after cycle limit — expected OPEN")
	}

	// #1320: the engine-authored cycle-limit comment must be matched by
	// HasPrefix, never Contains — a stage-output comment quoting the marker
	// text in prose must not satisfy this check. simgh has no comment-body
	// fetch on the GitHubClient interface exposed to a scenario the way the
	// live harness's `gh issue view --json comments` is, so this port
	// substitutes the equally-discriminating, equally engine-only signal
	// CLAUDE.md documents for this exact transition: fabrik:paused +
	// fabrik:awaiting-input applied together IS pauseForCIFixCycleLimit's
	// own signature (see its call sites) — nothing else in this pipeline
	// (no review gate, no rebase gate) can produce that pair. This is
	// recorded as a fidelity/coverage gap in the coverage matrix (R4): the
	// live test's HasPrefix discriminator itself has no direct sim
	// equivalent because sim's Sim/Instrumented never exposes issue-comment
	// bodies back to a scenario at all (comments are write-only from the
	// scenario's side — see FIDELITY.md).

	// #1323 non-vacuity: the cycle-limit path was reached via maxCycles
	// genuinely distinct commits, not a single no-op latch — gitCommitCount
	// from the pre-CI-fix baseline to the final head must be at least
	// maxCycles.
	finalPR, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR (final): %v", err)
	}
	finalCommits := gitCommitCount(t, simBareDir, baselineSHA, finalPR.HeadSHA)
	if finalCommits < maxCycles {
		t.Fatalf("cycle-limit path not genuinely exercised: expected at least %d commits beyond baseline, got %d", maxCycles, finalCommits)
	}
	t.Logf("%s#%d paused after %d genuine CI-fix cycles (%d commits beyond baseline) — cycle limit verified", env.OwnerRepo, num, maxCycles, finalCommits)
}
