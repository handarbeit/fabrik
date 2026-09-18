//go:build !windows

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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

	// WorkerPID = our own test process — very much alive, with its own
	// genuine identity fingerprint recorded too (as trackWorkerDescendants
	// would in production), so this exercises the same-worker match path,
	// not just the old-format (empty WorkerComm/WorkerLStart) fallback.
	workerComm, workerLStart, err := pidFingerprint(os.Getpid())
	if err != nil {
		t.Fatalf("pidFingerprint(self): %v", err)
	}
	if err := upsertTrackedDescendant(trackedDescendant{
		PID: bystander.Process.Pid, Comm: comm, LStart: lstart,
		WorkerPID: os.Getpid(), WorkerComm: workerComm, WorkerLStart: workerLStart,
		IssueNumber: 1798,
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

// TestSweepStaleDescendants_WorkerPIDReused_TreatedAsDead pins R5's PID-reuse
// guard on the WorkerPID side (as opposed to TestReapTrackedDescendants_R5_
// MismatchedFingerprintNeverSignalled, which covers the descendant's own PID
// reuse): a registry entry recorded against a worker whose PID has since
// been reassigned to a wholly unrelated live process must not be treated as
// "invocation still in flight" merely because *a* process exists at that PID
// number. Without re-verifying the worker's own identity fingerprint, such
// an entry would be skipped by every future sweep forever — a PID-reuse
// permanent leak, not merely a delayed reap.
func TestSweepStaleDescendants_WorkerPIDReused_TreatedAsDead(t *testing.T) {
	t.Chdir(t.TempDir())

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

	// A live, real process (our own test binary) stands in for "the PID the
	// original dead worker used to have, now reused by something else" —
	// deliberately recorded with a fingerprint that does NOT match what's
	// actually running there now, simulating reuse since the original
	// worker (whatever it was) exited.
	if err := upsertTrackedDescendant(trackedDescendant{
		PID: orphan.Process.Pid, Comm: comm, LStart: lstart,
		WorkerPID: os.Getpid(), WorkerComm: "not-the-real-worker-comm", WorkerLStart: "not-the-real-worker-lstart",
		IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	scanned, reaped, skipped := sweepStaleDescendants()
	if scanned != 1 {
		t.Errorf("expected scanned=1, got %d", scanned)
	}
	if reaped != 1 {
		t.Errorf("expected 1 reaped (worker PID's fingerprint mismatch must NOT be trusted as 'still in flight'), got %d (skipped=%d)", reaped, skipped)
	}
	if !waitUntilDead(t, orphan.Process.Pid, 5*time.Second) {
		t.Error("orphan still alive after sweepStaleDescendants — worker-PID-reuse guard did not trigger a reap")
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

// TestTrackWorkerDescendants_TransientFingerprintFailureRetried pins a review
// finding: trackWorkerDescendants must not permanently give up on a PID after
// a single fingerprint failure. pidFingerprint's single-PID `ps` call cannot
// tell "the process already exited" apart from "the ps invocation itself
// failed transiently" (e.g. sentinelProbeTimeout expiring under exactly the
// host contention this issue is about) — both surface as a generic error. If
// the discovery loop marked the PID permanently "seen" on any such failure,
// a live descendant could be silently and permanently dropped from tracking
// under load, defeating the reaper for the scenario it matters most in.
//
// Neutralization: before the fix (marking seen[pid]=true unconditionally on
// a fingerprint error), this test fails — the injected failures land on the
// PID's one and only discovery attempt, so it is never recorded and this
// test's deadline loop times out with zero registry entries.
func TestTrackWorkerDescendants_TransientFingerprintFailureRetried(t *testing.T) {
	t.Chdir(t.TempDir())

	origScan := descendantScanInterval
	descendantScanInterval = 20 * time.Millisecond
	defer func() { descendantScanInterval = origScan }()

	// Session-leading "worker" shell with a real descendant in its session —
	// mirrors TestSessionScopedDescendants_FindsRealChild's setup.
	shell := exec.Command("/bin/sh", "-c", "sleep 5 & wait")
	setCmdProcAttr(shell)
	if err := shell.Start(); err != nil {
		t.Fatalf("starting shell: %v", err)
	}
	defer func() {
		_ = shell.Process.Kill()
		_ = shell.Wait()
	}()
	workerPID := shell.Process.Pid

	origFn := pidFingerprintFn
	var mu sync.Mutex
	failuresInjected := 0
	pidFingerprintFn = func(pid int) (string, string, error) {
		if pid == workerPID {
			return origFn(pid) // don't interfere with the worker's own fingerprint
		}
		mu.Lock()
		defer mu.Unlock()
		if failuresInjected < 2 {
			failuresInjected++
			return "", "", context.DeadlineExceeded // simulated transient ps timeout
		}
		return origFn(pid)
	}
	defer func() { pidFingerprintFn = origFn }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		trackWorkerDescendants(ctx, workerPID, 1798, "", "Implement")
		close(done)
	}()

	var got []trackedDescendant
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := descendantsForWorker(workerPID)
		if err != nil {
			t.Fatalf("descendantsForWorker: %v", err)
		}
		if len(entries) > 0 {
			got = entries
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if len(got) == 0 {
		t.Fatal("descendant was never recorded despite the injected transient failures eventually clearing — the retry-after-transient-failure behavior did not happen")
	}

	mu.Lock()
	defer mu.Unlock()
	if failuresInjected < 2 {
		t.Fatalf("expected at least 2 injected failures to have been consumed before the eventual success, got %d — test setup issue, not confirming the retry behavior", failuresInjected)
	}
}

// TestTrackWorkerDescendants_WorkerFingerprintTransientFailureRetried pins a
// second review finding on the same theme: the worker's OWN identity
// fingerprint (workerComm/workerLStart, stamped onto every discovered
// descendant so sweepStaleDescendants can later tell "still the same worker"
// apart from "PID reused") must also be retried on transient failure, not
// fetched once with the error discarded. A single bad `ps` call at the
// moment trackWorkerDescendants starts (the same host-contention condition
// this reaper exists to handle) must not silently and permanently degrade
// every descendant of this invocation to the liveness-only fallback for the
// invocation's entire lifetime.
//
// Neutralization: before the fix (a single un-retried fetch before the
// loop), this test fails — the injected failures land on that one fetch
// attempt, so workerComm/workerLStart stay empty forever and the recorded
// descendant's WorkerComm/WorkerLStart never become non-empty.
func TestTrackWorkerDescendants_WorkerFingerprintTransientFailureRetried(t *testing.T) {
	t.Chdir(t.TempDir())

	origScan := descendantScanInterval
	descendantScanInterval = 20 * time.Millisecond
	defer func() { descendantScanInterval = origScan }()

	shell := exec.Command("/bin/sh", "-c", "sleep 5 & wait")
	setCmdProcAttr(shell)
	if err := shell.Start(); err != nil {
		t.Fatalf("starting shell: %v", err)
	}
	defer func() {
		_ = shell.Process.Kill()
		_ = shell.Wait()
	}()
	workerPID := shell.Process.Pid

	origFn := pidFingerprintFn
	var mu sync.Mutex
	workerFailuresInjected := 0
	pidFingerprintFn = func(pid int) (string, string, error) {
		if pid != workerPID {
			return origFn(pid) // don't interfere with the descendant's own fingerprint
		}
		mu.Lock()
		defer mu.Unlock()
		if workerFailuresInjected < 2 {
			workerFailuresInjected++
			return "", "", context.DeadlineExceeded // simulated transient ps timeout
		}
		return origFn(pid)
	}
	defer func() { pidFingerprintFn = origFn }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		trackWorkerDescendants(ctx, workerPID, 1798, "", "Implement")
		close(done)
	}()

	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := descendantsForWorker(workerPID)
		if err != nil {
			t.Fatalf("descendantsForWorker: %v", err)
		}
		if len(entries) > 0 && entries[0].WorkerComm != "" && entries[0].WorkerLStart != "" {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if !found {
		t.Fatal("descendant's WorkerComm/WorkerLStart never became non-empty despite the injected transient worker-fingerprint failures eventually clearing — the retry behavior did not happen")
	}

	mu.Lock()
	defer mu.Unlock()
	if workerFailuresInjected < 2 {
		t.Fatalf("expected at least 2 injected worker-fingerprint failures to have been consumed before the eventual success, got %d — test setup issue, not confirming the retry behavior", workerFailuresInjected)
	}
}

// TestInvokeClaude_ReapWaitsForTrackerGoroutine pins a fourth review finding:
// at invocation end, watchdogCancel() must not be immediately followed by
// reapTrackedDescendants — the goroutine running trackWorkerDescendants must
// actually observe the cancellation and return first. If it is mid-tick
// (blocked inside pidFingerprintFn for a just-discovered descendant) when
// cancellation fires, that descendant can be persisted to the registry
// *after* reapTrackedDescendants has already read and cleared the worker's
// entries — silently deferring its reap from "unconditional at invocation
// end" (R2) to the next R3 backstop sweep, bounded by JanitorIntervalHours
// (potentially hours).
//
// Neutralization: an injected delay on every pidFingerprintFn call widens
// the race window so the fake claude process reliably exits (triggering
// watchdogCancel and, pre-fix, the immediate reap) while trackWorkerDescendants
// is still mid-tick discovering the descendant backgrounded moments earlier.
// Before the fix (reapTrackedDescendants called with no wait for the tracker
// goroutine to actually stop), this test is reliably red: the descendant is
// discovered but not yet persisted to the registry when the reap runs, so it
// survives past invocation end and this test's deadline.
func TestInvokeClaude_ReapWaitsForTrackerGoroutine(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"set -m\n" +
		"sleep 30 >/dev/null 2>&1 &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"disown 2>/dev/null || true\n" +
		"sleep 0.15\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"

	origFn := pidFingerprintFn
	pidFingerprintFn = func(pid int) (string, string, error) {
		// Delays every fingerprint lookup in trackWorkerDescendants's tick
		// body (both the worker's own and each descendant's) so a tick that
		// starts before the fake script exits is still in flight when
		// cmd.Wait() returns and watchdogCancel() fires — reproducing the
		// exact "mid-tick at invocation end" race the fix addresses.
		time.Sleep(400 * time.Millisecond)
		return origFn(pid)
	}
	defer func() { pidFingerprintFn = origFn }()

	invokeClaudeFakeScript(t, script)

	childPID := readPIDFile(t, pidFile)
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Errorf("descendant PID %d, discovered mid-tick right at invocation end, was not reaped — reapTrackedDescendants must have run before trackWorkerDescendants finished persisting it to the registry (missing wait for the tracker goroutine to actually stop)", childPID)
	}
}

// TestReapTrackedDescendants_TransientProbeFailure_EntryRetained pins a fifth
// review finding: reapTrackedDescendants used to add every entry's PID to
// the to-be-removed list unconditionally, before checking pidFingerprint's
// result — so a transient probe failure (e.g. the ps-subprocess timeout
// under exactly the host contention this reaper exists to handle) was
// treated identically to "confirmed gone or reused," permanently dropping
// the registry entry for a descendant that is actually still alive and
// still correctly identified. That silently defeats R3's backstop for
// exactly the descendants R3 exists to eventually catch.
//
// Neutralization: reverting the fix (unconditionally appending to processed
// before the fingerprint check) turns this test red — the entry is dropped
// from the registry despite the descendant remaining alive and correctly
// identified.
func TestReapTrackedDescendants_TransientProbeFailure_EntryRetained(t *testing.T) {
	t.Chdir(t.TempDir())

	descendant := exec.Command("sleep", "30")
	if err := descendant.Start(); err != nil {
		t.Fatalf("starting descendant: %v", err)
	}
	defer func() {
		_ = descendant.Process.Kill()
		_ = descendant.Wait()
	}()
	pid := descendant.Process.Pid

	comm, lstart, err := pidFingerprint(pid)
	if err != nil {
		t.Fatalf("pidFingerprint(descendant): %v", err)
	}

	fakeWorkerPID := 999999998 // implausible PID, never a real process
	if err := upsertTrackedDescendant(trackedDescendant{
		PID: pid, Comm: comm, LStart: lstart,
		WorkerPID: fakeWorkerPID, IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	origFn := pidFingerprintFn
	pidFingerprintFn = func(p int) (string, string, error) {
		if p == pid {
			return "", "", context.DeadlineExceeded // simulated transient ps timeout
		}
		return origFn(p)
	}
	defer func() { pidFingerprintFn = origFn }()

	reaped, skipped := reapTrackedDescendants(fakeWorkerPID, 1798)
	if reaped != 0 {
		t.Errorf("expected 0 reaped (a transient probe failure must never authorize a kill), got %d", reaped)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", skipped)
	}
	if !pidAlive(pid) {
		t.Fatal("descendant process was killed despite a transient (not confirmed) fingerprint failure")
	}

	remaining, err := descendantsForWorker(fakeWorkerPID)
	if err != nil {
		t.Fatalf("descendantsForWorker: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected the entry to be RETAINED after a transient probe failure (for a later retry), got %d remaining entries", len(remaining))
	}
}

// TestSweepStaleDescendants_TransientProbeFailure_EntryRetained is
// TestReapTrackedDescendants_TransientProbeFailure_EntryRetained's sibling
// for the R3 backstop sweep: sweepStaleDescendants had the identical
// unconditional-append-before-fingerprint-check defect for the descendant's
// own PID (the worker-liveness gate above it was already fixed separately).
func TestSweepStaleDescendants_TransientProbeFailure_EntryRetained(t *testing.T) {
	t.Chdir(t.TempDir())

	descendant := exec.Command("sleep", "30")
	if err := descendant.Start(); err != nil {
		t.Fatalf("starting descendant: %v", err)
	}
	defer func() {
		_ = descendant.Process.Kill()
		_ = descendant.Wait()
	}()
	pid := descendant.Process.Pid

	comm, lstart, err := pidFingerprint(pid)
	if err != nil {
		t.Fatalf("pidFingerprint(descendant): %v", err)
	}

	fakeWorkerPID := 999999997 // implausible PID — sweep treats its owner as dead
	if err := upsertTrackedDescendant(trackedDescendant{
		PID: pid, Comm: comm, LStart: lstart,
		WorkerPID: fakeWorkerPID, IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	origFn := pidFingerprintFn
	pidFingerprintFn = func(p int) (string, string, error) {
		if p == pid {
			return "", "", context.DeadlineExceeded // simulated transient ps timeout
		}
		return origFn(p)
	}
	defer func() { pidFingerprintFn = origFn }()

	scanned, reaped, skipped := sweepStaleDescendants()
	if scanned != 1 {
		t.Errorf("expected scanned=1, got %d", scanned)
	}
	if reaped != 0 {
		t.Errorf("expected 0 reaped (a transient probe failure must never authorize a kill), got %d", reaped)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", skipped)
	}
	if !pidAlive(pid) {
		t.Fatal("descendant process was killed despite a transient (not confirmed) fingerprint failure")
	}

	remaining, err := allTrackedDescendants()
	if err != nil {
		t.Fatalf("allTrackedDescendants: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected the entry to be RETAINED after a transient probe failure (for a later sweep retry), got %d remaining entries", len(remaining))
	}
}

// TestSweepStaleDescendants_WorkerFingerprintTransientFailure_LeftAlone pins
// a bot-review finding (Pruefer, PR #1806): workerIdentityStillMatches used
// to call pidFingerprint directly (bypassing the pidFingerprintFn seam) and
// treated ANY error — including a transient ps-subprocess timeout — as a
// confirmed identity mismatch (return false). Since
// isProcessAlive(d.WorkerPID) && workerIdentityStillMatches(d) is the guard
// that leaves an entry alone for R2, a transient failure of the WORKER's own
// fingerprint probe (not the descendant's) would fail closed toward the kill
// path even though the worker is genuinely alive and unchanged — the
// opposite of every other pidFingerprintFn call site in this file, and
// exactly the R5 fail-closed violation sweepStaleDescendants's own doc
// comment warns against ("sweeping it early risks killing a subprocess the
// worker still genuinely needs").
func TestSweepStaleDescendants_WorkerFingerprintTransientFailure_LeftAlone(t *testing.T) {
	t.Chdir(t.TempDir())

	worker := exec.Command("sleep", "30")
	if err := worker.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	defer func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	}()
	workerPID := worker.Process.Pid
	workerComm, workerLStart, err := pidFingerprint(workerPID)
	if err != nil {
		t.Fatalf("pidFingerprint(worker): %v", err)
	}

	descendant := exec.Command("sleep", "30")
	if err := descendant.Start(); err != nil {
		t.Fatalf("starting descendant: %v", err)
	}
	defer func() {
		_ = descendant.Process.Kill()
		_ = descendant.Wait()
	}()
	pid := descendant.Process.Pid
	comm, lstart, err := pidFingerprint(pid)
	if err != nil {
		t.Fatalf("pidFingerprint(descendant): %v", err)
	}

	if err := upsertTrackedDescendant(trackedDescendant{
		PID: pid, Comm: comm, LStart: lstart,
		WorkerPID: workerPID, WorkerComm: workerComm, WorkerLStart: workerLStart,
		IssueNumber: 1798,
	}); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}

	origFn := pidFingerprintFn
	pidFingerprintFn = func(p int) (string, string, error) {
		if p == workerPID {
			return "", "", context.DeadlineExceeded // simulated transient ps timeout on the WORKER's own fingerprint
		}
		return origFn(p)
	}
	defer func() { pidFingerprintFn = origFn }()

	scanned, reaped, skipped := sweepStaleDescendants()
	if scanned != 1 {
		t.Errorf("expected scanned=1, got %d", scanned)
	}
	if reaped != 0 {
		t.Errorf("expected 0 reaped (a transient failure of the worker's own fingerprint must never authorize a kill), got %d", reaped)
	}
	if skipped != 0 {
		t.Errorf("expected 0 skipped (the entry should be left alone for R2, not even counted as processed), got %d", skipped)
	}
	if !pidAlive(pid) {
		t.Fatal("descendant process was killed despite the owning worker being alive — only its fingerprint probe failed transiently")
	}
	if !pidAlive(workerPID) {
		t.Fatal("test setup invariant violated: worker process should still be alive")
	}

	remaining, err := allTrackedDescendants()
	if err != nil {
		t.Fatalf("allTrackedDescendants: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected the entry to be RETAINED (owning worker still in flight), got %d remaining entries", len(remaining))
	}
}

// TestTrackWorkerDescendants_ConcurrentInvocationsShareProcessTableScan pins
// a review finding (Pruefer, PR #1806): trackWorkerDescendants used to call
// sessionScopedDescendants directly, which shells out to `ps` on every tick
// of every concurrently-running invocation's own independent goroutine. With
// MaxConcurrent invocations in flight, that meant MaxConcurrent redundant
// full-process-table scans every descendantScanInterval — the same class of
// `ps`/`lsof` CPU cost R4 found responsible for real spikes elsewhere in this
// PR (#1805). sharedProcessTableScan's TTL cache should coalesce those N
// independent scans down to roughly one per interval, regardless of how many
// invocations are concurrently tracking.
func TestTrackWorkerDescendants_ConcurrentInvocationsShareProcessTableScan(t *testing.T) {
	origScan := descendantScanInterval
	descendantScanInterval = 30 * time.Millisecond
	defer func() { descendantScanInterval = origScan }()

	origFn := listProcessArgvFn
	var calls atomic.Int64
	listProcessArgvFn = func() ([]procArgvEntry, error) {
		calls.Add(1)
		return []procArgvEntry{}, nil
	}
	defer func() { listProcessArgvFn = origFn }()

	// Force the shared cache cold so an earlier test's fresh entry can't make
	// this test spuriously pass with zero real calls. Reset it again on
	// return: this test's stubbed listProcessArgvFn leaves the cache holding
	// a fake, always-empty result under a real, recent timestamp — without
	// clearing it, any other test that calls trackWorkerDescendants /
	// sharedProcessTableScan shortly afterward (while descendantScanInterval
	// has already reverted to its real, longer default via the defer above)
	// would silently reuse this stale entry instead of performing a genuine
	// scan, for up to that default's duration. Mirrors descendantScanInterval's
	// own save/defer-restore immediately above.
	sharedProcessTableMu.Lock()
	sharedProcessTableAt = time.Time{}
	sharedProcessTableMu.Unlock()
	defer func() {
		sharedProcessTableMu.Lock()
		sharedProcessTableAt = time.Time{}
		sharedProcessTableProcs = nil
		sharedProcessTableErr = nil
		sharedProcessTableMu.Unlock()
	}()

	const numInvocations = 5
	const runDuration = 220 * time.Millisecond // ~7 ticks at 30ms
	ctx, cancel := context.WithTimeout(context.Background(), runDuration)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < numInvocations; i++ {
		wg.Add(1)
		go func(workerPID int) {
			defer wg.Done()
			trackWorkerDescendants(ctx, workerPID, 1798, "owner/repo", "Validate")
		}(100000 + i) // implausible PIDs — sessionScopedDescendantsFromProcs finds nothing, which is fine; only call count matters here
	}
	wg.Wait()

	got := calls.Load()
	expectedTicks := int64(runDuration/descendantScanInterval) + 2 // +2 slack for start/stop jitter
	maxAcceptable := expectedTicks * 2                             // well under numInvocations(5)x if sharing works; would be ~numInvocations*expectedTicks if it didn't
	if got > maxAcceptable {
		t.Errorf("listProcessArgvFn called %d times across %d concurrent invocations over ~%d ticks — expected roughly one shared scan per tick (<=%d), not one per invocation per tick (independent scanning would give up to ~%d)",
			got, numInvocations, expectedTicks, maxAcceptable, expectedTicks*numInvocations)
	}
	if got == 0 {
		t.Error("listProcessArgvFn was never called — test setup is broken (expected at least one real scan)")
	}
}

// TestTrackWorkerDescendants_TransientRegistryWriteFailureRetried pins a
// review finding (handarbeit-pruefer, PR #1806): seen[pid] was set
// unconditionally, immediately before upsertTrackedDescendant's error was
// discarded (`_ = upsertTrackedDescendant(d)`). If the durable write fails —
// e.g. .fabrik/state/ briefly unwritable under the same host contention this
// reaper exists to handle elsewhere in this same function — the descendant
// was never actually persisted to the registry, yet seen[pid] permanently
// prevented it from ever being reprocessed on a later tick for the rest of
// the invocation. That made it invisible to both reapTrackedDescendants (R2)
// and sweepStaleDescendants (R3) — the exact "transient failure permanently
// and silently defeats tracking" failure mode this function's
// pidFingerprintFn error handling (a few lines above) already guards against,
// recurring here on the write side instead of the read side.
//
// Neutralization: reverting the fix (seen[pid] = true unconditionally, write
// error discarded) turns this test red — the descendant is never recorded
// even after the registry write path becomes writable again, since the first
// (failed) attempt already marked it seen.
func TestTrackWorkerDescendants_TransientRegistryWriteFailureRetried(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Block the registry write with a local-path trick (no network/fault
	// library needed, per this repo's testing conventions): create
	// ".fabrik/state" as a regular file, so saveDescendantRegistry's
	// os.MkdirAll(".fabrik/state", ...) fails with ENOTDIR.
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0700); err != nil {
		t.Fatalf("mkdir .fabrik: %v", err)
	}
	blockerPath := filepath.Join(dir, ".fabrik", "state")
	if err := os.WriteFile(blockerPath, []byte("blocking"), 0600); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}

	origScan := descendantScanInterval
	descendantScanInterval = 20 * time.Millisecond
	defer func() { descendantScanInterval = origScan }()

	// Session-leading "worker" shell with a real descendant in its session —
	// mirrors TestTrackWorkerDescendants_TransientFingerprintFailureRetried's
	// setup.
	shell := exec.Command("/bin/sh", "-c", "sleep 5 & wait")
	setCmdProcAttr(shell)
	if err := shell.Start(); err != nil {
		t.Fatalf("starting shell: %v", err)
	}
	defer func() {
		_ = shell.Process.Kill()
		_ = shell.Wait()
	}()
	workerPID := shell.Process.Pid

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		trackWorkerDescendants(ctx, workerPID, 1798, "", "Implement")
		close(done)
	}()

	// Give the tracker several ticks to discover the descendant and attempt
	// (and fail) the registry write while the write path is blocked.
	time.Sleep(120 * time.Millisecond)

	// Unblock: remove the blocking file so a later upsert's os.MkdirAll can
	// succeed.
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("removing blocker file: %v", err)
	}

	var got []trackedDescendant
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := descendantsForWorker(workerPID)
		if err != nil {
			t.Fatalf("descendantsForWorker: %v", err)
		}
		if len(entries) > 0 {
			got = entries
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if len(got) == 0 {
		t.Fatal("descendant was never recorded even after the registry write path became writable again — a transient write failure permanently dropped tracking for the rest of the invocation")
	}
}
