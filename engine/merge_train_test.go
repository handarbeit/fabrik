package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// trainTestEngine builds an Engine wired with the given mock client and invoker.
// It configures a Queued holding stage and a Done stage for merge-train use, and sets
// a short CIBackstopTimeout so CI-polling tests terminate quickly — pollForMergeable/
// pollTrainCI's blocking-loop deadline is CIBackstopTimeout (ADR-1410, R6), not
// CIWaitTimeout (now a liveness-stall dwell consumed only by the async CI gate).
// statusField is pre-set so advanceToNextStage can advance members from Queued to
// Done in landing tests.
func trainTestEngine(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker, wm *WorktreeManager) *Engine {
	t.Helper()
	holdingStageConfig := &stages.Stage{
		Name:         "Queued",
		Order:        10,
		HoldingStage: true,
		MaxTurns:     10,
	}
	eng := NewWithDeps(
		Config{
			Owner:                  "owner",
			Repo:                   "repo",
			ProjectNum:             1,
			User:                   "testuser",
			Token:                  "token",
			MaxConcurrent:          5,
			MaxMergeTrainEjections: 3,
			MergeTrain:             "on",
			CIBackstopTimeout:      100 * time.Millisecond, // fast deadline for tests (ADR-1410)
			Stages: []*stages.Stage{
				{Name: "Research", Order: 1, Prompt: "Do research"},
				{Name: "Plan", Order: 2, Prompt: "Make a plan"},
				{Name: "Implement", Order: 3, Prompt: "Implement it"},
				holdingStageConfig,
				{Name: "Done", Order: 99, Prompt: "Cleanup"},
			},
		},
		client,
		claude,
		wm,
	)
	// Pre-set statusField so advanceToNextStage can find the "Done" board column.
	// "Implement" (opt-implement) is included so rerouteQueuedMemberOffHolding
	// (#1208) can resolve its reroute target — the stage stageBeforeHolding derives
	// structurally as the highest-Order non-Unmanaged stage before Queued (Order 10),
	// which is Implement (Order 3) in this stage set.
	eng.statusField = &gh.StatusField{
		FieldID: "sf-test-1",
		Options: map[string]string{
			"Done":      "opt-done",
			"Queued":    "opt-queued",
			"Implement": "opt-implement",
		},
	}
	return eng
}

// makeTrainItem creates a minimal ProjectItem for a batch member.
func makeTrainItem(number int, title string) gh.ProjectItem {
	return gh.ProjectItem{
		Number: number,
		Title:  title,
		Repo:   "owner/repo",
		Status: "Queued",
	}
}

// setupBareRepoForTrain creates a bare git repo with an initial commit on main
// and returns (bareDir, worktreeRoot). It configures git user.name/email so commits
// don't fail due to missing identity.
func setupBareRepoForTrain(t *testing.T) (bareDir, worktreeRoot string) {
	t.Helper()
	skipIfNoGit(t)

	tmp := t.TempDir()
	bareDir = filepath.Join(tmp, "repo.git")
	worktreeRoot = filepath.Join(tmp, "worktrees")

	// Create a source repo to clone from.
	srcDir := filepath.Join(tmp, "src")
	mustGit(t, srcDir, "init", "-b", "main")
	mustGit(t, srcDir, "config", "user.email", "test@test.com")
	mustGit(t, srcDir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(srcDir, "README.md"), "# hello\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "initial commit")

	// Bare clone.
	cmd := exec.Command("git", "clone", "--bare", srcDir, bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %s: %v", out, err)
	}

	// Set fetch refspec and refresh HEAD.
	mustGitDir(t, bareDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	mustGitDir(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	mustGitDir(t, bareDir, "remote", "set-head", "origin", "--auto")
	mustGitDir(t, bareDir, "config", "user.email", "test@test.com")
	mustGitDir(t, bareDir, "config", "user.name", "Test")

	return bareDir, worktreeRoot
}

// addBranchToRepo creates a branch on the src repo (not bare) and pushes to bare.
// srcDir must already exist as a clone of bareDir.
func addMemberBranch(t *testing.T, srcDir, bareDir, branchName, fileName, content string) string {
	t.Helper()
	mustGit(t, srcDir, "checkout", "-b", branchName)
	writeFile(t, filepath.Join(srcDir, fileName), content)
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "add "+fileName)
	mustGit(t, srcDir, "push", bareDir, branchName)
	sha := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	mustGit(t, srcDir, "checkout", "main")
	return sha
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %s: %v", args, dir, strings.TrimSpace(string(out)), err)
	}
}

func mustGitDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("git %v (best-effort): %s", args, strings.TrimSpace(string(out)))
	}
}

func gitOutputDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ── pollTrainCI tests (Task 11f) ──────────────────────────────────────────────

func TestPollTrainCI_MergeableStateClean_ReturnsGreen(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return &tr, "clean", nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIGreen {
		t.Errorf("expected TrainCIGreen for clean mergeable_state, got %v", result)
	}
}

func TestPollTrainCI_MergeableStateUnstable_ReturnsGreen(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return &tr, "unstable", nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIGreen {
		t.Errorf("expected TrainCIGreen for unstable mergeable_state, got %v", result)
	}
}

func TestPollTrainCI_FailedCheckRun_ReturnsRed(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "failure", OutputText: "line 141: assertion failed"},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, diag := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIRed {
		t.Errorf("expected TrainCIRed for failed check run, got %v", result)
	}
	// R1/AC1/AC2: pollTrainCI must return the failing check's diagnostic, not just the
	// enum — this is the point of capture that #1420 fixes. Neutralizing it (returning
	// an empty diagnostic here) must turn this assertion red.
	if diag == nil {
		t.Fatal("expected a non-nil diagnostic for a red result, got nil")
	}
	if len(diag.FailedChecks) != 1 || diag.FailedChecks[0].Name != "build" {
		t.Fatalf("expected diag.FailedChecks to name the failing check \"build\", got %+v", diag.FailedChecks)
	}
	if diag.FailedChecks[0].OutputText != "line 141: assertion failed" {
		t.Errorf("expected diag.FailedChecks[0].OutputText to carry the check's output, got %q", diag.FailedChecks[0].OutputText)
	}
}

func TestPollTrainCI_AllChecksPass_ReturnsGreen(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "blocked", nil // not clean — falls through to check runs
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIGreen {
		t.Errorf("expected TrainCIGreen when all checks pass, got %v", result)
	}
}

func TestPollTrainCI_Timeout_ReturnsPending(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			// Small sleep ensures at least 10ms passes so the 1ms deadline fires
			// after the API call — triggering the post-API deadline check.
			time.Sleep(10 * time.Millisecond)
			return nil, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{Name: "build", Status: "in_progress"}}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner:                  "owner",
			Repo:                   "repo",
			MaxConcurrent:          5,
			MaxMergeTrainEjections: 3,
			CIBackstopTimeout:      1 * time.Millisecond, // expires during first API call (ADR-1410)
			Stages: []*stages.Stage{
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
			},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIPending {
		t.Errorf("expected TrainCIPending on CIBackstopTimeout, got %v", result)
	}
}

// TestPollForMergeable_BackstopTimeout_ReturnsFalseAndDegrades is the R6
// acceptance test (ADR-1410): a batch member whose integration PR never
// reaches a mergeable_state GitHub will accept is not wedged indefinitely —
// pollForMergeable's blocking loop is bounded by CIBackstopTimeout, posts a
// "landing timeout" comment on the first survivor, and returns false so the
// batch simply retries on the next merge-train cycle (no fabrik:paused, no
// escalation — it degrades, matching pollTrainCI's TrainCIPending shape
// above rather than reproducing #342's destructive pause).
func TestPollForMergeable_BackstopTimeout_ReturnsFalseAndDegrades(t *testing.T) {
	var commentPosted bool
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			// Small sleep ensures the post-API deadline check (not the 30s
			// poll-interval sleep) is what trips — mirrors
			// TestPollTrainCI_Timeout_ReturnsPending's pattern.
			time.Sleep(10 * time.Millisecond)
			return &gh.PRDetails{Number: prNumber, MergeableState: ""}, nil // mergeable never resolves, no HeadSHA
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			commentPosted = true
			return 1, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner:                  "owner",
			Repo:                   "repo",
			MaxConcurrent:          5,
			MaxMergeTrainEjections: 3,
			CIBackstopTimeout:      1 * time.Millisecond, // expires during first API call (ADR-1410)
			Stages: []*stages.Stage{
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
			},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if result {
		t.Error("expected pollForMergeable to return false when CIBackstopTimeout elapses with mergeability never resolved")
	}
	if !commentPosted {
		t.Error("expected a landing-timeout comment to be posted on the first survivor")
	}
}

// ── R6 (ADR-1441): pollForMergeable check-run classification ────────────────
//
// pollForMergeable previously read only mergeable_state and treated
// gh.MergeableStateAccepted (clean/unstable) as an unconditional green light
// — the same defect ADR-1153 fixed for pollTrainCI, left unfixed here and
// explicitly flagged as a "candidate fast-follow" that never got its own
// issue. These are the direct regression tests for that fix at this call
// site (AC7), mirroring the pr_settle.go/ci.go fixtures for AC1 — the
// non-vacuousness requirement (AC8) is that these must independently fail if
// only the pr_settle.go/ci.go site were fixed and this one were not.

// TestPollForMergeable_UnstableWithFailedCheckRun_ReturnsFalse is the direct
// AC7 regression test: an integration PR reporting mergeable_state=unstable
// with a concluded failure check run on its head SHA must not be judged
// landable. Must fail against pre-ADR-1441 pollForMergeable, which never
// called FetchCheckRuns at all.
func TestPollForMergeable_UnstableWithFailedCheckRun_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "unstable", HeadSHA: "sha-unstable-failed"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "Test and vet", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if result {
		t.Error("expected pollForMergeable to return false for mergeable_state=unstable with a failed check run")
	}
}

// TestPollForMergeable_UnstableWithPendingCheckRun_KeepsPollingThenDegrades
// covers R2's failing-vs-pending discrimination at this call site: a
// still-running non-required check under mergeable_state=unstable must not
// be treated as a confirmed failure (which would return false immediately
// for the wrong reason) nor as green — it keeps polling until
// CIBackstopTimeout, then degrades like any other unresolved landing (a
// timeout comment is posted, matching TestPollForMergeable_
// BackstopTimeout_ReturnsFalseAndDegrades's shape).
func TestPollForMergeable_UnstableWithPendingCheckRun_KeepsPollingThenDegrades(t *testing.T) {
	var commentPosted bool
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			time.Sleep(10 * time.Millisecond)
			return &gh.PRDetails{Number: prNumber, MergeableState: "unstable", HeadSHA: "sha-unstable-pending"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "Test and vet", Status: "in_progress"},
			}, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			commentPosted = true
			return 1, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner:                  "owner",
			Repo:                   "repo",
			MaxConcurrent:          5,
			MaxMergeTrainEjections: 3,
			CIBackstopTimeout:      1 * time.Millisecond,
			Stages: []*stages.Stage{
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
			},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if result {
		t.Error("expected pollForMergeable to return false — a pending non-required check must not be judged landable")
	}
	if !commentPosted {
		t.Error("expected a landing-timeout comment once CIBackstopTimeout elapses with the check still pending")
	}
}

// TestPollForMergeable_Dirty_ReturnsFalseImmediately confirms the pre-existing
// dirty short-circuit survives the R6 rewrite unchanged: a merge conflict is
// an immediate, definitive rejection — no check-run fetch, no timeout
// comment (distinguishing it from the degrade-on-timeout path above).
func TestPollForMergeable_Dirty_ReturnsFalseImmediately(t *testing.T) {
	checkRunsFetched := false
	commentPosted := false
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "dirty", HeadSHA: "sha-dirty"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			checkRunsFetched = true
			return nil, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			commentPosted = true
			return 1, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if result {
		t.Error("expected pollForMergeable to return false immediately for mergeable_state=dirty")
	}
	if checkRunsFetched {
		t.Error("FetchCheckRuns must NOT be called when mergeable_state=dirty — the conflict is dispositive on its own")
	}
	if commentPosted {
		t.Error("a definitive dirty rejection is not a timeout — no landing-timeout comment should be posted")
	}
}

// TestPollForMergeable_CleanZeroCheckRuns_ReturnsTrue confirms ADR-033/
// ADR-1441's fast path is preserved at this call site: mergeable_state=clean
// with no check-run footprint at all still lands immediately (mirrors
// settlePRMergeState's own zero-check-runs "no CI configured" branch).
func TestPollForMergeable_CleanZeroCheckRuns_ReturnsTrue(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean", HeadSHA: "sha-clean"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // GitHub Actions disabled — no check-run footprint
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if !result {
		t.Error("expected pollForMergeable to return true for mergeable_state=clean with zero check runs")
	}
}

// TestPollForMergeable_RequiredContextFailedZeroCheckRuns_ReturnsFalse mirrors
// TestPollTrainCI_EmptyCheckRuns_RequiredContextFailedViaCommitStatus_ReturnsRed
// for the landing path: a confirmed required-context failure via a classic
// commit status (zero check runs — the local-CI-takeover case #933 was filed
// for) must block landing even though mergeable_state alone would otherwise
// be the only signal available.
func TestPollForMergeable_RequiredContextFailedZeroCheckRuns_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "blocked", HeadSHA: "sha-rc-failed"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // no check runs at all — GitHub Actions disabled
		},
		fetchCombinedStatusFn: func(owner, repo, ref string) ([]gh.CommitStatus, error) {
			return []gh.CommitStatus{{Context: "fantasy/local-test", State: "failure"}}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"fantasy/local-test"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if result {
		t.Error("expected pollForMergeable to return false for a required context confirmed-failed via classic commit status with zero check runs")
	}
}

// TestPollForMergeable_FetchCheckRunsError_DoesNotTreatAsGreen is the
// regression test for a review finding on this PR: a FetchCheckRuns error
// means the PR's real check-run state is unknown, not "confirmed zero check
// runs." classifyLandingCI's zero-check-runs fallback treats an accepted
// mergeable_state (clean/unstable) as sufficient for green — reachable this
// way if a fetch error were (incorrectly) passed through as an empty
// checkRuns slice, which would let an unobserved, possibly-failing check run
// through as green on a transient API error. mergeable_state=unstable here
// means a real check-run failure may be sitting unobserved; the fetch error
// must never be treated as evidence there's nothing to see. Mirrors
// pollTrainCI's if/else-if/else shape, which already gets this right (an
// error takes neither the "has checks" nor the "zero checks" branch).
func TestPollForMergeable_FetchCheckRunsError_DoesNotTreatAsGreen(t *testing.T) {
	var commentPosted bool
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			time.Sleep(10 * time.Millisecond)
			return &gh.PRDetails{Number: prNumber, MergeableState: "unstable", HeadSHA: "sha-fetch-error"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, fmt.Errorf("transient API error")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			commentPosted = true
			return 1, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner:                  "owner",
			Repo:                   "repo",
			MaxConcurrent:          5,
			MaxMergeTrainEjections: 3,
			CIBackstopTimeout:      1 * time.Millisecond,
			Stages: []*stages.Stage{
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
			},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	survivors := []trainMember{{item: gh.ProjectItem{Number: 7}, prNum: 7}}
	result := eng.pollForMergeable(ctx, "owner", "repo", 42, survivors)
	if result {
		t.Error("expected pollForMergeable to return false when FetchCheckRuns errors — a fetch failure must not be treated as confirmed zero check runs")
	}
	if !commentPosted {
		t.Error("expected a landing-timeout comment once CIBackstopTimeout elapses while FetchCheckRuns keeps erroring")
	}
}

// TestPollTrainCI_AllSkippedRequiredContext_NotGreen covers the #933
// regression for the merge-train path: an all-skipped/neutral check-run set
// on the trial head must NOT read as TrainCIGreen when a required context is
// configured but hasn't confirmed success — it should keep polling until
// CIBackstopTimeout, landing on TrainCIPending, never TrainCIGreen.
func TestPollTrainCI_AllSkippedRequiredContext_NotGreen(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "blocked", nil // not clean/unstable — falls through to check runs
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			// Sleep past CIBackstopTimeout (100ms in trainTestEngine) so the
			// post-classification deadline check trips on the first loop
			// iteration instead of waiting through the 30s poll interval.
			time.Sleep(150 * time.Millisecond)
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "skipped"},
				{Name: "fantasy/local-test", Status: "completed", Conclusion: "neutral"},
			}, nil
		},
		fetchCombinedStatusFn: func(owner, repo, ref string) ([]gh.CommitStatus, error) {
			return nil, nil // no classic commit status posted for this SHA either
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"fantasy/local-test"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result == TrainCIGreen {
		t.Fatal("expected pollTrainCI to NOT report TrainCIGreen when a required context is only skipped/neutral/absent on the trial head")
	}
	if result != TrainCIPending {
		t.Errorf("expected TrainCIPending (CIBackstopTimeout reached while required context stays unconfirmed), got %v", result)
	}
}

// TestPollTrainCI_RequiredContextFailedViaCommitStatus_ReturnsRed covers a
// required context whose only producer is a classic commit status (not a
// check run) reporting a confirmed failure on the trial head — this must
// return TrainCIRed, mirroring settlePRMergeState's PRMergeBlocked for the
// same scenario (Requirement 3: Fabrik must be able to see this at all).
func TestPollTrainCI_RequiredContextFailedViaCommitStatus_ReturnsRed(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
			}, nil
		},
		fetchCombinedStatusFn: func(owner, repo, ref string) ([]gh.CommitStatus, error) {
			return []gh.CommitStatus{{Context: "fantasy/local-test", State: "failure"}}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"fantasy/local-test"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIRed {
		t.Errorf("expected TrainCIRed for a required context confirmed-failed via classic commit status, got %v", result)
	}
}

// TestPollTrainCI_EmptyCheckRuns_RequiredContextFailedViaCommitStatus_ReturnsRed
// covers the zero-check-runs case (e.g. GitHub Actions entirely disabled —
// the local-CI-takeover case #933 was filed for): the required-context
// pre-filter used to live only inside the `len(checkRuns) > 0` branch, so a
// trial head with NO check runs at all never got checked against a required
// classic commit status and would just poll to TrainCIPending instead of
// ejecting the poisoning member from the batch. This must return TrainCIRed.
func TestPollTrainCI_EmptyCheckRuns_RequiredContextFailedViaCommitStatus_ReturnsRed(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // no check runs at all — GitHub Actions disabled
		},
		fetchCombinedStatusFn: func(owner, repo, ref string) ([]gh.CommitStatus, error) {
			return []gh.CommitStatus{{Context: "fantasy/local-test", State: "failure"}}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"fantasy/local-test"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIRed {
		t.Errorf("expected TrainCIRed for a required context confirmed-failed via classic commit status with zero check runs, got %v", result)
	}
}

// TestPollTrainCI_Unconfigured_AllSkippedChecks_StillGreen confirms the fix
// is a no-op for repos without required_status_contexts configured (same
// vanilla-GHA-common-case protection as TestSettle_Unconfigured_AllSkippedChecks_StillReady).
func TestPollTrainCI_Unconfigured_AllSkippedChecks_StillGreen(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "skipped"},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	// No RequiredStatusContexts configured.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIGreen {
		t.Errorf("expected TrainCIGreen for skipped checks on an unconfigured repo (no behavior change), got %v", result)
	}
}

// TestPollTrainCI_MergeableStateAccepted_NonRequiredCheckStillPending_NotGreen
// reproduces the #1150 shape (#1153): the repo's sole required check has
// completed successfully, driving mergeable_state to "clean"/"unstable", but
// a non-required check (e.g. the actual test suite, left unmarked-required —
// handarbeit/fabrik's own configuration) is still in_progress on the trial
// SHA. Before this fix, the mergeable_state shortcut would return
// TrainCIGreen immediately; the check-run completeness pass below it never
// ran. The result here must not be TrainCIGreen.
func TestPollTrainCI_MergeableStateAccepted_NonRequiredCheckStillPending_NotGreen(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return &tr, "clean", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "Analyze (go)", Status: "completed", Conclusion: "success"},
				{Name: "Test and vet", Status: "in_progress"},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	// No RequiredStatusContexts configured — matches handarbeit/fabrik's
	// actual unconfigured state; "Analyze (go)" is required only via GitHub
	// branch protection, which Fabrik cannot see directly (ADR-933).

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result == TrainCIGreen {
		t.Fatal("expected pollTrainCI to NOT report TrainCIGreen while a non-required check is still in_progress, even with an accepted mergeable_state")
	}
	if result != TrainCIPending {
		t.Errorf("expected TrainCIPending (CIBackstopTimeout reached while the non-required check stays pending), got %v", result)
	}
}

func TestPollTrainCI_ContextCancelled_ReturnsPending(t *testing.T) {
	var callCount int
	var mu sync.Mutex
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			mu.Lock()
			callCount++
			mu.Unlock()
			return nil, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{Name: "build", Status: "in_progress"}}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIPending {
		t.Errorf("expected TrainCIPending when context cancelled, got %v", result)
	}
}

// ── ejectMember tests (Task 11d) ─────────────────────────────────────────────

func TestEjectMember_PostsComment(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member := makeTrainItem(1, "Test Issue")
	eng.ejectMember("owner", "repo", member, "conflict with #2", nil, nil, true)
	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()

	if len(calls) == 0 {
		t.Fatal("expected ejection comment to be posted")
	}
	if !strings.Contains(calls[0].body, "ejected") {
		t.Errorf("ejection comment should mention 'ejected', got: %s", calls[0].body)
	}
}

// TestEjectMember_StayInQueueWordingIsDistinguishable verifies the #1208 requirement
// that an operator can tell from the ejection comment alone which of the four causes
// fired: stayInQueue=true (the three pre-#1208 causes) must say the member remains in
// Queued, while stayInQueue=false (the new unresolved-review-finding cause) must say
// the opposite — that it has left Queued to be addressed via the review pipeline.
func TestEjectMember_StayInQueueWordingIsDistinguishable(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	stayMember := makeTrainItem(1, "Stays")
	eng.ejectMember("owner", "repo", stayMember, "conflict", nil, nil, true)

	leaveMember := makeTrainItem(2, "Leaves")
	eng.ejectMember("owner", "repo", leaveMember, "unresolved review finding", nil, nil, false)

	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected 2 ejection comments, got %d", len(calls))
	}

	stayBody := calls[0].body
	leaveBody := calls[1].body

	if !strings.Contains(stayBody, "remains in the Queued column") {
		t.Errorf("stayInQueue=true comment should say the member remains in Queued, got: %s", stayBody)
	}
	if strings.Contains(stayBody, "has left the Queued column") {
		t.Errorf("stayInQueue=true comment should not say the member left Queued, got: %s", stayBody)
	}

	if !strings.Contains(leaveBody, "has left the Queued column") {
		t.Errorf("stayInQueue=false comment should say the member left Queued, got: %s", leaveBody)
	}
	if strings.Contains(leaveBody, "remains in the Queued column") {
		t.Errorf("stayInQueue=false comment should not say the member remains in Queued, got: %s", leaveBody)
	}

	if stayBody == leaveBody {
		t.Error("stayInQueue=true and stayInQueue=false ejection comments must be textually distinguishable")
	}
}

func TestEjectMember_PausesAfterMaxEjections(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	// MaxMergeTrainEjections = 3

	member := makeTrainItem(5, "Problem Issue")

	// First two ejections should not add pause labels.
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	client.mu.Lock()
	pauseCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pauseCount++
		}
	}
	client.mu.Unlock()
	if pauseCount != 0 {
		t.Errorf("expected no pause labels after 2 ejections, got %d", pauseCount)
	}

	// Third ejection should trigger pause.
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	client.mu.Lock()
	pauseCount = 0
	awaitCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pauseCount++
		}
		if c.labelName == "fabrik:awaiting-input" {
			awaitCount++
		}
	}
	client.mu.Unlock()

	if pauseCount == 0 {
		t.Error("expected fabrik:paused after 3 ejections")
	}
	if awaitCount == 0 {
		t.Error("expected fabrik:awaiting-input after 3 ejections")
	}
}

func TestEjectMember_EjectionCountIsPerMember(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member1 := makeTrainItem(1, "Issue 1")
	member2 := makeTrainItem(2, "Issue 2")

	// Eject member 1 three times and member 2 once.
	eng.ejectMember("owner", "repo", member1, "conflict", nil, nil, true)
	eng.ejectMember("owner", "repo", member1, "conflict", nil, nil, true)
	eng.ejectMember("owner", "repo", member1, "conflict", nil, nil, true) // triggers pause for #1
	eng.ejectMember("owner", "repo", member2, "conflict", nil, nil, true) // should NOT trigger pause for #2
	client.mu.Lock()
	pausedIssues := make(map[int]bool)
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedIssues[c.issueNumber] = true
		}
	}
	client.mu.Unlock()

	if !pausedIssues[1] {
		t.Error("expected issue #1 to be paused after 3 ejections")
	}
	if pausedIssues[2] {
		t.Error("expected issue #2 NOT to be paused after only 1 ejection")
	}
}

// TestEjectMember_PauseVisibleToCacheAndEcho verifies that when ejectMember pauses a
// member after MaxMergeTrainEjections, the pause labels are mirrored into the board
// cache and registered as pending webhook echoes — not just posted to GitHub. Before
// the fix, ejectMember called AddLabelToIssue with no ApplyLabelAdded/RegisterEcho,
// leaving the cached snapshot stale until the next full board fetch.
func TestEjectMember_PauseVisibleToCacheAndEcho(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_005", ItemID: "PVTI_005", Number: 5, Repo: "owner/repo", Status: "Queued"},
	}, "PVT_1")
	eng.readClient = cache

	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	member := makeTrainItem(5, "Problem Issue")

	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true) // triggers pause
	labels, err := cache.FetchLabels("owner", "repo", 5)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	labelSet := map[string]bool{}
	for _, l := range labels {
		labelSet[l] = true
	}
	if !labelSet["fabrik:paused"] {
		t.Error("expected fabrik:paused to be write-through applied to the cache")
	}
	if !labelSet["fabrik:awaiting-input"] {
		t.Error("expected fabrik:awaiting-input to be write-through applied to the cache")
	}

	wm.mu.Lock()
	_, pausedEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 5)+"+"+"fabrik:paused")]
	_, awaitingEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 5)+"+"+"fabrik:awaiting-input")]
	wm.mu.Unlock()
	if !pausedEcho {
		t.Error("expected fabrik:paused webhook echo to be registered")
	}
	if !awaitingEcho {
		t.Error("expected fabrik:awaiting-input webhook echo to be registered")
	}
}

