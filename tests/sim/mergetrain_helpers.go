package sim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file is the merge-train scenario package's shared fixture (#1452): a
// pipeline shape with a Queued holding stage, an Env constructor tuned for
// fast real-git trial cycling, a QueueMember helper that seeds a batch
// member directly at Queued (mirroring tests/e2e/mergetrain_helpers.go's
// QueueMember — the train is column-driven, ADR-059 D1, so an
// externally-placed Queued item with a linked PR is a valid member without
// running Specify..Validate), and the verdict-seeder that resolves the
// "declare poison sets by SHA, verdicts discovered by the algorithm"
// requirement (R2) without predicting a trial SHA in advance.
//
// See adrs/1452-mergetrain-sim-harness-seams.md for why the two engine-side
// seams this package leans on (Engine.SetTrainCIPollIntervalForTest,
// Engine.SetGeneratedFilesForTest) exist and why the verdict seeder — not a
// synchronous hook — is the chosen mechanism despite the race it accepts.

// mergeTrainBranchPrefix mirrors engine's own unexported trainBranchPrefix
// constant (engine/worktree.go) — duplicated here rather than exported,
// following tests/e2e/mergetrain_helpers.go's own precedent
// (WaitForIntegrationPR matches the same literal directly).
const mergeTrainBranchPrefix = "fabrik/merge-train/"

// mergeTrainStages returns the pipeline shape every scenario in this file
// uses: Specify through Validate (never actually dispatched — QueueMember
// seeds members directly at Queued, bypassing the pipeline entirely, exactly
// as tests/e2e's QueueMember does) plus Queued (the HoldingStage merge-train
// input column, ADR-059 D1) and Done.
func mergeTrainStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Research", Order: 2},
		{Name: "Plan", Order: 3},
		{Name: "Implement", Order: 4, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 5},
		{Name: "Validate", Order: 6},
		{Name: "Queued", Order: 7, HoldingStage: true},
		{Name: "Done", Order: 8, CleanupWorktree: true},
	}
}

// mergeTrainEnvOptions configures mergeTrainEnv. Every field has a working
// default; a scenario only sets what it cares about.
type mergeTrainEnvOptions struct {
	// Mode sets cfg.MergeTrain — "on" (default) or "off" (R8/AC8).
	Mode string

	// ConfigureCfg, when non-nil, runs after this file's own merge-train
	// defaults are applied (short CIBackstopTimeout, small MaxBatchSize) —
	// an escape hatch for a scenario needing e.g. a smaller
	// MaxMergeTrainEjections or MaxTrainTrialsPerWindow, mirroring
	// EnvOptions.ConfigureCfg's own role.
	ConfigureCfg func(*engine.Config)
}

// mergeTrainEnv wraps NewEnv with the configuration every merge-train
// scenario needs: cfg.MergeTrain (R8), a short CIBackstopTimeout and a
// millisecond-scale CI poll interval (via SetTrainCIPollIntervalForTest —
// see adrs/1452-mergetrain-sim-harness-seams.md) so a red-batch bisection
// matrix's dozen-plus real trials complete in a test-suite-appropriate time
// instead of pollTrainCI/pollForMergeable's real 30s literal, and a small
// MaxBatchSize matching production's own default (5) so scenario batches
// stay small without a scenario having to say so itself.
//
// CIBackstopTimeout is 10s, not the verdict seeder's own typical
// sub-millisecond turnaround, deliberately: it is the safety net for the one
// case startTrialVerdictSeeder's own doc comment accepts as a known,
// non-flaw race — losing the race against the engine's first FetchCheckRuns
// read on a fresh trial PR. Under this package's own heavy parallel real-git
// load (many scenarios' worth of concurrent merge/push/gc), that race can be
// lost repeatedly before the seeder's goroutine gets scheduled; a backstop
// too close to the seeder's happy-path latency turns ordinary contention into
// a spurious TrainCIPending/timeout instead of the one extra
// SetTrainCIPollIntervalForTest-scaled retry the doc comment promises.
func mergeTrainEnv(t *testing.T, opts mergeTrainEnvOptions) *Env {
	t.Helper()
	mode := opts.Mode
	if mode == "" {
		mode = "on"
	}
	env := NewEnv(t, EnvOptions{
		Stages: mergeTrainStages(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MergeTrain = mode
			cfg.CIBackstopTimeout = 10 * time.Second
			cfg.MaxBatchSize = 5
			if opts.ConfigureCfg != nil {
				opts.ConfigureCfg(cfg)
			}
		},
	})
	env.Engine.SetTrainCIPollIntervalForTest(15 * time.Millisecond)
	return env
}

