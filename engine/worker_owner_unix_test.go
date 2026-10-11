//go:build !windows

package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/workerenv"
	"github.com/handarbeit/fabrik/internal/workerowner"
	"github.com/handarbeit/fabrik/stages"
)

// setsidHelper moves the calling process into a brand-new session. Used by
// TestMain's FABRIK_TEST_SETSID_HELPER mode.
func setsidHelper() { _, _ = unix.Setsid() }

// markedHelper starts a Go test-binary helper (readable environment on macOS,
// unlike SIP-protected /bin/sleep) in a session of its own, carrying
// FABRIK_WORKER_OWNER=owner when owner != "". It is reaped by a Wait goroutine,
// so a signalled helper is observed as exited rather than as a zombie.
func markedHelper(t *testing.T, owner string) (pid int, exited func() bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FABRIK_TEST_SENTINEL_HELPER=1")
	if owner != "" {
		cmd.Env = append(cmd.Env, workerenv.OwnerVar+"="+owner)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

func waitExited(exited func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if exited() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return exited()
}

// reapedPID returns the PID of a process that has exited and been reaped.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func fastMarkedReap(t *testing.T) {
	t.Helper()
	fastScan(t)
	prev := markedReapGrace
	markedReapGrace = 200 * time.Millisecond
	t.Cleanup(func() { markedReapGrace = prev })
}

// ownerPathIn returns the owner-file path for name under the owners directory.
func ownerPathIn(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(workerOwnersDir(), name+".json")
}

func seedRecord(t *testing.T, r workerRecord) {
	t.Helper()
	if err := appendWorkerRecord(r); err != nil {
		t.Fatal(err)
	}
}

// Acceptance 4 (periodic sweep): a marked orphan of a dead worker is reaped; an
// unmarked process — ppid/pgid-shaped exactly like it — is untouched.
func TestPeriodicSweep_ReapsMarkedOrphanAndLeavesUnmarkedAlone(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "dead-worker")
	markedPID, markedExited := markedHelper(t, owner)
	_, unmarkedExited := markedHelper(t, "")
	_, otherExited := markedHelper(t, ownerPathIn(t, "someone-else"))
	_ = markedPID

	seedRecord(t, workerRecord{ID: "w1", PID: reapedPID(t), Owner: owner, IssueNumber: 2128, SpawnedAt: time.Now()})

	if _, reaped, _, _ := sweepOrphanedWorkerSessions(); reaped < 1 {
		t.Fatalf("reaped = %d, want >= 1", reaped)
	}
	if !waitExited(markedExited, 5*time.Second) {
		t.Fatal("marked orphan of a dead worker survived the sweep")
	}
	if waitExited(unmarkedExited, 300*time.Millisecond) {
		t.Fatal("an unmarked process was killed on shape alone")
	}
	if waitExited(otherExited, 50*time.Millisecond) {
		t.Fatal("a process marked for a different owner was killed")
	}
}

// Non-vacuity for the above: with the marker scan neutralised the orphan lives.
func TestPeriodicSweep_NeutralisedMarkerScanLeavesOrphan(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	sessionReapIgnoreMarkers = true
	t.Cleanup(func() { sessionReapIgnoreMarkers = false })
	owner := ownerPathIn(t, "dead-worker")
	_, markedExited := markedHelper(t, owner)
	seedRecord(t, workerRecord{ID: "w1", PID: reapedPID(t), Owner: owner, IssueNumber: 2128, SpawnedAt: time.Now()})
	sweepOrphanedWorkerSessions()
	if waitExited(markedExited, 500*time.Millisecond) {
		t.Fatal("orphan was reaped although the marker scan was neutralised: the reap test is vacuous")
	}
}

// R4 / the #2097 failure: a marked process whose worker is LIVE is never
// touched, whatever its parent or session.
func TestPeriodicSweep_NeverReapsMarkedProcessOfLiveWorker(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "live-worker")
	_, exited := markedHelper(t, owner)
	rec := beginWorkerRecordWithOwner(os.Getpid(), 2128, "o/r", "Validate", owner)
	if rec.LStart == "" {
		t.Skip("no process fingerprint available")
	}
	for i := 0; i < 2; i++ {
		sweepOrphanedWorkerSessions()
	}
	if waitExited(exited, 500*time.Millisecond) {
		t.Fatal("a marked process of a LIVE worker was reaped (the #2097 failure)")
	}
	if recs, _ := allWorkerRecords(); len(recs) != 1 {
		t.Fatalf("live worker record dropped: %+v", recs)
	}
}