// TestRecordMergeTrainCloneSkip_EscalatesAfterMaxRetries is the deterministic regression
// test for #1543's follow-up: prepareTrainWorker's batch[0] repo anchor can be a
// different item on every poll, so ADR-1543's identity-gated retry boundary can wedge
// silently forever once it can never match. This proves recordMergeTrainCloneSkip
// escalates (posts a comment on the current anchor) once the skip streak reaches
// e.cfg.MaxRetries, and stays silent below it — sequential, non-racing calls, mirroring
// TestEjectMember_PausesAfterMaxEjections's shape for the sibling counter.
//
// It also asserts the anchor is NOT paused: an earlier revision called pauseIssue on
// the (arbitrary, rotating) anchor, which permanently exiled a healthy Queued member
// from dispatch eligibility every time the streak fired against a new anchor — review
// feedback on this PR. The fix is comment-only; the anchor's dispatch eligibility must
// be unaffected.
func TestRecordMergeTrainCloneSkip_EscalatesAfterMaxRetries(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 3

	anchor := makeTrainItem(42, "Anchor Issue")
	repoKey := "owner/repo"

	// First two skips: below threshold, no pause/comment.
	eng.recordMergeTrainCloneSkip(repoKey, anchor)
	eng.recordMergeTrainCloneSkip(repoKey, anchor)
	client.mu.Lock()
	commentsBefore := len(client.addCommentCalls)
	client.mu.Unlock()
	if commentsBefore != 0 {
		t.Fatalf("expected no comment before threshold, got %d", commentsBefore)
	}

	// Third skip reaches MaxRetries: escalate.
	eng.recordMergeTrainCloneSkip(repoKey, anchor)
	client.mu.Lock()
	comments := len(client.addCommentCalls)
	var pausedAdded, awaitingAdded bool
	for _, c := range client.addLabelCalls {
		if c.issueNumber != 42 {
			continue
		}
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
		if c.labelName == "fabrik:awaiting-input" {
			awaitingAdded = true
		}
	}
	client.mu.Unlock()
	if comments != 1 {
		t.Fatalf("expected exactly 1 comment on escalation, got %d", comments)
	}
	if pausedAdded {
		t.Error("expected the anchor NOT to receive fabrik:paused after escalation (would exile a healthy, rotating item)")
	}
	if awaitingAdded {
		t.Error("expected the anchor NOT to receive fabrik:awaiting-input after escalation")
	}
}

// TestRecordMergeTrainCloneSkip_MessageNamesPinnedOwner verifies the escalation comment
// identifies the specific issue whose failed clone attempt pinned cloneInFlight's
// ownerKey — the operator needs to know which issue's fabrik:paused to clear, since the
// escalation comment lands on the anchor item, which is never itself paused (see
// #1543's follow-up discussion).
func TestRecordMergeTrainCloneSkip_MessageNamesPinnedOwner(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 1

	anchor := makeTrainItem(50, "Anchor Issue")
	repoKey := "owner/repo"
	eng.cloneInFlight.Store(repoKey, &cloneCall{done: make(chan struct{}), ownerKey: "owner/repo#99"})

	eng.recordMergeTrainCloneSkip(repoKey, anchor)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "owner/repo#99") {
		t.Errorf("expected comment to name the pinned owner owner/repo#99, got: %s", body)
	}
	if !strings.Contains(body, "#50") {
		t.Errorf("expected comment to reference the anchor issue #50, got: %s", body)
	}
}

// TestRecordMergeTrainCloneSkip_ResetAfterEscalationGivesFreshBudget verifies the
// counter is reset once escalation fires, so a further single skip right after doesn't
// immediately re-escalate — mirrors ejectMember's own reset-on-pause behavior.
func TestRecordMergeTrainCloneSkip_ResetAfterEscalationGivesFreshBudget(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 2

	anchor := makeTrainItem(7, "Anchor Issue")
	repoKey := "owner/repo"

	eng.recordMergeTrainCloneSkip(repoKey, anchor) // count=1
	eng.recordMergeTrainCloneSkip(repoKey, anchor) // count=2, escalates + resets to 0
	eng.recordMergeTrainCloneSkip(repoKey, anchor) // count=1 again, below threshold

	client.mu.Lock()
	comments := len(client.addCommentCalls)
	client.mu.Unlock()
	if comments != 1 {
		t.Errorf("expected exactly 1 escalation comment (counter reset after firing), got %d", comments)
	}
}

// TestResetMergeTrainCloneSkip_ClearsStreakOnSuccess verifies that a successful
// ensureRepoReady call (resetMergeTrainCloneSkip) clears an in-progress streak, so a
// later unrelated skip doesn't inherit credit toward escalation from before the repo
// last became ready — matching resetEjectionCount's post-landing-clear precedent.
func TestResetMergeTrainCloneSkip_ClearsStreakOnSuccess(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 2

	anchor := makeTrainItem(9, "Anchor Issue")
	repoKey := "owner/repo"

	eng.recordMergeTrainCloneSkip(repoKey, anchor) // count=1
	eng.resetMergeTrainCloneSkip(repoKey)          // clears the streak
	eng.recordMergeTrainCloneSkip(repoKey, anchor) // count=1 again, not 2

	client.mu.Lock()
	comments := len(client.addCommentCalls)
	client.mu.Unlock()
	if comments != 0 {
		t.Errorf("expected no escalation (streak was reset), got %d comment(s)", comments)
	}
}

// TestRecordMergeTrainCloneSkip_CounterIsPerRepo verifies the skip streak is tracked
// independently per repo, mirroring TestEjectMember_EjectionCountIsPerMember's
// per-member independence for the sibling counter.
func TestRecordMergeTrainCloneSkip_CounterIsPerRepo(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 3

	anchorA := makeTrainItem(1, "Repo A Anchor")
	anchorB := makeTrainItem(2, "Repo B Anchor")

	eng.recordMergeTrainCloneSkip("owner/repoA", anchorA)
	eng.recordMergeTrainCloneSkip("owner/repoA", anchorA)
	eng.recordMergeTrainCloneSkip("owner/repoA", anchorA) // escalates for repoA
	eng.recordMergeTrainCloneSkip("owner/repoB", anchorB) // should NOT escalate for repoB

	client.mu.Lock()
	defer client.mu.Unlock()
	commentedIssues := make(map[int]bool)
	for _, c := range client.addCommentCalls {
		commentedIssues[c.issueNumber] = true
	}
	if !commentedIssues[1] {
		t.Error("expected anchor #1 (repoA) to receive the escalation comment after 3 skips")
	}
	if commentedIssues[2] {
		t.Error("expected anchor #2 (repoB) NOT to receive the escalation comment after only 1 skip")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Errorf("expected no fabrik:paused label anywhere (escalation is comment-only), got it on #%d", c.issueNumber)
		}
	}
}

// TestFireRunawayGuard_PauseVisibleToCacheAndEcho verifies the same cache/echo
// write-through for the runaway-guard pause path.
func TestFireRunawayGuard_PauseVisibleToCacheAndEcho(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_007", ItemID: "PVTI_007", Number: 7, Repo: "owner/repo", Status: "Queued"},
	}, "PVT_1")
	eng.readClient = cache

	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	member := makeTrainItem(7, "Runaway Issue")
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 20)

	labels, err := cache.FetchLabels("owner", "repo", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	labelSet := map[string]bool{}
	for _, l := range labels {
		labelSet[l] = true
	}
	if !labelSet["fabrik:paused"] {
		t.Error("expected fabrik:paused to be write-through applied to the cache")
	}
	if !labelSet["fabrik:awaiting-input"] {
		t.Error("expected fabrik:awaiting-input to be write-through applied to the cache")
	}

	wm.mu.Lock()
	_, pausedEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 7)+"+"+"fabrik:paused")]
	_, awaitingEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 7)+"+"+"fabrik:awaiting-input")]
	wm.mu.Unlock()
	if !pausedEcho {
		t.Error("expected fabrik:paused webhook echo to be registered")
	}
	if !awaitingEcho {
		t.Error("expected fabrik:awaiting-input webhook echo to be registered")
	}
}

// ── #1208 Queued review-finding ejection: stageBeforeHolding / reroute / eject ──

func TestStageBeforeHolding_ReturnsHighestOrderPrecedingStage(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
		{Name: "Implement", Order: 3},
		{Name: "Queued", Order: 10, HoldingStage: true},
		{Name: "Done", Order: 99, CleanupWorktree: true},
	}}
	hs := holdingStage(cfg)
	got := stageBeforeHolding(cfg, hs)
	if got == nil || got.Name != "Implement" {
		t.Fatalf("expected Implement (highest Order < holding stage's Order), got %+v", got)
	}
}

func TestStageBeforeHolding_SkipsUnmanagedStages(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Backlog", Order: 5, Unmanaged: true},
		{Name: "Queued", Order: 10, HoldingStage: true},
	}}
	hs := holdingStage(cfg)
	got := stageBeforeHolding(cfg, hs)
	if got == nil || got.Name != "Research" {
		t.Fatalf("expected Research (Backlog is Unmanaged and must be skipped), got %+v", got)
	}
}

func TestStageBeforeHolding_NilHoldingStage(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{{Name: "Research", Order: 1}}}
	if got := stageBeforeHolding(cfg, nil); got != nil {
		t.Fatalf("expected nil for nil holding stage, got %+v", got)
	}
}

func TestStageBeforeHolding_NoPrecedingStage(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Queued", Order: 1, HoldingStage: true},
	}}
	hs := holdingStage(cfg)
	if got := stageBeforeHolding(cfg, hs); got != nil {
		t.Fatalf("expected nil when nothing precedes the holding stage, got %+v", got)
	}
}

func TestRerouteQueuedMemberOffHolding_MovesStatusToPrecedingStage(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}
	ok := eng.rerouteQueuedMemberOffHolding("PVT_1", item)
	if !ok {
		t.Fatal("expected reroute to succeed")
	}
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call, got %d", len(client.updateStatusCalls))
	}
	call := client.updateStatusCalls[0]
	if call.projectID != "PVT_1" || call.optionID != "opt-implement" {
		t.Errorf("update call = %+v, expected move to opt-implement", call)
	}
}

// TestRerouteQueuedMemberOffHolding_DoesNotTouchLabelsOrClearCycles pins the #1208
// stall-risk constraint: the reroute must be a plain status move only. It must never
// add/remove stage:Validate:complete (already present from the original Validate
// completion) and must never clear ReviewCycles — either would break the "MaxReviewCycles
// applies for free across the eject/re-queue cycle" property the design relies on.
func TestRerouteQueuedMemberOffHolding_DoesNotTouchLabelsOrClearCycles(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	repoStr := "owner/repo"
	eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: repoStr, Number: 1, StageName: "Implement"})
	eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: repoStr, Number: 1, StageName: "Implement"})

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: repoStr, Status: "Queued",
		Labels: []string{"stage:Implement:complete"}}
	if !eng.rerouteQueuedMemberOffHolding("PVT_1", item) {
		t.Fatal("expected reroute to succeed")
	}

	if len(client.addLabelCalls) != 0 || len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label mutations from reroute, got add=%v remove=%v", client.addLabelCalls, client.removeLabelCalls)
	}

	snap, err := eng.store.Get(repoStr, 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.ReviewCycles("Implement"); got != 2 {
		t.Errorf("expected ReviewCycles to remain 2 (untouched by reroute), got %d", got)
	}
}

func TestRerouteQueuedMemberOffHolding_NoPrecedingStage_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", MaxConcurrent: 1,
			Stages: []*stages.Stage{
				{Name: "Queued", Order: 1, HoldingStage: true},
			},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)
	eng.statusField = &gh.StatusField{FieldID: "sf-1", Options: map[string]string{"Queued": "opt-queued"}}

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}
	if eng.rerouteQueuedMemberOffHolding("PVT_1", item) {
		t.Fatal("expected reroute to fail when no stage precedes the holding stage")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update call on failure, got %d", len(client.updateStatusCalls))
	}
}

func TestRerouteQueuedMemberOffHolding_StatusMoveFails_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{updateProjectItemStatusFn: func(string, string, string, string) error { return fmt.Errorf("boom") }}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}
	if eng.rerouteQueuedMemberOffHolding("PVT_1", item) {
		t.Fatal("expected reroute to fail when UpdateProjectItemStatus errors")
	}
}

// TestEjectQueuedMemberForReviewFindings_RerouteFailure_NoCommentNoCount verifies the
// reroute-then-eject ordering: when the status move fails, ejectQueuedMemberForReviewFindings
// must not post an ejection comment or increment the ejection counter — a failed reroute
// looks like nothing happened, so a retry on the next settle scan pass can't double-count.
func TestEjectQueuedMemberForReviewFindings_RerouteFailure_NoCommentNoCount(t *testing.T) {
	client := &mockGitHubClient{updateProjectItemStatusFn: func(string, string, string, string) error { return fmt.Errorf("boom") }}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}
	eng.ejectQueuedMemberForReviewFindings("PVT_1", item, 1)

	client.mu.Lock()
	comments := len(client.addCommentCalls)
	client.mu.Unlock()
	if comments != 0 {
		t.Errorf("expected no ejection comment when reroute fails, got %d", comments)
	}

	eng.mergeTrainEjectionsMu.Lock()
	count := eng.mergeTrainEjectionCounts["owner/repo#1"]
	eng.mergeTrainEjectionsMu.Unlock()
	if count != 0 {
		t.Errorf("expected ejection counter to remain 0 when reroute fails, got %d", count)
	}
}

// TestEjectQueuedMemberForReviewFindings_Success verifies the full happy path: status
// moves off Queued, an ejection comment distinguishable from the stay-in-Queue wording
// is posted, and the ejection counter increments.
func TestEjectQueuedMemberForReviewFindings_Success(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}
	eng.ejectQueuedMemberForReviewFindings("PVT_1", item, 2)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call, got %d", len(client.updateStatusCalls))
	}

	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 ejection comment, got %d", len(calls))
	}
	if !strings.Contains(calls[0].body, "has left the Queued column") {
		t.Errorf("expected ejection comment to use the leaves-Queued wording, got: %s", calls[0].body)
	}
	if !strings.Contains(calls[0].body, "2 unresolved review-thread finding") {
		t.Errorf("expected ejection comment to name the finding count, got: %s", calls[0].body)
	}

	eng.mergeTrainEjectionsMu.Lock()
	count := eng.mergeTrainEjectionCounts["owner/repo#1"]
	eng.mergeTrainEjectionsMu.Unlock()
	if count != 1 {
		t.Errorf("expected ejection counter to be 1, got %d", count)
	}
}

// TestEjectQueuedMemberForReviewFindings_MaxReviewCyclesComposesAcrossCycle is the
// key #1208 stall-risk regression named in the issue's Requirements and Risks
// sections: a member ejected repeatedly for the same unresolved review finding,
// rerouted back to Implement (the stage preceding Queued in this test's stage
// set), and re-picked-up by the ordinary review-reinvoke path (handleReviewGate,
// completely unmodified by this issue) must have its ReviewCycles counter keep
// counting across every eject/reroute cycle — never reset — so it eventually
// escalates via pauseForReviewCycleLimit instead of oscillating Queued<->Implement
// forever. Also confirms MaxMergeTrainEjections (a separate, already-existing
// bound reused via ejectMember) fires independently of ReviewCycles, per the
// issue's own framing of the two bounds as distinct.
func TestEjectQueuedMemberForReviewFindings_MaxReviewCyclesComposesAcrossCycle(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxReviewCycles = 3

	item := gh.ProjectItem{
		Number: 1,
		ItemID: "PVTI_1",
		Repo:   "owner/repo",
		Status: "Queued",
		Labels: []string{"stage:Implement:complete"},
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Please fix this.", ReviewThreadID: "RT_1"},
		},
	}
	// implementStage mirrors stageBeforeHolding's own resolution for trainTestEngine's
	// stage set (Implement, Order 3, is the stage immediately preceding Queued, Order 10).
	implementStage := &stages.Stage{Name: "Implement", Order: 3, Prompt: "implement"}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	pausedByEnd := false
	for i := 0; i <= eng.cfg.MaxReviewCycles; i++ {
		// Step 1: eject the member off Queued — simulates settleQueuedReviewFindings
		// having detected the still-unresolved thread on this poll.
		eng.ejectQueuedMemberForReviewFindings("PVT_1", item, 1)

		// Step 2: simulate the ordinary review-reinvoke path picking the rerouted
		// item back up on the very next poll — the exact same, unmodified
		// handleReviewGate any non-Queued item with an unresolved thread goes through.
		advancedItems := make(map[string]bool)
		pctx := &phase1Ctx{
			ctx: context.Background(), board: board, item: item, stage: implementStage,
			hasComplete: true, advancedItems: advancedItems,
		}
		if claimed := eng.handleReviewGate(pctx); !claimed {
			t.Fatalf("iteration %d: expected handleReviewGate to claim the item", i)
		}
		eng.wg.Wait()

		if advancedItems["owner/repo#1"] {
			continue // a reinvoke dispatched this cycle — keep going
		}
		pausedByEnd = true // no dispatch this cycle: the cap was hit and it paused
		break
	}

	if !pausedByEnd {
		t.Fatal("expected the cycle to eventually pause at MaxReviewCycles instead of dispatching forever")
	}

	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.ReviewCycles("Implement"); got != eng.cfg.MaxReviewCycles {
		t.Errorf("ReviewCycles(Implement) = %d; want exactly %d — must not reset across the eject/reroute cycle", got, eng.cfg.MaxReviewCycles)
	}

	client.mu.Lock()
	hasPaused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	client.mu.Unlock()
	if !hasPaused {
		t.Error("expected fabrik:paused to be added once the review cycle limit was reached")
	}

	// MaxMergeTrainEjections (3 in trainTestEngine) is a separate bound reused via
	// ejectMember: it fires — and resets — purely from repeated ejections in this
	// loop, independent of ReviewCycles. Assert it is a valid, in-range value
	// rather than an exact count, since ejectMember resets its own counter to 0
	// once it pauses (so the exact value depends on loop length vs. the cap).
	eng.mergeTrainEjectionsMu.Lock()
	ejections := eng.mergeTrainEjectionCounts["owner/repo#1"]
	eng.mergeTrainEjectionsMu.Unlock()
	if ejections < 0 || ejections >= eng.cfg.MaxMergeTrainEjections {
		t.Errorf("unexpected ejection counter value: %d (want in [0, %d))", ejections, eng.cfg.MaxMergeTrainEjections)
	}
}

// TestEjectQueuedMemberForReviewFindings_MaxMergeTrainEjectionsFiresIndependently
// confirms the issue's Risks-section expectation directly: MaxMergeTrainEjections
// (ejectMember's own pre-existing consecutive-ejection pause cap) can pause a
// member purely from repeated review-finding ejections, entirely independent of —
// and potentially before — MaxReviewCycles ever fires, since the two bounds are
// driven by different counters incremented from different call sites.
func TestEjectQueuedMemberForReviewFindings_MaxMergeTrainEjectionsFiresIndependently(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxMergeTrainEjections = 2
	eng.cfg.MaxReviewCycles = 100 // deliberately far out of reach in this test

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}

	eng.ejectQueuedMemberForReviewFindings("PVT_1", item, 1)
	client.mu.Lock()
	pausedAfterFirst := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAfterFirst = true
		}
	}
	client.mu.Unlock()
	if pausedAfterFirst {
		t.Fatal("did not expect fabrik:paused after only 1 ejection (MaxMergeTrainEjections=2)")
	}

	eng.ejectQueuedMemberForReviewFindings("PVT_1", item, 1)
	client.mu.Lock()
	pausedAfterSecond := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAfterSecond = true
		}
	}
	client.mu.Unlock()
	if !pausedAfterSecond {
		t.Error("expected fabrik:paused after 2 ejections (MaxMergeTrainEjections=2), purely from review-finding ejections")
	}

	// ReviewCycles is untouched by this path entirely — this test never dispatches
	// through handleReviewGate — confirming the two bounds are driven independently.
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (MaxMergeTrainEjections must fire independently of it)", got)
	}
}

// ── #1208 pending-eject signal: mark/take/apply ─────────────────────────────────

func TestTakePendingReviewEject_UnflaggedMember_NoOp(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	count, ok := eng.takePendingReviewEject("owner/repo", 1)
	if ok || count != 0 {
		t.Fatalf("expected (0, false) for an unflagged member, got (%d, %v)", count, ok)
	}
}

func TestMarkAndTakePendingReviewEject_TakeClearsTheSignal(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	eng.markPendingReviewEject("owner/repo", 1, 3)

	count, ok := eng.takePendingReviewEject("owner/repo", 1)
	if !ok || count != 3 {
		t.Fatalf("expected (3, true) on first take, got (%d, %v)", count, ok)
	}

	// A second take must observe the signal already cleared — one-shot semantics.
	count, ok = eng.takePendingReviewEject("owner/repo", 1)
	if ok || count != 0 {
		t.Fatalf("expected (0, false) on second take (already consumed), got (%d, %v)", count, ok)
	}
}

func TestMarkPendingReviewEject_ScopedPerRepoAndIssue(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	eng.markPendingReviewEject("owner/repo", 1, 1)
	eng.markPendingReviewEject("owner/repo", 2, 5)
	eng.markPendingReviewEject("owner/other", 1, 9)

	if count, ok := eng.takePendingReviewEject("owner/repo", 1); !ok || count != 1 {
		t.Errorf("owner/repo#1: got (%d, %v), want (1, true)", count, ok)
	}
	if count, ok := eng.takePendingReviewEject("owner/repo", 2); !ok || count != 5 {
		t.Errorf("owner/repo#2: got (%d, %v), want (5, true)", count, ok)
	}
	if count, ok := eng.takePendingReviewEject("owner/other", 1); !ok || count != 9 {
		t.Errorf("owner/other#1: got (%d, %v), want (9, true)", count, ok)
	}
}

func TestApplyPendingReviewEjects_EjectsFlaggedMembersOnly(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	members := []trainMember{
		{item: gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}},
		{item: gh.ProjectItem{Number: 2, ItemID: "PVTI_2", Repo: "owner/repo", Status: "Queued"}},
		{item: gh.ProjectItem{Number: 3, ItemID: "PVTI_3", Repo: "owner/repo", Status: "Queued"}},
	}
	eng.markPendingReviewEject("owner/repo", 2, 4)

	remaining, ejectedCount := eng.applyPendingReviewEjects("PVT_1", "owner/repo", members)

	if ejectedCount != 1 {
		t.Fatalf("expected ejectedCount=1, got %d", ejectedCount)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected 2 remaining members, got %d", len(remaining))
	}
	for _, m := range remaining {
		if m.item.Number == 2 {
			t.Errorf("ejected member #2 must not appear in remaining, got: %+v", remaining)
		}
	}

	if len(client.updateStatusCalls) != 1 {
		t.Errorf("expected the flagged member to be rerouted (1 status update), got %d", len(client.updateStatusCalls))
	}

	// The signal is one-shot — a second call with the same members must not re-eject.
	remaining2, ejectedCount2 := eng.applyPendingReviewEjects("PVT_1", "owner/repo", members)
	if ejectedCount2 != 0 || len(remaining2) != 3 {
		t.Errorf("expected second apply to be a no-op (signal already consumed), got ejectedCount=%d remaining=%d", ejectedCount2, len(remaining2))
	}
}

func TestApplyPendingReviewEjects_NoFlags_ReturnsAllUnchanged(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	members := []trainMember{
		{item: gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}},
		{item: gh.ProjectItem{Number: 2, ItemID: "PVTI_2", Repo: "owner/repo", Status: "Queued"}},
	}

	remaining, ejectedCount := eng.applyPendingReviewEjects("PVT_1", "owner/repo", members)
	if ejectedCount != 0 {
		t.Errorf("expected ejectedCount=0, got %d", ejectedCount)
	}
	if len(remaining) != 2 {
		t.Errorf("expected all members to remain, got %d", len(remaining))
	}
}

// ── dispatch guard tests ──────────────────────────────────────────────────────

func TestDispatchMergeTrainWorker_SkipsWhenAlreadyAssembling(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	// Pre-populate in-flight state.
	existingState := &mergeTrainWorkerState{assembling: true, trialName: "existing"}
	eng.mergeTrainInFlight.Store("owner/repo", existingState)

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1")}
	eng.dispatchMergeTrainWorker(context.Background(), batch, "")

	// No goroutine should have been launched (wg count stays 0).
	done := make(chan struct{})
	go func() {
		eng.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Good — no workers launched.
	case <-time.After(100 * time.Millisecond):
		t.Error("wg.Wait() timed out — a goroutine was unexpectedly launched")
	}
}

func TestDispatchMergeTrainWorker_LogsGreenState(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	// Pre-populate with green CI result.
	existingState := &mergeTrainWorkerState{assembling: false, prNum: 99, CIResult: TrainCIGreen}
	eng.mergeTrainInFlight.Store("owner/repo", existingState)

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1")}
	// Just verify it doesn't panic or launch a worker.
	eng.dispatchMergeTrainWorker(context.Background(), batch, "")
}

func TestDispatchMergeTrainWorker_EmptyBatch_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	eng.dispatchMergeTrainWorker(context.Background(), nil, "")
	eng.dispatchMergeTrainWorker(context.Background(), []gh.ProjectItem{}, "")
	// Should not panic or store anything.
}

// ── sanitizeBranchName test ──────────────────────────────────────────────────