// QueueMember seeds one merge-train batch member: an issue at "Queued" and a
// linked PR on the fabrik/issue-<N> branch (the exact convention
// github.Client.FetchLinkedPR/simgh.FetchLinkedPR resolve by — see
// tests/e2e/mergetrain_helpers.go's QueueMember doc comment; the sim's
// production-parity requirement (R6, ADR-059) makes this the same
// convention, not a coincidence) carrying files, committed on top of "main".
// Distinct files across members merge cleanly; two members writing the same
// path produce a genuine git conflict (R1/R4) — the caller controls this via
// files, mirroring the live harness's path/content parameters.
func QueueMember(t *testing.T, env *Env, marker string, files map[string]string) (issueNum, prNum int) {
	t.Helper()
	title := fmt.Sprintf("merge-train member %s", marker)
	num := FileIssue(t, env, title, "merge-train member. marker="+marker, "Queued")
	branch := fmt.Sprintf("fabrik/issue-%d", num)

	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "main", files,
		fmt.Sprintf("merge-train member commit for #%d (%s)", num, marker))
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("QueueMember(#%d): seeding commit: %v", num, err)
	}

	// Reserve the PR's number from the same monotonic Env sequence FileIssue
	// draws from, rather than letting SeedPR auto-assign from the repo's own
	// internal counter: a scenario queuing several members in one Env calls
	// FileIssue (issue) and SeedPR (PR) alternately in the same repo's
	// shared issue-and-PR number space, and env.issueSeqNext has no
	// visibility into numbers SeedPR's own auto-assign hands out — letting
	// both draw from it independently produces a collision as soon as a
	// second member is queued (confirmed: FileIssueInRepo: simgh:
	// acme/widgets#2 already exists). Explicit end-to-end reservation here
	// keeps every number this Env ever mints unique by construction.
	env.issueSeqMu.Lock()
	env.issueSeqNext++
	prNum = env.issueSeqNext
	env.issueSeqMu.Unlock()

	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Number:      prNum,
		Head:        branch,
		Base:        "main",
		Title:       title,
		Body:        fmt.Sprintf("merge-train member PR for #%d.\n\nCloses #%d\n", num, num),
		IssueNumber: num,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("QueueMember(#%d): seeding PR: %v", num, err)
	}

	pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("QueueMember(#%d): could not resolve seeded linked PR: %v", num, err)
	}
	t.Logf("queued member: issue #%d, PR #%d, marker=%s", num, pr.Number, marker)
	return num, pr.Number
}

// resyncIssueSeqAfterEngineActivity re-synchronizes env's own issue/PR
// number reservation counter (issueSeqNext) with the sim's real internal
// per-repo counter after a poll has run. Engine activity — chiefly
// CreateDraftPR/CreatePR for merge-train trial and landing PRs — auto-assigns
// numbers from the sim's own sequence independently of anything
// QueueMember/FileIssue reserved through issueSeqNext, so a scenario that
// queues a *fresh* member after running a poll (a fresh partner for a second
// ejection round, e.g. mergetrain_ejection_test.go's
// TestMergeTrainEjection_MaxEjectionsPausesMember) would otherwise collide
// with a number the engine already claimed mid-poll for a trial or landing
// PR. Call this once between RunPoll and the next QueueMember/FileIssue in
// any scenario that interleaves the two. Scans every PR (open or closed —
// state doesn't affect whether a number is taken) for the highest number
// seen and bumps issueSeqNext past it; a no-op when nothing has moved past
// env's own bookkeeping.
func resyncIssueSeqAfterEngineActivity(t *testing.T, env *Env) {
	t.Helper()
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("resyncIssueSeqAfterEngineActivity: ListPRs: %v", err)
	}
	highest := 0
	for _, pr := range prs {
		if pr.Number > highest {
			highest = pr.Number
		}
	}
	env.issueSeqMu.Lock()
	if highest > env.issueSeqNext {
		env.issueSeqNext = highest
	}
	env.issueSeqMu.Unlock()
}

