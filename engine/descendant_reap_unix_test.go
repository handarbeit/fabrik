//go:build !windows

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// detachHelperSetpgid moves the calling process into a brand-new process
// group (its own PID becomes its PGID) without touching its session ID.
// Used by TestMain's FABRIK_TEST_DETACH_HELPER subprocess mode (see
// process_item_test.go) to simulate a descendant that has already left its
// worker's process group — exactly the #1142 log evidence (killProcGroup's
// PGID-scoped kill completes successfully and the descendant still
// survives) — while remaining a member of the worker's session, which the
// new session-scoped reaper matches on.
func detachHelperSetpgid() {
	_ = unix.Setpgid(0, 0)
}

// pidAlive reports whether pid is still alive via a signal-0 probe (mirrors
// isProcessAlive's semantics without depending on it, keeping these tests
// independent of production helper behavior).
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// readPIDFile reads a bare integer PID written by a fake-claude script.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			pid, perr := strconv.Atoi(string(bytesTrimSpace(data)))
			if perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID file %s never became readable", path)
	return 0
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

// waitUntilDead polls until pid is no longer alive or the deadline elapses.
func waitUntilDead(t *testing.T, pid int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// invokeClaudeFakeScript runs InvokeClaude against a fake `claude` binary
// (a shell script), with descendantScanInterval shrunk so trackWorkerDescendants
// gets at least a couple of ticks before the fake script exits (the fake
// scripts below sleep briefly before completing to give it room). Returns
// once InvokeClaude returns, or fails the test on timeout.
func invokeClaudeFakeScript(t *testing.T, script string) {
	t.Helper()
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	origScan := descendantScanInterval
	descendantScanInterval = 50 * time.Millisecond
	defer func() { descendantScanInterval = origScan }()

	workDir := t.TempDir()
	stage := &stages.Stage{Name: "Implement", Prompt: "Do implement"}
	issue := gh.ProjectItem{Number: 1798, Title: "DescendantReap"}

	type result struct {
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		_, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		ch <- result{completed, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("InvokeClaude: %v", res.err)
		}
		if !res.completed {
			t.Errorf("expected completed=true")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("InvokeClaude did not return within 20s")
	}
}

const fakeClaudeCompleteJSON = `{"type":"result","subtype":"success","result":"done\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_reap","num_turns":1,"total_cost_usd":0.001,"is_error":false}`

// TestInvokeClaude_AC1_AC2_BackgroundDescendantOutsideWorktreeReaped covers
// AC1 (a still-running background descendant left behind by a clean exit is
// reaped) and AC2 (its cwd is outside the worktree — the case that motivated
// the issue; #1142's t.TempDir()-rooted test fixture). The descendant is
// backgrounded under job control (`set -m`), which — per direct local
// verification during Plan — moves it into its own new process group,
// exactly reproducing the production evidence that killProcGroup's PGID kill
// completes without reaching it: only the new session-scoped reaper can.
func TestInvokeClaude_AC1_AC2_BackgroundDescendantOutsideWorktreeReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	outsideDir := t.TempDir() // stands in for a t.TempDir()-rooted test fixture, outside the worktree
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"set -m\n" +
		"cd '" + outsideDir + "'\n" +
		"sleep 30 >/dev/null 2>&1 &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"disown 2>/dev/null || true\n" +
		"sleep 0.3\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"

	invokeClaudeFakeScript(t, script)

	childPID := readPIDFile(t, pidFile)
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Errorf("background descendant PID %d (cwd outside worktree) still alive after invocation end", childPID)
	}
}

// TestReapWorktreeProcesses_AC2_Neutralization is AC2's required
// neutralization check: it proves the pre-existing cwd-rooted reaper
// (ADR-063's reapWorktreeProcesses, the "current cwd-only implementation"
// AC2 refers to) does NOT catch a descendant whose cwd lies outside the
// worktree — confirming TestInvokeClaude_AC1_AC2_BackgroundDescendantOutsideWorktreeReaped
// above pins genuinely new behavior (the session-scoped reaper), not a
// coincidental pass already provided by the old mechanism. Reverting the new
// reap call in runClaude would turn this exact fixture from "reaped" back to
// "leaked," which is what this test independently confirms about the old
// mechanism alone.
func TestReapWorktreeProcesses_AC2_Neutralization(t *testing.T) {
	worktreeDir := t.TempDir() // stands in for the issue's real worktree
	outsideDir := t.TempDir()  // stands in for a t.TempDir()-rooted test fixture, outside the worktree
	descendant := exec.Command("sh", "-c", "cd '"+outsideDir+"' && exec sleep 30")
	if err := descendant.Start(); err != nil {
		t.Fatalf("starting descendant: %v", err)
	}
	defer func() {
		_ = descendant.Process.Kill()
		_ = descendant.Wait()
	}()

	var logLines []string
	logf := func(issueNumber int, tag, format string, args ...any) {
		logLines = append(logLines, fmt.Sprintf("[#%d %s] "+format, append([]any{issueNumber, tag}, args...)...))
	}

	reapWorktreeProcesses(worktreeDir, 1798, logf)

	if !pidAlive(descendant.Process.Pid) {
		t.Fatalf("cwd-only reapWorktreeProcesses unexpectedly killed a descendant whose cwd (%s) is outside the worktree (%s) — AC2 neutralization check itself is broken, or the old reaper's matching changed", outsideDir, worktreeDir)
	}
	t.Logf("confirmed: cwd-only reapWorktreeProcesses left the outside-worktree descendant untouched (log: %v)", logLines)
}

