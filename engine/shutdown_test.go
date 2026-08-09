package engine

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// TestWaitGroupTimeout_CompletesInTime verifies waitGroupTimeout returns true
// when the WaitGroup completes before the deadline.
func TestWaitGroupTimeout_CompletesInTime(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		wg.Done()
	}()

	start := time.Now()
	ok := waitGroupTimeout(&wg, 5*time.Second)
	elapsed := time.Since(start)

	if !ok {
		t.Error("waitGroupTimeout: expected true (completed in time), got false")
	}
	if elapsed > time.Second {
		t.Errorf("waitGroupTimeout took %v, expected near-immediate return once wg.Done() fires", elapsed)
	}
}

// TestWaitGroupTimeout_TimesOut verifies waitGroupTimeout returns false, and
// returns promptly, when the WaitGroup never completes within the deadline —
// the exact "worker that does not exit on SIGINT/SIGTERM" scenario the
// Verification Note requires AC3's test to exercise.
func TestWaitGroupTimeout_TimesOut(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1) // never Done() — simulates a stuck worker

	start := time.Now()
	ok := waitGroupTimeout(&wg, 100*time.Millisecond)
	elapsed := time.Since(start)

	if ok {
		t.Error("waitGroupTimeout: expected false (timed out), got true")
	}
	if elapsed > time.Second {
		t.Errorf("waitGroupTimeout took %v, expected to return near the 100ms deadline, not hang", elapsed)
	}
}

// seedInFlightIssue registers repo#number in the store as carrying an active
// Worker for stageName — the shape runShutdownPause enumerates via
// snap.Worker() != nil.
func seedInFlightIssue(eng *Engine, repo string, number int, stageName string) {
	eng.store.Apply(itemstate.WorkerEntered{Repo: repo, Number: number, StageName: stageName, StartedAt: time.Now()})
}

// TestRunShutdownPause_PausesInFlightIssues_AC1 verifies that every issue with
// an active Worker in the store gets fabrik:paused + fabrik:awaiting-input
// applied and stage:<Name>:in_progress removed, asserted against the mock
// GitHub client's call log (the API layer), not in-memory state, per AC1.
func TestRunShutdownPause_PausesInFlightIssues_AC1(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	seedInFlightIssue(eng, "owner/repo", 10, "Implement")
	seedInFlightIssue(eng, "owner/repo", 20, "Research")

	eng.wg.Add(1)
	eng.runShutdownPause()

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, tc := range []struct {
		number int
		stage  string
	}{
		{10, "Implement"},
		{20, "Research"},
	} {
		var gotPaused, gotAwaitingInput, gotInProgressRemoved bool
		for _, c := range client.addLabelCalls {
			if c.issueNumber != tc.number {
				continue
			}
			if c.labelName == "fabrik:paused" {
				gotPaused = true
			}
			if c.labelName == "fabrik:awaiting-input" {
				gotAwaitingInput = true
			}
		}
		for _, c := range client.removeLabelCalls {
			if c.issueNumber == tc.number && c.labelName == fmt.Sprintf("stage:%s:in_progress", tc.stage) {
				gotInProgressRemoved = true
			}
		}
		if !gotPaused {
			t.Errorf("issue #%d: expected fabrik:paused to be applied", tc.number)
		}
		if !gotAwaitingInput {
			t.Errorf("issue #%d: expected fabrik:awaiting-input to be applied", tc.number)
		}
		if !gotInProgressRemoved {
			t.Errorf("issue #%d: expected stage:%s:in_progress to be removed", tc.number, tc.stage)
		}
	}
}

// TestRunShutdownPause_PostsAuditComment_AC2 verifies each paused issue
// carries exactly one audit comment naming the shutdown.
func TestRunShutdownPause_PostsAuditComment_AC2(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	seedInFlightIssue(eng, "owner/repo", 30, "Validate")

	eng.wg.Add(1)
	eng.runShutdownPause()

	client.mu.Lock()
	defer client.mu.Unlock()

	var matches []string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 30 && strings.Contains(c.body, "daemon clean stop") {
			matches = append(matches, c.body)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 audit comment for issue #30, got %d: %v", len(matches), matches)
	}
	if !strings.Contains(matches[0], "Validate") {
		t.Errorf("audit comment should mention stage name %q; got: %q", "Validate", matches[0])
	}
}

// TestRunShutdownPause_Idempotent_NoDuplicateComment_AC2 verifies that a
// retried shutdown pause (the same in-flight issue processed twice — e.g. a
// retried label write racing a duplicate call) does not double-post the
// audit comment, satisfying AC2's "exactly one" requirement even under
// retry. Uses a live CacheImpl so the fabrik:paused label written by the
// first call is actually reflected back into the store's snapshot labels —
// the signal pauseIssueForDaemonShutdown's alreadyPaused guard reads.
func TestRunShutdownPause_Idempotent_NoDuplicateComment_AC2(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	// testEngineWithCache bootstraps owner/repo#1 in "Research".
	seedInFlightIssue(eng, "owner/repo", 1, "Research")

	eng.wg.Add(1)
	eng.runShutdownPause()

	eng.wg.Add(1)
	eng.runShutdownPause() // simulates a retried/duplicate shutdown pause call

	client.mu.Lock()
	defer client.mu.Unlock()

	var commentCount int
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 1 && strings.Contains(c.body, "daemon clean stop") {
			commentCount++
		}
	}
	if commentCount != 1 {
		t.Errorf("expected exactly 1 audit comment across 2 calls (idempotency guard), got %d", commentCount)
	}

	// Non-vacuity check: the second call's alreadyPaused guard depends on
	// fabrik:paused having actually landed in the store after the first call.
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !hasLabel(snap.Labels(), "fabrik:paused") {
		t.Fatal("precondition failed: fabrik:paused was not recorded in the store after the first call — idempotency check is vacuous")
	}
}