// greenCheckRun and redCheckRun build a single completed check run for
// startTrialVerdictSeeder's verdictFor callback. name defaults to "ci" when
// empty, matching this file's scenarios' habit of not caring about the
// specific check name.
func greenCheckRun(name string) gh.CheckRun {
	if name == "" {
		name = "ci"
	}
	return gh.CheckRun{Name: name, Status: "completed", Conclusion: "success"}
}

func redCheckRun(name string) gh.CheckRun {
	if name == "" {
		name = "ci"
	}
	return gh.CheckRun{Name: name, Status: "completed", Conclusion: "failure"}
}

// closesRE extracts member issue numbers from a merge-train trial PR body's
// "Closes #N" lines (assembleAndValidateInner builds one per survivor).
var closesRE = regexp.MustCompile(`Closes #(\d+)`)

func parseClosesNumbers(body string) []int {
	matches := closesRE.FindAllStringSubmatch(body, -1)
	nums := make([]int, 0, len(matches))
	for _, m := range matches {
		if n, err := strconv.Atoi(m[1]); err == nil {
			nums = append(nums, n)
		}
	}
	return nums
}

// startTrialVerdictSeeder is R2/R3's control lever: "check-run verdicts
// keyed by trial SHA mean a scenario can declare exactly which subsets of
// members are poisonous and let the real bisection algorithm discover
// them." Since assembleTrialBranch's trial SHA is a genuine git merge
// result — unpredictable in advance (Research's Constraint 3;
// baseTrialName also uses real time.Now(), so even the trial's branch name
// can't be precomputed) — this runs as a background poll loop that watches
// env's repo for newly opened merge-train trial PRs (head branch prefix
// mergeTrainBranchPrefix) and, the first time it observes one, calls
// verdictFor with the member issue numbers parsed from the PR body's
// "Closes #N" lines and seeds the returned check runs on that PR's real
// head SHA — all via the raw (uninstrumented) *simgh.Sim, so seeding never
// itself appears in the mutation log a scenario's ordering assertions read.
//
// verdictFor returning nil seeds nothing for that trial (the all-green
// default: pollTrainCI's zero-check-run branch never goes green for a draft
// trial PR — see FIDELITY.md's mergeable_state precedence, "draft" sorts
// before "clean" — so a trial actually needs at least one seeded check run
// to resolve either way; letting a scenario request "nothing seeded" is a
// deliberate way to prove the pending/timeout path, not a shortcut to
// green).
//
// Races against the engine's own first FetchCheckRuns read on a fresh trial
// PR are possible (this runs in a separate goroutine, polling every 2ms) —
// accepted deliberately, not a flaw: a lost race reads zero check runs on a
// draft PR, which pollTrainCI/pollForMergeable classify as TrainCIPending
// (never green — the draft mergeable_state fallback is unreachable), so the
// worst case is one extra SetTrainCIPollIntervalForTest-scaled retry before
// the seeder catches up on the next iteration, never a wrong verdict. See
// adrs/1452-mergetrain-sim-harness-seams.md.
//
// The returned stop function is also registered with t.Cleanup, so a
// scenario only needs to call it explicitly when a fresh seeder must
// replace this one before the test ends (e.g. after RestartEnv, which
// constructs a new *Env with its own Sim).
func startTrialVerdictSeeder(t *testing.T, env *Env, verdictFor func(members []int) []gh.CheckRun) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seen := make(map[int]bool)
		for {
			select {
			case <-done:
				return
			default:
			}
			prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
			if err == nil {
				for _, pr := range prs {
					if seen[pr.Number] || pr.State != "open" || !strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
						continue
					}
					seen[pr.Number] = true
					members := parseClosesNumbers(pr.Body)
					verdict := verdictFor(members)
					for _, cr := range verdict {
						env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, cr)
					}
				}
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	stop = func() {
		stopOnce.Do(func() { close(done) })
		wg.Wait()
	}
	t.Cleanup(stop)
	return stop
}