// TestInvokeClaude_AC8_NohupDisownDetachmentReaped reproduces #1787's
// detachment style specifically: nohup + disown under job control, so the
// descendant leaves both the process group (killProcGroup blind spot) and
// the shell's own job table (disown), matching the issue's evidence that
// PGID membership and shell bookkeeping alone cannot be relied on.
func TestInvokeClaude_AC8_NohupDisownDetachmentReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"set -m\n" +
		"nohup sleep 30 >/dev/null 2>&1 &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"disown 2>/dev/null || true\n" +
		"sleep 0.3\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"

	invokeClaudeFakeScript(t, script)

	childPID := readPIDFile(t, pidFile)
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Errorf("nohup/disown-detached descendant PID %d still alive after invocation end", childPID)
	}
}

// TestInvokeClaude_AC9_ProcessGroupRemovedDescendantReaped spawns a child
// (the test binary itself, re-exec'd via FABRIK_TEST_DETACH_HELPER) that
// explicitly moves itself into a brand-new process group before the
// invocation ends, while remaining in the worker's session — the literal
// #1142 scenario (killProcGroup's kill(-PGID, SIGKILL) completes and the
// descendant survives it because it is no longer a group member).
func TestInvokeClaude_AC9_ProcessGroupRemovedDescendantReaped(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"FABRIK_TEST_DETACH_HELPER=1 '" + testBin + "' -test.run='^$' >/dev/null 2>&1 &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"sleep 0.3\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"

	invokeClaudeFakeScript(t, script)

	childPID := readPIDFile(t, pidFile)
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Errorf("process-group-detached descendant PID %d still alive after invocation end", childPID)
	}
}

// TestSessionScopedDescendants_AC4_UnrelatedProcessNeverProposed pins AC4 at
// the discovery layer: a real, long-lived process with no session
// relationship to a fake worker PID is never returned as a candidate by
// sessionScopedDescendants — it is never even proposed for tracking, let
// alone signalled.
func TestSessionScopedDescendants_AC4_UnrelatedProcessNeverProposed(t *testing.T) {
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatalf("starting bystander: %v", err)
	}
	defer func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	}()

	// A fake "worker" PID with no session relationship to the bystander
	// (our own PID's session, inherited by the bystander too, is guaranteed
	// distinct from PID 1's session).
	pids, err := sessionScopedDescendants(1)
	if err != nil {
		t.Fatalf("sessionScopedDescendants: %v", err)
	}
	for _, pid := range pids {
		if pid == bystander.Process.Pid {
			t.Fatalf("bystander PID %d incorrectly proposed as a descendant of PID 1's session", pid)
		}
	}
}

// TestInvokeClaude_AC4_UnrelatedBystanderProcessSurvives runs a full
// InvokeClaude cycle (with its own detached descendant, to exercise the real
// discovery+reap path) alongside a concurrently-running, wholly unrelated
// bystander process, and confirms the bystander is untouched afterward —
// AC4 exercised end-to-end rather than at a single function boundary.
func TestInvokeClaude_AC4_UnrelatedBystanderProcessSurvives(t *testing.T) {
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatalf("starting bystander: %v", err)
	}
	defer func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	}()

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"set -m\n" +
		"sleep 30 >/dev/null 2>&1 &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"disown 2>/dev/null || true\n" +
		"sleep 0.3\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"

	invokeClaudeFakeScript(t, script)

	childPID := readPIDFile(t, pidFile)
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Errorf("worker's own detached descendant PID %d still alive after invocation end", childPID)
	}
	if !pidAlive(bystander.Process.Pid) {
		t.Fatal("unrelated bystander process was killed by the worker's own descendant reap")
	}
}

