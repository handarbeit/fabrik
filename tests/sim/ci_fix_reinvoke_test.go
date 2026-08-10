package sim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// ciFixSentinel is the required-context name standing in for the live
// suite's "ci-fix-sentinel" check.
const ciFixSentinel = "ci-fix-sentinel"

// ciFixReinvokeSeq gives every ciFixCommentScript call a unique commit —
// mirrors simclaude's own commitStageMarker (invocationSeq in
// tests/sim/simclaude/git.go), which this package cannot call directly
// (unexported, different package).
var ciFixReinvokeSeq atomic.Uint64

// ciFixCommentScript returns a simclaude.CommentScript for the CI-fix
// reinvoke's InvokeForComments call (dispatchCIFixReinvoke re-invokes via
// *comment* processing, not a normal stage Invoke — see engine/ci.go's doc
// comment on dispatchCIFixReinvoke). It commits, pushes, and seeds a check
// run for its own new SHA with the given conclusion — all before returning
// — so no poll can ever observe a SHA with no check run seeded for it yet.
//
// R5 fidelity notes, both surfaced by this port:
//
//  1. Production's processComments (engine/comments.go) never pushes the
//     worktree itself — a live comment-review Claude session pushes as part
//     of its own tool use, per the -comment skills' "commit and push"
//     instructions. tests/sim/simclaude's DefaultCommentScript faithfully
//     mirrors that: it commits locally but never pushes (see git.go), which
//     is correct for scenarios that only need the *stage to complete* (e.g.
//     TestFailureShape_CommentReviewInvocation) but is silently wrong for
//     one that also needs the new commit to be *observable* — as this
//     scenario does, since the CI-fix loop's whole premise is a new PR head
//     SHA existing check runs can be seeded against.
//  2. Seeding the new SHA's check run reactively from the test body (fetch
//     the post-reinvoke SHA, then seed) — this port's first draft — has a
//     genuine race: CommentCallCount increments the instant InvokeForComments
//     *starts* (Invoker.pickCommentScript, tests/sim/simclaude/invoker.go),
//     not once it finishes, so a test polling on that count can observe the
//     new SHA before a script-external seed call has run, or (worse, seen
//     empirically running this port under concurrent load) the CI gate's own
//     "post-push registration delay (hadChecks)" wait state can persist
//     across an unrelated number of polls while the test's own goroutine is
//     scheduled out. Seeding from *inside* the script, atomically with the
//     push, closes the window entirely: by the time the engine's next poll
//     can observe the new SHA at all, its check run already exists.
func ciFixCommentScript(sim *simgh.Instrumented, ownerRepo, conclusion string) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		sha, err := commitAndPushCIFixAttempt(workDir, issue.Number)
		if err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		sim.Sim().SeedCheckRun(ownerRepo, sha, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: conclusion})
		if serr := sim.Sim().Err(); serr != nil {
			return "", false, engine.TokenUsage{}, fmt.Errorf("seeding check run for %s: %w", sha, serr)
		}
		usage := engine.TokenUsage{InputTokens: 500, OutputTokens: 100, CostUSD: 0.02, TurnsUsed: 2, MaxTurns: 50}
		return "FABRIK_STAGE_COMPLETE\n", true, usage, nil
	}
}