// allGreenVerdict is startTrialVerdictSeeder's verdictFor for a scenario
// where every trial should resolve green regardless of membership.
func allGreenVerdict(members []int) []gh.CheckRun {
	return []gh.CheckRun{greenCheckRun("")}
}

// poisonVerdict returns a verdictFor that reds out any trial whose member
// set intersects poison, and greens every other trial — the R2 poison-matrix
// idiom: "a scenario declares which member combinations are red by
// scripting check runs keyed on trial SHA, then asserts the bisection
// reaches the correct survivor and ejection sets."
func poisonVerdict(poison ...int) func(members []int) []gh.CheckRun {
	poisoned := make(map[int]bool, len(poison))
	for _, n := range poison {
		poisoned[n] = true
	}
	return func(members []int) []gh.CheckRun {
		for _, m := range members {
			if poisoned[m] {
				return []gh.CheckRun{redCheckRun("")}
			}
		}
		return []gh.CheckRun{greenCheckRun("")}
	}
}

// interactionPoisonVerdict returns a verdictFor that reds out a trial only
// when every member in combo is present together — the R2 "a member
// poisonous only in combination with another" shape. A trial containing
// some but not all of combo (e.g. either member validated alone during
// one-at-a-time fallback) is green.
func interactionPoisonVerdict(combo ...int) func(members []int) []gh.CheckRun {
	want := make(map[int]bool, len(combo))
	for _, n := range combo {
		want[n] = true
	}
	return func(members []int) []gh.CheckRun {
		present := make(map[int]bool, len(members))
		for _, m := range members {
			present[m] = true
		}
		for n := range want {
			if !present[n] {
				return []gh.CheckRun{greenCheckRun("")}
			}
		}
		return []gh.CheckRun{redCheckRun("")}
	}
}

// commentsOn returns issueNumber's current comments, failing the test on a
// fetch error.
func commentsOn(t *testing.T, env *Env, issueNumber int) []gh.Comment {
	t.Helper()
	comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, issueNumber)
	if err != nil {
		t.Fatalf("FetchIssueComments(#%d): %v", issueNumber, err)
	}
	return comments
}

// hasCommentContaining reports whether issueNumber carries a comment whose
// body contains substr.
func hasCommentContaining(t *testing.T, env *Env, issueNumber int, substr string) bool {
	t.Helper()
	for _, c := range commentsOn(t, env, issueNumber) {
		if strings.Contains(c.Body, substr) {
			return true
		}
	}
	return false
}

// draftPRCount returns how many CreateDraftPR calls the mutation log has
// recorded so far — R3's trial-count assertion reads this directly (each
// real combined-Validate trial, main-loop or bisection sub-trial, opens
// exactly one draft CI PR).
func draftPRCount(env *Env) int {
	return len(env.Sim.Log().ByMethod("CreateDraftPR"))
}

