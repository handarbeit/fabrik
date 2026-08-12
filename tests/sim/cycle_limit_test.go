package sim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// This file is R2: drive a cycle limit to its terminal pause, with an
// assertion pinning the exact cycle count (not one early, not one late).
// All three cycle limits — review, rebase, CI-fix — share one dispatch
// primitive, dispatchWithCycleLimit (engine/catch_up_handlers.go): read
// counter → in-flight guard → `if cycleCount >= maxCycles { pause } else
// { increment; dispatch }`. A scenario proving "exactly maxCycles reinvokes,
// pause on what would be reinvoke maxCycles+1" for one is a faithful
// template for the other two, since the boundary semantics are identical
// code, not merely similar.
//
//   - CI-fix cycle limit: already covered by ci_fix_reinvoke_test.go's
//     TestCIFixReinvokeCycleLimit (maxCycles=2, exact-count assertion via
//     gitCommitCount) — not duplicated here.
//   - Review cycle limit: already covered by review_authority_test.go's
//     TestReviewAuthorityCycleLimitPauses (maxCycles=2, exact-count
//     assertion via env.Claude.CommentCallCount, plus the ADR-1518
//     refund-trap avoidance Research flagged — it drives genuinely distinct
//     CHANGES_REQUESTED reviews rather than relying on any single review
//     being reprocessed) — not duplicated here either. Both pre-date this
//     issue but satisfy its R2/AC3 requirement for their own gate exactly.
//   - Rebase cycle limit: TestRebaseCycleLimit below — genuinely new, the
//     one of the three with no existing sim coverage.

// rebaseCycleLimitCommentPrefix is the literal prefix of
// pauseForRebaseCycleLimit's own comment (engine/merge_gate.go). Copied here
// rather than imported (unexported) — mirrors ci_fix_reinvoke_test.go's
// ciFixCycleLimitCommentPrefix/no_work_needed_test.go's identical pattern.
const rebaseCycleLimitCommentPrefix = "🏭 **Fabrik — rebase cycle limit reached**"

// conflictingPath is the file both the PR branch (via Implement's default
// script) and a direct commit to main touch with different content — an
// add/add conflict a real git trial merge cannot resolve automatically,
// which is exactly what checkMergeabilityGate's PRMergeConflicting
// classification requires (simgh computes mergeability via an actual trial
// merge — see tests/sim/README.md's "backed by real git" section, not a
// flag a test can simply declare). Matches simclaude's own default-script
// path convention (".simclaude/<StageName>.md" — see simclaude/git.go's
// commitStageMarker).
const conflictingPath = ".simclaude/Implement.md"

// rebaseConflictSeq gives every rebaseConflictScript call unique, still-
// conflicting content — mirrors ci_fix_reinvoke_test.go's ciFixReinvokeSeq.
var rebaseConflictSeq atomic.Uint64

// rebaseConflictScript is the rebase-reinvoke's InvokeForComments script
// (dispatchRebaseReinvoke re-invokes via comment processing, not a normal
// stage Invoke — see dispatchReinvoke). It keeps re-writing conflictingPath
// with new content that still differs from main's own version at that
// path, and pushes for real — mirroring #1323's "genuinely distinct,
// permanently unfixable" design (ciFixCommentScript's identical shape for
// the CI-fix cycle limit): the conflict is never resolved, so
// checkMergeabilityGate re-detects it on every subsequent poll and the
// cycle counter climbs on genuine repeated dispatch, not a no-op latch.
func rebaseConflictScript() simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		seq := rebaseConflictSeq.Add(1)
		full := filepath.Join(workDir, conflictingPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", false, engine.TokenUsage{}, fmt.Errorf("rebaseConflictScript: mkdir: %w", err)
		}
		content := fmt.Sprintf("still-conflicting rebase attempt %d for issue #%d\n", seq, issue.Number)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", false, engine.TokenUsage{}, fmt.Errorf("rebaseConflictScript: write: %w", err)
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
		if _, err := run("add", conflictingPath); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		if _, err := run(
			"-c", "user.name=simclaude",
			"-c", "user.email=simclaude@fabrik.test",
			"commit", "-m", fmt.Sprintf("chore(simclaude): rebase attempt %d for issue #%d (still conflicting)", seq, issue.Number),
		); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		branch := fmt.Sprintf("fabrik/issue-%d", issue.Number)
		if _, err := run("push", "--force", "origin", "HEAD:refs/heads/"+branch); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		usage := engine.TokenUsage{InputTokens: 500, OutputTokens: 100, CostUSD: 0.02, TurnsUsed: 2, MaxTurns: 50}
		return "FABRIK_STAGE_COMPLETE\n", true, usage, nil
	}
}