func TestSanitizeBranchName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"main", "main"},
		{"release/v1", "release-v1"},
		{"feature/foo/bar", "feature-foo-bar"},
		{"no-slashes", "no-slashes"},
	}
	for _, tc := range cases {
		got := sanitizeBranchName(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeBranchName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── bisection cost-cap derivation tests (Task 1) ──────────────────────────────

func TestCeilLog2(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 0}, {1, 0}, {2, 1}, {3, 2}, {4, 2}, {5, 3}, {8, 3}, {9, 4}, {16, 4}, {17, 5},
	}
	for _, tc := range cases {
		if got := ceilLog2(tc.in); got != tc.want {
			t.Errorf("ceilLog2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveMaxBatchSize(t *testing.T) {
	if got := (&Engine{cfg: Config{MaxBatchSize: 0}}).effectiveMaxBatchSize(); got != 5 {
		t.Errorf("effectiveMaxBatchSize() with unset (0) = %d, want 5 (default)", got)
	}
	if got := (&Engine{cfg: Config{MaxBatchSize: 3}}).effectiveMaxBatchSize(); got != 3 {
		t.Errorf("effectiveMaxBatchSize() with 3 = %d, want 3", got)
	}
}

func TestEffectiveBisectCap_Derivation(t *testing.T) {
	// FR-5 / D-f default: 2·⌈log₂(max_batch_size)⌉ + 1.
	cases := []struct{ batchSize, wantCap int }{
		{1, 1}, {2, 3}, {4, 5}, {5, 7}, {8, 7}, {16, 9},
	}
	for _, tc := range cases {
		e := &Engine{cfg: Config{MaxBatchSize: tc.batchSize}}
		if got := e.effectiveBisectCap(); got != tc.wantCap {
			t.Errorf("effectiveBisectCap() with MaxBatchSize=%d = %d, want %d", tc.batchSize, got, tc.wantCap)
		}
	}
}

func TestEffectiveBisectCap_ExplicitOverride(t *testing.T) {
	e := &Engine{cfg: Config{MaxBatchSize: 5, MaxBisectValidations: 2}}
	if got := e.effectiveBisectCap(); got != 2 {
		t.Errorf("effectiveBisectCap() with explicit override 2 = %d, want 2", got)
	}
}

// ── capBatch tests (Task 2, FR-4) ─────────────────────────────────────────────

func TestCapBatch(t *testing.T) {
	items := []gh.ProjectItem{
		{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}, {Number: 5}, {Number: 6},
	}
	// Larger than cap → capped to first N, entry order preserved.
	got := capBatch(items, 5)
	if len(got) != 5 {
		t.Fatalf("capBatch(6 items, 5) len = %d, want 5", len(got))
	}
	for i, it := range got {
		if it.Number != i+1 {
			t.Errorf("capBatch entry %d = #%d, want #%d (entry order not preserved)", i, it.Number, i+1)
		}
	}
	// Set ≤ cap → unchanged.
	small := []gh.ProjectItem{{Number: 1}, {Number: 2}}
	if got := capBatch(small, 5); len(got) != 2 {
		t.Errorf("capBatch(2 items, 5) len = %d, want 2 (unchanged)", len(got))
	}
	// max ≤ 0 → no cap.
	if got := capBatch(items, 0); len(got) != 6 {
		t.Errorf("capBatch(6 items, 0) len = %d, want 6 (no cap)", len(got))
	}
}

// ── Integration tests (Tasks 11a-e + Task 12, real git) ──────────────────────

// setupTrainRepo creates a bare clone with main configured and a source clone for
// creating branches, returning (bareDir, srcDir, worktreeRoot, WorktreeManager).
func setupTrainRepo(t *testing.T) (bareDir, srcDir, worktreeRoot string, wm *WorktreeManager) {
	t.Helper()
	skipIfNoGit(t)

	tmp := t.TempDir()
	srcDir = filepath.Join(tmp, "src")
	bareDir = filepath.Join(tmp, "repo.git")
	worktreeRoot = filepath.Join(tmp, "worktrees")

	// Create the source repo.
	mustGit(t, srcDir, "init", "-b", "main")
	mustGit(t, srcDir, "config", "user.email", "test@test.com")
	mustGit(t, srcDir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(srcDir, "counter.txt"), "0\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "initial")

	// Bare clone.
	cmd := exec.Command("git", "clone", "--bare", srcDir, bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %s: %v", out, err)
	}
	mustGitDir(t, bareDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	mustGitDir(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	mustGitDir(t, bareDir, "remote", "set-head", "origin", "--auto")
	mustGitDir(t, bareDir, "config", "user.email", "test@test.com")
	mustGitDir(t, bareDir, "config", "user.name", "Test")

	wm = NewWorktreeManagerForRepo(bareDir, worktreeRoot, "test-repo")
	wm.logfFn = func(n int, tag, format string, args ...any) {
		t.Logf("[#%d %s] "+format, append([]any{n, tag}, args...)...)
	}
	return bareDir, srcDir, worktreeRoot, wm
}

// pushBranchToBare creates a branch in srcDir, writes a file, commits, and pushes to bareDir.
// Returns the HEAD SHA.
func pushBranchToBare(t *testing.T, srcDir, bareDir, branchName, fileName, content string) string {
	t.Helper()
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "checkout", "-b", branchName)
	writeFile(t, filepath.Join(srcDir, fileName), content)
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "add "+fileName)
	mustGit(t, srcDir, "push", bareDir, branchName+":"+branchName)
	sha := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "branch", "-D", branchName)
	return sha
}

// TestMergeTrainWorker_CleanBatch verifies that a batch of members that all merge
// cleanly produces a draft integration PR (Task 11a).
func TestMergeTrainWorker_CleanBatch(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	// Create two member branches with non-conflicting changes.
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "file1.txt", "content1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "file2.txt", "content2\n")

	var createdPRs []createDraftPRCall
	var mu sync.Mutex

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs = append(createdPRs, createDraftPRCall{owner, repo, title, head, base, body, issueNumber})
			mu.Unlock()
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil // CI green immediately
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	// Register the WM.
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	eng.store.EnterRepoWorker("owner/repo")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	mu.Lock()
	n := len(createdPRs)
	mu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 draft PR, got %d", n)
	}
	mu.Lock()
	if n > 0 && !strings.Contains(createdPRs[0].head, "merge-train") {
		t.Errorf("draft PR head %q should contain 'merge-train'", createdPRs[0].head)
	}
	mu.Unlock()

	// Landing runs on TrainCIGreen and clears the in-flight entry when done.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight to be cleared after landing completes")
	}
	if eng.store.RepoWorkerActive("owner/repo") {
		t.Error("expected store repo-worker liveness to be cleared after landing completes")
	}
	if state.CIResult != TrainCIGreen {
		t.Errorf("expected TrainCIGreen, got %v", state.CIResult)
	}
}

// TestMergeTrainWorker_PendingReviewEject_DiscardsGreenTrialAndReforms verifies
// Hook 2 (#1208): a pending review-finding eject flagged for a member while its
// batch's trial is assembling/CI-polling must cause the worker to discard that
// trial — even though its CI result is green — and re-form with the reduced
// membership, rather than landing the flagged member via landGreenBatch. This is
// the "checkpoint-consumed-by-the-worker-itself" half of the coordination design;
// settleQueuedReviewFindings (the settle-scan half) is exercised separately.
func TestMergeTrainWorker_PendingReviewEject_DiscardsGreenTrialAndReforms(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "file1.txt", "content1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "file2.txt", "content2\n")

	var createdPRs int
	var mu sync.Mutex

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs++
			mu.Unlock()
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil // CI green immediately, every trial
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	// Flag #1 for a pending review-finding eject BEFORE the worker runs — simulates
	// settleQueuedReviewFindings having observed an unresolved thread on #1 while a
	// worker was already in flight for this repo.
	eng.markPendingReviewEject("owner/repo", 1, 2)

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_1", trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	eng.store.EnterRepoWorker("owner/repo")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	// #1 must have been rerouted off Queued (the ejection), not landed.
	if len(client.updateStatusCalls) == 0 {
		t.Fatal("expected at least 1 status update call rerouting #1 off Queued")
	}
	foundReroute := false
	for _, c := range client.updateStatusCalls {
		if c.optionID == "opt-implement" {
			foundReroute = true
		}
	}
	if !foundReroute {
		t.Errorf("expected a reroute-to-Implement status update among %+v", client.updateStatusCalls)
	}

	client.mu.Lock()
	var ejectionComment string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 1 {
			ejectionComment = c.body
		}
	}
	client.mu.Unlock()
	if !strings.Contains(ejectionComment, "has left the Queued column") {
		t.Errorf("expected #1's ejection comment to use the leaves-Queued wording, got: %q", ejectionComment)
	}

	// The pending-eject signal must have been consumed (one-shot).
	if _, ok := eng.takePendingReviewEject("owner/repo", 1); ok {
		t.Error("expected the pending-eject signal for #1 to already be consumed")
	}

	// The worker must have re-formed and landed #2 normally — trainInFlight/store
	// liveness clears only once the (re-formed) train actually finishes.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight to be cleared once the re-formed train finishes")
	}
	if eng.store.RepoWorkerActive("owner/repo") {
		t.Error("expected store repo-worker liveness to be cleared once the re-formed train finishes")
	}

	// At least 2 draft PRs: the discarded 2-member trial, and the re-formed
	// 1-member trial that actually lands #2.
	mu.Lock()
	prs := createdPRs
	mu.Unlock()
	if prs < 2 {
		t.Errorf("expected at least 2 draft PR creations (discarded trial + re-formed trial), got %d", prs)
	}
}

// TestMergeTrainWorker_UnresolvableConflict verifies ejection when Claude cannot
// resolve a conflict (Task 11c).
func TestMergeTrainWorker_UnresolvableConflict(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	// Both branches modify the same line — guaranteed conflict.
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "counter.txt", "branch1-value\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "counter.txt", "branch2-value\n")

	var addCommentIssues []int
	var createdPRs int
	var mu sync.Mutex

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			mu.Lock()
			addCommentIssues = append(addCommentIssues, issueNumber)
			mu.Unlock()
			return 1, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs++
			mu.Unlock()
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	// Claude returns success but doesn't actually fix the conflict (simulates failure).
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Don't fix the conflict — return success but leave conflict markers.
			return "unable to resolve", false, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	// member 1 merges cleanly, member 2 conflicts.
	// Since both modify counter.txt, the first merge goes in, second conflicts.
	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	mu.Lock()
	prs := createdPRs
	comments := append([]int(nil), addCommentIssues...)
	mu.Unlock()

	// One draft PR should be created (for the survivor — issue #1).
	if prs != 1 {
		t.Errorf("expected 1 draft PR (for survivor #1), got %d", prs)
	}
	// Issue #2 should have received an ejection comment.
	ejectedIssue2 := false
	for _, n := range comments {
		if n == 2 {
			ejectedIssue2 = true
		}
	}
	if !ejectedIssue2 {
		t.Error("expected ejection comment on issue #2")
	}
}

// TestMergeTrainWorker_UsageLimitDuringConflictResolution verifies ADR-1120: a Claude
// usage-limit hit during inline conflict resolution must NOT eject the member (unlike a
// genuine unresolvable conflict, exercised by TestMergeTrainWorker_UnresolvableConflict
// above) — it's an account-wide condition unrelated to whether #2's conflict is
// resolvable. assembleTrialBranch should instead propagate a fatal error, aborting the
// whole trial assembly so no draft PR is created and no ejection comment is posted.
func TestMergeTrainWorker_UsageLimitDuringConflictResolution(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	// Both branches modify the same line — guaranteed conflict.
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "counter.txt", "branch1-value\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "counter.txt", "branch2-value\n")

	var addCommentIssues []int
	var createdPRs int
	var mu sync.Mutex

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			mu.Lock()
			addCommentIssues = append(addCommentIssues, issueNumber)
			mu.Unlock()
			return 1, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs++
			mu.Unlock()
			return 99, nil
		},
	}
	// Claude conflict resolution hits the account usage limit instead of resolving.
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeUsageLimitError{Message: "usage limit reached", ResetTime: "10:20pm (America/Edmonton)"}
		},
	}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	eng.store.EnterRepoWorker("owner/repo")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	mu.Lock()
	prs := createdPRs
	comments := append([]int(nil), addCommentIssues...)
	mu.Unlock()

	// No draft PR — the whole trial assembly must abort as a fatal error, not land
	// a partial survivor set.
	if prs != 0 {
		t.Errorf("expected 0 draft PRs (fatal assembly error), got %d", prs)
	}
	// Issue #2 must NOT receive an ejection comment — the usage limit says nothing
	// about whether its conflict is resolvable.
	for _, n := range comments {
		if n == 2 {
			t.Error("issue #2 must not be ejected on a Claude usage-limit hit")
		}
	}
	// The account-wide suspension must have been activated by the detection.
	if _, suspended := eng.claudeSuspendedUntilTime(time.Now()); !suspended {
		t.Error("expected account-wide Claude suspension to be active after the usage-limit hit")
	}
	// In-flight entry must still be cleared despite the fatal error (ADR-067).
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight to be cleared after fatal assembly error")
	}
	if eng.store.RepoWorkerActive("owner/repo") {
		t.Error("expected store repo-worker liveness to be cleared after fatal assembly error")
	}
}

// TestMergeTrainWorker_ZeroSurvivors verifies FR-6: when FetchLinkedPR fails for
// all members (ejecting each one), no draft PR is created and the in-flight entry
// is cleared (Task 11e).
func TestMergeTrainWorker_ZeroSurvivors(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)

	var createdPRs int
	var mu sync.Mutex

	// All FetchLinkedPR calls return an error → all members ejected immediately.
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, fmt.Errorf("PR not found for issue #%d", issueNumber)
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs++
			mu.Unlock()
			return 99, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	eng.store.EnterRepoWorker("owner/repo")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	mu.Lock()
	prs := createdPRs
	mu.Unlock()

	if prs != 0 {
		t.Errorf("expected 0 draft PRs for zero-survivor batch, got %d", prs)
	}
	// In-flight entry must be cleared (FR-6: no silent abandonment).
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight to be cleared after zero-survivor batch")
	}
	if eng.store.RepoWorkerActive("owner/repo") {
		t.Error("expected store repo-worker liveness to be cleared after zero-survivor batch")
	}
}

// TestMergeTrainWorker_ConflictResolvedByClaude verifies Task 11b: Claude resolves
// a textual conflict and the resolved member appears in survivors (Task 12).
func TestMergeTrainWorker_ConflictResolvedByClaude(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	// Both branches modify counter.txt — but Claude will fix it.
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "counter.txt", "from-branch-1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "counter.txt", "from-branch-2\n")

	var createdPRs []createDraftPRCall
	var mu sync.Mutex

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs = append(createdPRs, createDraftPRCall{owner, repo, title, head, base, body, issueNumber})
			mu.Unlock()
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}

	// Claude resolves the conflict by writing a resolved file and committing.
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Write the resolved file (no conflict markers).
			resolvedContent := "from-branch-1\nfrom-branch-2\n"
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte(resolvedContent), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write resolved file: %w", err)
			}
			// Stage and commit.
			addCmd := exec.Command("git", "add", "-A")
			addCmd.Dir = workDir
			if out, err := addCmd.CombinedOutput(); err != nil {
				return fmt.Sprintf("git add failed: %s", out), false, TokenUsage{}, nil
			}
			commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
				fmt.Sprintf("chore(merge-train): resolve conflict for #%d", issue.Number))
			commitCmd.Dir = workDir
			if out, err := commitCmd.CombinedOutput(); err != nil {
				return fmt.Sprintf("git commit failed: %s", out), false, TokenUsage{}, nil
			}
			return "resolved successfully", true, TokenUsage{}, nil
		},
	}

	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	mu.Lock()
	n := len(createdPRs)
	mu.Unlock()

	// Both members should be in the draft PR (conflict resolved → both survive).
	if n != 1 {
		t.Fatalf("expected 1 draft PR after Claude resolution, got %d", n)
	}
	mu.Lock()
	body := createdPRs[0].body
	mu.Unlock()
	if !strings.Contains(body, "#1") || !strings.Contains(body, "#2") {
		t.Errorf("PR body should reference both members, got: %s", body)
	}
}

// TestPrepareTrainWorker_FailurePathClearsMarkerAndSemaphore verifies the ADR-067
// invariant directly: when prepareTrainWorker fails after acquiring the semaphore
// (here, no holding stage configured), its own defer must release the semaphore AND
// clear mergeTrainInFlight — since ok=false means runMergeTrainWorker's top-level
// defer never gets registered, prepareTrainWorker's own-failure defer is the only
// thing that can prevent a leaked semaphore slot or a permanently wedged train.
func TestPrepareTrainWorker_FailurePathClearsMarkerAndSemaphore(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	// Remove the holding stage so prepareTrainWorker's holdingStage(e.cfg) == nil
	// check fires — one of its four early-return failure branches.
	eng.cfg.Stages = []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "Do research"},
	}

	batch := makeSeamBatch(1)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	eng.store.EnterRepoWorker("owner/repo")

	_, _, ok := eng.prepareTrainWorker(context.Background(), state, "owner", "repo", batch)
	if ok {
		t.Fatal("expected prepareTrainWorker to fail with no holding stage configured")
	}

	if _, found := eng.mergeTrainInFlight.Load("owner/repo"); found {
		t.Error("expected mergeTrainInFlight cleared by prepareTrainWorker's own-failure defer")
	}
	if eng.store.RepoWorkerActive("owner/repo") {
		t.Error("expected store repo-worker liveness cleared by prepareTrainWorker's own-failure defer")
	}

	// The semaphore must be released too: acquiring MaxConcurrent slots must succeed
	// without blocking if prepareTrainWorker didn't leak the one it took.
	acquired := 0
	for i := 0; i < eng.cfg.MaxConcurrent; i++ {
		select {
		case eng.sem <- struct{}{}:
			acquired++
		default:
			t.Fatalf("semaphore slot %d unavailable — prepareTrainWorker leaked its acquired slot", i)
		}
	}
	for i := 0; i < acquired; i++ {
		<-eng.sem
	}
}

// TestEnsureTrainWorktree verifies the WorktreeManager train methods (Task 2 integration).
func TestEnsureTrainWorktree(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, worktreeRoot, _ := setupTrainRepo(t)

	bareDir := filepath.Join(filepath.Dir(srcDir), "repo.git")
	wm := NewWorktreeManagerForRepo(bareDir, worktreeRoot, "test-repo")
	wm.logfFn = func(n int, tag, format string, args ...any) {
		t.Logf("[#%d %s] "+format, append([]any{n, tag}, args...)...)
	}

	wtDir, err := wm.EnsureTrainWorktree("test-trial-123", "main")
	if err != nil {
		t.Fatalf("EnsureTrainWorktree: %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("train worktree directory not created: %v", err)
	}

	// Branch should be fabrik/merge-train/test-trial-123.
	out := strings.TrimSpace(gitOutputDir(t, wtDir, "rev-parse", "--abbrev-ref", "HEAD"))
	if out != "fabrik/merge-train/test-trial-123" {
		t.Errorf("expected branch fabrik/merge-train/test-trial-123, got %s", out)
	}

	// Cleanup.
	if err := wm.CleanupTrainWorktree("test-trial-123", true); err != nil {
		t.Errorf("CleanupTrainWorktree: %v", err)
	}
}

// TestEnsureTrainWorktreeAt verifies base-SHA pinning (D-b): the trial branch is forked
// off the exact SHA passed, not the moving branch tip.
func TestEnsureTrainWorktreeAt(t *testing.T) {
	skipIfNoGit(t)
	bareDir, _, _, wm := setupTrainRepo(t)

	// Resolve the pinned base SHA from the bare repo's origin/main.
	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	wtDir, err := wm.EnsureTrainWorktreeAt("pinned-trial", baseSHA)
	if err != nil {
		t.Fatalf("EnsureTrainWorktreeAt: %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("train worktree directory not created: %v", err)
	}

	head := strings.TrimSpace(gitOutputDir(t, wtDir, "rev-parse", "HEAD"))
	if head != baseSHA {
		t.Errorf("worktree HEAD = %s, want pinned base SHA %s", head, baseSHA)
	}

	if err := wm.CleanupTrainWorktree("pinned-trial", true); err != nil {
		t.Errorf("CleanupTrainWorktree: %v", err)
	}
}

// TestAssembleAndValidate_LeavesWorktreeForCallerCleanup locks in the redundancy fix
// (issue #1151, Requirement 4): assembleAndValidate must no longer remove the local
// trial worktree itself right after opening the draft CI PR — the worktree must still
// exist when assembleAndValidate returns, and is removed only once the caller's own
// cleanup (cleanupTrialArtifacts / CleanupTrainWorktree) runs exactly once.
func TestAssembleAndValidate_LeavesWorktreeForCallerCleanup(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "file1.txt", "content1\n")
	baseSHA := strings.TrimSpace(gitOutputDir(t, wm.baseDir, "rev-parse", "refs/remotes/origin/main"))

	client := &mockGitHubClient{
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil // CI green immediately
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
	}
	members := []trainMember{{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1}}
	const trialName = "assemble-persist-trial"

	survivors, result, prNum, _, err := eng.assembleAndValidate(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleAndValidate: %v", err)
	}
	if len(survivors) != 1 {
		t.Fatalf("expected 1 survivor, got %d", len(survivors))
	}
	if result != TrainCIGreen {
		t.Fatalf("expected TrainCIGreen, got %v", result)
	}
	if prNum != 99 {
		t.Fatalf("expected prNum 99, got %d", prNum)
	}

	wtDir := wm.trainWorktreeDir(trialName)
	if _, statErr := os.Stat(wtDir); statErr != nil {
		t.Fatalf("expected trial worktree %s to still exist after assembleAndValidate returns (caller owns cleanup): %v", wtDir, statErr)
	}

	if err := wm.CleanupTrainWorktree(trialName, true); err != nil {
		t.Fatalf("CleanupTrainWorktree: %v", err)
	}
	if _, statErr := os.Stat(wtDir); !os.IsNotExist(statErr) {
		t.Errorf("expected trial worktree %s removed after caller's cleanup, stat err = %v", wtDir, statErr)
	}
}

// ── landMergeTrainBatch unit tests ────────────────────────────────────────────

// makeQueuedMember returns a trainMember with Status "Queued".
func makeQueuedMember(number, prNum int, title string) trainMember {
	return trainMember{
		item: gh.ProjectItem{
			Number: number,
			Title:  title,
			ItemID: fmt.Sprintf("item-%d", number),
			Repo:   "owner/repo",
			Status: "Queued",
		},
		prNum: prNum,
	}
}

// TestLandMergeTrainBatch_HappyPath verifies the full FR-1 through FR-5 landing sequence:
// integration PR is created (not draft, with batch marker, no Closes #N), polled to clean,
// merged, each member is advanced to Done, and their PRs are closed with a landing comment.
func TestLandMergeTrainBatch_HappyPath(t *testing.T) {
	survivors := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	var createPRTitle, createPRBody string
	var mergePRNum int

	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return nil, nil // no existing integration PR
		},
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRTitle = title
			createPRBody = body
			return 100, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			mergePRNum = prNumber
			return nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)

	state := &mergeTrainWorkerState{
		trialName: "merge-train-main-12345",
		projectID: "PVT_test",
	}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	// FR-1 + connectivity: integration PR created with correct title, the batch
	// marker, AND a Closes #N per member issue (auto-closes them on merge, linking
	// each issue to the landing PR).
	expectedTitle := "[merge-train] batch: #1, #2"
	if createPRTitle != expectedTitle {
		t.Errorf("integration PR title: got %q, want %q", createPRTitle, expectedTitle)
	}
	if !strings.Contains(createPRBody, mergeTrainBatchMarker) {
		t.Errorf("integration PR body missing batch marker %q", mergeTrainBatchMarker)
	}
	for _, want := range []string{"Closes #1", "Closes #2"} {
		if !strings.Contains(createPRBody, want) {
			t.Errorf("integration PR body missing %q (member-issue auto-close)", want)
		}
	}

	// FR-2: integration PR is merged.
	if mergePRNum != 100 {
		t.Errorf("expected MergePR called with integration PR #100, got #%d", mergePRNum)
	}

	// FR-3: both members advanced to Done.
	client.mu.Lock()
	advancedItems := make([]string, len(client.updateStatusCalls))
	for i, c := range client.updateStatusCalls {
		advancedItems[i] = c.itemID
	}
	closed := make([]int, len(client.closeIssueCalls))
	for i, c := range client.closeIssueCalls {
		closed[i] = c.issueNumber
	}
	comments := client.addCommentCalls
	client.mu.Unlock()

	if len(advancedItems) != 2 {
		t.Errorf("expected 2 board status updates (Queued→Done), got %d", len(advancedItems))
	}

	// FR-3: each member's PR (10, 11) AND its issue (1, 2) are closed. The integration
	// PR (#100) must NOT be closed via CloseIssue (it is merged). Closing the issue is
	// the connectivity fix — the member PR is closed-not-merged, so its Closes #N never
	// fires; the landing closes the issue explicitly (belt to the integration PR's
	// Closes #N auto-close).
	wantClosed := map[int]bool{10: false, 11: false, 1: false, 2: false}
	for _, n := range closed {
		if n == 100 {
			t.Errorf("integration PR #100 must not be CloseIssue'd (it is merged)")
		}
		if _, ok := wantClosed[n]; ok {
			wantClosed[n] = true
		}
	}
	for n, seen := range wantClosed {
		if !seen {
			t.Errorf("expected #%d closed (member PR or issue); closes seen: %v", n, closed)
		}
	}

	// Each closure must be preceded by a landed comment citing integration PR #100.
	foundLandedComment := false
	for _, c := range comments {
		if strings.Contains(c.body, "#100") && strings.Contains(c.body, "Fabrik merge-train") {
			foundLandedComment = true
			break
		}
	}
	if !foundLandedComment {
		t.Errorf("expected a 'Landed via merge-train batch PR #100' comment, not found in %v", comments)
	}

	// FR-4: worktree cleanup ran. The in-flight marker itself is cleared by
	// runMergeTrainWorker's top-level defer, not by landMergeTrainBatch (ADR-067) —
	// covered by TestMergeTrainWorker_CleanBatch's end-to-end assertion instead.
}

// TestLandMergeTrainBatch_ExistingOpenPR_SkipsFR1 verifies restart idempotency:
// if ListPRs returns a PR whose body contains the batch marker, CreatePR is not called.
func TestLandMergeTrainBatch_ExistingOpenPR_SkipsFR1(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	createPRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return []gh.PRDetails{
				{Number: 200, State: "open", Merged: false, Body: "text " + mergeTrainBatchMarker + " more"},
			}, nil
		},
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 999, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	if createPRCalled {
		t.Error("CreatePR must not be called when an existing integration PR is found (FR-1 skip)")
	}

	// Integration PR #200 should be merged instead.
	client.mu.Lock()
	mergedPRs := client.mergePRCalls
	client.mu.Unlock()
	if len(mergedPRs) != 1 || mergedPRs[0].prNumber != 200 {
		t.Errorf("expected MergePR #200, got %v", mergedPRs)
	}
}

// TestLandMergeTrainBatch_ReusesDraftCIPR_MarksReady is the regression for the
// landing bug the e2e caught: the trial's draft CI PR carries mergeTrainBatchMarker
// and IS the landing integration PR, so findIntegrationPR must reuse it — and
// because it is a draft, landing must MarkPRReady before merging (GitHub refuses to
// merge a draft). Before the fix, the draft body lacked the marker so findIntegrationPR
// returned nil and landing tried to CreatePR a second PR on the same trial branch,
// which GitHub rejects with a 422 ("a pull request already exists").
func TestLandMergeTrainBatch_ReusesDraftCIPR_MarksReady(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	createPRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return []gh.PRDetails{
				{Number: 200, State: "open", Merged: false, Draft: true, Body: "draft CI PR " + mergeTrainBatchMarker},
			}, nil
		},
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 999, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil },
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	if createPRCalled {
		t.Error("CreatePR must not be called — the draft CI PR must be reused (regression: 422 collision)")
	}
	client.mu.Lock()
	ready := client.markPRReadyCalls
	merged := client.mergePRCalls
	client.mu.Unlock()

	readyOK := false
	for _, c := range ready {
		if c.prNumber == 200 {
			readyOK = true
		}
	}
	if !readyOK {
		t.Errorf("draft integration PR #200 must be MarkPRReady'd before merge; got %+v", ready)
	}
	mergedOK := false
	for _, c := range merged {
		if c.prNumber == 200 {
			mergedOK = true
		}
	}
	if !mergedOK {
		t.Errorf("integration PR #200 must be merged; got %+v", merged)
	}
}

// TestLandMergeTrainBatch_AlreadyMergedPR_SkipsFR2 verifies restart idempotency:
// if the found integration PR is already merged, FR-2 (MergePR) is skipped and
// FR-3 (member advancement) proceeds.
func TestLandMergeTrainBatch_AlreadyMergedPR_SkipsFR2(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	mergePRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return []gh.PRDetails{
				{Number: 300, State: "closed", Merged: true, Body: mergeTrainBatchMarker},
			}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			mergePRCalled = true
			return nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	// FR-2 skip: MergePR must not be called.
	if mergePRCalled {
		t.Error("MergePR must not be called when integration PR is already merged (FR-2 skip)")
	}

	// FR-3: member should still be advanced.
	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := len(client.closeIssueCalls)
	client.mu.Unlock()

	if advanced != 1 {
		t.Errorf("expected 1 board status update (FR-3), got %d", advanced)
	}
	if closed != 2 {
		t.Errorf("expected 2 closes — member PR #10 + member issue #1 (connectivity fix), got %d", closed)
	}
}

