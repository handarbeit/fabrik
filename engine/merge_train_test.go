package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// trainTestEngine builds an Engine wired with the given mock client and invoker.
// It configures a Queued holding stage and a Done stage for merge-train use, and sets
// a short CIWaitTimeout so CI-polling tests terminate quickly. statusField is pre-set
// so advanceToNextStage can advance members from Queued to Done in landing tests.
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
			CIWaitTimeout:          100 * time.Millisecond, // fast timeout for tests
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
	eng.statusField = &gh.StatusField{
		FieldID: "sf-test-1",
		Options: map[string]string{
			"Done":   "opt-done",
			"Queued": "opt-queued",
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
	result := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
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
	result := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
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
				{Name: "build", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIRed {
		t.Errorf("expected TrainCIRed for failed check run, got %v", result)
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
	result := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
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
			CIWaitTimeout:          1 * time.Millisecond, // expires during first API call
			Stages: []*stages.Stage{
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
			},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIPending {
		t.Errorf("expected TrainCIPending on CIWaitTimeout, got %v", result)
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
	result := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
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
	eng.ejectMember(context.Background(), "owner", "repo", member, "conflict with #2")

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

func TestEjectMember_PausesAfterMaxEjections(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	// MaxMergeTrainEjections = 3

	member := makeTrainItem(5, "Problem Issue")
	ctx := context.Background()

	// First two ejections should not add pause labels.
	eng.ejectMember(ctx, "owner", "repo", member, "conflict")
	eng.ejectMember(ctx, "owner", "repo", member, "conflict")

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
	eng.ejectMember(ctx, "owner", "repo", member, "conflict")

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
	ctx := context.Background()

	// Eject member 1 three times and member 2 once.
	eng.ejectMember(ctx, "owner", "repo", member1, "conflict")
	eng.ejectMember(ctx, "owner", "repo", member1, "conflict")
	eng.ejectMember(ctx, "owner", "repo", member1, "conflict") // triggers pause for #1
	eng.ejectMember(ctx, "owner", "repo", member2, "conflict") // should NOT trigger pause for #2

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
	if state.CIResult != TrainCIGreen {
		t.Errorf("expected TrainCIGreen, got %v", state.CIResult)
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

	// FR-4: mergeTrainInFlight cleared (cleanup ran).
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight to be cleared after successful landing")
	}
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

	// Cleanup must still run (mergeTrainInFlight cleared).
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight to be cleared after merge failure (cleanup deferred)")
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
}

func (rv *recordingValidator) fn(_ context.Context, members []trainMember) TrainCIResult {
	present := make(map[int]bool, len(members))
	nums := make([]int, 0, len(members))
	for _, m := range members {
		present[m.item.Number] = true
		nums = append(nums, m.item.Number)
	}
	rv.mu.Lock()
	rv.calls = append(rv.calls, nums)
	rv.mu.Unlock()
	if rv.redWhen(present) {
		return TrainCIRed
	}
	return TrainCIGreen
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

	eng.dissolveBatch(context.Background(), state, p, 200, "merge-train-main-1", members, "the base branch advanced")

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
	// In-flight marker cleared.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after dissolve")
	}
}

// TestDissolveBatch_NoPR verifies dissolve is a no-op on the PR close when prNum==0
// (an orphaned trial branch with no integration PR) yet still comments and clears.
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

	eng.dissolveBatch(context.Background(), state, p, 0, "merge-train-main-1", []gh.ProjectItem{makeTrainItem(1, "One")}, "orphan")

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("dissolve with prNum==0 must not close any PR, got %v", client.closeIssueCalls)
	}
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after dissolve")
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
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after landing")
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
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after dissolve")
	}
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
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after complete-deferred landing")
	}
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
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected mergeTrainInFlight cleared after orphan dissolve")
	}
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

	eng.trainValidateFn = func(_ context.Context, _ []trainMember) TrainCIResult {
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
		return TrainCIGreen
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