// commitAndPushCIFixAttempt appends a uniquely-numbered line to a scratch
// file, commits it, and pushes to the issue's fabrik/issue-<N> branch —
// mirroring #1323's live fixture design (an unconditional, mechanically
// verifiable action every reinvoke takes, independent of any judgment about
// fixability) and, unlike simclaude's own DefaultCommentScript, actually
// pushing so the new commit is observable via FetchLinkedPR. Returns the new
// HEAD SHA.
func commitAndPushCIFixAttempt(workDir string, issueNumber int) (string, error) {
	seq := ciFixReinvokeSeq.Add(1)
	relPath := filepath.Join(".simclaude", "ci-fix-attempts.log")
	full := filepath.Join(workDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("commitAndPushCIFixAttempt: mkdir: %w", err)
	}
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("commitAndPushCIFixAttempt: open: %w", err)
	}
	if _, err := fmt.Fprintf(f, "attempt-%d\n", seq); err != nil {
		f.Close()
		return "", fmt.Errorf("commitAndPushCIFixAttempt: write: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("commitAndPushCIFixAttempt: close: %w", err)
	}

	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), out, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("add", relPath); err != nil {
		return "", err
	}
	if _, err := run(
		"-c", "user.name=simclaude",
		"-c", "user.email=simclaude@fabrik.test",
		"commit", "-m", fmt.Sprintf("chore(simclaude): ci-fix attempt %d for issue #%d", seq, issueNumber),
	); err != nil {
		return "", err
	}
	branch := fmt.Sprintf("fabrik/issue-%d", issueNumber)
	if _, err := run("push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return "", err
	}
	return run("rev-parse", "HEAD")
}

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
		// StartTime near real time.Now(), NOT the package default
		// (2026-01-01) — checkCIGate's mergeable_state-derived timeout
		// branches (classifyCIFromMergeableState, engine/ci.go) compare
		// time.Since(appliedAt) against real wall-clock time, where appliedAt
		// comes from FetchLabelAppliedAt (GitHub-anchored, driven by this
		// Env's injected Clock). With the far-past default StartTime, the
		// very first poll where a check run hasn't been seeded yet for the
		// current SHA (a real gap in this scenario: the SHA is only known,
		// and its check run only seeded, reactively after Validate dispatches)
		// would already read as many months stale relative to real "now" —
		// instantly exceeding ciWaitTimeout (30m default) and pausing before
		// the CI-fix reinvoke ever gets a chance to fire. Scenarios that WANT
		// an instant timeout (timeout_test.go) deliberately exploit this same
		// mismatch; this scenario needs the opposite.
		StartTime: time.Now(),
		// engine.Config has no built-in zero-value default for
		// MaxCiFixCycles the way cmd/root.go's flag resolution does
		// (defaulting to 5 there) — NewWithDeps takes the struct literal
		// as-is, so an Env that doesn't set this explicitly gets 0, which
		// makes pauseForCIFixCycleLimit fire on the very first CI failure.
		// This scenario needs room for a genuine reinvoke, so set it
		// explicitly (mirroring production's own default).
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 },
	})
	// dispatchCIFixReinvoke re-invokes via InvokeForComments, not Invoke — see
	// ciFixCommentScript's doc comment for why the default comment script
	// (commits but never pushes) isn't sufficient for this scenario. The
	// reinvoke's own commit gets a "success" check run — it's meant to be
	// the fix that clears the gate.
	env.Claude.ForStageComments("Validate", ciFixCommentScript(env.Sim, env.OwnerRepo, "success"))
	// Direct-merge fallback (matches smoke_test.go): deterministic Done
	// advancement once the gate clears, without simulating an async native
	// auto-merge convergence (auto_merge_test.go's concern, not this one's).
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	// SeedRequiredContexts is load-bearing, not cosmetic: without it,
	// deriveMergeableState (tests/sim/simgh/prs.go) has nothing to check
	// before this scenario reactively seeds its first check run, and an
	// empty required-contexts set vacuously derives "clean" — racing the CI
	// gate closed before fabrik:awaiting-ci is ever observably present.
	// Registering the context up front makes the pre-seed state genuinely
	// "blocked" instead, closing that window.
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

	// The CI-fix reinvoke dispatches via InvokeForComments, not Invoke
	// (dispatchCIFixReinvoke, engine/ci.go). Waiting for the PR's HeadSHA to
	// actually change — not CommentCallCount, which increments the instant
	// the invocation *starts* (see ciFixCommentScript's doc comment) — is
	// the race-free signal that the reinvoke has genuinely landed a new,
	// check-run-seeded commit (the live test's WaitForPRCommentContaining a
	// second "stage: Validate" comment has no sim equivalent since sim posts
	// no comments at all; this is the strongest sim-observable substitute).
	var sha2 string
	AdvanceUntil(t, env, func(env *Env) bool {
		cur, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		if err != nil || cur == nil {
			return false
		}
		if cur.HeadSHA != sha1 {
			sha2 = cur.HeadSHA
			return true
		}
		return false
	}, 80)
	if sha2 == "" {
		t.Fatal("CI-fix reinvoke did not push a new commit — head SHA unchanged")
	}
	t.Logf("CI-fix reinvoke landed new SHA %s (check run seeded atomically by the script)", sha2)

	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-ci", 80)
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	WaitForIssueClosed(t, env, num, 80)
	WaitForProjectStatus(t, env, num, "Done", 80)

	// No-rebase-storm guard (baselined post-initial-dispatch — see doc
	// comment): exactly one CI-fix commit beyond baselineSHA, measured on the
	// PR's own head (sha2) — mirroring what the live test's PRCommitCount
	// actually counts (commits on the PR branch), not commits reachable on
	// the base branch after merge (which would also include the merge commit
	// itself and is not what "no rebase storm" is about).
	finalCommits := gitCommitCount(t, simBareDir, baselineSHA, sha2)
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
		// See TestCIFixReinvoke's StartTime comment — same reasoning applies
		// here: this test wants the genuine cycle-limit path to fire, not an
		// incidental real-wall-clock timeout from the far-past default.
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = maxCycles
		},
	})
	// The sentinel is permanently unfixable — every reinvoke's own commit
	// gets a "failure" check run, self-seeded atomically by the script (see
	// ciFixCommentScript's doc comment).
	env.Claude.ForStageComments("Validate", ciFixCommentScript(env.Sim, env.OwnerRepo, "failure"))
	// See TestCIFixReinvoke's SeedRequiredContexts comment — same
	// vacuous-clean race applies here before the first check run is seeded.
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

	// The initial Validate dispatch used the plain engine Invoke path (not
	// ciFixCommentScript), so its check run must still be seeded reactively
	// from here — only the reinvokes below self-seed.
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, baselineSHA, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "failure"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	// maxCycles reinvokes must happen before the cycle limit pauses the
	// issue on the (maxCycles+1)th ciFailure classification. Waiting for the
	// PR's HeadSHA to change — not CommentCallCount, which increments the
	// instant the invocation *starts* — is the race-free "this reinvoke has
	// genuinely landed" signal (see ciFixCommentScript's doc comment).
	seenSHAs := map[string]bool{baselineSHA: true}
	lastSHA := baselineSHA
	for reinvokeNum := 1; reinvokeNum <= maxCycles; reinvokeNum++ {
		var curSHA string
		AdvanceUntil(t, env, func(env *Env) bool {
			cur, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
			if err != nil || cur == nil || cur.HeadSHA == lastSHA {
				return false
			}
			curSHA = cur.HeadSHA
			return true
		}, 80)
		if curSHA == "" {
			t.Fatalf("CI-fix reinvoke #%d did not land a new commit", reinvokeNum)
		}
		if seenSHAs[curSHA] {
			t.Fatalf("CI-fix reinvoke #%d landed a SHA already seen (%s) — no new commit was pushed", reinvokeNum, curSHA)
		}
		seenSHAs[curSHA] = true
		lastSHA = curSHA
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