// TestLandMergeTrainBatch_MemberAlreadyInDone_SkipsFR3 verifies that a member whose
// Status is "Done" is silently skipped during the FR-3 advancement loop.
func TestLandMergeTrainBatch_MemberAlreadyInDone_SkipsFR3(t *testing.T) {
	survivors := []trainMember{
		{item: gh.ProjectItem{Number: 1, Title: "Done Member", ItemID: "item-1", Repo: "owner/repo", Status: "Done"}, prNum: 10},
		makeQueuedMember(2, 11, "Queued Member"),
	}

	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			return 100, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	// Only the Queued member should be advanced; the Done member must be skipped.
	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := client.closeIssueCalls
	client.mu.Unlock()

	if advanced != 1 {
		t.Errorf("expected 1 board status update (Done member skipped), got %d", advanced)
	}
	// The Queued member closes its PR (#11) AND its issue (#2); the Done member
	// (#1 / PR #10) is skipped entirely — neither closed.
	if len(closed) != 2 {
		t.Errorf("expected 2 closes (PR #11 + issue #2 for the Queued member); Done member skipped; got %v", closed)
	}
	for _, c := range closed {
		if c.issueNumber == 10 || c.issueNumber == 1 {
			t.Errorf("Done member (#1 / PR #10) must be skipped, not closed; got %v", closed)
		}
		if c.issueNumber != 11 && c.issueNumber != 2 {
			t.Errorf("unexpected close #%d (want PR #11 or issue #2); got %v", c.issueNumber, closed)
		}
	}
}

// TestLandMergeTrainBatch_MergeAPIFailure verifies that a MergePR error results in
// an error comment on the first batch member issue, members remain in Queued
// (no UpdateProjectItemStatus calls), and cleanup still runs.
func TestLandMergeTrainBatch_MergeAPIFailure(t *testing.T) {
	survivors := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			return 100, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return fmt.Errorf("merge rejected: branch protection rules not satisfied")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := len(client.closeIssueCalls)
	comments := client.addCommentCalls
	client.mu.Unlock()

	// Members must not be advanced or closed after a merge failure.
	if advanced != 0 {
		t.Errorf("expected 0 board status updates after merge failure, got %d", advanced)
	}
	if closed != 0 {
		t.Errorf("expected 0 PR closures after merge failure, got %d", closed)
	}

	// An error comment must be posted on the first batch member issue.
	foundErrComment := false
	for _, c := range comments {
		if c.issueNumber == 1 && strings.Contains(c.body, "merge failure") {
			foundErrComment = true
			break
		}
	}
	if !foundErrComment {
		t.Errorf("expected a merge-failure comment on issue #1, got comments: %v", comments)
	}

	// Cleanup (worktree removal) must still run via the deferred func inside
	// landMergeTrainBatch. The in-flight marker itself is cleared by
	// runMergeTrainWorker's top-level defer, not here (ADR-067).
}

// TestLandSingleton_MergeAPIFailure_CINotGreen_NoEscalation is the issue #1094
// regression test for the landSingleton call site: pollForMergeable already judged the
// singleton landing PR acceptable (mergeable_state=clean), but MergePR's own new
// precondition check observes a flipped state and refuses with gh.ErrNotMergeableCI.
// landSingleton must handle this exactly like any other MergePR failure — log and
// return, leaving the member in Queued with no Done advancement, no PR/issue closure,
// and no fabrik:paused or fabrik:rebase-needed escalation label — confirming the new
// precondition does not strand a PR pollForMergeable already judged acceptable.
func TestLandSingleton_MergeAPIFailure_CINotGreen_NoEscalation(t *testing.T) {
	m := makeQueuedMember(9, 90, "Issue Nine")
	client := &mockGitHubClient{
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 900, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return fmt.Errorf("%w: mergeable_state=%q", gh.ErrNotMergeableCI, "blocked")
		},
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm, holdingStg: holdingStage(eng.cfg)}

	eng.landSingleton(context.Background(), state, p, m, "merge-train-singleton-ci")

	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := len(client.closeIssueCalls)
	labels := client.addLabelCalls
	client.mu.Unlock()

	if advanced != 0 {
		t.Errorf("expected 0 board status updates after CI-not-green refusal, got %d", advanced)
	}
	if closed != 0 {
		t.Errorf("expected 0 issue/PR closures after CI-not-green refusal, got %d", closed)
	}
	for _, c := range labels {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be applied for a CI-not-green MergePR refusal — it should retry on the next merge-train cycle")
		}
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("fabrik:rebase-needed must NOT be applied for a CI-not-green MergePR refusal — that would incorrectly consume a rebase cycle")
		}
	}
}

// TestLandMergeTrainBatch_MergeAPIFailure_CINotGreen_NoEscalation is the issue #1094
// regression test for this call site: pollForMergeable already judged the integration
// PR acceptable (mergeable_state=clean), but MergePR's own new precondition check
// observes a flipped state (a TOCTOU window between the two GETs) and refuses with
// gh.ErrNotMergeableCI. This must be handled exactly like any other MergePR failure
// here — members stay in Queued, no Done advancement, no PR closure, no fabrik:paused
// or fabrik:rebase-needed escalation label — confirming the new precondition does not
// strand a PR the calling gate already judged acceptable.
func TestLandMergeTrainBatch_MergeAPIFailure_CINotGreen_NoEscalation(t *testing.T) {
	survivors := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			return 100, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return fmt.Errorf("%w: mergeable_state=%q", gh.ErrNotMergeableCI, "blocked")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := len(client.closeIssueCalls)
	labels := client.addLabelCalls
	client.mu.Unlock()

	// Members must not be advanced or closed after a CI-not-green merge refusal —
	// same invariant as any other merge failure at this call site.
	if advanced != 0 {
		t.Errorf("expected 0 board status updates after CI-not-green refusal, got %d", advanced)
	}
	if closed != 0 {
		t.Errorf("expected 0 PR closures after CI-not-green refusal, got %d", closed)
	}
	for _, c := range labels {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be applied for a CI-not-green MergePR refusal — it should retry on the next merge-train cycle")
		}
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("fabrik:rebase-needed must NOT be applied for a CI-not-green MergePR refusal — that would incorrectly consume a rebase cycle")
		}
	}
}

// TestLandMergeTrainBatch_ResetsEjectionCounter verifies that a successful landing resets
// the per-member ejection counter, so stale history from a prior train does not count
// toward the pause cap on a future train.
func TestLandMergeTrainBatch_ResetsEjectionCounter(t *testing.T) {
	survivors := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	client := &mockGitHubClient{
		listPRsFn:  func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 100, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, number int) error { return nil },
	}

	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	// Pre-seed stale ejection counts from a prior train.
	eng.mergeTrainEjectionsMu.Lock()
	eng.mergeTrainEjectionCounts["owner/repo#1"] = 2
	eng.mergeTrainEjectionCounts["owner/repo#2"] = 1
	eng.mergeTrainEjectionsMu.Unlock()

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	// After landing, both counters must be zeroed.
	eng.mergeTrainEjectionsMu.Lock()
	count1 := eng.mergeTrainEjectionCounts["owner/repo#1"]
	count2 := eng.mergeTrainEjectionCounts["owner/repo#2"]
	eng.mergeTrainEjectionsMu.Unlock()

	if count1 != 0 {
		t.Errorf("expected ejection counter for member #1 to be 0 after landing, got %d", count1)
	}
	if count2 != 0 {
		t.Errorf("expected ejection counter for member #2 to be 0 after landing, got %d", count2)
	}
}

// ── bisection AC tests: mock combined-Validate keyed on batch membership (Tasks 10-11) ──

// recordingValidator is a membership-keyed combined-Validate stub for the trainValidateFn
// seam. It is red iff redWhen(present) is true (present = set of member issue numbers in the
// validated batch) and records the sequence of validated member-number sets for assertions.
type recordingValidator struct {
	mu      sync.Mutex
	calls   [][]int
	redWhen func(present map[int]bool) bool
	// diagFor optionally overrides the synthetic default diagnostic returned on a red
	// result, keyed on the validated batch's member-number set — for tests asserting
	// specific diagnostic content (#1420 AC1-AC6). nil means every red result gets the
	// same generic synthetic diagnostic below.
	diagFor func(present map[int]bool) *trainCIDiagnostic
}

func (rv *recordingValidator) fn(_ context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic) {
	present := make(map[int]bool, len(members))
	nums := make([]int, 0, len(members))
	for _, m := range members {
		present[m.item.Number] = true
		nums = append(nums, m.item.Number)
	}
	rv.mu.Lock()
	rv.calls = append(rv.calls, nums)
	rv.mu.Unlock()
	if !rv.redWhen(present) {
		return TrainCIGreen, nil
	}
	if rv.diagFor != nil {
		return TrainCIRed, rv.diagFor(present)
	}
	return TrainCIRed, &trainCIDiagnostic{
		FailedChecks: []gh.CheckRun{{Name: "ci/test", Status: "completed", Conclusion: "failure", OutputText: "synthetic seam failure output"}},
		PRNum:        900,
		TrialSHA:     "seam-trial-sha",
	}
}

func (rv *recordingValidator) count() int {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	return len(rv.calls)
}

func (rv *recordingValidator) last() []int {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if len(rv.calls) == 0 {
		return nil
	}
	return rv.calls[len(rv.calls)-1]
}

// seamTrainEngine wires an Engine with the membership-keyed validation seam installed and a
// mock GitHub client sufficient for landing (integration PR create/merge, singleton landing,
// ejection comments). wm should be a real bare repo (setupTrainRepo) so DefaultBaseBranch
// resolves; the base-SHA pin and all trial git work are skipped under the seam.
func seamTrainEngine(t *testing.T, wm *WorktreeManager, redWhen func(map[int]bool) bool) (*Engine, *mockGitHubClient, *recordingValidator) {
	t.Helper()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 100 + n, HeadSHA: fmt.Sprintf("sha-%d", n), State: "open"}, nil
		},
		listPRsFn:  func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 900, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()
	rv := &recordingValidator{redWhen: redWhen}
	eng.trainValidateFn = rv.fn
	return eng, client, rv
}

// makeSeamBatch builds a batch of n members numbered 1..n with ItemIDs set (for advancement).
func makeSeamBatch(n int) []gh.ProjectItem {
	batch := make([]gh.ProjectItem, 0, n)
	for i := 1; i <= n; i++ {
		it := makeTrainItem(i, fmt.Sprintf("Issue %d", i))
		it.ItemID = fmt.Sprintf("item-%d", i)
		batch = append(batch, it)
	}
	return batch
}

// ejectionCommentCount counts ejection comments posted on a given member issue number.
func ejectionCommentCount(client *mockGitHubClient, issueNumber int) int {
	client.mu.Lock()
	defer client.mu.Unlock()
	n := 0
	for _, c := range client.addCommentCalls {
		if c.issueNumber == issueNumber && strings.Contains(c.body, "ejected") {
			n++
		}
	}
	return n
}

// ejectionCommentBodies returns the bodies of every ejection comment posted on a given
// member issue number, in posting order — for asserting on comment *content* (#1420
// R1-R4), not just that an ejection occurred.
func ejectionCommentBodies(client *mockGitHubClient, issueNumber int) []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	var bodies []string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == issueNumber && strings.Contains(c.body, "ejected") {
			bodies = append(bodies, c.body)
		}
	}
	return bodies
}

// TestMergeTrainBisect_GreenCommonPath is the D-d hard invariant: a green batch costs exactly
// one combined validation, performs zero bisection, and lands.
func TestMergeTrainBisect_GreenCommonPath(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false }) // always green

	batch := makeSeamBatch(3)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	if got := rv.count(); got != 1 {
		t.Errorf("green common path must cost exactly 1 combined validation (zero bisection), got %d", got)
	}
	for i := 1; i <= 3; i++ {
		if c := ejectionCommentCount(client, i); c != 0 {
			t.Errorf("green path must not eject member #%d, got %d ejection comment(s)", i, c)
		}
	}
	client.mu.Lock()
	merges := len(client.mergePRCalls)
	client.mu.Unlock()
	if merges != 1 {
		t.Errorf("expected the integration PR to be merged once (batch landed), got %d", merges)
	}
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after green landing")
	}
}

// TestMergeTrainBisect_SinglePoisoner verifies FR-1/FR-2/FR-3: a single poisoner is isolated
// in O(log N) validations and ejected; the survivor batch is re-formed and re-validated.
func TestMergeTrainBisect_SinglePoisoner(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// Red iff #3 is present. #3 is the poisoner.
	eng, client, rv := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] })

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	// #3 ejected exactly once.
	if c := ejectionCommentCount(client, 3); c != 1 {
		t.Errorf("expected #3 ejected once as the poisoner, got %d ejection comment(s)", c)
	}
	// No other member ejected.
	for _, n := range []int{1, 2, 4, 5} {
		if c := ejectionCommentCount(client, n); c != 0 {
			t.Errorf("expected member #%d not ejected, got %d", n, c)
		}
	}
	// O(log N): total combined validations ≤ per-episode cost cap + 1 re-form validation.
	cap := eng.effectiveBisectCap()
	if got := rv.count(); got > cap+1 {
		t.Errorf("expected O(log N) validations (≤ %d), got %d", cap+1, got)
	}
	// Survivor batch {1,2,4,5} was re-formed and re-validated (the final validation).
	last := rv.last()
	if fmt.Sprint(last) != fmt.Sprint([]int{1, 2, 4, 5}) {
		t.Errorf("expected survivor batch {1,2,4,5} re-validated last, got %v", last)
	}
	// Survivors landed (integration PR merged).
	client.mu.Lock()
	merges := len(client.mergePRCalls)
	client.mu.Unlock()
	if merges != 1 {
		t.Errorf("expected survivor integration PR merged once, got %d", merges)
	}
}

// TestMergeTrainBisect_RepeatedEjectionPauses verifies D-a: a bisection-identified ejection
// increments the SAME shared MaxMergeTrainEjections counter, and hitting the cap pauses the
// member with fabrik:paused + fabrik:awaiting-input.
func TestMergeTrainBisect_RepeatedEjectionPauses(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] }) // #3 poisons

	// Pre-seed #3's ejection counter to one below the cap (proves the shared counter).
	eng.mergeTrainEjectionsMu.Lock()
	eng.mergeTrainEjectionCounts["owner/repo#3"] = eng.cfg.MaxMergeTrainEjections - 1
	eng.mergeTrainEjectionsMu.Unlock()

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	client.mu.Lock()
	paused, awaiting := false, false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 3 && c.labelName == "fabrik:paused" {
			paused = true
		}
		if c.issueNumber == 3 && c.labelName == "fabrik:awaiting-input" {
			awaiting = true
		}
	}
	client.mu.Unlock()
	if !paused {
		t.Error("expected #3 to be paused (fabrik:paused) at the shared eject cap")
	}
	if !awaiting {
		t.Error("expected #3 to get fabrik:awaiting-input at the shared eject cap")
	}
}

// ── #1420 acceptance tests: ejection comment carries the combined-Validate diagnostic ──

// TestMergeTrainBisect_FirstEjectionCommentCarriesDiagnostic covers #1420 AC1/AC3: the
// FIRST ejection comment for a bisection-isolated poisoner — not only a later or pause
// comment — must contain the failing check name and its output. A test that only asserts
// ejection occurred (the pre-#1420 state) would pass vacuously; this asserts on comment
// body text instead.
func TestMergeTrainBisect_FirstEjectionCommentCarriesDiagnostic(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] }) // #3 poisons

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	comments := ejectionCommentBodies(client, 3)
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 ejection comment for #3, got %d", len(comments))
	}
	first := comments[0]
	if !strings.Contains(first, "ci/test") {
		t.Errorf("first ejection comment must name the failing check, got: %s", first)
	}
	if !strings.Contains(first, "synthetic seam failure output") {
		t.Errorf("first ejection comment must include the failure output, got: %s", first)
	}
}

// TestMergeTrainBisect_EjectionCarriesInnermostRunDiagnostic covers the threading property
// itself, not just that some diagnostic content arrives (Review feedback on PR #1426):
// bisect's recursive call must forward halfDiag — the diagnostic of the half just found
// red — not the diag it was entered with. Neutralizing that (recursing with the incoming
// diag instead of halfDiag) leaves every existing diagnostic-content test passing, because
// none of them distinguish diagnostics by recursion level; this test does, by keying the
// seam's returned diagnostic on the exact membership set validated at each level. #3 poisons
// a 5-member batch, forcing two full halving levels before isolation:
//
//	[1,2,3,4,5] (outer, red)  →  [3,4,5] (middle, red)  →  [3] (innermost, red, base case)
//
// The ejected member must carry only the innermost level's diagnostic — the run that
// actually isolated it — never the middle or outer level's, which recursion passed through
// but did not originate from.
func TestMergeTrainBisect_EjectionCarriesInnermostRunDiagnostic(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, rv := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] }) // #3 poisons

	sameSet := func(present map[int]bool, want ...int) bool {
		if len(present) != len(want) {
			return false
		}
		for _, w := range want {
			if !present[w] {
				return false
			}
		}
		return true
	}
	rv.diagFor = func(present map[int]bool) *trainCIDiagnostic {
		switch {
		case sameSet(present, 1, 2, 3, 4, 5):
			return &trainCIDiagnostic{
				FailedChecks: []gh.CheckRun{{Name: "ci/level-outer", Status: "completed", Conclusion: "failure", OutputText: "outer batch output"}},
				PRNum:        900, TrialSHA: "sha-outer",
			}
		case sameSet(present, 3, 4, 5):
			return &trainCIDiagnostic{
				FailedChecks: []gh.CheckRun{{Name: "ci/level-middle", Status: "completed", Conclusion: "failure", OutputText: "middle batch output"}},
				PRNum:        900, TrialSHA: "sha-middle",
			}
		case sameSet(present, 3):
			return &trainCIDiagnostic{
				FailedChecks: []gh.CheckRun{{Name: "ci/level-innermost", Status: "completed", Conclusion: "failure", OutputText: "innermost isolating output"}},
				PRNum:        900, TrialSHA: "sha-innermost",
			}
		default:
			t.Fatalf("unexpected membership validated red: %v", present)
			return nil
		}
	}

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	comments := ejectionCommentBodies(client, 3)
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 ejection comment for #3, got %d", len(comments))
	}
	body := comments[0]
	if !strings.Contains(body, "ci/level-innermost") || !strings.Contains(body, "innermost isolating output") {
		t.Errorf("expected the ejection comment to carry the innermost (isolating) run's diagnostic, got: %s", body)
	}
	if strings.Contains(body, "ci/level-outer") || strings.Contains(body, "outer batch output") {
		t.Errorf("ejection comment must not carry the outer (initial full-batch) run's diagnostic, got: %s", body)
	}
	if strings.Contains(body, "ci/level-middle") || strings.Contains(body, "middle batch output") {
		t.Errorf("ejection comment must not carry the middle-level run's diagnostic, got: %s", body)
	}
}

// TestMergeTrainBisect_EjectionCommentNamesOtherBatchMembers covers #1420 R4/AC5: the
// ejection comment must name the other members the isolated poisoner's batch was
// combined against, so an operator knows the failure is combination-only before
// investigating their own branch.
func TestMergeTrainBisect_EjectionCommentNamesOtherBatchMembers(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] }) // #3 poisons

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	comments := ejectionCommentBodies(client, 3)
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 ejection comment for #3, got %d", len(comments))
	}
	body := comments[0]
	for _, other := range []string{"#1", "#2", "#4", "#5"} {
		if !strings.Contains(body, other) {
			t.Errorf("expected ejection comment to name batch member %s, got: %s", other, body)
		}
	}
	if strings.Contains(body, "No other members were present") {
		t.Errorf("expected a populated batch-context sentence (not the singleton fallback), got: %s", body)
	}
}

// TestMergeTrainBisect_SingleMemberTrain_NoOtherMembers covers #1440 R1/R2/R3/AC1/AC2/AC4:
// a red batch of exactly one member has no poisoner to isolate and must never reach
// handleRedBatch's bisection machinery (AC1) — the trainRedBatchHook seam proves this
// structurally, not just via trial count (bisect's own len==1 base case already costs zero
// extra validations even on unmodified main, so a trial-count-only assertion would be
// vacuous per AC6). The disposition must read as "this PR's own validation failed," never
// promise a retry "in a future train with a different composition," and never attribute the
// failure to a conflict (AC2) — and it must pause immediately, on this first occurrence,
// without touching the shared 3-strike mergeTrainEjectionCounts counter (R3/AC4).
func TestMergeTrainBisect_SingleMemberTrain_NoOtherMembers(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return true }) // always red

	var redBatchHookCalls int
	eng.trainRedBatchHook = func() { redBatchHookCalls++ }

	batch := makeSeamBatch(1)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	// AC1: handleRedBatch (multi-member bisection machinery) must never be reached.
	if redBatchHookCalls != 0 {
		t.Errorf("expected handleRedBatch to be skipped for a red singleton, but it was called %d time(s)", redBatchHookCalls)
	}
	// AC1: no bisection sub-trial beyond the initial validation.
	if got := rv.count(); got != 1 {
		t.Errorf("expected exactly 1 validation (no bisection sub-trials) for a red singleton, got %d", got)
	}

	client.mu.Lock()
	var bodies []string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 1 {
			bodies = append(bodies, c.body)
		}
	}
	client.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly 1 comment posted for #1, got %d: %v", len(bodies), bodies)
	}
	body := bodies[0]

	// AC2: never the bisection-ejection/conflict framing.
	if strings.Contains(body, "different composition") {
		t.Errorf("red-singleton comment must not promise retry in a different composition, got: %s", body)
	}
	if strings.Contains(body, "conflict") {
		t.Errorf("red-singleton comment must not attribute the failure to a conflict, got: %s", body)
	}
	if !strings.Contains(body, "own combined Validate is failing") {
		t.Errorf("expected the comment to state the PR's own validation failed, got: %s", body)
	}
	if !strings.Contains(body, "No other members were present") {
		t.Errorf("expected the single-member-train sentence, got: %s", body)
	}

	// R3/AC4: the shared ejection counter is untouched for a red singleton...
	if count := eng.mergeTrainEjectionCounts["owner/repo#1"]; count != 0 {
		t.Errorf("expected mergeTrainEjectionCounts to be untouched for a red singleton, got %d", count)
	}
	// ...but the member is paused on this first (and only) disposition.
	var sawPaused, sawAwaitingInput bool
	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.issueNumber != 1 {
			continue
		}
		switch c.labelName {
		case "fabrik:paused":
			sawPaused = true
		case "fabrik:awaiting-input":
			sawAwaitingInput = true
		}
	}
	client.mu.Unlock()
	if !sawPaused || !sawAwaitingInput {
		t.Errorf("expected fabrik:paused and fabrik:awaiting-input applied on the first red-singleton disposition, got labels: %v", client.addLabelCalls)
	}
}

// TestMergeTrainRedSingleton_NoRetrialOnNextPoll covers #1440 R4/AC5 together with #1545
// R1: escalating a red singleton to fabrik:paused on its first disposition must reroute it
// off the Queued holding column (#1545) onto stageBeforeHolding ("Implement" in
// trainTestEngine's stage set), which by construction excludes it from every future Queued
// batch snapshot — an unchanged red singleton never re-forms into an identical trial on the
// next poll. Before #1545 this exclusion depended entirely on groupQueuedByRepo's
// fabrik:paused poison-well guard while the member sat, unreachable, in Queued; this test
// now asserts the reroute itself (the board Status actually moves), and separately confirms
// the post-reroute Status is what drives the next-poll exclusion, not merely the label.
func TestMergeTrainRedSingleton_NoRetrialOnNextPoll(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return true }) // always red

	batch := makeSeamBatch(1)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	if got := rv.count(); got != 1 {
		t.Fatalf("expected exactly 1 validation trial after the first episode, got %d", got)
	}

	// #1545 R1: the member must be rerouted off Queued to stageBeforeHolding ("Implement"
	// in this stage set) before being paused.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call rerouting #1 off Queued, got %d: %+v", len(client.updateStatusCalls), client.updateStatusCalls)
	}
	if got := client.updateStatusCalls[0].optionID; got != "opt-implement" {
		t.Errorf("expected reroute target option opt-implement (Implement), got %q", got)
	}

	var pausedLabel bool
	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 1 && c.labelName == "fabrik:paused" {
			pausedLabel = true
		}
	}
	client.mu.Unlock()
	if !pausedLabel {
		t.Fatalf("expected fabrik:paused applied to #1 after the red-singleton disposition")
	}

	// Simulate the next poll's Queued snapshot: #1 has left Queued for Implement (the
	// board Status move rerouteQueuedMemberOffHolding just performed) and also carries the
	// fabrik:paused label ejectRedSingleton applied. The exclusion below follows from the
	// Status no longer being "Queued" — not merely from the label, unlike before #1545,
	// when the item never left Queued at all.
	items := []gh.ProjectItem{
		{Number: 1, Status: "Implement", Repo: "owner/repo", Labels: []string{"fabrik:paused"}},
	}
	groups := groupQueuedByRepo(items, "Queued", "owner/repo")
	if len(groups) != 0 {
		t.Errorf("expected the rerouted red singleton to be excluded from the next poll's train batch, got %d group(s): %+v", len(groups), groups)
	}

	// Nothing was (re-)dispatched for a second episode, so no additional trial occurred.
	if got := rv.count(); got != 1 {
		t.Errorf("expected no additional validation trial on the next poll, got %d", got)
	}
}

// TestEjectRedSingleton_RerouteFailure_NoCommentNoPause covers #1545 R2/AC2: mirrors
// TestEjectQueuedMemberForReviewFindings_RerouteFailure_NoCommentNoCount for the
// standalone-validation-failure cause — when the reroute off Queued fails, nothing is
// posted and the member is not paused. A failed reroute must look like nothing happened,
// so the very next poll's train re-forms the same singleton and retries the whole
// disposition from scratch, rather than half-applying the pause without the reroute that
// makes it reachable.
func TestEjectRedSingleton_RerouteFailure_NoCommentNoPause(t *testing.T) {
	client := &mockGitHubClient{updateProjectItemStatusFn: func(string, string, string, string) error { return fmt.Errorf("boom") }}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	m := trainMember{item: makeTrainItem(1, "Issue 1")}
	eng.ejectRedSingleton("PVT_1", "owner", "repo", m, nil)

	client.mu.Lock()
	comments := len(client.addCommentCalls)
	labels := append([]addLabelCall(nil), client.addLabelCalls...)
	client.mu.Unlock()

	if comments != 0 {
		t.Errorf("expected no red-singleton comment when reroute fails, got %d", comments)
	}
	for _, c := range labels {
		if c.issueNumber == 1 && (c.labelName == "fabrik:paused" || c.labelName == "fabrik:awaiting-input") {
			t.Errorf("expected no pause labels applied when reroute fails, got %+v", c)
		}
	}
}