// claudeResolveConflict returns a CommentScript exercising the ordinary
// successful inline conflict-resolution path (R4): it overwrites path with
// resolvedContent, stages, and commits — the same shape
// engine/merge_train_test.go's TestMergeTrainWorker_ConflictResolvedByClaude
// pins against the production engine directly. Register it via
// env.Claude.ForStageComments(<holding stage name>, ...) — resolveConflict-
// WithClaude invokes InvokeForComments with the *holding* stage (e.g.
// "Queued"), never the member's own pipeline stage (Research finding,
// confirmed against engine/merge_train.go's resolveTrainConflict call site).
func claudeResolveConflict(path, resolvedContent string) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if err := os.WriteFile(filepath.Join(workDir, path), []byte(resolvedContent), 0644); err != nil {
			return "", false, engine.TokenUsage{}, fmt.Errorf("write resolved %s: %w", path, err)
		}
		addCmd := exec.Command("git", "add", "-A")
		addCmd.Dir = workDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			return string(out), false, engine.TokenUsage{}, nil
		}
		commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
			fmt.Sprintf("chore(merge-train): resolve conflict for #%d", issue.Number))
		commitCmd.Dir = workDir
		if out, err := commitCmd.CombinedOutput(); err != nil {
			return string(out), false, engine.TokenUsage{}, nil
		}
		return "resolved successfully", true, engine.TokenUsage{}, nil
	}
}

// claudeLeaveUnresolved returns a CommentScript mirroring
// TestMergeTrainWorker_UnresolvableConflict: it returns a "tried and
// failed" response without touching the worktree at all, leaving the
// conflict markers exactly as the failed `git merge` left them — R4's
// unresolvable-conflict shape, which assembleTrialBranch must eject rather
// than land.
func claudeLeaveUnresolved() simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "unable to resolve", false, engine.TokenUsage{}, nil
	}
}

// claudeUsageLimitDuringConflict returns a CommentScript mirroring
// TestMergeTrainWorker_UsageLimitDuringConflictResolution (ADR-1120): the
// resolution attempt itself hits the account-wide Claude usage limit
// rather than succeeding or failing to resolve — R4's "resolution errors
// because it could not be attempted at all" shape, which must abort the
// whole trial assembly as a fatal error rather than eject the member (the
// usage limit says nothing about whether the conflict is resolvable).
// *claudeerr.UsageLimitError is the exported type engine/claude.go's
// unexported claudeUsageLimitError is a plain alias of (internal/claudeerr,
// #1449 Scope item 2), so this satisfies the engine's errors.As check with
// no engine-side change needed.
func claudeUsageLimitDuringConflict() simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "", false, engine.TokenUsage{}, &claudeerr.UsageLimitError{Message: `terminal_reason="blocking_limit"`}
	}
}

// claudePrematureCommitInMixedMode returns a CommentScript for R4's third
// failure shape: a mixed conflict (spanning both a declared generated path
// and a normal path) where Claude violates buildTrainConflictComment's
// explicit "do not commit the generated path" instruction by committing
// everything anyway — mirroring
// TestMergeTrainWorker_ClaudePrematureCommitInMixedModeEjectsMember. It
// resolves normalPath correctly but leaves generatedPath's conflict markers
// untouched, then commits regardless. regenerateAndCommit's premature-
// commit guard (MERGE_HEAD already gone) must trip and eject the member —
// proving assembleTrialBranch's unconditional pre-merge-HEAD hard reset
// undoes the premature commit rather than letting it contaminate the trial
// branch.
func claudePrematureCommitInMixedMode(normalPath, resolvedNormalContent string) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if err := os.WriteFile(filepath.Join(workDir, normalPath), []byte(resolvedNormalContent), 0644); err != nil {
			return "", false, engine.TokenUsage{}, fmt.Errorf("write resolved %s: %w", normalPath, err)
		}
		addCmd := exec.Command("git", "add", "-A")
		addCmd.Dir = workDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			return string(out), false, engine.TokenUsage{}, nil
		}
		// Commit everything staged — including the generated path's own
		// conflict markers, still present in the index — violating the
		// mixed-mode "do not commit the generated path" instruction.
		commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
			fmt.Sprintf("chore(merge-train): premature commit for #%d", issue.Number))
		commitCmd.Dir = workDir
		if out, err := commitCmd.CombinedOutput(); err != nil {
			return string(out), false, engine.TokenUsage{}, nil
		}
		return "resolved (prematurely committed)", true, engine.TokenUsage{}, nil
	}
}
