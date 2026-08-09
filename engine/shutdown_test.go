package engine

import (
	"bufio"
	"context"
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
	"github.com/handarbeit/fabrik/tui"
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

// TestBeginShutdownPause_NoAddWaitRace is a regression test for a confirmed
// "sync: WaitGroup misuse: Add called concurrently with Wait" panic found
// during review of #1393: the original code called e.wg.Add(1) (for the
// pause-write phase) *after* cancel(), racing the main poll loop's
// drainAndExit -> waitGroupTimeout -> e.wg.Wait() call with no
// happens-before relationship between the two. Whenever the sole in-flight
// worker's own e.wg.Done() dropped the counter to zero in the same window,
// the runtime panicked — a crash in the very code meant to make shutdown
// clean. beginShutdownPause fixes this by calling e.wg.Add(1) strictly
// before cancel(); this test simulates the race many times (one fake
// in-flight worker racing Done() against beginShutdownPause's Add, with a
// concurrent Wait() standing in for drainAndExit) and would panic-crash the
// test binary within a handful of iterations if the ordering ever
// regresses.
func TestBeginShutdownPause_NoAddWaitRace(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	const iterations = 2000
	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())

		// Simulate one in-flight worker whose own Done() may land at any
		// moment relative to beginShutdownPause's internal Add(1)/cancel().
		eng.wg.Add(1)
		var workerDone sync.WaitGroup
		workerDone.Add(1)
		go func() {
			defer workerDone.Done()
			eng.wg.Done()
		}()

		// Simulate drainAndExit's waitGroupTimeout -> e.wg.Wait(): in
		// production the main poll loop's only path to that call is
		// observing ctx.Done(), so this goroutine gates on the same signal
		// rather than calling Wait() unconditionally — an ungated Wait()
		// here would race the fake worker's Done() independently of
		// beginShutdownPause entirely, which doesn't exercise the ordering
		// guarantee under test.
		var waiterDone sync.WaitGroup
		waiterDone.Add(1)
		go func() {
			defer waiterDone.Done()
			<-ctx.Done()
			eng.wg.Wait()
		}()

		eng.beginShutdownPause(cancel)

		workerDone.Wait()
		waiterDone.Wait()
	}
}

// TestBeginShutdownPause_SnapshotSurvivesRaceWithWorkerExit is a regression
// test for a review finding on #1393: runShutdownPause used to enumerate
// in-flight issues itself, from inside its own freshly launched goroutine,
// strictly *after* cancel() had already run. A worker whose invocation is
// cancelled before/while starting its Claude subprocess can reach its own
// cancellation branch (commit/push/releaseLock in finalizeStageOutcome) and
// its goroutine-level deferred WorkerExited (poll.go) in a handful of
// milliseconds — fast enough, once cancel() fires, to beat a freshly
// scheduled runShutdownPause goroutine to its own e.store.All() read. That
// issue's Worker() would already be nil by the time the scan ran, silently
// dropping it from the pause set: no fabrik:paused, no audit comment — and
// since the worker's own release() already cleared in_progress and the lock,
// R7's startup scan can't catch it either. This test simulates that exact
// race by clearing the seeded issue's Worker() from inside the cancel
// function itself — the earliest any real worker could possibly react to the
// cancellation beginShutdownPause is about to trigger — and asserts the
// issue is still paused, proving the fix (capturing the snapshot
// synchronously, before cancel() is called) actually closes the window
// rather than merely narrowing it.
func TestBeginShutdownPause_SnapshotSurvivesRaceWithWorkerExit(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	seedInFlightIssue(eng, "owner/repo", 999, "Implement")

	_, realCancel := context.WithCancel(context.Background())
	cancel := func() {
		// Simulate the fast-cancelling worker's own exit path completing
		// synchronously inside cancel() — the earliest point any real worker
		// could possibly observe and react to this cancellation.
		eng.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 999})
		realCancel()
	}

	eng.beginShutdownPause(cancel)
	if !waitGroupTimeout(&eng.wg, 5*time.Second) {
		t.Fatal("runShutdownPause did not complete in time")
	}

	// Non-vacuity check: confirm the race actually happened. If Worker() were
	// still non-nil here, this test would pass against the pre-fix code too,
	// proving nothing about the fix.
	snap, err := eng.store.Get("owner/repo", 999)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.Worker() != nil {
		t.Fatal("test setup error: Worker() was not cleared before runShutdownPause ran — the race this test simulates never happened")
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	var gotPaused bool
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 999 && c.labelName == "fabrik:paused" {
			gotPaused = true
		}
	}
	if !gotPaused {
		t.Error("issue #999: expected fabrik:paused despite its Worker() being cleared immediately after cancel() fired — the pre-cancel snapshot should have preserved it")
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
	eng.runShutdownPause(eng.inFlightSnapshot())

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
	eng.runShutdownPause(eng.inFlightSnapshot())

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
// the live re-check pauseInterruptedIssue performs under its per-issue
// mutex reads exactly this.
func TestRunShutdownPause_Idempotent_NoDuplicateComment_AC2(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	// testEngineWithCache bootstraps owner/repo#1 in "Research".
	seedInFlightIssue(eng, "owner/repo", 1, "Research")

	eng.wg.Add(1)
	eng.runShutdownPause(eng.inFlightSnapshot())

	eng.wg.Add(1)
	eng.runShutdownPause(eng.inFlightSnapshot()) // simulates a retried/duplicate shutdown pause call

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

// TestPauseInterruptedIssue_ConcurrentCallers_NoDuplicateComment is a
// regression test for a review finding on #1393: handleStopRequest (TUI
// single-issue stop) and pauseIssueForDaemonShutdown (daemon-wide clean-stop
// pause) can race for the *same* issue — a human hits the TUI's stop key at
// almost the same instant the daemon receives SIGINT. Before the fix, each
// caller computed its own "already paused?" snapshot before calling
// pauseInterruptedIssue, so both could observe "not yet paused" and both
// post an audit comment. pauseInterruptedIssue now serializes on a per-issue
// mutex and re-reads live store state under that lock, so whichever caller
// runs second always sees the first caller's already-applied fabrik:paused
// label. This test launches both callers concurrently, released at the same
// instant via a barrier channel, for many distinct issues (a fresh issue
// each time so every iteration is a genuine, unresolved race, unlike
// reusing one issue where only the first iteration would actually race) and
// asserts each issue never accumulates more than one audit comment.
func TestPauseInterruptedIssue_ConcurrentCallers_NoDuplicateComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	const iterations = 200
	for i := 0; i < iterations; i++ {
		number := 1000 + i
		seedInFlightIssue(eng, "owner/repo", number, "Research")

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			eng.handleStopRequest(context.Background(), tui.StopRequest{
				IssueNumber: number,
				Repo:        "owner/repo",
				StageName:   "Research",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			eng.wg.Add(1)
			eng.runShutdownPause(eng.inFlightSnapshot())
		}()
		close(start) // release both goroutines at the same instant
		wg.Wait()
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	counts := make(map[int]int)
	for _, c := range client.addCommentCalls {
		counts[c.issueNumber]++
	}
	for number, count := range counts {
		if count > 1 {
			t.Errorf("issue #%d: expected at most 1 audit comment from the concurrent TUI-stop/daemon-shutdown race, got %d", number, count)
		}
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
	eng.runShutdownPause(eng.inFlightSnapshot())
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