// TestEjectRedSingleton_Success covers #1545 R1/R3/R4/AC1/AC4: on a successful reroute,
// the member's board Status moves off Queued to stageBeforeHolding ("Implement" in
// trainTestEngine's stage set), the posted comment names that target stage, and the
// member is paused there.
//
// Pruefer review finding (PR #1550): handleRevalidateLabel (engine/item.go) is hardcoded
// to the literal stage name "Validate" — it does not generalize over stageBeforeHolding's
// (Order-derived) result the way that resolution function itself does. trainTestEngine's
// stage set has no stage named "Validate" at all (Research/Plan/Implement/Queued/Done),
// so this test exercises exactly the case where naming the fabrik:revalidate mechanism
// would be wrong: it would silently no-op against stage:Implement:complete. The comment
// must therefore name the real blocking labels instead of fabrik:revalidate here — the
// Validate-target case (where fabrik:revalidate is correct) is covered separately by
// TestEjectRedSingleton_Success_ValidateTarget.
func TestEjectRedSingleton_Success(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	m := trainMember{item: makeTrainItem(1, "Issue 1")}
	eng.ejectRedSingleton("PVT_1", "owner", "repo", m, nil)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call rerouting #1 off Queued, got %d", len(client.updateStatusCalls))
	}
	if got := client.updateStatusCalls[0].optionID; got != "opt-implement" {
		t.Errorf("expected reroute target option opt-implement (Implement), got %q", got)
	}

	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 red-singleton comment, got %d", len(calls))
	}
	body := calls[0].body
	if !strings.Contains(body, "has left the Queued column for Implement") {
		t.Errorf("expected the comment to name the reroute target stage, got: %s", body)
	}
	if strings.Contains(body, "apply `fabrik:revalidate`") {
		t.Errorf("expected the comment NOT to recommend applying fabrik:revalidate — it only forces re-entry of a stage literally named Validate, and this target is Implement — got: %s", body)
	}
	if !strings.Contains(body, "will not help here") {
		t.Errorf("expected the comment to explain fabrik:revalidate would not help for a non-Validate target, got: %s", body)
	}
	if !strings.Contains(body, "`stage:Implement:complete`") {
		t.Errorf("expected the comment to name the real blocking completion label stage:Implement:complete, got: %s", body)
	}
	if strings.Contains(body, "then remove `fabrik:paused` to re-enter the train") {
		t.Errorf("expected the stale bare-unpause instruction to be gone, got: %s", body)
	}

	var sawPaused, sawAwaitingInput bool
	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.issueNumber != 1 {
			continue
		}
		switch c.labelName {
		case "fabrik:paused":
			sawPaused = true
		case "fabrik:awaiting-input":
			sawAwaitingInput = true
		}
	}
	client.mu.Unlock()
	if !sawPaused || !sawAwaitingInput {
		t.Errorf("expected fabrik:paused and fabrik:awaiting-input applied after the reroute, got labels: %v", client.addLabelCalls)
	}
}

// TestEjectRedSingleton_Success_ValidateTarget covers the counterpart to the Pruefer
// finding above: when stageBeforeHolding genuinely resolves to a stage literally named
// "Validate" (the shape every production .fabrik/stages/*.yaml actually has), the
// recovery instruction must point at fabrik:revalidate — the one case where
// handleRevalidateLabel's hardcoded "Validate" match actually applies and the mechanism
// genuinely works.
func TestEjectRedSingleton_Success_ValidateTarget(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner:                  "owner",
			Repo:                   "repo",
			ProjectNum:             1,
			User:                   "testuser",
			Token:                  "token",
			MaxConcurrent:          5,
			MaxMergeTrainEjections: 3,
			MergeTrain:             "on",
			CIBackstopTimeout:      100 * time.Millisecond,
			Stages: []*stages.Stage{
				{Name: "Research", Order: 1, Prompt: "Do research"},
				{Name: "Plan", Order: 2, Prompt: "Make a plan"},
				{Name: "Validate", Order: 3, Prompt: "Validate it"},
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
				{Name: "Done", Order: 99, Prompt: "Cleanup"},
			},
		},
		client,
		claude,
		NewWorktreeManager(t.TempDir()),
	)
	eng.statusField = &gh.StatusField{
		FieldID: "sf-test-1",
		Options: map[string]string{
			"Done":     "opt-done",
			"Queued":   "opt-queued",
			"Validate": "opt-validate",
		},
	}

	m := trainMember{item: makeTrainItem(1, "Issue 1")}
	eng.ejectRedSingleton("PVT_1", "owner", "repo", m, nil)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call rerouting #1 off Queued, got %d", len(client.updateStatusCalls))
	}
	if got := client.updateStatusCalls[0].optionID; got != "opt-validate" {
		t.Errorf("expected reroute target option opt-validate (Validate), got %q", got)
	}

	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 red-singleton comment, got %d", len(calls))
	}
	body := calls[0].body
	if !strings.Contains(body, "has left the Queued column for Validate") {
		t.Errorf("expected the comment to name the reroute target stage, got: %s", body)
	}
	if !strings.Contains(body, "apply `fabrik:revalidate`") {
		t.Errorf("expected the comment to recommend applying fabrik:revalidate when the target is literally Validate, got: %s", body)
	}
}

// TestEjectMember_TruncatesOversizedDiagnostic covers #1420 R3/AC4: an output exceeding
// the inline budget must produce a truncated body plus a run/job link, and the whole
// comment must stay within GitHub's ~65536-char comment limit.
func TestEjectMember_TruncatesOversizedDiagnostic(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member := makeTrainItem(7, "Big Output Issue")
	hugeOutput := strings.Repeat("x", 50000)
	diag := &trainCIDiagnostic{
		FailedChecks: []gh.CheckRun{
			{Name: "ci/huge", Status: "completed", Conclusion: "failure", OutputText: hugeOutput, HTMLURL: "https://github.com/owner/repo/runs/123"},
		},
		PRNum:    900,
		TrialSHA: "deadbeef",
	}
	eng.ejectMember("owner", "repo", member, "ejected from merge-train — combined Validate red", diag, nil, true)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an ejection comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if len(body) > 65536 {
		t.Errorf("ejection comment body exceeds GitHub's comment size limit: %d chars", len(body))
	}
	if !strings.Contains(body, "chars omitted") {
		t.Errorf("expected a truncated-output marker, got a body of length %d", len(body))
	}
	if !strings.Contains(body, "https://github.com/owner/repo/runs/123") {
		t.Errorf("expected a details/run link in the truncated comment, got: %s", body)
	}
}

// TestTruncateBlockHard_ClosesDanglingCodeFence covers a gap in R3's truncation policy:
// renderDiagnosticBlock's whole-block hard cap (trainDiagBlockMax) can land its cut
// partway through one of renderFailedChecks' per-check ``` fences when enough failing
// checks are inlined — an odd number of ``` markers past the cut would render every
// following section (the batch-context sentence, the "remains in Queued" boilerplate)
// as part of an open code block instead of prose. truncateBlockHard must always leave a
// balanced (even) fence count.
func TestTruncateBlockHard_ClosesDanglingCodeFence(t *testing.T) {
	// A block whose hard-cap cut point (at byte 20) lands inside an open fence.
	block := "0123456789" + "```\nsome code that runs past the cut point\n```" + " trailing prose"
	got := truncateBlockHard(block, 20)
	if strings.Count(got, "```")%2 != 0 {
		t.Fatalf("expected a balanced (even) number of ``` fence markers, got %d in: %q", strings.Count(got, "```"), got)
	}
	if !strings.HasSuffix(got, "\n```") {
		t.Errorf("expected the dangling fence to be closed with a trailing \\n```, got: %q", got)
	}
}

// TestTruncateBlockHard_NoSplitOnEvenFenceCount covers the already-balanced case: a cut
// point that lands after a complete, closed fence must not add a spurious extra fence.
func TestTruncateBlockHard_NoSplitOnEvenFenceCount(t *testing.T) {
	block := "```\ncode\n```" + strings.Repeat("z", 100)
	got := truncateBlockHard(block, 12) // cuts exactly after the closing fence
	if got != "```\ncode\n```" {
		t.Errorf("expected no extra fence appended for an already-balanced cut, got: %q", got)
	}
}

// TestTruncateBlockHard_DoesNotSplitMultibyteRune covers UTF-8 safety: a byte-index cut
// must never land in the middle of a multi-byte rune, which would produce invalid UTF-8
// in the posted comment.
func TestTruncateBlockHard_DoesNotSplitMultibyteRune(t *testing.T) {
	block := strings.Repeat("a", 9) + "é" + strings.Repeat("b", 20) // 'é' is 2 bytes, straddling index 10
	got := truncateBlockHard(block, 10)
	if !utf8.ValidString(got) {
		t.Errorf("truncateBlockHard produced invalid UTF-8: %q (bytes: %v)", got, []byte(got))
	}
}

// TestTruncateMiddle_DoesNotSplitMultibyteRune covers the same UTF-8 safety concern for
// the per-check truncation helper: CI output text is arbitrary third-party content and
// may contain non-ASCII, so both the head and tail cut points must land on rune
// boundaries rather than splitting a multi-byte rune into invalid UTF-8.
func TestTruncateMiddle_DoesNotSplitMultibyteRune(t *testing.T) {
	// 'é' (2 bytes) straddles the head cut at byte 9; '日' (3 bytes) straddles the tail
	// cut near the end.
	s := strings.Repeat("a", 9) + "é" + strings.Repeat("x", 50) + "日" + strings.Repeat("b", 9)
	got := truncateMiddle(s, len(s)-1, 10, 11)
	if !utf8.ValidString(got) {
		t.Errorf("truncateMiddle produced invalid UTF-8: %q (bytes: %v)", got, []byte(got))
	}
}

// TestEjectMember_BlockHardCapNeverLeavesDanglingCodeFence is an end-to-end check that
// enough large failing checks to exceed trainDiagBlockMax still produce a
// balanced-fence, well-formed comment via the real ejectMember path.
func TestEjectMember_BlockHardCapNeverLeavesDanglingCodeFence(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member := makeTrainItem(8, "Many Failing Checks Issue")
	var checks []gh.CheckRun
	for i := 0; i < 5; i++ {
		checks = append(checks, gh.CheckRun{
			// Long name + long URL, on top of 5 near-max-inline-cap outputs, is what
			// pushes the assembled block past trainDiagBlockMax in practice.
			Name:       fmt.Sprintf("ci/check-%d-%s", i, strings.Repeat("x", 60)),
			Status:     "completed",
			Conclusion: "failure",
			OutputText: strings.Repeat(fmt.Sprintf("line %d ", i), 1000), // well over the per-check inline cap
			HTMLURL:    "https://github.com/owner/repo/actions/runs/" + strings.Repeat("9", 80),
		})
	}
	diag := &trainCIDiagnostic{FailedChecks: checks, PRNum: 901, TrialSHA: "cafef00d"}
	eng.ejectMember("owner", "repo", member, "ejected from merge-train — combined Validate red", diag, nil, true)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an ejection comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "(truncated)") {
		t.Fatalf("expected the block-level hard cap to have engaged, got a body of length %d", len(body))
	}
	if strings.Count(body, "```")%2 != 0 {
		t.Errorf("expected a balanced (even) number of ``` fence markers, got %d — the block-level truncation left a code fence open:\n%s", strings.Count(body, "```"), body)
	}
}

// TestMergeTrainBisect_PauseCommentNamesCause covers #1420 R5/AC6: the pause-after-N
// comment must name or link the cause, not just instruct the operator to "resolve the
// underlying conflict" with nothing to go on.
func TestMergeTrainBisect_PauseCommentNamesCause(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] }) // #3 poisons

	// Pre-seed #3's ejection counter to one below the cap so this run triggers the pause.
	eng.mergeTrainEjectionsMu.Lock()
	eng.mergeTrainEjectionCounts["owner/repo#3"] = eng.cfg.MaxMergeTrainEjections - 1
	eng.mergeTrainEjectionsMu.Unlock()

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	client.mu.Lock()
	var pauseBody string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 3 && strings.Contains(c.body, "pausing after") {
			pauseBody = c.body
		}
	}
	client.mu.Unlock()
	if pauseBody == "" {
		t.Fatal("expected a pause comment to be posted for #3")
	}
	if !strings.Contains(pauseBody, "ci/test") {
		t.Errorf("expected pause comment to name the failing check, got: %s", pauseBody)
	}
	if !strings.Contains(pauseBody, "issuecomment-") {
		t.Errorf("expected pause comment to link the ejection comment, got: %s", pauseBody)
	}
}

// TestMergeTrainBisect_CostCapFallbackLogs verifies FR-5: exceeding the per-red-batch
// validation cap degrades to one-at-a-time landing with a clear log line (no silent
// truncation). Uses an interaction (red iff {#1,#2}) and a low MaxBisectValidations so the
// cap fires before isolation.
func TestMergeTrainBisect_CostCapFallbackLogs(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[1] && p[2] })
	eng.cfg.MaxBisectValidations = 2 // force the cost cap to fire during bisection

	batch := makeSeamBatch(4)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := captureStdout(func() {
		eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)
	})

	if !strings.Contains(out, "cost cap") {
		t.Errorf("expected a cost-cap log line (no silent truncation); stdout was:\n%s", out)
	}
	if !strings.Contains(out, "one-at-a-time") {
		t.Errorf("expected a one-at-a-time fallback log line; stdout was:\n%s", out)
	}
	// The fallback lands each of the 4 members as its own singleton (marker-free CreatePR).
	client.mu.Lock()
	singletonPRs := 0
	for _, c := range client.createPRCalls {
		if strings.Contains(c.title, "singleton") {
			singletonPRs++
		}
	}
	client.mu.Unlock()
	if singletonPRs != 4 {
		t.Errorf("expected 4 singleton landing PRs under the fallback, got %d", singletonPRs)
	}
}

// TestMergeTrainOneAtATime_RedSingletonUsesSameDisposition covers #1440's extension of the
// red-singleton fix to landOneAtATime's own TrainCIRed branch: a member validated completely
// alone ([]trainMember{m}) after bisection degrades to the one-at-a-time fallback is
// structurally the same true-singleton scenario the top-level arity guard targets — it must
// get the identical ejectRedSingleton disposition (no "different composition" promise, no
// shared-counter churn, immediate pause), not the pre-#1440 ejectMember wording that would be
// equally misleading here. Forces the cost cap to fire before bisection can isolate #3 as an
// ordinary poisoner, so #3's red-singleton disposition is only reachable via landOneAtATime.
func TestMergeTrainOneAtATime_RedSingletonUsesSameDisposition(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// Only #3 is ever red, alone or combined with anyone else.
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] })
	eng.cfg.MaxBisectValidations = 2 // force the cost cap before bisection isolates #3 itself

	batch := makeSeamBatch(4)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := captureStdout(func() {
		eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)
	})

	if !strings.Contains(out, "one-at-a-time") {
		t.Fatalf("expected the cost cap to force a one-at-a-time fallback; stdout was:\n%s", out)
	}

	client.mu.Lock()
	var bodies []string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 3 {
			bodies = append(bodies, c.body)
		}
	}
	client.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly 1 comment posted for #3, got %d: %v", len(bodies), bodies)
	}
	body := bodies[0]

	if strings.Contains(body, "different composition") {
		t.Errorf("red-singleton comment reached via one-at-a-time must not promise retry in a different composition, got: %s", body)
	}
	if strings.Contains(body, "conflict") {
		t.Errorf("red-singleton comment reached via one-at-a-time must not attribute the failure to a conflict, got: %s", body)
	}
	if !strings.Contains(body, "own combined Validate is failing") {
		t.Errorf("expected the comment to state the PR's own validation failed, got: %s", body)
	}

	if count := eng.mergeTrainEjectionCounts["owner/repo#3"]; count != 0 {
		t.Errorf("expected mergeTrainEjectionCounts to be untouched for a red singleton reached via one-at-a-time, got %d", count)
	}

	var sawPaused bool
	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 3 && c.labelName == "fabrik:paused" {
			sawPaused = true
		}
	}
	client.mu.Unlock()
	if !sawPaused {
		t.Errorf("expected fabrik:paused applied to #3 on its first (and only) red-singleton disposition, got labels: %v", client.addLabelCalls)
	}
}

// TestMergeTrainBisect_InteractionFallsBack verifies the interaction case (D-e): a non-
// isolable cross-PR interaction (each half green alone, the union red) with ample budget
// triggers the one-at-a-time fallback rather than falsely isolating/ejecting a single member.
func TestMergeTrainBisect_InteractionFallsBack(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// Red iff BOTH #1 and #2 are present; either alone is green. Ample (default) budget.
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[1] && p[2] })

	batch := makeSeamBatch(4)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := captureStdout(func() {
		eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)
	})

	// Fell back to one-at-a-time (not a bespoke isolation path).
	if !strings.Contains(out, "one-at-a-time") {
		t.Errorf("expected the interaction to degrade to one-at-a-time; stdout was:\n%s", out)
	}
	// No member was ejected as "the batch poisoner" (bisection must not falsely isolate).
	if strings.Contains(out, "batch poisoner") {
		t.Errorf("interaction must not falsely isolate a single poisoner; stdout was:\n%s", out)
	}
	// Each member is landed as its own singleton batch.
	client.mu.Lock()
	singletonPRs := 0
	for _, c := range client.createPRCalls {
		if strings.Contains(c.title, "singleton") {
			singletonPRs++
		}
	}
	client.mu.Unlock()
	if singletonPRs != 4 {
		t.Errorf("expected 4 singleton landing PRs under the fallback, got %d", singletonPRs)
	}
}

// TestLandOneAtATime_PendingReviewEject_SkipsLandingAndEjectsInstead verifies Hook 2
// (#1208) inside the one-at-a-time fallback: a pending review-finding eject flagged
// for a member at the moment its own singleton trial validates must be honored
// instead of that singleton's normal green/red outcome — the member is rerouted off
// Queued and ejected with the distinct #1208 wording, not landed via landSingleton
// (even though its singleton trial is green) and not ejected via the ordinary
// fails-even-in-isolation red path.
func TestLandOneAtATime_PendingReviewEject_SkipsLandingAndEjectsInstead(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })

	// Red iff both #1 and #2 are present (forces bisection -> non-isolable
	// interaction -> the landOneAtATime fallback with the full original 2-member
	// set). Marks #2's pending-eject signal at the exact moment its own singleton
	// trial validates — simulating settleQueuedReviewFindings having flagged it
	// mid-CI-wait, just before landOneAtATime's own checkpoint would otherwise
	// decide its fate via the normal green/red switch.
	eng.trainValidateFn = func(_ context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic) {
		present := make(map[int]bool, len(members))
		for _, m := range members {
			present[m.item.Number] = true
		}
		if len(members) == 1 && members[0].item.Number == 2 {
			eng.markPendingReviewEject("owner/repo", 2, 5)
		}
		if present[1] && present[2] {
			return TrainCIRed, &trainCIDiagnostic{Note: "interaction", PRNum: 900, TrialSHA: "seam-sha"}
		}
		return TrainCIGreen, nil
	}

	batch := makeSeamBatch(2)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_1"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := captureStdout(func() {
		eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)
	})

	if !strings.Contains(out, "one-at-a-time") {
		t.Fatalf("expected the interaction to degrade to one-at-a-time; stdout was:\n%s", out)
	}

	client.mu.Lock()
	var singletonTitles []string
	for _, c := range client.createPRCalls {
		if strings.Contains(c.title, "singleton") {
			singletonTitles = append(singletonTitles, c.title)
		}
	}
	client.mu.Unlock()
	if len(singletonTitles) != 1 {
		t.Fatalf("expected exactly 1 singleton landing PR (#1 only — #2 must be ejected, not landed), got %d: %v", len(singletonTitles), singletonTitles)
	}
	if strings.Contains(singletonTitles[0], "#2") {
		t.Errorf("expected #2 NOT to be landed as a singleton, got title: %s", singletonTitles[0])
	}

	foundReroute := false
	for _, c := range client.updateStatusCalls {
		if c.optionID == "opt-implement" {
			foundReroute = true
		}
	}
	if !foundReroute {
		t.Errorf("expected #2 to be rerouted to Implement, got status updates: %+v", client.updateStatusCalls)
	}

	client.mu.Lock()
	var ejectionComment string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 2 {
			ejectionComment = c.body
		}
	}
	client.mu.Unlock()
	if !strings.Contains(ejectionComment, "has left the Queued column") {
		t.Errorf("expected #2's ejection comment to use the leaves-Queued wording, got: %q", ejectionComment)
	}

	if _, ok := eng.takePendingReviewEject("owner/repo", 2); ok {
		t.Error("expected the pending-eject signal for #2 to already be consumed")
	}
}

// ── D5: main-moved rebase/revalidate + durable in-flight reconstruction ───────

// TestEffectiveMaxTrainRebaseCycles verifies the default (3) and explicit override
// of the per-batch main-moved rebase-cycle bound (ADR-059 D5, FR-2).
func TestEffectiveMaxTrainRebaseCycles(t *testing.T) {
	if got := (&Engine{cfg: Config{MaxTrainRebaseCycles: 0}}).effectiveMaxTrainRebaseCycles(); got != 3 {
		t.Errorf("effectiveMaxTrainRebaseCycles() with unset (0) = %d, want 3 (default)", got)
	}
	if got := (&Engine{cfg: Config{MaxTrainRebaseCycles: 5}}).effectiveMaxTrainRebaseCycles(); got != 5 {
		t.Errorf("effectiveMaxTrainRebaseCycles() with 5 = %d, want 5", got)
	}
	if got := (&Engine{cfg: Config{MaxTrainRebaseCycles: -2}}).effectiveMaxTrainRebaseCycles(); got != 3 {
		t.Errorf("effectiveMaxTrainRebaseCycles() with negative = %d, want 3 (default)", got)
	}
}

// TestIsTrainPR verifies a PR is recognised as a merge-train PR by either the batch
// marker in its body (landing integration PR) or the fabrik/merge-train/ head-branch
// prefix (draft CI PR, which carries no marker) — FR-1/FR-4.
func TestIsTrainPR(t *testing.T) {
	cases := []struct {
		name string
		pr   gh.PRDetails
		want bool
	}{
		{"marker in body", gh.PRDetails{Body: "before " + mergeTrainBatchMarker + " after"}, true},
		{"train head branch", gh.PRDetails{HeadRefName: "fabrik/merge-train/merge-train-main-1"}, true},
		{"both", gh.PRDetails{Body: mergeTrainBatchMarker, HeadRefName: "fabrik/merge-train/x"}, true},
		{"neither", gh.PRDetails{Body: "just a normal PR", HeadRefName: "fabrik/issue-42"}, false},
		{"empty", gh.PRDetails{}, false},
	}
	for _, tc := range cases {
		if got := isTrainPR(tc.pr); got != tc.want {
			t.Errorf("isTrainPR(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTrialNameFromBranch verifies stripping the fabrik/merge-train/ prefix from a
// head ref, and the empty return for non-train branches.
func TestTrialNameFromBranch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fabrik/merge-train/merge-train-main-123", "merge-train-main-123"},
		{"fabrik/merge-train/", ""},
		{"fabrik/issue-1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := trialNameFromBranch(tc.in); got != tc.want {
			t.Errorf("trialNameFromBranch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseTrainMembers verifies extraction of distinct #N member references from a
// train PR body, preserving first-seen order and de-duplicating.
func TestParseTrainMembers(t *testing.T) {
	body := "batch: #7, #3, #7 and again #12 (see #3)"
	got := parseTrainMembers(body)
	want := []int{7, 3, 12}
	if len(got) != len(want) {
		t.Fatalf("parseTrainMembers len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseTrainMembers[%d] = %d, want %d (order/dedupe)", i, got[i], want[i])
		}
	}
	if n := parseTrainMembers("no references here"); len(n) != 0 {
		t.Errorf("parseTrainMembers(no refs) = %v, want empty", n)
	}
}

// TestFilterBatchByNumbers verifies the batch subset is intersected by issue number
// and keeps entry order.
func TestFilterBatchByNumbers(t *testing.T) {
	batch := []gh.ProjectItem{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}}
	got := filterBatchByNumbers(batch, []int{3, 1})
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 3 {
		t.Errorf("filterBatchByNumbers = %v, want [#1 #3] in entry order", got)
	}
	if n := filterBatchByNumbers(batch, []int{99}); len(n) != 0 {
		t.Errorf("filterBatchByNumbers(no match) = %v, want empty", n)
	}
}

// TestContainsBranch is a small guard for the reconstruction branch-presence check.
func TestContainsBranch(t *testing.T) {
	s := []string{"fabrik/merge-train/a", "fabrik/merge-train/b"}
	if !containsBranch(s, "fabrik/merge-train/b") {
		t.Error("containsBranch should find present branch")
	}
	if containsBranch(s, "fabrik/merge-train/c") {
		t.Error("containsBranch should not find absent branch")
	}
}

// TestTrialBehind verifies the behind signal is read from FetchCommitsBehind: >0 means
// behind (main moved), 0 means up to date, and an error is treated as up to date
// (fail-safe: never block landing on a probe failure) — FR-2.
func TestTrialBehind(t *testing.T) {
	mk := func(fn func(owner, repo, base, head string) (int, error)) *Engine {
		return trainTestEngine(t, &mockGitHubClient{fetchCommitsBehindFn: fn}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	}
	if e := mk(func(_, _, _, _ string) (int, error) { return 2, nil }); !e.trialBehind("o", "r", "main", "fabrik/merge-train/x") {
		t.Error("trialBehind should be true when behind_by > 0")
	}
	if e := mk(func(_, _, _, _ string) (int, error) { return 0, nil }); e.trialBehind("o", "r", "main", "fabrik/merge-train/x") {
		t.Error("trialBehind should be false when behind_by == 0")
	}
	if e := mk(func(_, _, _, _ string) (int, error) { return 0, fmt.Errorf("boom") }); e.trialBehind("o", "r", "main", "fabrik/merge-train/x") {
		t.Error("trialBehind should be false (fail-safe) on probe error")
	}
}

// TestListTrainBranchesOnOrigin verifies the ls-remote probe returns only the
// fabrik/merge-train/* branches present on origin, as bare names (FR-1/FR-4).
func TestListTrainBranchesOnOrigin(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	// origin (for wm.baseDir) is srcDir; create a merge-train branch there plus a
	// non-train branch that must be excluded.
	mustGit(t, srcDir, "branch", "fabrik/merge-train/merge-train-main-1")
	mustGit(t, srcDir, "branch", "fabrik/issue-99")

	got, err := wm.ListTrainBranchesOnOrigin()
	if err != nil {
		t.Fatalf("ListTrainBranchesOnOrigin: %v", err)
	}
	if len(got) != 1 || got[0] != "fabrik/merge-train/merge-train-main-1" {
		t.Errorf("ListTrainBranchesOnOrigin = %v, want [fabrik/merge-train/merge-train-main-1]", got)
	}
}

// TestListTrainBranchesOnOrigin_None verifies an empty result when no merge-train
// branches exist on origin (the common fresh-train case).
func TestListTrainBranchesOnOrigin_None(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	got, err := wm.ListTrainBranchesOnOrigin()
	if err != nil {
		t.Fatalf("ListTrainBranchesOnOrigin: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListTrainBranchesOnOrigin (none) = %v, want empty", got)
	}
}

// TestDissolveBatch verifies FR-5 dissolve semantics: the integration/CI PR is closed,
// an explanatory comment is posted on every member, the in-flight marker is cleared,
// and members are left untouched in Queued (no board status mutation).
func TestDissolveBatch(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm}
	members := []gh.ProjectItem{makeTrainItem(1, "One"), makeTrainItem(2, "Two")}

	eng.dissolveBatch(state, p, 200, "merge-train-main-1", members, "the base branch advanced")

	client.mu.Lock()
	defer client.mu.Unlock()
	// PR closed.
	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 200 {
		t.Errorf("expected integration PR #200 closed, got %v", client.closeIssueCalls)
	}
	// Explanatory comment on each member.
	dissolveComments := 0
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			dissolveComments++
		}
	}
	if dissolveComments != 2 {
		t.Errorf("expected 2 dissolve comments (one per member), got %d", dissolveComments)
	}
	// Members untouched in Queued — no board status update.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("dissolve must not mutate member board status, got %d update(s)", len(client.updateStatusCalls))
	}
	// The in-flight marker itself is cleared by runMergeTrainWorker's top-level
	// defer, not by dissolveBatch (ADR-067) — covered by
	// TestLandGreenBatch_ExhaustionDissolves via the worker's own dissolve path.
}

// TestDissolveBatch_NoPR verifies dissolve is a no-op on the PR close when prNum==0
// (an orphaned trial branch with no integration PR) yet still comments.
func TestDissolveBatch_NoPR(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1"}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm}

	eng.dissolveBatch(state, p, 0, "merge-train-main-1", []gh.ProjectItem{makeTrainItem(1, "One")}, "orphan")

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("dissolve with prNum==0 must not close any PR, got %v", client.closeIssueCalls)
	}
}