// R4: an inconclusive worker (alive, no recorded identity) is left alone too.
func TestPeriodicSweep_NeverReapsMarkedProcessOfInconclusiveWorker(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "inconclusive-worker")
	_, exited := markedHelper(t, owner)
	seedRecord(t, workerRecord{ID: "w1", PID: os.Getpid(), Owner: owner, IssueNumber: 2128, SpawnedAt: time.Now()}) // no LStart
	sweepOrphanedWorkerSessions()
	if waitExited(exited, 500*time.Millisecond) {
		t.Fatal("a marked process of an inconclusive worker was reaped")
	}
}

// A recycled worker PID is as good as dead: its orphans are reaped.
func TestPeriodicSweep_ReapsMarkedOrphanOfRecycledWorker(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "recycled-worker")
	_, exited := markedHelper(t, owner)
	seedRecord(t, workerRecord{ID: "w1", PID: os.Getpid(), LStart: "Mon Jan  2 15:04:05 2006", Owner: owner, IssueNumber: 2128, SpawnedAt: time.Now()})
	sweepOrphanedWorkerSessions()
	if !waitExited(exited, 5*time.Second) {
		t.Fatal("marked orphan of a recycled worker PID survived")
	}
}

// Restart survival: the sweep reads only workers.json, so a record written by a
// previous engine run still finds its orphan. The record is pruned (and its
// owner file removed) only after the marked set has been empty twice.
func TestPeriodicSweep_PrunesRecordAndOwnerFileOnlyAfterMarkedSetEmpties(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "prune")
	if err := workerowner.Write(owner, workerowner.Info{PID: 99999}); err != nil {
		t.Fatal(err)
	}
	_, exited := markedHelper(t, owner)
	seedRecord(t, workerRecord{ID: "w1", PID: reapedPID(t), Owner: owner, IssueNumber: 2128, SpawnedAt: time.Now()})

	_, _, _, pruned := sweepOrphanedWorkerSessions()
	if pruned != 0 {
		t.Fatal("record pruned on the pass that still found a marked orphan")
	}
	if !waitExited(exited, 5*time.Second) {
		t.Fatal("orphan survived")
	}
	var total int
	for i := 0; i < 3; i++ {
		_, _, _, p := sweepOrphanedWorkerSessions()
		total += p
	}
	if total != 1 {
		t.Fatalf("pruned %d, want 1", total)
	}
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Fatalf("owner file not removed with its record: %v", err)
	}
}

func TestInvocationEndSweep_ReapsMarkedOrphan(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "inv-end")
	_, exited := markedHelper(t, owner)
	rec := workerRecord{ID: "w1", PID: reapedPID(t), Owner: owner, IssueNumber: 2128, SpawnedAt: time.Now()}
	seedRecord(t, rec)
	if n := sweepWorkerSessionAtInvocationEnd(rec); n < 1 {
		t.Fatalf("reaped = %d, want >= 1", n)
	}
	if !waitExited(exited, 5*time.Second) {
		t.Fatal("marked orphan survived the invocation-end sweep")
	}
}

func TestInvocationEndSweep_LeavesLiveWorkersMarkedProcessAlone(t *testing.T) {
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	owner := ownerPathIn(t, "inv-end-live")
	_, exited := markedHelper(t, owner)
	rec := beginWorkerRecordWithOwner(os.Getpid(), 2128, "o/r", "Validate", owner)
	if rec.LStart == "" {
		t.Skip("no process fingerprint available")
	}
	if n := sweepWorkerSessionAtInvocationEnd(rec); n != 0 {
		t.Fatalf("reaped = %d for a live worker", n)
	}
	if waitExited(exited, 500*time.Millisecond) {
		t.Fatal("marked process of a live worker was reaped at invocation end")
	}
}

func TestSweepStaleWorkerOwnerFiles(t *testing.T) {
	useWorkerRecordsDir(t)
	dir := workerOwnersDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldOrphan := mk("old.json", 48*time.Hour)
	youngOrphan := mk("young.json", time.Hour)
	oldReferenced := mk("ref.json", 48*time.Hour)
	other := mk("note.txt", 48*time.Hour)
	n := sweepStaleWorkerOwnerFiles([]workerRecord{{ID: "w", Owner: oldReferenced}}, time.Now())
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for p, wantGone := range map[string]bool{oldOrphan: true, youngOrphan: false, oldReferenced: false, other: false} {
		_, err := os.Stat(p)
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s gone=%v, want %v", filepath.Base(p), gone, wantGone)
		}
	}
}