// TestRebaseCycleLimit is R2's rebase-cycle-limit scenario (ADR-028,
// checkMergeabilityGate + dispatchWithCycleLimit's rebase-reinvoke branch,
// engine/catch_up_handlers.go). Sequence: Implement's default-script commit
// lands conflictingPath on the PR branch; a direct SeedCommit to "main"
// lands the same path with different content, producing a genuine,
// persistent base-branch conflict that checkMergeabilityGate re-detects
// every poll (fabrik:rebase-needed applied); rebaseConflictScript's
// maxCycles genuinely distinct, still-conflicting reinvokes exhaust
// MaxRebaseCycles; the (maxCycles+1)th classification routes to
// pauseForRebaseCycleLimit instead of another reinvoke.
func TestRebaseCycleLimit(t *testing.T) {
	t.Parallel()
	const maxCycles = 2
	env := NewEnv(t, EnvOptions{
		Stages: ciFixStages(),
		// Real-time-anchored gate branches need StartTime near time.Now() —
		// see TestCIFixReinvoke's identical StartTime comment.
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxRebaseCycles = maxCycles
		},
	})
	env.Claude.ForStageComments("Validate", rebaseConflictScript())

	num := FileIssue(t, env, "rebase cycle limit", "Prove the rebase cycle limit terminates the loop.", "Implement")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	// The PR now exists with Implement's own default-script commit at
	// conflictingPath. Commit directly to main at the same path with
	// different content — simgh's real trial-merge mergeability check
	// (gitFacts) will find a genuine, persistent add/add conflict from this
	// point on.
	env.Sim.Sim().SeedCommit(env.OwnerRepo, "main", map[string]string{conflictingPath: "external change on main\n"}, "external: conflicting change")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCommit: %v", err)
	}

	WaitForIssueLabel(t, env, num, "fabrik:rebase-needed", 80)
	t.Logf("fabrik:rebase-needed confirmed on #%d — merge gate genuinely engaged", num)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	baselineSHA := pr.HeadSHA
	simBareDir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}

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
			t.Fatalf("rebase reinvoke #%d did not land a new commit", reinvokeNum)
		}
		if seenSHAs[curSHA] {
			t.Fatalf("rebase reinvoke #%d landed a SHA already seen (%s) — no new commit was pushed", reinvokeNum, curSHA)
		}
		seenSHAs[curSHA] = true
		lastSHA = curSHA
		t.Logf("rebase reinvoke %d/%d landed SHA %s", reinvokeNum, maxCycles, curSHA)
	}

	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 20)

	item := projectItem(t, env, num)
	if item.IsClosed {
		t.Fatal("issue closed after rebase cycle limit — expected OPEN")
	}
	if !hasCommentWithPrefix(item.Comments, rebaseCycleLimitCommentPrefix) {
		t.Fatalf("no engine-authored rebase cycle-limit comment (prefix %q) found on #%d — the issue paused, but not provably via pauseForRebaseCycleLimit",
			rebaseCycleLimitCommentPrefix, num)
	}

	finalPR, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR (final): %v", err)
	}
	finalCommits := gitCommitCount(t, simBareDir, baselineSHA, finalPR.HeadSHA)
	if finalCommits != maxCycles {
		t.Fatalf("expected exactly %d rebase-reinvoke commit(s) beyond baseline, got %d — cycle limit not reached via genuinely distinct cycles (or reached one early/late)", maxCycles, finalCommits)
	}
	t.Logf("%s#%d paused after exactly %d genuine rebase cycles — cycle limit verified", env.OwnerRepo, num, maxCycles)
}