// trialNameGen returns a unique-trial-name generator mirroring the worker's (first
// call == base name; subsequent calls suffixed) for direct landGreenBatch tests.
func trialNameGen(base string) func() string {
	seq := 0
	return func() string {
		n := base
		if seq > 0 {
			n = fmt.Sprintf("%s-t%d", base, seq)
		}
		seq++
		return n
	}
}

// TestLandGreenBatch_BehindOnceThenLands verifies FR-2: when the validated-green trial
// has fallen behind its base (main moved) exactly once, the batch is rebased off the
// new base, re-validated green, and then lands (members advanced to Done, integration
// PR merged) without dissolving.
func TestLandGreenBatch_BehindOnceThenLands(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false }) // always green

	// Behind on the first landing-gate check, up to date thereafter.
	var mu sync.Mutex
	behindCalls := 0
	client.fetchCommitsBehindFn = func(_, _, _, _ string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		behindCalls++
		if behindCalls == 1 {
			return 1, nil // main moved
		}
		return 0, nil // caught up after rebase
	}

	survivors := []trainMember{makeQueuedMember(1, 101, "One"), makeQueuedMember(2, 102, "Two")}
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm, nextTrialName: trialNameGen("merge-train-main-1")}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.landGreenBatch(ctx, state, p, survivors)

	// One rebase → one extra combined validation (the re-validate off the new base).
	if got := rv.count(); got != 1 {
		t.Errorf("expected exactly 1 re-validation after a single rebase, got %d", got)
	}
	client.mu.Lock()
	merges := len(client.mergePRCalls)
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()
	if merges != 1 {
		t.Errorf("expected the rebased batch to land (1 merge), got %d", merges)
	}
	if advances != 2 {
		t.Errorf("expected 2 members advanced to Done after landing, got %d", advances)
	}
	// The in-flight marker itself is cleared by runMergeTrainWorker's top-level
	// defer, not by landGreenBatch/landMergeTrainBatch (ADR-067).
}

// TestLandGreenBatch_PendingReviewEjectDuringRebase_DiscardsTrialWithoutLanding
// closes the gap the outer re-form loop's and landOneAtATime's Hook 2 checkpoints
// don't cover (#1208): landGreenBatch's own main-moved rebase-and-revalidate loop
// can itself spend a full combined-Validate wait without ever returning control to
// runMergeTrainWorker, where the primary Hook 2 lives. A review-finding eject
// flagged for a member while that rebase re-validation is running must still stop
// the batch from landing — it must not ride the freshly-green rebased trial to
// landMergeTrainBatch just because this loop never re-checked the signal.
func TestLandGreenBatch_PendingReviewEjectDuringRebase_DiscardsTrialWithoutLanding(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false }) // always green

	// Behind on the first landing-gate check (forces exactly one rebase cycle),
	// up to date thereafter — same shape as TestLandGreenBatch_BehindOnceThenLands.
	var mu sync.Mutex
	behindCalls := 0
	client.fetchCommitsBehindFn = func(_, _, _, _ string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		behindCalls++
		if behindCalls == 1 {
			return 1, nil // main moved
		}
		return 0, nil // caught up after rebase
	}

	survivors := []trainMember{makeQueuedMember(1, 101, "One"), makeQueuedMember(2, 102, "Two")}
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	eng.store.EnterRepoWorker("owner/repo")
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm, nextTrialName: trialNameGen("merge-train-main-1")}

	// Flag #1 for a pending review-finding eject — simulates settleQueuedReviewFindings
	// observing an unresolved thread on #1 while the rebase re-validate is in flight.
	eng.markPendingReviewEject("owner/repo", 1, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.landGreenBatch(ctx, state, p, survivors)

	// The rebased trial must NOT land: no merge, no member advanced to Done.
	client.mu.Lock()
	merges := len(client.mergePRCalls)
	client.mu.Unlock()
	if merges != 0 {
		t.Errorf("expected the flagged trial to be discarded rather than landed, got %d merge(s)", merges)
	}

	// #1 must have been rerouted off Queued and carry the distinct ejection wording.
	foundReroute := false
	client.mu.Lock()
	for _, c := range client.updateStatusCalls {
		if c.optionID == "opt-implement" {
			foundReroute = true
		}
	}
	client.mu.Unlock()
	if !foundReroute {
		t.Error("expected #1 to be rerouted off Queued to Implement")
	}
	bodies := ejectionCommentBodies(client, 1)
	if len(bodies) != 1 || !strings.Contains(bodies[0], "has left the Queued column") {
		t.Errorf("expected #1's ejection comment to use the leaves-Queued wording, got: %v", bodies)
	}

	// The pending-eject signal must have been consumed (one-shot).
	if _, ok := eng.takePendingReviewEject("owner/repo", 1); ok {
		t.Error("expected the pending-eject signal for #1 to already be consumed")
	}

	// #2 (not flagged) must not have been touched — no comment, no reroute — it
	// simply re-forms fresh on a future poll's dispatchMergeTrainWorker call.
	if len(ejectionCommentBodies(client, 2)) != 0 {
		t.Error("expected #2 to be left untouched, not ejected")
	}
}

// TestLandGreenBatch_ExhaustionDissolves verifies FR-2/FR-5: when the trial keeps
// falling behind past MaxTrainRebaseCycles, the batch is dissolved — members left
// untouched in Queued (no advancement, no merge) and the in-flight marker cleared.
func TestLandGreenBatch_ExhaustionDissolves(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false }) // always green
	eng.cfg.MaxTrainRebaseCycles = 2                                                   // small bound for a fast test

	client.fetchCommitsBehindFn = func(_, _, _, _ string) (int, error) { return 1, nil } // never catches up

	survivors := []trainMember{makeQueuedMember(1, 101, "One"), makeQueuedMember(2, 102, "Two")}
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm, nextTrialName: trialNameGen("merge-train-main-1")}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.landGreenBatch(ctx, state, p, survivors)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.mergePRCalls) != 0 {
		t.Errorf("exhausted batch must not merge, got %d merge(s)", len(client.mergePRCalls))
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("exhausted batch must leave members in Queued (no advancement), got %d update(s)", len(client.updateStatusCalls))
	}
	dissolveComments := 0
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			dissolveComments++
		}
	}
	if dissolveComments != 2 {
		t.Errorf("expected 2 dissolve comments (one per member), got %d", dissolveComments)
	}
	// The in-flight marker itself is cleared by runMergeTrainWorker's top-level
	// defer, not by landGreenBatch/dissolveBatch (ADR-067).
}

// reconstructParams builds a trialParams suitable for direct reconstructTrainState /
// resume / complete-deferred tests against a real bare repo.
func reconstructParams(wm *WorktreeManager) trialParams {
	return trialParams{
		owner:         "owner",
		repo:          "repo",
		baseBranch:    "main",
		wm:            wm,
		nextTrialName: trialNameGen("merge-train-main-1"),
	}
}

// TestReconstructTrainState_Fresh verifies that with no durable artifacts (no train
// PRs, no origin branches), reconstruction returns false so the caller forms a fresh
// train — FR-1/FR-4 "fresh" route.
func TestReconstructTrainState_Fresh(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	client := &mockGitHubClient{listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil }}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{assembling: true}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	if eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), makeSeamBatch(2)) {
		t.Error("reconstructTrainState with no durable artifacts should return false (fresh)")
	}
}

// TestReconstructTrainState_ResumeOpenPR verifies FR-4 resume: a durable open train PR
// backed by a trial branch is resumed (CI re-polled green, then landed) without forming
// a fresh batch — no duplicate draft CI PR, members advanced to Done.
func TestReconstructTrainState_ResumeOpenPR(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	openPR := gh.PRDetails{
		Number:      300,
		State:       "open",
		Merged:      false,
		HeadRefName: "fabrik/merge-train/merge-train-main-1",
		HeadSHA:     "trialsha",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker,
	}
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false }) // green
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{openPR}, nil }

	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	handled := eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), makeSeamBatch(2))
	if !handled {
		t.Fatal("reconstructTrainState should have handled the open train PR (resume)")
	}
	if got := rv.count(); got != 1 {
		t.Errorf("resume should re-validate exactly once, got %d", got)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.createDraftPRCalls) != 0 {
		t.Errorf("resume must not open a fresh draft CI PR, got %d", len(client.createDraftPRCalls))
	}
	if len(client.mergePRCalls) != 1 || client.mergePRCalls[0].prNumber != 300 {
		t.Errorf("resume should merge the existing integration PR #300, got %v", client.mergePRCalls)
	}
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("resume should advance 2 members to Done, got %d", len(client.updateStatusCalls))
	}
}

// TestReconstructTrainState_CompleteDeferredLanding verifies FR-4 complete-deferred: an
// already-merged integration PR whose members are still Queued completes the deferred
// member lifecycle (advance to Done) rather than re-merging.
func TestReconstructTrainState_CompleteDeferredLanding(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	mergedPR := gh.PRDetails{
		Number:      400,
		State:       "closed",
		Merged:      true,
		HeadRefName: "fabrik/merge-train/merge-train-main-1",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker,
	}
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{mergedPR}, nil }

	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	handled := eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), makeSeamBatch(2))
	if !handled {
		t.Fatal("reconstructTrainState should have handled the merged integration PR (complete-deferred)")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	// Already merged → no merge, no re-validation, but members advanced.
	if len(client.mergePRCalls) != 0 {
		t.Errorf("complete-deferred must not re-merge an already-merged PR, got %d merge(s)", len(client.mergePRCalls))
	}
	if rv.count() != 0 {
		t.Errorf("complete-deferred must not re-validate, got %d validation(s)", rv.count())
	}
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("complete-deferred should advance 2 still-Queued members to Done, got %d", len(client.updateStatusCalls))
	}
	// The in-flight marker itself is cleared by prepareTrainWorker's own-failure
	// defer when reconstructTrainState returns true (ADR-067), not by
	// completeDeferredLanding/reconstructTrainState called directly here.
}

// TestReconstructTrainState_OrphanOpenPRNoBranch_Dissolves verifies FR-4/FR-5: an open
// train PR with no backing trial branch on origin is an orphaned remnant and is
// dissolved (PR closed, members left in Queued, marker cleared). This runs with real
// git (no validate seam) so the ls-remote branch probe executes.
func TestReconstructTrainState_OrphanOpenPRNoBranch_Dissolves(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t) // no fabrik/merge-train/* branch on origin
	orphanPR := gh.PRDetails{
		Number:      500,
		State:       "open",
		Merged:      false,
		HeadRefName: "fabrik/merge-train/merge-train-main-9",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker,
	}
	client := &mockGitHubClient{
		listPRsFn:    func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{orphanPR}, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm) // no trainValidateFn → ls-remote runs
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	handled := eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), makeSeamBatch(2))
	if !handled {
		t.Fatal("reconstructTrainState should have handled the orphaned open PR (dissolve)")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 500 {
		t.Errorf("orphan dissolve should close integration PR #500, got %v", client.closeIssueCalls)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("orphan dissolve must leave members in Queued, got %d status update(s)", len(client.updateStatusCalls))
	}
	dissolveComments := 0
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			dissolveComments++
		}
	}
	if dissolveComments != 2 {
		t.Errorf("expected 2 dissolve comments, got %d", dissolveComments)
	}
	// The in-flight marker itself is cleared by prepareTrainWorker's own-failure
	// defer when reconstructTrainState returns true (ADR-067), not by
	// dissolveBatch/reconstructTrainState called directly here.
}

// TestReconstructTrainState_HistoricalMergedPR_ProceedsFresh is a regression test for
// the "train stalls after the first landing" bug: ListPRs returns state=all, so a
// merged integration PR from a *prior* completed batch is still surfaced. Its members
// (#1, #2) already advanced to Done and are no longer in today's Queued snapshot
// (#10, #11). Reconstruction must recognise it as irrelevant and return false (proceed
// fresh) — NOT route to complete-deferred, find no still-Queued members, and abort
// today's batch. (FR-1/FR-4.)
func TestReconstructTrainState_HistoricalMergedPR_ProceedsFresh(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	historicalPR := gh.PRDetails{
		Number:      400,
		State:       "closed",
		Merged:      true,
		HeadRefName: "fabrik/merge-train/merge-train-main-1",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker, // yesterday's members
	}
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{historicalPR}, nil }

	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	// Today's fresh Queued batch — disjoint from the historical PR's members.
	batch := []gh.ProjectItem{makeTrainItem(10, "Ten"), makeTrainItem(11, "Eleven")}
	batch[0].ItemID, batch[1].ItemID = "item-10", "item-11"

	if eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), batch) {
		t.Fatal("historical merged PR (no still-Queued members) must not be handled — reconstruct should return false (fresh)")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if rv.count() != 0 {
		t.Errorf("historical PR must not trigger any (re)validation, got %d", rv.count())
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("historical PR must not be (re)merged, got %d merge(s)", len(client.mergePRCalls))
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("historical PR must not advance any member, got %d status update(s)", len(client.updateStatusCalls))
	}
}

// TestReconstructTrainState_StaleOpenPR_ClosedAndProceedsFresh verifies that a stale
// *open* train PR with no members still in today's Queued snapshot is closed (so it
// can't later hijack findIntegrationPR) and reconstruction proceeds fresh (returns
// false) rather than resuming or dissolving with unrelated members. (FR-1/FR-4.)
func TestReconstructTrainState_StaleOpenPR_ClosedAndProceedsFresh(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	staleOpenPR := gh.PRDetails{
		Number:      500,
		State:       "open",
		Merged:      false,
		HeadRefName: "fabrik/merge-train/merge-train-main-1",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker,
	}
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{staleOpenPR}, nil }

	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	batch := []gh.ProjectItem{makeTrainItem(10, "Ten"), makeTrainItem(11, "Eleven")}
	batch[0].ItemID, batch[1].ItemID = "item-10", "item-11"

	if eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), batch) {
		t.Fatal("stale open PR (no still-Queued members) must not be handled — reconstruct should return false (fresh)")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 500 {
		t.Errorf("stale open PR #500 should be closed, got %v", client.closeIssueCalls)
	}
	if len(client.mergePRCalls) != 0 || rv.count() != 0 || len(client.updateStatusCalls) != 0 {
		t.Errorf("stale open PR must not resume/land: merges=%d validations=%d advances=%d", len(client.mergePRCalls), rv.count(), len(client.updateStatusCalls))
	}
	dissolveComments := 0
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			dissolveComments++
		}
	}
	if dissolveComments != 0 {
		t.Errorf("stale open PR must not post dissolve comments on unrelated members, got %d", dissolveComments)
	}
}

// TestReconstructTrainState_OrphanedBranchNoPR_ProceedsFresh is a regression test: an
// orphaned fabrik/merge-train/* branch on origin (a crash remnant) with no relevant
// train PR must be cleaned up SILENTLY and reconstruction must proceed fresh (return
// false) — NOT dissolve with today's members (which would post "batch dissolved"
// comments on unrelated fresh Queued issues) and abort today's batch. Runs with real
// git (no seam) so the ls-remote probe executes. (FR-4/FR-5.)
func TestReconstructTrainState_OrphanedBranchNoPR_ProceedsFresh(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)
	// Orphaned trial branch on origin, no integration PR.
	mustGit(t, srcDir, "branch", "fabrik/merge-train/merge-train-main-9")

	client := &mockGitHubClient{
		listPRsFn:    func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil }, // no train PR
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm) // no trainValidateFn → ls-remote runs
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	batch := []gh.ProjectItem{makeTrainItem(10, "Ten"), makeTrainItem(11, "Eleven")}

	if eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), batch) {
		t.Fatal("orphaned branch with no relevant PR must not be handled — reconstruct should return false (fresh)")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	dissolveComments := 0
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			dissolveComments++
		}
	}
	if dissolveComments != 0 {
		t.Errorf("orphaned-branch cleanup must be silent (no dissolve comments on fresh members), got %d", dissolveComments)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("orphaned-branch cleanup must not touch member status, got %d update(s)", len(client.updateStatusCalls))
	}
}

// TestDispatchMergeTrainWorker_DifferentReposConcurrent verifies FR-3: the per-repo
// serialization guard keyed on owner/repo does NOT cross-block distinct repos — two
// repos' trains run at the same time under the shared MaxConcurrent semaphore. The
// combined-Validate seam acts as a barrier: each worker records its concurrency and
// waits for the other to arrive; if the guard wrongly cross-blocked, only one worker
// would ever be in flight and the observed maximum concurrency would be 1.
func TestDispatchMergeTrainWorker_DifferentReposConcurrent(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wmA := setupTrainRepo(t)
	_, _, _, wmB := setupTrainRepo(t)

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	bothArrived := make(chan struct{})
	var once sync.Once

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 1000 + n, HeadSHA: fmt.Sprintf("sha-%d", n), State: "open"}, nil
		},
		listPRsFn:  func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 900, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wmA)
	eng.mu.Lock()
	eng.worktreeManagers["ownerA/repoA"] = wmA
	eng.worktreeManagers["ownerB/repoB"] = wmB
	eng.mu.Unlock()

	eng.trainValidateFn = func(_ context.Context, _ []trainMember) (TrainCIResult, *trainCIDiagnostic) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		reached := inFlight == 2
		mu.Unlock()
		if reached {
			once.Do(func() { close(bothArrived) })
		}
		select {
		case <-bothArrived:
		case <-time.After(5 * time.Second): // guard against a hang if cross-blocked
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return TrainCIGreen, nil
	}

	itemA := gh.ProjectItem{Number: 1, Repo: "ownerA/repoA", Status: "Queued", ItemID: "a1"}
	itemB := gh.ProjectItem{Number: 2, Repo: "ownerB/repoB", Status: "Queued", ItemID: "b2"}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eng.dispatchMergeTrainWorker(ctx, []gh.ProjectItem{itemA}, "")
	eng.dispatchMergeTrainWorker(ctx, []gh.ProjectItem{itemB}, "")

	done := make(chan struct{})
	go func() { eng.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("workers did not finish — per-repo guard likely cross-blocked distinct repos")
	}

	mu.Lock()
	got := maxInFlight
	mu.Unlock()
	if got != 2 {
		t.Errorf("expected 2 repos' trains to validate concurrently, observed max concurrency %d", got)
	}
}

// TestDispatchMergeTrainWorker_SameRepoSuppressedDurably verifies FR-1: while a train
// is in flight for a repo (in-memory marker present), a second dispatch for the SAME
// repo does not launch another worker.
func TestDispatchMergeTrainWorker_SameRepoSuppressedDurably(t *testing.T) {
	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	// Simulate an in-flight train (e.g. resumed after reconstruction).
	eng.mergeTrainInFlight.Store("owner/repo", &mergeTrainWorkerState{assembling: true, trialName: "merge-train-main-1"})

	eng.dispatchMergeTrainWorker(context.Background(), []gh.ProjectItem{makeTrainItem(1, "One")}, "")

	done := make(chan struct{})
	go func() { eng.wg.Wait(); close(done) }()
	select {
	case <-done: // good — no worker launched
	case <-time.After(200 * time.Millisecond):
		t.Error("a duplicate worker was launched despite an in-flight train for the same repo")
	}
}

// TestBuildIntegrationPRBody_ClosesMembers verifies the landing integration PR body
// carries a "Closes #N" line per member, so merging it auto-closes the member issues
// (restoring issue↔landing-PR connectivity) — plus the batch marker.
func TestBuildIntegrationPRBody_ClosesMembers(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(7, 70, "Seven"), makeQueuedMember(9, 90, "Nine")}
	body := buildIntegrationPRBody(survivors)
	for _, want := range []string{"Closes #7", "Closes #9", mergeTrainBatchMarker} {
		if !strings.Contains(body, want) {
			t.Errorf("integration PR body missing %q\n---\n%s", want, body)
		}
	}
	// Must not close a PR/issue that isn't a member.
	if strings.Contains(body, "Closes #70") || strings.Contains(body, "Closes #90") {
		t.Errorf("body must Closes the issue numbers (7,9), not the PR numbers (70,90)\n%s", body)
	}
}

// ── ADR-059 D8: runaway guard ─────────────────────────────────────────────────

// TestEffectiveTrialWindow_Defaults verifies zero-means-default: N=20, M=60min.
func TestEffectiveTrialWindow_Defaults(t *testing.T) {
	eng := &Engine{cfg: Config{}, mergeTrainTrials: make(map[string][]time.Time)}
	n, m := eng.effectiveTrialWindow()
	if n != 20 {
		t.Errorf("effectiveTrialWindow() N = %d, want 20", n)
	}
	if m != 60*time.Minute {
		t.Errorf("effectiveTrialWindow() M = %v, want 60m", m)
	}
}

// TestEffectiveTrialWindow_Override verifies explicit config values are respected.
func TestEffectiveTrialWindow_Override(t *testing.T) {
	eng := &Engine{cfg: Config{MaxTrainTrialsPerWindow: 5, TrainTrialWindowDuration: 30 * time.Minute}, mergeTrainTrials: make(map[string][]time.Time)}
	n, m := eng.effectiveTrialWindow()
	if n != 5 {
		t.Errorf("effectiveTrialWindow() N = %d, want 5", n)
	}
	if m != 30*time.Minute {
		t.Errorf("effectiveTrialWindow() M = %v, want 30m", m)
	}
}

// TestRecordTrial_Increments verifies recordTrial appends timestamps and returns
// the growing count. isRunawayTripped returns false below the threshold.
func TestRecordTrial_Increments(t *testing.T) {
	eng := &Engine{cfg: Config{MaxTrainTrialsPerWindow: 3, TrainTrialWindowDuration: time.Hour}, mergeTrainTrials: make(map[string][]time.Time)}
	const key = "owner/repo"

	for i := 1; i <= 2; i++ {
		count := eng.recordTrial(key)
		if count != i {
			t.Errorf("recordTrial iteration %d: count = %d, want %d", i, count, i)
		}
		if _, tripped := eng.isRunawayTripped(key); tripped {
			t.Errorf("isRunawayTripped should be false before threshold (iteration %d)", i)
		}
	}
}

// TestIsRunawayTripped_AtThreshold verifies the guard trips at exactly N trials.
func TestIsRunawayTripped_AtThreshold(t *testing.T) {
	const N = 3
	eng := &Engine{cfg: Config{MaxTrainTrialsPerWindow: N, TrainTrialWindowDuration: time.Hour}, mergeTrainTrials: make(map[string][]time.Time)}
	const key = "owner/repo"

	for i := 0; i < N-1; i++ {
		eng.recordTrial(key)
	}
	if _, tripped := eng.isRunawayTripped(key); tripped {
		t.Error("guard must not trip before reaching N trials")
	}
	eng.recordTrial(key)
	count, tripped := eng.isRunawayTripped(key)
	if !tripped {
		t.Errorf("guard must trip at N=%d trials, count=%d", N, count)
	}
	if count != N {
		t.Errorf("count = %d, want %d", count, N)
	}
}

// TestResetTrialCounter clears the counter so isRunawayTripped returns false.
func TestResetTrialCounter(t *testing.T) {
	const N = 3
	eng := &Engine{cfg: Config{MaxTrainTrialsPerWindow: N, TrainTrialWindowDuration: time.Hour}, mergeTrainTrials: make(map[string][]time.Time)}
	const key = "owner/repo"

	for i := 0; i < N; i++ {
		eng.recordTrial(key)
	}
	if _, tripped := eng.isRunawayTripped(key); !tripped {
		t.Fatal("precondition: guard should be tripped before reset")
	}
	eng.resetTrialCounter(key)
	if _, tripped := eng.isRunawayTripped(key); tripped {
		t.Error("guard must not be tripped after reset")
	}
}

// TestRunawayGuard_Fires verifies that when the trial counter reaches N with no
// successful lands, the guard pauses all batch members and posts an alert comment.
// N=2: trial 1 (initial batch), trial 2 (bisect half) → guard trips during bisect
// while all members are still in the active survivors set from the initial red trial.
func TestRunawayGuard_Fires(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// Always red — no member ever lands.
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return true })
	eng.cfg.MaxTrainTrialsPerWindow = 2
	eng.cfg.TrainTrialWindowDuration = time.Hour

	batch := makeSeamBatch(3)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	client.mu.Lock()
	defer client.mu.Unlock()

	// All 3 batch members must have fabrik:paused + fabrik:awaiting-input applied.
	for _, issueNum := range []int{1, 2, 3} {
		paused, awaiting := false, false
		for _, c := range client.addLabelCalls {
			if c.issueNumber == issueNum && c.labelName == "fabrik:paused" {
				paused = true
			}
			if c.issueNumber == issueNum && c.labelName == "fabrik:awaiting-input" {
				awaiting = true
			}
		}
		if !paused {
			t.Errorf("member #%d: expected fabrik:paused (runaway guard)", issueNum)
		}
		if !awaiting {
			t.Errorf("member #%d: expected fabrik:awaiting-input (runaway guard)", issueNum)
		}
		// Alert comment posted on each member.
		hasAlert := false
		for _, c := range client.addCommentCalls {
			if c.issueNumber == issueNum && strings.Contains(c.body, "runaway guard") {
				hasAlert = true
			}
		}
		if !hasAlert {
			t.Errorf("member #%d: expected runaway guard alert comment", issueNum)
		}
	}

	// mergeTrainInFlight must be cleared after the guard fires.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after runaway guard fires")
	}
}

