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

	// FR-1: integration PR created with correct title, body marker, and no Closes #N.
	expectedTitle := "[merge-train] batch: #1, #2"
	if createPRTitle != expectedTitle {
		t.Errorf("integration PR title: got %q, want %q", createPRTitle, expectedTitle)
	}
	if !strings.Contains(createPRBody, mergeTrainBatchMarker) {
		t.Errorf("integration PR body missing batch marker %q", mergeTrainBatchMarker)
	}
	if strings.Contains(createPRBody, "Closes #") {
		t.Errorf("integration PR body must not contain 'Closes #N'")
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
	closedPRs := make([]int, len(client.closeIssueCalls))
	for i, c := range client.closeIssueCalls {
		closedPRs[i] = c.issueNumber
	}
	comments := client.addCommentCalls
	client.mu.Unlock()

	if len(advancedItems) != 2 {
		t.Errorf("expected 2 board status updates (Queued→Done), got %d", len(advancedItems))
	}

	// FR-3: member PRs closed.
	if len(closedPRs) != 2 {
		t.Errorf("expected 2 member PRs closed, got %d", len(closedPRs))
	}
	// Verify it's the member PRs (10 and 11), not the integration PR (100).
	for _, prNum := range closedPRs {
		if prNum != 10 && prNum != 11 {
			t.Errorf("unexpected PR closed: #%d (expected 10 or 11)", prNum)
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
	if closed != 1 {
		t.Errorf("expected 1 member PR close (FR-3), got %d", closed)
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
	if len(closed) != 1 || closed[0].issueNumber != 11 {
		t.Errorf("expected only member PR #11 closed, got %v", closed)
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
