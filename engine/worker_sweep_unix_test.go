//go:build !windows

package engine

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// fastScan makes sharedProcessTableScan's cache effectively transparent so a
// periodic sweep sees the processes a test just spawned rather than a snapshot
// taken by an unrelated earlier test.
func fastScan(t *testing.T) {
	t.Helper()
	prev := descendantScanInterval
	descendantScanInterval = time.Millisecond
	t.Cleanup(func() { descendantScanInterval = prev })
}

type deadLeaderSession struct {
	leaderPID int
	childPIDs []int
	comm      string
	lstart    string // the leader's fingerprint, captured while it was alive
}

// spawnSession starts a real Setsid session leader running script (which must
// echo the PID of each of its nChildren backgrounded children, one per line),
// then blocks the leader on stdin. It returns the still-live leader plus a
// release func that lets it exit. Every child is SIGKILLed on test cleanup.
func spawnSession(t *testing.T, script string, nChildren int) (leader *exec.Cmd, sess deadLeaderSession, release func()) {
	t.Helper()
	leader = exec.Command("sh", "-c", script+"\nread _x")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := leader.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	sess.leaderPID = leader.Process.Pid
	sc := bufio.NewScanner(stdout)
	for i := 0; i < nChildren; i++ {
		if !sc.Scan() {
			t.Fatalf("reading child pid %d: %v", i, sc.Err())
		}
		pid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			t.Fatalf("bad child pid %q: %v", sc.Text(), err)
		}
		sess.childPIDs = append(sess.childPIDs, pid)
	}
	t.Cleanup(func() {
		for _, pid := range sess.childPIDs {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(sess.leaderPID, syscall.SIGKILL)
		_ = leader.Process.Kill()
	})
	sess.comm, sess.lstart, err = pidFingerprint(sess.leaderPID)
	if err != nil {
		t.Fatalf("fingerprint of leader: %v", err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		stdin.Close()
		_ = leader.Wait()
	}
	return leader, sess, release
}

// spawnDeadLeaderSession returns a session whose leader has exited and been
// reaped, leaving its children as orphans with Getsid == leaderPID.
func spawnDeadLeaderSession(t *testing.T, script string, nChildren int) deadLeaderSession {
	t.Helper()
	_, sess, release := spawnSession(t, script, nChildren)
	release()
	for _, pid := range sess.childPIDs {
		if !pidAlive(pid) {
			t.Fatalf("child %d died with its leader", pid)
		}
	}
	if pidAlive(sess.leaderPID) {
		t.Fatalf("leader %d still alive", sess.leaderPID)
	}
	return sess
}

const sleepChildScript = "sleep 60 >/dev/null 2>&1 &\necho $!"

func recordFor(sess deadLeaderSession) workerRecord {
	return workerRecord{
		ID: "test-" + strconv.Itoa(sess.leaderPID), PID: sess.leaderPID,
		Comm: sess.comm, LStart: sess.lstart, IssueNumber: 1814, Stage: "Implement",
		SpawnedAt: time.Now(),
	}
}

// AC2 + R1 + R4: an orphan with no descendant-registry entry, whose leader is
// a dead recorded worker (record loaded from disk, as after an engine restart),
// is reaped by the periodic sweep; the record is then pruned after two clean
// empty scans.
func TestSweepOrphanedWorkerSessions_AC2_UnregisteredOrphanReapedAndRecordPruned(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	sess := spawnDeadLeaderSession(t, sleepChildScript, 1)
	if err := appendWorkerRecord(recordFor(sess)); err != nil {
		t.Fatal(err)
	}
	if d, _ := allTrackedDescendants(); len(d) != 0 {
		t.Fatalf("precondition: descendant registry must be empty, got %+v", d)
	}

	records, reaped, _, pruned := sweepOrphanedWorkerSessions()
	if records != 1 || reaped != 1 || pruned != 0 {
		t.Fatalf("first sweep: records=%d reaped=%d pruned=%d", records, reaped, pruned)
	}
	if !waitUntilDead(t, sess.childPIDs[0], 5*time.Second) {
		t.Fatal("orphan with no registry entry survived the periodic sweep")
	}

	time.Sleep(10 * time.Millisecond)
	if _, _, _, pruned := sweepOrphanedWorkerSessions(); pruned != 0 {
		t.Fatal("record pruned after a single empty scan; need two")
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, _, pruned := sweepOrphanedWorkerSessions(); pruned != 1 {
		t.Fatalf("expected record pruned after two empty scans, pruned=%d", pruned)
	}
	if recs, _ := allWorkerRecords(); len(recs) != 0 {
		t.Fatalf("record not removed: %+v", recs)
	}
}

// AC5 + R3: an orphaned shell with zero children (blocked in a builtin) is
// reaped — ownership is by session lineage, not process type.
func TestSweepOrphanedWorkerSessions_AC5_ZeroChildShellReaped(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	script := "/bin/sh -c 'read x < \"" + fifo + "\"' >/dev/null 2>&1 &\necho $!"
	sess := spawnDeadLeaderSession(t, script, 1)
	if kids, _ := exec.Command("pgrep", "-P", strconv.Itoa(sess.childPIDs[0])).Output(); len(strings.TrimSpace(string(kids))) != 0 {
		t.Fatalf("test shell unexpectedly has children: %s", kids)
	}
	if err := appendWorkerRecord(recordFor(sess)); err != nil {
		t.Fatal(err)
	}
	if _, reaped, _, _ := sweepOrphanedWorkerSessions(); reaped != 1 {
		t.Fatalf("reaped=%d, want 1", reaped)
	}
	if !waitUntilDead(t, sess.childPIDs[0], 5*time.Second) {
		t.Fatal("zero-children orphan shell survived")
	}
}

// AC4 neutralization: a dead-leader session with no record for its leader is
// never signalled, and a record for a different PID does not authorize it. The
// same session is then reaped once its own record exists, so the record — and
// only the record — is the ownership evidence. Removing the ownership check
// (sweeping every dead-leader session) fails the first assertions.
func TestSweepOrphanedWorkerSessions_AC4_NoRecordNoKill(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	sess := spawnDeadLeaderSession(t, sleepChildScript, 1)

	sweepOrphanedWorkerSessions() // no records at all
	if !pidAlive(sess.childPIDs[0]) {
		t.Fatal("killed a process descended from no recorded worker")
	}

	other := recordFor(sess)
	other.PID = sess.leaderPID + 100000
	other.ID = "other"
	if err := appendWorkerRecord(other); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	sweepOrphanedWorkerSessions()
	if !pidAlive(sess.childPIDs[0]) {
		t.Fatal("killed a process on the strength of an unrelated worker's record")
	}

	if err := appendWorkerRecord(recordFor(sess)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	sweepOrphanedWorkerSessions()
	if !waitUntilDead(t, sess.childPIDs[0], 5*time.Second) {
		t.Fatal("recorded dead worker's orphan was not reaped")
	}
}

// AC3: a live session leader with a matching fingerprint is never swept, even
// though its members share the SID.
func TestSweepOrphanedWorkerSessions_AC3_LiveWorkerUntouched(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	_, sess, release := spawnSession(t, sleepChildScript, 1)
	defer release()
	if err := appendWorkerRecord(recordFor(sess)); err != nil {
		t.Fatal(err)
	}
	if _, reaped, _, _ := sweepOrphanedWorkerSessions(); reaped != 0 {
		t.Fatalf("reaped=%d members of a live worker", reaped)
	}
	if !pidAlive(sess.childPIDs[0]) {
		t.Fatal("live worker's descendant was killed")
	}
}

// AC3 / R8: a dead worker's PID is now held by a new live process; the new
// holder's descendants (started after it) must survive. The record is
// backdated so the live holder is judged "recycled".
func TestSweepOrphanedWorkerSessions_R8_RecycledPIDNewHolderDescendantsSurvive(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	_, sess, release := spawnSession(t, sleepChildScript, 1)
	defer release()
	rec := recordFor(sess)
	rec.LStart = time.Now().Add(-48 * time.Hour).Format(lstartLayout) // the "dead" worker's start
	if err := appendWorkerRecord(rec); err != nil {
		t.Fatal(err)
	}
	// Child starts at/after the holder, so the upper bound (strictly before
	// holder start) must exclude it.
	if _, reaped, _, _ := sweepOrphanedWorkerSessions(); reaped != 0 {
		t.Fatalf("reaped=%d descendants of the live new holder", reaped)
	}
	if !pidAlive(sess.childPIDs[0]) || !pidAlive(sess.leaderPID) {
		t.Fatal("new live worker or its descendant was killed")
	}
}

// R8 discriminator table against a real orphan: the member is reaped only when
// its start time is >= the dead worker's and strictly < the live holder's;
// unparseable and equal timestamps fail open.
func TestReapWorkerSessionMembers_R8_HolderBounds(t *testing.T) {
	fastScan(t)
	cases := []struct {
		name       string
		recOffset  time.Duration // rec.LStart relative to member start
		holder     func(member time.Time) string
		wantReaped bool
	}{
		{"holder after member", -time.Hour, func(m time.Time) string { return m.Add(time.Hour).Format(lstartLayout) }, true},
		{"holder before member", -2 * time.Hour, func(m time.Time) string { return m.Add(-time.Hour).Format(lstartLayout) }, false},
		{"holder equal to member", -time.Hour, func(m time.Time) string { return m.Format(lstartLayout) }, false},
		{"holder unparseable", -time.Hour, func(m time.Time) string { return "garbage" }, false},
		{"member older than dead worker", time.Hour, func(m time.Time) string { return m.Add(2 * time.Hour).Format(lstartLayout) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := spawnDeadLeaderSession(t, sleepChildScript, 1)
			_, mstart, err := pidFingerprint(sess.childPIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			member, ok := parseLStart(mstart)
			if !ok {
				t.Fatalf("cannot parse %q", mstart)
			}
			rec := recordFor(sess)
			rec.LStart = member.Add(tc.recOffset).Format(lstartLayout)
			tally := reapWorkerSessionMembers(rec, sess.childPIDs, tc.holder(member), map[int]bool{}, "test")
			if (tally.reaped == 1) != tc.wantReaped {
				t.Fatalf("reaped=%d skipped=%d, wantReaped=%v", tally.reaped, tally.skipped, tc.wantReaped)
			}
			if tc.wantReaped && !waitUntilDead(t, sess.childPIDs[0], 5*time.Second) {
				t.Fatal("member reaped per tally but still alive")
			}
			if !tc.wantReaped && !pidAlive(sess.childPIDs[0]) {
				t.Fatal("member killed despite failing the R8 bounds")
			}
			if !tc.wantReaped && tally.skipped != 1 {
				t.Fatalf("expected the member to be counted as skipped (record kept), got %+v", tally)
			}
		})
	}
}

func TestClassifyWorker(t *testing.T) {
	self := os.Getpid()
	comm, lstart, err := pidFingerprint(self)
	if err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	orig := pidFingerprintFn
	t.Cleanup(func() { pidFingerprintFn = orig })

	if s, _ := classifyWorker(workerRecord{PID: dead.Process.Pid, LStart: lstart}); s != workerDead {
		t.Errorf("dead pid: got %v", s)
	}
	if s, _ := classifyWorker(workerRecord{PID: self, Comm: comm, LStart: lstart}); s != workerLive {
		t.Errorf("matching fingerprint: got %v", s)
	}
	if s, h := classifyWorker(workerRecord{PID: self, Comm: comm, LStart: "Mon Jan  1 00:00:00 2001"}); s != workerRecycled || h != lstart {
		t.Errorf("lstart mismatch: got %v holder=%q", s, h)
	}
	// comm alone differing (equal lstart) is the same worker.
	if s, _ := classifyWorker(workerRecord{PID: self, Comm: "renamed", LStart: lstart}); s != workerLive {
		t.Errorf("comm-only change: got %v", s)
	}
	// R9: alive PID and no recorded identity.
	if s, _ := classifyWorker(workerRecord{PID: self}); s != workerInconclusive {
		t.Errorf("empty fingerprint with live pid: got %v", s)
	}
	// R8: transient lookup failure is inconclusive.
	pidFingerprintFn = func(int) (string, string, error) { return "", "", errors.New("ps timeout") }
	if s, _ := classifyWorker(workerRecord{PID: self, Comm: comm, LStart: lstart}); s != workerInconclusive {
		t.Errorf("fingerprint error: got %v", s)
	}
}

// R9: a record with no fingerprint and an alive PID is never swept; once the
// PID is dead it is swept but bounded by SpawnedAt, and the record is kept
// whenever a member was skipped.
func TestSweepOrphanedWorkerSessions_R9_EmptyFingerprint(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)

	_, live, release := spawnSession(t, sleepChildScript, 1)
	defer release()
	rec := recordFor(live)
	rec.Comm, rec.LStart = "", ""
	if err := appendWorkerRecord(rec); err != nil {
		t.Fatal(err)
	}
	sweepOrphanedWorkerSessions()
	if !pidAlive(live.childPIDs[0]) {
		t.Fatal("liveness-only record caused a kill while the PID was alive")
	}
	if recs, _ := allWorkerRecords(); len(recs) != 1 {
		t.Fatalf("record must be kept: %+v", recs)
	}

	// Now a dead-PID, empty-fingerprint record whose SpawnedAt postdates the
	// member: the member cannot belong to it, so it is skipped and the record kept.
	dead := spawnDeadLeaderSession(t, sleepChildScript, 1)
	rec2 := recordFor(dead)
	rec2.Comm, rec2.LStart = "", ""
	rec2.ID = "future"
	rec2.SpawnedAt = time.Now().Add(time.Hour)
	if err := appendWorkerRecord(rec2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	_, reaped, skipped, pruned := sweepOrphanedWorkerSessions()
	if reaped != 0 || skipped != 1 || pruned != 0 || !pidAlive(dead.childPIDs[0]) {
		t.Fatalf("reaped=%d skipped=%d pruned=%d alive=%v", reaped, skipped, pruned, pidAlive(dead.childPIDs[0]))
	}

	// With a plausible SpawnedAt the same member is reaped.
	if err := updateWorkerRecord("future", func(r *workerRecord) { r.SpawnedAt = time.Now() }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, reaped, _, _ := sweepOrphanedWorkerSessions(); reaped != 1 {
		t.Fatalf("reaped=%d, want 1", reaped)
	}
	if !waitUntilDead(t, dead.childPIDs[0], 5*time.Second) {
		t.Fatal("member not reaped")
	}
}

// Fail open: a transient fingerprint failure on a member skips it, keeps it
// alive and keeps the record (never counts toward pruning).
func TestSweepOrphanedWorkerSessions_TransientMemberFingerprintFailureKeepsRecord(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	sess := spawnDeadLeaderSession(t, sleepChildScript, 1)
	if err := appendWorkerRecord(recordFor(sess)); err != nil {
		t.Fatal(err)
	}
	orig := pidFingerprintFn
	pidFingerprintFn = func(pid int) (string, string, error) {
		if pid == sess.childPIDs[0] {
			return "", "", errors.New("ps timeout")
		}
		return orig(pid)
	}
	t.Cleanup(func() { pidFingerprintFn = orig })

	for i := 0; i < 3; i++ {
		time.Sleep(10 * time.Millisecond)
		_, reaped, skipped, pruned := sweepOrphanedWorkerSessions()
		if reaped != 0 || skipped != 1 || pruned != 0 {
			t.Fatalf("pass %d: reaped=%d skipped=%d pruned=%d", i, reaped, skipped, pruned)
		}
	}
	if !pidAlive(sess.childPIDs[0]) {
		t.Fatal("member killed despite inconclusive fingerprint")
	}
	if recs, _ := allWorkerRecords(); len(recs) != 1 || recs[0].EmptyScans != 0 {
		t.Fatalf("record must be kept with EmptyScans reset: %+v", recs)
	}
}

// A scan error changes nothing (fail open).
func TestSweepOrphanedWorkerSessions_ScanErrorFailsOpen(t *testing.T) {
	useWorkerRecordsDir(t)
	fastScan(t)
	if err := appendWorkerRecord(workerRecord{ID: "x", PID: 999999, EmptyScans: 1}); err != nil {
		t.Fatal(err)
	}
	orig := listProcessArgvFn
	listProcessArgvFn = func() ([]procArgvEntry, error) { return nil, errors.New("ps failed") }
	t.Cleanup(func() { listProcessArgvFn = orig })
	time.Sleep(5 * time.Millisecond)
	if _, _, _, pruned := sweepOrphanedWorkerSessions(); pruned != 0 {
		t.Fatal("pruned on scan error")
	}
	if recs, _ := allWorkerRecords(); len(recs) != 1 || recs[0].EmptyScans != 1 {
		t.Fatalf("record mutated on scan error: %+v", recs)
	}
}

func TestBeginWorkerRecord_CapturesFingerprintAndBackfills(t *testing.T) {
	useWorkerRecordsDir(t)
	self := os.Getpid()
	rec := beginWorkerRecord(self, 1814, "o/r", "Implement")
	if rec.Comm == "" || rec.LStart == "" || rec.PID != self || rec.IssueNumber != 1814 || rec.Stage != "Implement" {
		t.Fatalf("unexpected record %+v", rec)
	}
	if recs, _ := allWorkerRecords(); len(recs) != 1 || recs[0].ID != rec.ID {
		t.Fatalf("record not persisted: %+v", recs)
	}

	orig := pidFingerprintFn
	fail := true
	pidFingerprintFn = func(pid int) (string, string, error) {
		if fail {
			return "", "", errors.New("transient")
		}
		return orig(pid)
	}
	t.Cleanup(func() { pidFingerprintFn = orig })
	rec2 := beginWorkerRecord(self, 1814, "o/r", "Implement")
	if rec2.LStart != "" || rec2.Comm != "" {
		t.Fatalf("fingerprint failure must leave fields empty: %+v", rec2)
	}
	fail = false
	fastScan(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backfillWorkerFingerprint(ctx, rec2.ID, self)
	recs, _ := allWorkerRecords()
	var got workerRecord
	for _, r := range recs {
		if r.ID == rec2.ID {
			got = r
		}
	}
	if got.LStart == "" || got.Comm == "" {
		t.Fatalf("backfill did not persist the fingerprint: %+v", got)
	}
}

// invokeFakeClaudeNoTick runs InvokeClaude against a fake claude script with
// the descendant tracker's tick pushed out to an hour, so it can never record
// anything — isolating the registry-independent invocation-end sweep.
func invokeFakeClaudeNoTick(t *testing.T, script string) {
	t.Helper()
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	origDelay := claudeWaitDelay
	claudeWaitDelay = time.Second
	t.Cleanup(func() { claudeWaitDelay = origDelay })
	origScan := descendantScanInterval
	descendantScanInterval = time.Hour
	t.Cleanup(func() { descendantScanInterval = origScan })

	done := make(chan error, 1)
	go func() {
		_, _, _, err := InvokeClaude(context.Background(), &stages.Stage{Name: "Implement", Prompt: "p"},
			gh.ProjectItem{Number: 1814, Title: "t"}, nil, false, t.TempDir(), InvokeOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("InvokeClaude: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("InvokeClaude did not return")
	}
}

// AC1 + R5: a descendant spawned in the final instant before its worker exits
// — never seen by the tracker (its tick is an hour) — is reaped at invocation
// end, and the worker record is pruned.
func TestInvokeClaude_AC1_UnrecordedLateDescendantReapedAtInvocationEnd(t *testing.T) {
	useWorkerRecordsDir(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "#!/bin/sh\ncat >/dev/null\nset -m\n" +
		"sleep 60 >/dev/null 2>&1 &\necho $! > '" + pidFile + "'\ndisown 2>/dev/null || true\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"
	invokeFakeClaudeNoTick(t, script)

	childPID := readPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Fatalf("unrecorded late descendant %d survived invocation end", childPID)
	}
	if d, _ := allTrackedDescendants(); len(d) != 0 {
		t.Fatalf("test invalid: tracker recorded descendants %+v", d)
	}
	if recs, _ := allWorkerRecords(); len(recs) != 0 {
		t.Fatalf("worker record not pruned after a clean invocation-end sweep: %+v", recs)
	}
}

// AC6 (real runClaude, mixed detached children): after an invocation, no
// process with the dead worker as session leader remains. A Getsid
// enumeration — not the registry — is the oracle.
func TestInvokeClaude_AC6_NoDeadLeaderSessionMembersRemain(t *testing.T) {
	useWorkerRecordsDir(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	workerFile := filepath.Join(dir, "worker.pid")
	script := "#!/bin/sh\ncat >/dev/null\necho $$ > '" + workerFile + "'\n" +
		"nohup sleep 60 >/dev/null 2>&1 &\n" +
		"( sleep 60 >/dev/null 2>&1 & )\n" +
		"/bin/sh -c 'read x < \"" + fifo + "\"' >/dev/null 2>&1 &\n" +
		"set -m\nsleep 60 >/dev/null 2>&1 &\ndisown 2>/dev/null || true\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"
	invokeFakeClaudeNoTick(t, script)

	workerPID := readPIDFile(t, workerFile)
	t.Cleanup(func() {
		if pids, err := sessionScopedDescendants(workerPID); err == nil {
			for _, p := range pids {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	var left []int
	for time.Now().Before(deadline) {
		var err error
		if left, err = sessionScopedDescendants(workerPID); err != nil {
			t.Fatal(err)
		}
		// A SIGKILLed process may be briefly visible as a zombie.
		alive := left[:0]
		for _, p := range left {
			if pidAlive(p) && !isZombie(p) {
				alive = append(alive, p)
			}
		}
		if left = alive; len(left) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session members of dead worker %d survive: %v", workerPID, left)
}

func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// The janitor emits the additive second summary line and leaves the #1798 line
// untouched.
func TestRunProcessSweepJanitor_LogsSessionSweepLine(t *testing.T) {
	t.Chdir(t.TempDir())
	useWorkerRecordsDir(t)
	fastScan(t)
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	e.events = make(chan tui.Event, 16)
	e.runProcessSweepJanitor(context.Background())
	close(e.events)
	var msgs []string
	for ev := range e.events {
		if le, ok := ev.(tui.LogEvent); ok {
			msgs = append(msgs, le.Message)
		}
	}
	joined := strings.Join(msgs, "|")
	if !strings.Contains(joined, "cycle complete: scanned 0 registry entries, reaped 0, skipped 0") {
		t.Errorf("#1798 summary line changed or missing: %q", joined)
	}
	if !strings.Contains(joined, "session sweep complete: worker records 0, session members reaped 0, skipped 0, records pruned 0") {
		t.Errorf("session sweep summary line missing: %q", joined)
	}
}