// TestRunawayGuard_NormalBisectionNotTripped verifies R7: a batch with a single real
// poisoner isolates the poisoner, lands the survivors, and never trips the guard,
// because a successful landing resets the counter.
func TestRunawayGuard_NormalBisectionNotTripped(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// Red iff #3 is present; #3 is the sole poisoner.
	eng, client, _ := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] })
	// Low threshold — still must not trip because survivors land and reset the counter.
	eng.cfg.MaxTrainTrialsPerWindow = 10
	eng.cfg.TrainTrialWindowDuration = time.Hour

	batch := makeSeamBatch(5)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	client.mu.Lock()
	defer client.mu.Unlock()

	// #3 ejected (poisoner), survivors land — no member should have fabrik:paused
	// from the runaway guard (the ejection-pause from MaxMergeTrainEjections is a
	// different code path; here MaxMergeTrainEjections=3 so #3 gets paused by ejectMember
	// after the first ejection only when the counter is pre-seeded — it won't here).
	for _, issueNum := range []int{1, 2, 4, 5} {
		for _, c := range client.addLabelCalls {
			if c.issueNumber == issueNum && c.labelName == "fabrik:paused" {
				// Only look for "runaway guard" comments — ejection-pause is a distinct path.
				for _, cc := range client.addCommentCalls {
					if cc.issueNumber == issueNum && strings.Contains(cc.body, "runaway guard") {
						t.Errorf("survivor #%d got a runaway guard alert — guard must not fire when survivors land", issueNum)
					}
				}
			}
		}
	}

	// Survivors integration PR must be merged exactly once.
	merges := len(client.mergePRCalls)
	if merges != 1 {
		t.Errorf("expected survivors to land (1 merge), got %d merges", merges)
	}
}

// TestRunawayGuard_BisectionExceedsThresholdWithoutTripping is the A3 regression test for
// #1528: it reproduces the reported bed's exact trial shape (a 3-member batch with a single
// poisoner, #3) against a threshold the *raw* trial count provably exceeds but the *counted*
// (non-green) trial count does not.
//
// Trial-by-trial trace (member 3 is the sole poisoner):
//  1. {1,2,3} red   (counted: initial batch validation)
//  2. {1}     green (not counted — bisection sub-trial proving half A clean)
//  3. {2,3}   red   (counted — half B still contains the poisoner)
//  4. {2}     green (not counted — bisection sub-trial proving half A of {2,3} clean)
//  5. {3}     red   (counted — isolates the poisoner)
//  6. {1,2}   green (not counted — the survivor-validation trial that lands #1 and #2)
//
// Raw trial count is 6; the guard-counted (non-green) count is 3. With
// MaxTrainTrialsPerWindow=5, 6 > 5 (satisfying A3's "trial count exceeds
// MaxTrainTrialsPerWindow" requirement) while 3 < 5, so the guard must never fire and the
// survivors must land — exactly the scenario the reported bed hit with the default N=6.
func TestRunawayGuard_BisectionExceedsThresholdWithoutTripping(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// Red iff #3 is present; #3 is the sole poisoner.
	eng, client, rv := seamTrainEngine(t, wm, func(p map[int]bool) bool { return p[3] })
	eng.cfg.MaxTrainTrialsPerWindow = 5
	eng.cfg.TrainTrialWindowDuration = time.Hour

	batch := makeSeamBatch(3)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)

	// Non-vacuity: the raw trial count must actually exceed the threshold — this is what
	// makes the test meaningful (fixed code completes all 6 raw trials since bisection is
	// never preempted; the pre-fix code trips the guard early and never reaches trial 6 at
	// all, which is itself a symptom of the bug this test guards against — see #1528 PR body
	// for the recorded red run). Non-fatal so the rest of the assertions still run and
	// surface the fuller picture on regression.
	if got := rv.count(); got <= eng.cfg.MaxTrainTrialsPerWindow {
		t.Errorf("raw trial count %d does not exceed MaxTrainTrialsPerWindow %d", got, eng.cfg.MaxTrainTrialsPerWindow)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	// No member — poisoner or survivor — may receive a runaway guard alert or pause.
	for _, issueNum := range []int{1, 2, 3} {
		for _, c := range client.addCommentCalls {
			if c.issueNumber == issueNum && strings.Contains(c.body, "runaway guard") {
				t.Errorf("member #%d got a runaway guard alert — a successful bisection must never trip the guard", issueNum)
			}
		}
		for _, c := range client.addLabelCalls {
			if c.issueNumber == issueNum && c.labelName == "fabrik:paused" {
				t.Errorf("member #%d got fabrik:paused — a successful bisection must never trip the runaway guard", issueNum)
			}
		}
	}

	// #3 must still be ejected as the isolated poisoner (bisection itself must keep working).
	ejected := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 3 && strings.Contains(c.body, "isolated by halving bisection") {
			ejected = true
		}
	}
	if !ejected {
		t.Error("expected #3 to be ejected as the isolated poisoner")
	}

	// #1 and #2 must land (exactly one integration-PR merge).
	if merges := len(client.mergePRCalls); merges != 1 {
		t.Errorf("expected survivors #1 and #2 to land (1 merge), got %d merges", merges)
	}
}

// ── #1533 fireRunawayGuard atomicity/idempotency ────────────────────────────

// TestFireRunawayGuard_IdempotentAcrossTwoFirings covers R2/A3: a member appearing in two
// separate fireRunawayGuard calls' items slices within the same guard episode (the shape
// Hook 1's two call sites, or a racing Hook 1/Hook 2 pair, can produce) must receive the
// alert comment exactly once, not once per call.
func TestFireRunawayGuard_IdempotentAcrossTwoFirings(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	memberA := makeTrainItem(1, "Member One") // present in both firings
	memberB := makeTrainItem(2, "Member Two") // only in the second firing

	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{memberA}, 6)
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{memberA, memberB}, 6)

	client.mu.Lock()
	defer client.mu.Unlock()

	alertCount := func(issueNum int) int {
		n := 0
		for _, c := range client.addCommentCalls {
			if c.issueNumber == issueNum && strings.Contains(c.body, "runaway guard") {
				n++
			}
		}
		return n
	}
	if got := alertCount(1); got != 1 {
		t.Errorf("member #1 (in both firings): expected exactly 1 alert comment, got %d", got)
	}
	if got := alertCount(2); got != 1 {
		t.Errorf("member #2 (only in the second firing): expected exactly 1 alert comment, got %d", got)
	}
}

// TestFireRunawayGuard_ReAlertsAfterOperatorResumeRaisesCount verifies #1533 review finding
// 2: the alert text itself instructs operators to manually remove fabrik:paused/
// fabrik:awaiting-input to resume the train, and that recovery path never calls
// resetTrialCounter (which only fires on a successful land). If the same member trips again
// afterward, it does so at a strictly HIGHER trial count than before — trials cannot
// accumulate while every Queued member stays paused, so a higher count is only possible once
// an operator resume let new trials actually run. That must produce a fresh alert, not be
// silently skipped as "already alerted this episode" the way TestFireRunawayGuard_
// IdempotentAcrossTwoFirings correctly does for two firings at the SAME count.
func TestFireRunawayGuard_ReAlertsAfterOperatorResumeRaisesCount(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member := makeTrainItem(3, "Resumed Member")

	// First trip: fires at count 6, alerts and pauses the member.
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 6)
	// A second, genuinely new trip: count is now 8, not 6 — only possible if new trials ran
	// after an operator resumed the member (no resetTrialCounter call in between here, exactly
	// mirroring the recovery path's own behavior).
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 8)

	client.mu.Lock()
	defer client.mu.Unlock()
	alerts := 0
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 3 && strings.Contains(c.body, "runaway guard") {
			alerts++
		}
	}
	if alerts != 2 {
		t.Errorf("expected 2 alerts across two genuinely new trips (counts 6 then 8, no resetTrialCounter in between), got %d", alerts)
	}
}

// TestFireRunawayGuard_CommentFailureLeavesMarkerAndRetriable covers R1's residual case: an
// AddComment failure must not silently strand the (already-paused) member. It should be left
// with the durable fabrik:awaiting-runaway-alert marker instead, and must NOT be recorded as
// already-alerted — a later fireRunawayGuard call (or the settle scan) must still retry it.
func TestFireRunawayGuard_CommentFailureLeavesMarkerAndRetriable(t *testing.T) {
	client := &mockGitHubClient{}
	client.addCommentFn = func(owner, repo string, issueNumber int, body string) (int, error) {
		return 0, fmt.Errorf("simulated transient failure")
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member := makeTrainItem(9, "Flaky Member")
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 6)

	client.mu.Lock()
	paused, marker, commentAttempts := false, false, 0
	pausedIdx, markerIdx := -1, -1
	for i, c := range client.addLabelCalls {
		if c.issueNumber == 9 && c.labelName == "fabrik:paused" {
			paused = true
			if pausedIdx == -1 {
				pausedIdx = i
			}
		}
		if c.issueNumber == 9 && c.labelName == runawayAlertMarkerLabel {
			marker = true
			if markerIdx == -1 {
				markerIdx = i
			}
		}
	}
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 9 {
			commentAttempts++
		}
	}
	client.mu.Unlock()

	if !paused {
		t.Error("expected fabrik:paused applied even though the alert comment failed — pause is not gated on the comment")
	}
	if !marker {
		t.Errorf("expected %s marker applied so the settle scan retries the failed alert", runawayAlertMarkerLabel)
	}
	if commentAttempts != 1 {
		t.Errorf("expected exactly 1 comment attempt, got %d", commentAttempts)
	}
	// #1533 review: fabrik:paused must land before the awaiting-runaway-alert marker, so a
	// crash between the two label writes can never leave a member carrying the marker
	// without actually being paused — the invariant runaway_alert_settle.go's settle scan
	// depends on (it does not gate on fabrik:paused's absence, unlike its sibling scans).
	if pausedIdx == -1 || markerIdx == -1 || pausedIdx > markerIdx {
		t.Errorf("expected fabrik:paused applied before %s (paused at call %d, marker at call %d)", runawayAlertMarkerLabel, pausedIdx, markerIdx)
	}

	// Not marked alerted: a second firing for the same member within the same episode
	// must retry the comment, not skip it as already-delivered.
	client.addCommentFn = nil // succeeds this time
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 6)

	client.mu.Lock()
	defer client.mu.Unlock()
	commentAttempts = 0
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 9 {
			commentAttempts++
		}
	}
	if commentAttempts != 2 {
		t.Errorf("expected the second firing to retry the comment (2 total attempts), got %d", commentAttempts)
	}
}

// TestResetTrialCounter_ClearsRunawayAlertedIdempotency verifies that resetTrialCounter (the
// guard's own "episode ends" signal) clears mergeTrainRunawayAlerted for the repo, so a member
// alerted in one episode is eligible for a fresh alert in the next.
func TestResetTrialCounter_ClearsRunawayAlertedIdempotency(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	member := makeTrainItem(4, "Repeat Offender")
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 6)
	eng.resetTrialCounter("owner/repo")
	eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, 6)

	client.mu.Lock()
	defer client.mu.Unlock()
	alerts := 0
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 4 && strings.Contains(c.body, "runaway guard") {
			alerts++
		}
	}
	if alerts != 2 {
		t.Errorf("expected 2 alerts across two separate episodes (separated by resetTrialCounter), got %d", alerts)
	}
}

// TestRouteQueuedGroup_RunawayGuardHook2AlertsEveryMember is the A2 test: it covers Hook 2
// (routeQueuedGroup's "already tripped — pausing before dispatch" path) specifically, and
// asserts every member in the passed items slice receives the alert comment. Before #1533,
// only fireRunawayGuard's Hook 1 call sites had direct unit coverage.
func TestRouteQueuedGroup_RunawayGuardHook2AlertsEveryMember(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxTrainTrialsPerWindow = 1
	eng.cfg.TrainTrialWindowDuration = time.Hour

	repoKey := "owner/repo"
	eng.recordTrial(repoKey) // trips the counter (threshold 1)

	items := []gh.ProjectItem{
		makeTrainItem(1, "Member One"),
		makeTrainItem(2, "Member Two"),
	}

	eng.routeQueuedGroup(context.Background(), repoKey, items, "PVT_test")

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, issueNum := range []int{1, 2} {
		hasAlert := false
		for _, c := range client.addCommentCalls {
			if c.issueNumber == issueNum && strings.Contains(c.body, "runaway guard") {
				hasAlert = true
			}
		}
		if !hasAlert {
			t.Errorf("member #%d: expected a runaway guard alert comment from Hook 2 (routeQueuedGroup)", issueNum)
		}
		paused := false
		for _, c := range client.addLabelCalls {
			if c.issueNumber == issueNum && c.labelName == "fabrik:paused" {
				paused = true
			}
		}
		if !paused {
			t.Errorf("member #%d: expected fabrik:paused from Hook 2", issueNum)
		}
	}

	// routeQueuedGroup must return immediately after firing the guard — no worker dispatched.
	if _, ok := eng.mergeTrainInFlight.Load(repoKey); ok {
		t.Error("expected no worker dispatched when the runaway guard is already tripped")
	}
}

// TestFireRunawayGuard_RacesSettleRunawayGuardAlert_NoDuplicateAlert covers the residual race
// between fireRunawayGuard and settleRunawayGuardAlert for the same member (#1533 R2/A3): a
// member that already picked up the fabrik:awaiting-runaway-alert marker earlier in the same
// episode (an earlier AddComment attempt failed) can still appear in a later Hook 1 call's
// own current/survivors list (the worker's own in-flight member set, not a fresh board read
// that would exclude an already-fabrik:paused item — unlike Hook 2's groupQueuedByRepo). If
// that later fireRunawayGuard call races a concurrent settleRunawayGuardAlertScan retry for
// the same member, both must not independently succeed in posting the alert — exactly one
// comment must land, never two, regardless of which one wins the race.
func TestFireRunawayGuard_RacesSettleRunawayGuardAlert_NoDuplicateAlert(t *testing.T) {
	var commentCount int32
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			atomic.AddInt32(&commentCount, 1)
			// Widen the race window so both goroutines are genuinely in-flight
			// concurrently rather than incidentally serialized by scheduling luck.
			time.Sleep(5 * time.Millisecond)
			return 1, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxTrainTrialsPerWindow = 1
	eng.cfg.TrainTrialWindowDuration = time.Hour

	// Simulates the state left behind by an earlier fireRunawayGuard call within this
	// same episode whose AddComment failed: paused, marker applied, not yet alerted.
	member := makeTrainItem(20, "Stale Marker Member")
	member.Labels = append(member.Labels, runawayAlertMarkerLabel, "fabrik:paused", "fabrik:awaiting-input")

	// Seed a real trial so isRunawayTripped's count matches across both goroutines:
	// settleRunawayGuardAlert always re-derives its own count live (#1533 review, finding
	// 2), so fireRunawayGuard must be given the same live count here rather than an
	// arbitrary literal — otherwise the two calls would (correctly, but irrelevantly to
	// this test) disagree on whether the other's alert is fresh enough to skip.
	eng.recordTrial("owner/repo")
	count, tripped := eng.isRunawayTripped("owner/repo")
	if !tripped {
		t.Fatalf("expected the seeded trial to trip the guard (count=%d)", count)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		eng.fireRunawayGuard(context.Background(), "owner", "repo", []gh.ProjectItem{member}, count)
	}()
	go func() {
		defer wg.Done()
		eng.settleRunawayGuardAlert(member)
	}()
	wg.Wait()

	if got := atomic.LoadInt32(&commentCount); got != 1 {
		t.Errorf("expected exactly 1 alert comment posted across the racing calls, got %d", got)
	}
}

// TestMergeTrainRunawayGuard is the e2e runaway guard test: a persistently-red batch
// where every trial fails and no member ever lands trips the guard within N trials,
// pausing all Queued members. Follows the pattern of TestMergeTrainBisect_CostCapFallbackLogs.
// N=2 so the guard trips during the first bisection sub-trial, before any member is ejected,
// ensuring all original batch members are still in survivors when fireRunawayGuard is called.
func TestMergeTrainRunawayGuard(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	// All batches always red — no member ever lands.
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return true })
	eng.cfg.MaxTrainTrialsPerWindow = 2
	eng.cfg.TrainTrialWindowDuration = time.Hour

	batch := makeSeamBatch(3)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := captureStdout(func() {
		eng.runMergeTrainWorker(ctx, state, "owner", "repo", batch)
	})

	// Guard must fire within the configured trial bound.
	if rv.count() > 2 {
		t.Errorf("guard must fire within N=2 trials, got %d trials", rv.count())
	}

	// Log must mention the runaway guard.
	if !strings.Contains(out, "runaway guard") {
		t.Errorf("expected 'runaway guard' in log output; stdout was:\n%s", out)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	// All 3 members must be paused + awaiting-input.
	for _, issueNum := range []int{1, 2, 3} {
		paused, awaiting := false, false
		for _, c := range client.addLabelCalls {
			if c.issueNumber == issueNum && c.labelName == "fabrik:paused" {
				paused = true
			}
			if c.issueNumber == issueNum && c.labelName == "fabrik:awaiting-input" {
				awaiting = true
			}
		}
		if !paused {
			t.Errorf("e2e: member #%d must have fabrik:paused (runaway guard)", issueNum)
		}
		if !awaiting {
			t.Errorf("e2e: member #%d must have fabrik:awaiting-input (runaway guard)", issueNum)
		}
		hasAlert := false
		for _, c := range client.addCommentCalls {
			if c.issueNumber == issueNum && strings.Contains(c.body, "runaway guard") {
				hasAlert = true
			}
		}
		if !hasAlert {
			t.Errorf("e2e: member #%d: expected runaway guard alert comment", issueNum)
		}
	}

	// No integration PR should be created (guard fires before landing).
	for _, c := range client.createPRCalls {
		if strings.Contains(c.title, "[merge-train] batch") {
			t.Errorf("e2e: integration landing PR must not be created when guard fires, got PR: %q", c.title)
		}
	}

	// mergeTrainInFlight must be cleared.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("e2e: expected mergeTrainInFlight cleared after runaway guard fires")
	}
}

// ── Generated-file conflict resolution (issue #1235, FR-1..FR-5) ─────────────

// pushMultiFileBranchToBare creates a branch in srcDir with several files written in a
// single commit, pushes it to bareDir, and returns the HEAD SHA. Unlike pushBranchToBare
// (single file), this lets a test construct a member whose conflict spans more than one
// path in a single merge step (needed for the FR-5 mixed-conflict scenario).
func pushMultiFileBranchToBare(t *testing.T, srcDir, bareDir, branchName string, files map[string]string) string {
	t.Helper()
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "checkout", "-b", branchName)
	for name, content := range files {
		writeFile(t, filepath.Join(srcDir, name), content)
	}
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "update "+branchName)
	mustGit(t, srcDir, "push", bareDir, branchName+":"+branchName)
	sha := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "branch", "-D", branchName)
	return sha
}

// TestMergeTrainWorker_GeneratedConflictRegeneratedWithoutClaude verifies FR-1/FR-2: a
// conflict confined entirely to a declared generated path is regenerated via the
// declared command and never reaches Claude.
func TestMergeTrainWorker_GeneratedConflictRegeneratedWithoutClaude(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Both branches independently add generated.txt with different content — an
	// add/add conflict confined to the declared generated path.
	sha1 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", "generated.txt", "stale-content-from-1\n")
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "generated.txt", "stale-content-from-2\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf 'regenerated-content\\n' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "generated-only-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, trialSHA, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("expected both members to survive, got %d", len(survivors))
	}
	if trialSHA == "" {
		t.Fatal("expected a non-empty trial SHA")
	}

	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("expected Claude never invoked for a generated-only conflict, got %d call(s)", len(claude.forCommentsCalls))
	}

	wtDir := wm.trainWorktreeDir(trialName)
	got, err := os.ReadFile(filepath.Join(wtDir, "generated.txt"))
	if err != nil {
		t.Fatalf("reading regenerated file: %v", err)
	}
	if string(got) != "regenerated-content\n" {
		t.Errorf("generated.txt content = %q, want %q", got, "regenerated-content\n")
	}

	if remaining, err := unmergedPaths(wtDir); err != nil {
		t.Fatalf("unmergedPaths: %v", err)
	} else if len(remaining) != 0 {
		t.Errorf("expected no remaining unmerged paths, got %v", remaining)
	}
}

// TestMergeTrainWorker_MixedGeneratedAndNormalConflict verifies FR-5: a conflict whose
// paths span both a declared generated file and a normal file still dispatches the
// non-generated portion to Claude, while the generated portion is regenerated instead
// of being handed to Claude — and regeneration runs only after Claude's part is staged.
func TestMergeTrainWorker_MixedGeneratedAndNormalConflict(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Member 1 touches only counter.txt and generated.txt cleanly relative to main —
	// its merge is clean (first member merged, nothing to conflict with yet).
	sha1 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", map[string]string{
		"counter.txt":   "counter-from-1\n",
		"generated.txt": "gen-from-1\n",
	})
	// Member 2 diverges from main on both files too — merging it into the trial
	// (which now has member 1's changes) conflicts on both counter.txt (non-generated,
	// modify/modify) and generated.txt (declared generated, add/add).
	sha2 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", map[string]string{
		"counter.txt":   "counter-from-2\n",
		"generated.txt": "gen-from-2\n",
	})

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	const resolvedCounter = "counter-from-1\ncounter-from-2\n"
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if len(comments) == 0 || !strings.Contains(comments[0].Body, "generated.txt") {
				t.Errorf("expected the synthetic conflict comment to name generated.txt as out of scope")
			}
			// Resolve only counter.txt — leave generated.txt's conflict markers alone,
			// stage only the resolved file, and do not commit (per the mixed-mode
			// instructions in buildTrainConflictComment).
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte(resolvedCounter), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write resolved counter.txt: %w", err)
			}
			addCmd := exec.Command("git", "add", "--", "counter.txt")
			addCmd.Dir = workDir
			if out, err := addCmd.CombinedOutput(); err != nil {
				return string(out), false, TokenUsage{}, nil
			}
			return "resolved non-generated part", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf 'gen-regenerated\\n' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "mixed-conflict-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, trialSHA, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("expected both members to survive, got %d", len(survivors))
	}
	if trialSHA == "" {
		t.Fatal("expected a non-empty trial SHA")
	}
	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected Claude invoked exactly once for the non-generated part, got %d", len(claude.forCommentsCalls))
	}

	wtDir := wm.trainWorktreeDir(trialName)

	gotCounter, err := os.ReadFile(filepath.Join(wtDir, "counter.txt"))
	if err != nil {
		t.Fatalf("reading counter.txt: %v", err)
	}
	if string(gotCounter) != resolvedCounter {
		t.Errorf("counter.txt content = %q, want %q", gotCounter, resolvedCounter)
	}

	gotGenerated, err := os.ReadFile(filepath.Join(wtDir, "generated.txt"))
	if err != nil {
		t.Fatalf("reading generated.txt: %v", err)
	}
	if string(gotGenerated) != "gen-regenerated\n" {
		t.Errorf("generated.txt content = %q, want %q (must be regenerated, not Claude-authored)", gotGenerated, "gen-regenerated\n")
	}

	if remaining, err := unmergedPaths(wtDir); err != nil {
		t.Fatalf("unmergedPaths: %v", err)
	} else if len(remaining) != 0 {
		t.Errorf("expected no remaining unmerged paths, got %v", remaining)
	}

	// The whole conflict (Claude's part + regeneration) must land as a single commit —
	// no merge left in progress.
	checkMergeHead := exec.Command("git", "rev-parse", "--verify", "MERGE_HEAD")
	checkMergeHead.Dir = wtDir
	if err := checkMergeHead.Run(); err == nil {
		t.Error("expected MERGE_HEAD to be gone (commit finalized) after mixed resolution")
	}
}

// TestMergeTrainWorker_DeletionConflictOnGeneratedPathRoutesToClaude guards against a
// review-flagged gap: classifyConflictedPaths must not treat a deletion-involving
// conflict (DD/UD/DU) on a declared generated path as eligible for regeneration.
// Without the status-code check, this scenario would skip Claude and have
// regenerateAndCommit silently recreate a file one contributor deleted — reproducing,
// via a side door, the exact class of bug this issue exists to prevent (a generated
// artefact's content diverging from what the batch's contributors actually intended).
func TestMergeTrainWorker_DeletionConflictOnGeneratedPathRoutesToClaude(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Establish generated.txt on main before branching, so member branches diverge from
	// a common base version — required to produce a delete/modify conflict rather than
	// the add/add conflict the other generated-path tests exercise.
	writeFile(t, filepath.Join(srcDir, "generated.txt"), "base-content\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "add generated.txt to main")
	mustGit(t, srcDir, "push", bareDir, "main:main")
	mustGitDir(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	// Member 1 deletes generated.txt — merges cleanly as the first member (no conflict
	// yet, trial simply applies the delete).
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "checkout", "-b", "fabrik/issue-1")
	mustGit(t, srcDir, "rm", "generated.txt")
	mustGit(t, srcDir, "commit", "-m", "delete generated.txt")
	mustGit(t, srcDir, "push", bareDir, "fabrik/issue-1:fabrik/issue-1")
	sha1 := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "branch", "-D", "fabrik/issue-1")

	// Member 2 modifies generated.txt relative to the same base — merging it into the
	// now-deleted trial state produces a delete/modify conflict.
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "generated.txt", "modified-content\n")

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if len(comments) == 0 {
				t.Fatal("expected a synthetic conflict comment")
			}
			// A deletion-involving conflict on a generated path is routed through the
			// plain (non-mixed) Claude path — the file isn't named as an out-of-scope
			// generated path, since there's no separate regeneration step deferring to;
			// Claude resolves and commits generated.txt exactly like any other conflict.
			if strings.Contains(comments[0].Body, "OUT OF SCOPE") {
				t.Errorf("expected the plain (non-mixed) conflict comment, got the mixed-mode out-of-scope variant: %s", comments[0].Body)
			}
			// Claude decides to honor the deletion — remove the file and commit, exactly
			// as it would for any other conflict it fully resolves.
			rmCmd := exec.Command("git", "rm", "-f", "generated.txt")
			rmCmd.Dir = workDir
			if out, err := rmCmd.CombinedOutput(); err != nil {
				return string(out), false, TokenUsage{}, nil
			}
			commitCmd := exec.Command("git", "commit", "--no-edit")
			commitCmd.Dir = workDir
			if out, err := commitCmd.CombinedOutput(); err != nil {
				return string(out), false, TokenUsage{}, nil
			}
			return "resolved by keeping the deletion", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf 'gen-regenerated\\n' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "deletion-conflict-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, trialSHA, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("expected both members to survive, got %d", len(survivors))
	}
	if trialSHA == "" {
		t.Fatal("expected a non-empty trial SHA")
	}
	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected Claude invoked once to resolve the deletion conflict, got %d — a deletion-involving conflict on a generated path must never be silently regenerated", len(claude.forCommentsCalls))
	}

	wtDir := wm.trainWorktreeDir(trialName)
	if _, err := os.Stat(filepath.Join(wtDir, "generated.txt")); !os.IsNotExist(err) {
		t.Errorf("expected generated.txt to remain deleted per Claude's resolution, got err=%v", err)
	}
}