// TestReapTrackedDescendants_R5_MismatchedFingerprintNeverSignalled pins R5's
// PID-reuse guard: a registry entry whose recorded fingerprint no longer
// matches the live process at that PID (simulating the PID having been
// reused for something unrelated since it was recorded) is skipped, not
// killed, and the stale entry is dropped from the registry regardless.
func TestReapTrackedDescendants_R5_MismatchedFingerprintNeverSignalled(t *testing.T) {
	t.Chdir(t.TempDir())

	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatalf("starting bystander: %v", err)
	}
	defer func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	}()

	comm, lstart, err := pidFingerprint(bystander.Process.Pid)
	if err != nil {
		t.Fatalf("pidFingerprint(bystander): %v", err)
	}

	fakeWorkerPID := 999999999 // implausible PID, never a real process
	// Deliberately corrupt the recorded comm so the fingerprint re-check
	// below cannot match — simulating a PID reused since it was recorded.
	if err := upsertTrackedDescendant(trackedDescendant{
		PID: bystander.Process.Pid, Comm: "not-" + comm, LStart: lstart,
		WorkerPID: fakeWorkerPID, IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	reaped, skipped := reapTrackedDescendants(fakeWorkerPID, 1798)
	if reaped != 0 {
		t.Errorf("expected 0 reaped (fingerprint mismatch must block the kill), got %d", reaped)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", skipped)
	}
	if !pidAlive(bystander.Process.Pid) {
		t.Fatal("bystander process was killed despite a deliberate fingerprint mismatch")
	}

	remaining, err := descendantsForWorker(fakeWorkerPID)
	if err != nil {
		t.Fatalf("descendantsForWorker: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected the stale mismatched entry to be dropped from the registry, got %+v", remaining)
	}
}

// TestSweepStaleDescendants_LiveWorkerDescendantLeftAlone verifies the
// sweep's additional safety margin (beyond what any single AC requires): an
// entry whose WorkerPID is still alive is left alone by sweepStaleDescendants
// — it belongs to an in-flight invocation, which R2 will reap on its own —
// never signalled early just because it showed up in a sweep pass.
func TestSweepStaleDescendants_LiveWorkerDescendantLeftAlone(t *testing.T) {
	t.Chdir(t.TempDir())

	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatalf("starting bystander: %v", err)
	}
	defer func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	}()
	comm, lstart, err := pidFingerprint(bystander.Process.Pid)
	if err != nil {
		t.Fatalf("pidFingerprint(bystander): %v", err)
	}

	// WorkerPID = our own test process — very much alive.
	if err := upsertTrackedDescendant(trackedDescendant{
		PID: bystander.Process.Pid, Comm: comm, LStart: lstart,
		WorkerPID: os.Getpid(), IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	scanned, reaped, skipped := sweepStaleDescendants()
	if scanned != 1 {
		t.Errorf("expected scanned=1, got %d", scanned)
	}
	if reaped != 0 || skipped != 0 {
		t.Errorf("expected 0 reaped and 0 skipped (entry belongs to a live worker, left untouched), got reaped=%d skipped=%d", reaped, skipped)
	}
	if !pidAlive(bystander.Process.Pid) {
		t.Fatal("bystander process was killed even though its worker is still alive")
	}
}

// TestSweepStaleDescendants_AC3_OrphanFromPriorEngineRunReaped pins AC3: a
// registry entry written directly (simulating a prior engine run that
// discovered a descendant but crashed or was restarted before its own R2
// invocation-end reap ran) is reaped by the R3 backstop sweep — the case
// that breaks the self-reinforcing feedback loop for orphans that already
// escaped R1/R2, including across an engine restart, since the registry is
// durable (on disk) while runClaude's own in-memory tracking is not.
func TestSweepStaleDescendants_AC3_OrphanFromPriorEngineRunReaped(t *testing.T) {
	t.Chdir(t.TempDir())

	// A real orphan process, standing in for one left running by a prior
	// engine run. Reaped in a background goroutine as soon as it exits —
	// standing in for init's automatic zombie-reaping of a truly reparented
	// orphan; without this, our own test process (its literal, non-orphaned
	// parent here) would leave it a zombie after SIGKILL, which a signal-0
	// liveness probe still reports as "alive" until reaped.
	orphan := exec.Command("sleep", "30")
	if err := orphan.Start(); err != nil {
		t.Fatalf("starting orphan: %v", err)
	}
	waitDone := make(chan struct{})
	go func() {
		_ = orphan.Wait()
		close(waitDone)
	}()
	defer func() {
		_ = orphan.Process.Kill()
		<-waitDone
	}()
	comm, lstart, err := pidFingerprint(orphan.Process.Pid)
	if err != nil {
		t.Fatalf("pidFingerprint(orphan): %v", err)
	}

	// A dead workerPID: spawn and let it exit immediately, so isProcessAlive
	// on its PID reports false — standing in for "the engine instance that
	// discovered this descendant is no longer running."
	deadWorker := exec.Command("true")
	if err := deadWorker.Run(); err != nil {
		t.Fatalf("running deadWorker: %v", err)
	}
	deadWorkerPID := deadWorker.Process.Pid

	if err := upsertTrackedDescendant(trackedDescendant{
		PID: orphan.Process.Pid, Comm: comm, LStart: lstart,
		WorkerPID: deadWorkerPID, IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	scanned, reaped, skipped := sweepStaleDescendants()
	if scanned != 1 {
		t.Errorf("expected scanned=1, got %d", scanned)
	}
	if reaped != 1 {
		t.Errorf("expected 1 reaped, got %d (skipped=%d)", reaped, skipped)
	}
	if !waitUntilDead(t, orphan.Process.Pid, 5*time.Second) {
		t.Error("orphan process still alive after sweepStaleDescendants")
	}

	remaining, err := allTrackedDescendants()
	if err != nil {
		t.Fatalf("allTrackedDescendants: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected the reaped entry to be removed from the registry, got %+v", remaining)
	}
}