// TestRunShutdownPause_IdleQueue_NoWrites_AC5 verifies that a clean stop with
// no in-flight workers writes no labels and posts no comments (R9): it must
// not become slower or noisier than before this issue.
func TestRunShutdownPause_IdleQueue_NoWrites_AC5(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	start := time.Now()
	eng.wg.Add(1)
	eng.runShutdownPause()
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Errorf("idle-queue shutdown pause took %v, expected near-instant", elapsed)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label writes on idle queue, got %d: %v", len(client.addLabelCalls), client.addLabelCalls)
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comments on idle queue, got %d: %v", len(client.addCommentCalls), client.addCommentCalls)
	}
}

// TestDrainAndExit_BoundedByDrainDeadline_AC3 verifies drainAndExit returns
// within the configured drain deadline even when e.wg never completes — a
// deliberately unresponsive "worker" (a bare wg.Add(1) with no matching
// Done()), matching the Verification Note's requirement that AC3's test use
// a worker that does not exit on SIGINT/SIGTERM, so the assertion is
// non-vacuous against the kill escalation alone. If drainAndExit called
// e.wg.Wait() unboundedly (the pre-#1393 behavior), this test would hang
// until go test's own suite timeout — i.e. neutralizing the change under
// test makes the elapsed-time assertion fail, exactly as the Verification
// Note requires.
func TestDrainAndExit_BoundedByDrainDeadline_AC3(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.DrainDeadline = 150 * time.Millisecond

	eng.wg.Add(1) // never Done() — deliberately unresponsive worker

	restartDone := make(chan struct{})
	lockFile, err := os.CreateTemp(t.TempDir(), "fabrik-lock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer lockFile.Close()

	start := time.Now()
	if err := eng.drainAndExit(lockFile, restartDone); err != nil {
		t.Fatalf("drainAndExit: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < eng.cfg.DrainDeadline {
		t.Errorf("drainAndExit returned after %v, expected at least the drain deadline (%v) to elapse", elapsed, eng.cfg.DrainDeadline)
	}
	if elapsed > eng.cfg.DrainDeadline+2*time.Second {
		t.Errorf("drainAndExit took %v, expected to return promptly after the %v drain deadline, not hang", elapsed, eng.cfg.DrainDeadline)
	}
}

// TestForceQuit_DuringCleanStop_AC4 verifies that a second SIGINT/SIGTERM
// during an in-progress clean stop still force-exits promptly (R5) and is
// not blocked by the new shutdown-pause work, even when the drain can never
// complete on its own (a permanently in-flight e.wg counter — a worker that
// does not exit on SIGINT/SIGTERM, per the Verification Note — paired with a
// drain deadline far longer than this test's own timeout, so only the
// force-quit path, not the drain deadline, can explain a prompt exit).
//
// Runs Run() in a subprocess (see TestMain's FABRIK_TEST_FORCE_QUIT_HELPER
// branch) because the force-quit path calls os.Exit(1), which would
// otherwise terminate the test binary itself.
func TestForceQuit_DuringCleanStop_AC4(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cmd := exec.Command(testBin, "-test.run=^$")
	cmd.Env = append(os.Environ(), "FABRIK_TEST_FORCE_QUIT_HELPER=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "READY_FOR_SIGNALS") {
				close(ready)
				return
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		t.Fatal("helper process did not become ready in time")
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send first SIGINT: %v", err)
	}
	// Give the first signal's goroutine time to begin an (unfinishable) drain.
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send second SIGINT: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected helper process to exit with a non-zero status (force-quit exit 1), got: %v", err)
		}
		if code := exitErr.ExitCode(); code != 1 {
			t.Errorf("exit code = %d, want 1 (force-quit path); a different code likely means the process exited via a different path than force-quit", code)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		t.Fatal("AC4: force-quit did not exit promptly — the second signal was blocked by the clean-stop drain")
	}
}

// runForceQuitHelperProcess is invoked by TestMain when
// FABRIK_TEST_FORCE_QUIT_HELPER=1 is set (see TestForceQuit_DuringCleanStop_AC4).
// It runs Run() with a permanently in-flight e.wg counter and a drain
// deadline far longer than the parent test's own window, so only the
// force-quit path (not the drain deadline) can explain a prompt exit.
func runForceQuitHelperProcess() {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			PollSeconds:   300,
			DrainDeadline: time.Hour, // only force-quit, never the drain deadline, can explain a prompt exit
			Stages:        testStages(),
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManagerForRepo(mustMkdirTemp(), mustMkdirTemp(), "helper"),
	)

	dir := mustMkdirTemp()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdirall:", err)
		os.Exit(2)
	}
	eng.fabrikDir = dir

	// A permanently in-flight "worker" that never exits on SIGINT/SIGTERM —
	// the drain can never complete on its own; only force-quit can end Run().
	eng.wg.Add(1)

	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh
	go func() {
		<-readyCh
		fmt.Fprintln(os.Stderr, "READY_FOR_SIGNALS")
	}()

	if err := eng.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "Run:", err)
		os.Exit(3)
	}
	// Run returned nil — the drain completed cleanly (or the process wasn't
	// actually force-quit at all). Either way that's the AC4 failure mode;
	// surface it as a distinct exit code from the expected force-quit exit(1).
	os.Exit(4)
}

func mustMkdirTemp() string {
	dir, err := os.MkdirTemp("", "fabrik-force-quit-helper")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdtemp:", err)
		os.Exit(2)
	}
	return dir
}