// TestResolveTrainConflict_UnmergedPathsErrorFallsBackToPlainClaude documents and locks
// in a narrow, accepted-tradeoff fallback flagged in review: when unmergedPaths itself
// errors (e.g. a transient `git status --porcelain` failure) before conflicted paths can
// be classified against the generated-file set, resolveTrainConflict falls back to
// dispatching Claude for the full, unscoped conflict — exactly the pre-#1235 behavior,
// including for a conflict that might in fact be confined to a declared generated path.
// This is intentional (see adrs/1235-generated-file-regeneration-in-merge-train.md) since
// a git-level failure here says nothing about the conflict's shape, but the fallback
// branch itself previously had no test coverage.
func TestResolveTrainConflict_UnmergedPathsErrorFallsBackToPlainClaude(t *testing.T) {
	skipIfNoGit(t)
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if len(comments) == 0 || strings.Contains(comments[0].Body, "OUT OF SCOPE") {
				t.Errorf("expected the plain (non-mixed) conflict comment when classification could not run")
			}
			return "no-op", false, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, nil)

	// A plain (non-git) directory makes `git status --porcelain` fail immediately — the
	// exact unmergedPaths error resolveTrainConflict must fall back on, without ever
	// attempting to classify conflicted paths against the generated set.
	wtDir := t.TempDir()

	_, reason, err := eng.resolveTrainConflict(context.Background(), makeTrainItem(1, "Issue 1"), wtDir, holdingStage(eng.cfg), "deadbeef", "deadbeef", InvokeOptions{})
	if err != nil {
		t.Fatalf("resolveTrainConflict: %v", err)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty (caller falls back to its own generic ejection message)", reason)
	}
	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected Claude invoked once via the unmergedPaths-error fallback path, got %d", len(claude.forCommentsCalls))
	}
}

// TestMergeTrainWorker_RegenerationFailureEjectsMember verifies FR-4: when the declared
// regeneration command fails, the member is ejected with a diagnosable reason rather than
// falling back to Claude for textual resolution.
func TestMergeTrainWorker_RegenerationFailureEjectsMember(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", "generated.txt", "stale-content-from-1\n")
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "generated.txt", "stale-content-from-2\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "exit 1"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "regen-failure-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 1 || survivors[0].item.Number != 1 {
		t.Fatalf("expected only member #1 to survive, got %+v", survivors)
	}

	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("expected Claude never invoked when regeneration fails (no textual fallback), got %d call(s)", len(claude.forCommentsCalls))
	}

	var ejectionBody string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 2 {
			ejectionBody = c.body
		}
	}
	if ejectionBody == "" {
		t.Fatal("expected an ejection comment on issue #2")
	}
	if !strings.Contains(ejectionBody, "regeneration command") {
		t.Errorf("expected a diagnosable regeneration-failure reason in the ejection comment, got: %s", ejectionBody)
	}
}

// TestMergeTrainWorker_CancelledContextAbortsRegeneration guards against a review-flagged
// gap: regenerateAndCommit derived its regeneration command's timeout from
// context.Background() rather than the caller's ctx, so a worker-level cancellation
// (e.g. graceful shutdown) would not stop an in-flight regeneration command promptly —
// it would keep running for up to the full regenerationCommandTimeout regardless. Fixed
// by deriving the command's context from ctx via context.WithTimeout(ctx, ...). This
// test passes an already-cancelled ctx into assembleTrialBranch and confirms the
// regeneration command is never actually started (exec.CommandContext returns the
// context's error immediately) and the member is ejected with a reason identifying
// caller cancellation, not a generic failure or a 5-minute wait.
func TestMergeTrainWorker_CancelledContextAbortsRegeneration(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", "generated.txt", "stale-content-from-1\n")
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "generated.txt", "stale-content-from-2\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf 'should-never-run\\n' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "cancelled-ctx-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	survivors, _, err := eng.assembleTrialBranch(ctx, p, members, trialName)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("expected near-instant abort on an already-cancelled ctx, took %s (regenerationCommandTimeout is 5m — the fix must not silently wait it out)", elapsed)
	}
	if len(survivors) != 1 || survivors[0].item.Number != 1 {
		t.Fatalf("expected only member #1 to survive, got %+v", survivors)
	}

	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("expected Claude never invoked, got %d call(s)", len(claude.forCommentsCalls))
	}

	var ejectionBody string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 2 {
			ejectionBody = c.body
		}
	}
	if ejectionBody == "" {
		t.Fatal("expected an ejection comment on issue #2")
	}
	if !strings.Contains(ejectionBody, "caller cancellation") {
		t.Errorf("expected the ejection reason to identify caller cancellation, got: %s", ejectionBody)
	}
}

// TestMergeTrainWorker_ClaudePrematureCommitInMixedModeEjectsMember guards against the
// compliance risk flagged in ADR-1235: in the mixed case, Claude is instructed to leave
// the generated path untouched and not commit, but nothing stops it from ignoring that
// (e.g. running `git add -A && git commit` out of habit). If it does, the generated
// path's conflict markers get committed as "resolved" content, and regenerateAndCommit
// must detect that its own regenerated content is left staged but uncommitted afterward
// rather than silently reporting success.
func TestMergeTrainWorker_ClaudePrematureCommitInMixedModeEjectsMember(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", map[string]string{
		"counter.txt":   "counter-from-1\n",
		"generated.txt": "gen-from-1\n",
	})
	sha2 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", map[string]string{
		"counter.txt":   "counter-from-2\n",
		"generated.txt": "gen-from-2\n",
	})

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Violates the mixed-mode instructions: resolves counter.txt correctly, but
			// then stages *everything* (including the still-conflicted generated.txt)
			// and commits — exactly the compliance failure ADR-1235 flags as a risk.
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte("counter-from-1\ncounter-from-2\n"), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write resolved counter.txt: %w", err)
			}
			mustGit(t, workDir, "add", "-A")
			mustGit(t, workDir, "commit", "--no-edit", "-m", "premature commit despite instructions")
			return "resolved and committed (violating instructions)", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf 'gen-regenerated\\n' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "premature-commit-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 1 || survivors[0].item.Number != 1 {
		t.Fatalf("expected only member #1 to survive (member #2 ejected for the premature commit), got %+v", survivors)
	}
}

// TestMergeTrainWorker_ClaudeAbortMasqueradesAsResolved guards against a false-positive
// "resolved" verdict: buildTrainConflictComment's fallback instructions tell Claude to
// run `git merge --abort` when it judges a conflict unresolvable. An abort clears every
// conflict marker (and MERGE_HEAD) exactly as a genuine resolution would, so checking
// "no conflict markers remain" alone can't tell the two apart — without the preMergeHEAD
// guard, the member would be reported as a survivor even though its entire contribution
// was silently discarded by the abort. This exercises the plain (no generated paths)
// path, but the same ambiguity applied equally to the FR-5 mixed path before the guard.
func TestMergeTrainWorker_ClaudeAbortMasqueradesAsResolved(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", "counter.txt", "from-branch-1\n")
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "counter.txt", "from-branch-2\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Per buildTrainConflictComment's own fallback instructions: abort rather
			// than resolve. This leaves the worktree exactly as it was before the merge
			// was attempted — no conflict markers, no MERGE_HEAD, HEAD unchanged.
			mustGit(t, workDir, "merge", "--abort")
			return "conflict not safely resolvable, aborted", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "abort-masquerade-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 1 || survivors[0].item.Number != 1 {
		t.Fatalf("expected only member #1 to survive (member #2's abort must not count as resolved), got %+v", survivors)
	}
}

// TestMergeTrainWorker_EjectionAfterPrematureCommitDoesNotContaminateLaterMembers guards
// against a poisoned wtDir HEAD surviving an ejection. When regenerateAndCommit's
// premature-commit guard trips (Claude committed despite mixed-mode instructions not
// to), the ejection path's `git merge --abort` is a silent no-op — MERGE_HEAD is already
// gone by then — so without an explicit reset to the pre-merge SHA, the ejected member's
// bad commit (containing literal, unresolved conflict-marker text for the generated
// path) would remain as wtDir's HEAD and contaminate every later member's merge. This
// exercises a 3-member batch with the poisoned member in the middle, so a subsequent
// member's merge is actually attempted on top of the (correctly cleaned) worktree.
func TestMergeTrainWorker_EjectionAfterPrematureCommitDoesNotContaminateLaterMembers(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Member 1: clean merge, establishes counter.txt / generated.txt at known content.
	sha1 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", map[string]string{
		"counter.txt":   "counter-from-1\n",
		"generated.txt": "gen-from-1\n",
	})
	// Member 2: conflicts with member 1 on both counter.txt (non-generated) and
	// generated.txt (declared generated) — the mixed case. Its mock Claude invocation
	// violates the "don't commit" instruction, poisoning wtDir's HEAD if not cleaned up.
	sha2 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", map[string]string{
		"counter.txt":   "counter-from-2\n",
		"generated.txt": "gen-from-2\n",
	})
	// Member 3: independent of both — touches only unrelated.txt, based on main. Merges
	// cleanly against member 1's state IFF wtDir was actually reset after member 2's
	// ejection; if member 2's poisoned commit or dirty index survived, this merge either
	// fails outright or silently carries that contamination forward.
	sha3 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-3", "unrelated.txt", "from-3\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if issue.Number != 2 {
				t.Fatalf("expected Claude only invoked for member #2's mixed conflict, got #%d", issue.Number)
			}
			// Violates the mixed-mode instructions: resolves counter.txt correctly, but
			// then stages *everything* (including the still-conflicted generated.txt)
			// and commits — the exact compliance failure this test guards against.
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte("counter-from-1\ncounter-from-2\n"), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write resolved counter.txt: %w", err)
			}
			mustGit(t, workDir, "add", "-A")
			mustGit(t, workDir, "commit", "--no-edit", "-m", "premature commit despite instructions")
			return "resolved and committed (violating instructions)", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf 'gen-regenerated\\n' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
		{item: makeTrainItem(3, "Issue 3"), prNum: 12, headSHA: sha3},
	}
	const trialName = "ejection-no-contamination-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 || survivors[0].item.Number != 1 || survivors[1].item.Number != 3 {
		t.Fatalf("expected members #1 and #3 to survive (#2 ejected), got %+v", survivors)
	}

	wtDir := wm.trainWorktreeDir(trialName)

	// The worktree must be fully clean — no leftover staged/unmerged state from #2.
	if remaining, err := unmergedPaths(wtDir); err != nil {
		t.Fatalf("unmergedPaths: %v", err)
	} else if len(remaining) != 0 {
		t.Errorf("expected no remaining unmerged paths after ejection+cleanup, got %v", remaining)
	}
	diffCachedCmd := exec.Command("git", "diff", "--cached", "--quiet")
	diffCachedCmd.Dir = wtDir
	if err := diffCachedCmd.Run(); err != nil {
		t.Error("expected a clean index after ejection+cleanup, but staged changes remain")
	}

	// counter.txt and generated.txt must reflect only member #1's contribution — no
	// trace of member #2's conflict-marker content or premature commit.
	gotCounter, err := os.ReadFile(filepath.Join(wtDir, "counter.txt"))
	if err != nil {
		t.Fatalf("reading counter.txt: %v", err)
	}
	if string(gotCounter) != "counter-from-1\n" {
		t.Errorf("counter.txt content = %q, want %q (member #2's contribution must not survive ejection)", gotCounter, "counter-from-1\n")
	}
	gotGenerated, err := os.ReadFile(filepath.Join(wtDir, "generated.txt"))
	if err != nil {
		t.Fatalf("reading generated.txt: %v", err)
	}
	if string(gotGenerated) != "gen-from-1\n" {
		t.Errorf("generated.txt content = %q, want %q (member #2's poisoned commit must not survive ejection)", gotGenerated, "gen-from-1\n")
	}

	// Member #3's contribution must have landed cleanly on top of the reset state.
	gotUnrelated, err := os.ReadFile(filepath.Join(wtDir, "unrelated.txt"))
	if err != nil {
		t.Fatalf("reading unrelated.txt: %v", err)
	}
	if string(gotUnrelated) != "from-3\n" {
		t.Errorf("unrelated.txt content = %q, want %q", gotUnrelated, "from-3\n")
	}

	// No trace of member #2's premature commit message anywhere in wtDir's history.
	logOut := gitOutputDir(t, wtDir, "log", "--all", "--oneline")
	if strings.Contains(logOut, "premature commit despite instructions") {
		t.Error("expected member #2's premature commit to be unreachable from wtDir's HEAD after ejection")
	}
}

// TestMergeTrainWorker_UntrackedFileLeftByEjectedMemberDoesNotBlockNextMerge guards
// against a narrower gap than the premature-commit contamination case above: `git reset
// --hard preMergeHEAD` only rewinds tracked content — it does not remove untracked files
// a failed conflict-resolution attempt may have left behind in wtDir. Before this fix, a
// stray untracked file surviving an ejection would make the next member's `git merge`
// fail with git's own "untracked working tree file would be overwritten by merge" error
// instead of merging cleanly — and since that failure has no MERGE_HEAD and no unmerged
// paths, resolveTrainConflict would misclassify it as a conflict with nothing generated
// involved and dispatch Claude against a worktree with no conflict markers to resolve.
func TestMergeTrainWorker_UntrackedFileLeftByEjectedMemberDoesNotBlockNextMerge(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Members 1 and 2 both modify counter.txt — member 1 merges cleanly, member 2 conflicts
	// and is ejected (Claude fails to resolve).
	sha1 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", "counter.txt", "branch1-value\n")
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "counter.txt", "branch2-value\n")
	// Member 3 is independent of the conflict and adds a *new tracked* file at the same
	// path member #2's Claude session leaves *untracked* below — this is the merge that
	// would fail pre-fix.
	sha3 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-3", "stray.txt", "from-3\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if issue.Number != 2 {
				t.Fatalf("expected Claude only invoked for member #2's conflict, got #%d — member #3 should merge cleanly without any conflict resolution", issue.Number)
			}
			// Leaves a stray untracked file, then fails to resolve the conflict —
			// simulates a scratch file left behind by an unsuccessful resolution attempt.
			if err := os.WriteFile(filepath.Join(workDir, "stray.txt"), []byte("leftover\n"), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write stray.txt: %w", err)
			}
			return "unable to resolve", false, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
		{item: makeTrainItem(3, "Issue 3"), prNum: 12, headSHA: sha3},
	}
	const trialName = "untracked-cleanup-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 || survivors[0].item.Number != 1 || survivors[1].item.Number != 3 {
		t.Fatalf("expected members #1 and #3 to survive (#2 ejected), got %+v", survivors)
	}

	wtDir := wm.trainWorktreeDir(trialName)
	gotStray, err := os.ReadFile(filepath.Join(wtDir, "stray.txt"))
	if err != nil {
		t.Fatalf("reading stray.txt: %v", err)
	}
	if string(gotStray) != "from-3\n" {
		t.Errorf("stray.txt content = %q, want %q (member #3's tracked file must win over member #2's untracked leftover)", gotStray, "from-3\n")
	}
}

// TestMergeTrainWorker_SharedCommandStagesAllDeclaredPaths guards against a gap in
// regenerateAndCommit's staging: when multiple declared generatedFileSpec entries share
// a single regeneration Command (exactly the case the command-level dedup exists to
// support), running that command once regenerates every path it's responsible for as a
// side effect — not just the conflicted subset passed in as `specs` (the `matched`
// argument from resolveTrainConflict). Before this fix, only the conflicted paths were
// staged, leaving a non-conflicted sibling path's on-disk regeneration as an unstaged
// (or, for a not-yet-tracked path, untracked) working-tree change that would survive
// into the next member's merge in the same trial worktree.
func TestMergeTrainWorker_SharedCommandStagesAllDeclaredPaths(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Members 1 and 2 both add generated-a.txt with different content — an add/add
	// conflict confined to generated-a.txt. generated-b.txt is untouched by either
	// member and doesn't exist anywhere yet; it's produced only as a side effect of the
	// shared regeneration command below.
	sha1 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", "generated-a.txt", "branch1-a\n")
	sha2 := pushBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", "generated-a.txt", "branch2-a\n")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			t.Fatalf("expected Claude never invoked — conflict is confined to declared generated paths")
			return "", false, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	// Both declared paths share one command that writes deterministic content to both,
	// so regenerating generated-a.txt (the only conflicted/matched path) also rewrites
	// generated-b.txt on disk as a side effect.
	sharedCmd := []string{"bash", "-c", "printf 'regen-a\\n' > generated-a.txt; printf 'regen-b\\n' > generated-b.txt"}
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated-a.txt", Command: sharedCmd},
		{Path: "generated-b.txt", Command: sharedCmd},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "shared-command-stage-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("expected both members to survive (regeneration resolves the conflict), got %+v", survivors)
	}

	wtDir := wm.trainWorktreeDir(trialName)

	// generated-b.txt must be committed alongside generated-a.txt, not left as an
	// unstaged/untracked working-tree change.
	statusOut := gitOutputDir(t, wtDir, "status", "--porcelain")
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("expected a clean working tree after regeneration, got status:\n%s", statusOut)
	}

	gotB, err := os.ReadFile(filepath.Join(wtDir, "generated-b.txt"))
	if err != nil {
		t.Fatalf("reading generated-b.txt: %v", err)
	}
	if string(gotB) != "regen-b\n" {
		t.Errorf("generated-b.txt content = %q, want %q", gotB, "regen-b\n")
	}

	// generated-b.txt must actually be part of HEAD, not just present in the working tree.
	lsTreeOut := gitOutputDir(t, wtDir, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(lsTreeOut, "generated-b.txt") {
		t.Error("expected generated-b.txt to be committed as part of HEAD")
	}
}

// TestMergeTrainWorker_SharedCommandDoesNotOverwriteDeletionExcludedSibling guards
// against a review-flagged interaction between two of this PR's own fixes: the
// shared-command staging fix above (stage every declared path tied to an executed
// command, since running the command regenerates all of them as a side effect) and the
// deletion-involving-status fix (route a generated path conflicted via DD/UD/DU to
// Claude instead of regeneration). If a matched path and a deletion-excluded sibling
// share one command, naively applying the first fix would stage the sibling's
// regenerated content too — silently discarding Claude's deletion-aware resolution by
// way of a command it merely happens to share with an unrelated matched path.
func TestMergeTrainWorker_SharedCommandDoesNotOverwriteDeletionExcludedSibling(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	// Establish both declared paths on main so member branches diverge from a common
	// base version.
	writeFile(t, filepath.Join(srcDir, "generated-a.txt"), "base-a\n")
	writeFile(t, filepath.Join(srcDir, "generated-b.txt"), "base-b\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "add generated-a.txt and generated-b.txt to main")
	mustGit(t, srcDir, "push", bareDir, "main:main")
	mustGitDir(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	// Member 1 modifies generated-a.txt and deletes generated-b.txt — merges cleanly as
	// the first member (no conflict yet, trial simply applies both changes).
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "checkout", "-b", "fabrik/issue-1")
	writeFile(t, filepath.Join(srcDir, "generated-a.txt"), "a-from-1\n")
	mustGit(t, srcDir, "rm", "generated-b.txt")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "modify generated-a.txt, delete generated-b.txt")
	mustGit(t, srcDir, "push", bareDir, "fabrik/issue-1:fabrik/issue-1")
	sha1 := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	mustGit(t, srcDir, "checkout", "main")
	mustGit(t, srcDir, "branch", "-D", "fabrik/issue-1")

	// Member 2 diverges from the same base, modifying both files — merging it into the
	// trial (which now has member 1's modify of A and deletion of B) produces a
	// modify/modify conflict on A (UU, matched) and a delete/modify conflict on B (DU,
	// deletion-excluded) in the very same merge.
	sha2 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", map[string]string{
		"generated-a.txt": "a-from-2\n",
		"generated-b.txt": "b-from-2\n",
	})

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if len(comments) == 0 || !strings.Contains(comments[0].Body, "generated-a.txt") {
				t.Errorf("expected the mixed-mode comment to name generated-a.txt as out of scope")
			}
			// Claude honors the deletion for generated-b.txt: stage the removal, don't
			// touch generated-a.txt, don't commit (per the mixed-mode instructions).
			rmCmd := exec.Command("git", "rm", "-f", "generated-b.txt")
			rmCmd.Dir = workDir
			if out, err := rmCmd.CombinedOutput(); err != nil {
				return string(out), false, TokenUsage{}, nil
			}
			return "resolved generated-b.txt by keeping the deletion", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	// Both declared paths share one command, so regenerating generated-a.txt (the
	// matched path) also rewrites generated-b.txt on disk as a side effect — exactly
	// the interaction this test guards against.
	sharedCmd := []string{"bash", "-c", "printf 'regen-a\\n' > generated-a.txt; printf 'regen-b\\n' > generated-b.txt"}
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated-a.txt", Command: sharedCmd},
		{Path: "generated-b.txt", Command: sharedCmd},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "shared-command-deletion-exclusion-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("expected both members to survive, got %+v", survivors)
	}
	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected Claude invoked once to resolve generated-b.txt's deletion conflict, got %d", len(claude.forCommentsCalls))
	}

	wtDir := wm.trainWorktreeDir(trialName)

	gotA, err := os.ReadFile(filepath.Join(wtDir, "generated-a.txt"))
	if err != nil {
		t.Fatalf("reading generated-a.txt: %v", err)
	}
	if string(gotA) != "regen-a\n" {
		t.Errorf("generated-a.txt content = %q, want %q (regenerated)", gotA, "regen-a\n")
	}

	if _, err := os.Stat(filepath.Join(wtDir, "generated-b.txt")); !os.IsNotExist(err) {
		t.Errorf("expected generated-b.txt to remain deleted per Claude's resolution, but the shared command's regeneration side effect resurrected it (err=%v)", err)
	}

	lsTreeOut := gitOutputDir(t, wtDir, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(lsTreeOut, "generated-b.txt") {
		t.Error("expected generated-b.txt to NOT be part of HEAD — its deletion must survive the shared regeneration command")
	}

	statusOut := gitOutputDir(t, wtDir, "status", "--porcelain")
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("expected a clean working tree after resolution, got status:\n%s", statusOut)
	}
}

// TestMergeTrainWorker_ByteIdenticalPrematureCommitStillEjectsMember closes a blind spot
// in the premature-commit guard: if Claude's non-compliant commit happens to write
// byte-identical content to what the declared regeneration command would produce, a
// content-diff-based check (`git diff --cached --quiet`) sees no difference and can't
// tell the premature commit apart from a legitimate one. regenerateAndCommit now checks
// MERGE_HEAD structurally at entry, before running any regeneration or content
// comparison, so this is caught regardless of what the premature commit's content is.
func TestMergeTrainWorker_ByteIdenticalPrematureCommitStillEjectsMember(t *testing.T) {
	skipIfNoGit(t)
	bareDir, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-1", map[string]string{
		"counter.txt":   "counter-from-1\n",
		"generated.txt": "gen-from-1\n",
	})
	sha2 := pushMultiFileBranchToBare(t, srcDir, bareDir, "fabrik/issue-2", map[string]string{
		"counter.txt":   "counter-from-2\n",
		"generated.txt": "gen-from-2\n",
	})

	baseSHA := strings.TrimSpace(gitOutputDir(t, bareDir, "rev-parse", "refs/remotes/origin/main"))

	const regeneratedContent = "gen-regenerated\n"
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Violates the mixed-mode instructions: writes generated.txt with the exact
			// content the declared regen command would produce, then commits — the
			// worst case for a content-based guard, since the committed content is
			// indistinguishable from a correct regeneration.
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte("counter-from-1\ncounter-from-2\n"), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write resolved counter.txt: %w", err)
			}
			if err := os.WriteFile(filepath.Join(workDir, "generated.txt"), []byte(regeneratedContent), 0644); err != nil {
				return "", false, TokenUsage{}, fmt.Errorf("write byte-identical generated.txt: %w", err)
			}
			mustGit(t, workDir, "add", "-A")
			mustGit(t, workDir, "commit", "--no-edit", "-m", "premature commit with byte-identical content")
			return "resolved and committed (violating instructions)", true, TokenUsage{}, nil
		},
	}
	eng := trainTestEngine(t, &mockGitHubClient{}, claude, wm)
	eng.generatedFilesOverride = []generatedFileSpec{
		{Path: "generated.txt", Command: []string{"bash", "-c", "printf '" + regeneratedContent + "' > generated.txt"}},
	}

	p := trialParams{
		owner:      "owner",
		repo:       "repo",
		baseBranch: "main",
		baseSHA:    baseSHA,
		wm:         wm,
		holdingStg: holdingStage(eng.cfg),
	}
	members := []trainMember{
		{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: sha1},
		{item: makeTrainItem(2, "Issue 2"), prNum: 11, headSHA: sha2},
	}
	const trialName = "byte-identical-premature-commit-trial"
	defer wm.CleanupTrainWorktree(trialName, true)

	survivors, _, err := eng.assembleTrialBranch(context.Background(), p, members, trialName)
	if err != nil {
		t.Fatalf("assembleTrialBranch: %v", err)
	}
	if len(survivors) != 1 || survivors[0].item.Number != 1 {
		t.Fatalf("expected only member #1 to survive (member #2 ejected despite byte-identical content), got %+v", survivors)
	}
}

// TestMergeTrainMaxTurnsOverride_UsesCommentMaxTurnsBase covers #1472 (found by
// handarbeit-pruefer[bot] reviewing #1206/PR #1467): the extend-turns pre-grant for
// resolveConflictWithClaude's InvokeForComments call must be based on the same
// commentMaxTurns(stage) that scaledWallTime (engine/claude.go) divides by when scaling
// max_wall_time, not on stage.MaxTurns — otherwise the two bases disagree and the
// effective wall-time multiplier silently diverges from the intended 2x.
func TestMergeTrainMaxTurnsOverride_UsesCommentMaxTurnsBase(t *testing.T) {
	tests := []struct {
		name        string
		stage       *stages.Stage
		extendTurns bool
		want        int
	}{
		{
			name:        "no extend-turns label -> no override",
			stage:       &stages.Stage{MaxTurns: 100, CommentMaxTurns: 50},
			extendTurns: false,
			want:        0,
		},
		{
			name:        "differing max_turns/comment_max_turns (this repo's own configs) -> scales off comment_max_turns, not max_turns",
			stage:       &stages.Stage{MaxTurns: 100, CommentMaxTurns: 50},
			extendTurns: true,
			want:        100, // 2 * commentMaxTurns(50), NOT 2 * MaxTurns(100) = 200
		},
		{
			name:        "no explicit comment_max_turns -> falls back to MaxTurns as the base",
			stage:       &stages.Stage{MaxTurns: 100},
			extendTurns: true,
			want:        200, // commentMaxTurns falls back to stage.MaxTurns per commentMaxTurns()
		},
		{
			name:        "unlimited stage (MaxTurns 0, no CommentMaxTurns) -> commentMaxTurns falls back to 50 default -> still scales",
			stage:       &stages.Stage{},
			extendTurns: true,
			want:        100, // commentMaxTurns() defaults to 50 when both are 0
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeTrainMaxTurnsOverride(tt.stage, tt.extendTurns)
			if got != tt.want {
				t.Errorf("mergeTrainMaxTurnsOverride(%+v, extendTurns=%v) = %d, want %d", tt.stage, tt.extendTurns, got, tt.want)
			}
		})
	}
}