func TestRemoveWorkerOwnerFile_RefusesPathsOutsideOwnersDir(t *testing.T) {
	useWorkerRecordsDir(t)
	outside := filepath.Join(t.TempDir(), "precious.json")
	if err := os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	removeWorkerOwnerFile(outside)
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("file outside the owners dir was removed: %v", err)
	}
}

func TestBuildClaudeEnv_StripsAmbientOwnerMarker(t *testing.T) {
	env := buildClaudeEnv(&stages.Stage{Name: "Implement"}, gh.ProjectItem{Number: 1}, "/w", InvokeOptions{}, nil)
	found := false
	for _, kv := range env {
		if kv == workerenv.OwnerVar {
			found = true
		}
	}
	if !found {
		t.Fatalf("buildClaudeEnv must emit the bare %s removal sentinel: %v", workerenv.OwnerVar, env)
	}
	merged := mergeEnv([]string{workerenv.OwnerVar + "=/parent/worker.json", "X=1"}, env)
	for _, kv := range merged {
		if strings.HasPrefix(kv, workerenv.OwnerVar+"=") {
			t.Fatalf("ambient owner marker leaked into the worker env: %v", kv)
		}
	}
}

// End to end through the real runClaude: the worker's env carries the marker,
// the owner file names the live worker, and a helper that escapes the worker's
// session (invisible to every SID-keyed reaper) is reaped at invocation end by
// the marker alone. Neutralising the marker scan leaves it alive.
func runMarkedEscapeInvocation(t *testing.T) (helperPID int, ownerSeen string, fileInfo workerowner.Info) {
	t.Helper()
	useWorkerRecordsDir(t)
	fastMarkedReap(t)
	origSample := commandSessionSampleInterval
	commandSessionSampleInterval = time.Hour // never sampled: only the marker can find the helper
	t.Cleanup(func() { commandSessionSampleInterval = origSample })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	ownerFile := filepath.Join(dir, "owner.txt")
	infoCopy := filepath.Join(dir, "info.json")
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"printf '%s' \"$" + workerenv.OwnerVar + "\" > '" + ownerFile + "'\n" +
		// The engine writes the owner file just after Start(), so the script can
		// get here first; wait for it rather than copying nothing.
		"while [ ! -s \"$" + workerenv.OwnerVar + "\" ]; do sleep 0.02; done\n" +
		"cp \"$" + workerenv.OwnerVar + "\" '" + infoCopy + "'\n" +
		// Launched from a subshell that exits at once, so the helper is
		// reparented to init before any sample: its PPID chain no longer leads to
		// the worker, which is what a backgrounded command looks like once its
		// shell is gone. Left as a plain child, the end-of-invocation sample would
		// find its new session through the chain and reap it without the marker.
		"( FABRIK_TEST_SETSID_HELPER=1 '" + os.Args[0] + "' >/dev/null 2>&1 & echo $! > '" + pidFile + "' )\n" +
		"sleep 1\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"
	invokeFakeClaudeNoTick(t, script)

	helperPID = readPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(helperPID, syscall.SIGKILL) })
	b, _ := os.ReadFile(ownerFile)
	ownerSeen = string(b)
	if info, err := workerowner.Read(infoCopy); err == nil {
		fileInfo = info
	}
	return helperPID, ownerSeen, fileInfo
}

func TestInvokeClaude_MarkerReapsSessionEscapingDescendant(t *testing.T) {
	pid, owner, info := runMarkedEscapeInvocation(t)
	if !strings.HasPrefix(owner, workerOwnersDir()) {
		t.Fatalf("worker did not see %s under the owners dir: %q", workerenv.OwnerVar, owner)
	}
	if info.PID <= 1 || info.StartToken == "" {
		t.Fatalf("owner file did not name the live worker: %+v", info)
	}
	if !waitUntilDead(t, pid, 5*time.Second) {
		t.Fatalf("session-escaping descendant %d survived invocation end", pid)
	}
	if recs, _ := allWorkerRecords(); len(recs) != 0 {
		t.Fatalf("worker record not pruned: %+v", recs)
	}
}

func TestInvokeClaude_NeutralisedMarkerScanLeavesSessionEscapingDescendant(t *testing.T) {
	sessionReapIgnoreMarkers = true
	t.Cleanup(func() { sessionReapIgnoreMarkers = false })
	pid, _, _ := runMarkedEscapeInvocation(t)
	if waitUntilDead(t, pid, 500*time.Millisecond) {
		t.Fatal("descendant was reaped without the marker scan: the end-to-end test is vacuous")
	}
}
