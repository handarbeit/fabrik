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

	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/stages"
)

// The tests in session_reap_unix_test.go model a child that stays in the
// worker's own session. Live workers do not behave that way (#1989 validation
// finding): Claude's Bash tool runs every command through a shell that calls
// setsid(), so each command has a session of its own and the worker's SID
// reaches none of it. These tests model that real shape:
//
//	worker W (sid W) ── command shell C (sid C, setsid) ── child G (sid C, own pgid)
//
// The shell is launched from the worker with `&`, so its PPID chain leads to W
// while W lives. perl performs setsid()/setpgrp() so the fixture does not depend
// on the shell (dash, the Linux CI /bin/sh, has no usable `set -m` and ignores
// SIGINT for background jobs, which perl resets).

// bashToolCommandLaunch writes the command shell and child scripts into dir and
// returns the shell fragment a fake worker runs to start them. The child traps
// INT/TERM into dir/sigs and keeps running; both PIDs are written to dir.
func bashToolCommandLaunch(t *testing.T, dir string) string {
	t.Helper()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	childSh := write("child.sh", "trap 'echo x >> "+dir+"/sigs' INT TERM\necho $$ > "+dir+"/child.pid\nwhile :; do sleep 0.05; done\n")
	cmdSh := write("cmd.sh", "perl -e '$SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); setpgrp(0,0); exec @ARGV' sh "+childSh+" >/dev/null 2>&1 &\necho $$ > "+dir+"/shell.pid\nwait\n")
	return "perl -MPOSIX -e 'POSIX::setsid(); $SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); exec @ARGV' sh " + cmdSh + " >/dev/null 2>&1 &\n"
}

func bashToolStopScript(t *testing.T, dir string) string {
	return "#!/bin/sh\ncat >/dev/null\n" + bashToolCommandLaunch(t, dir) + "sleep 60\n"
}

// bashToolCleanExitScript starts the command, waits for it to be up and for the
// sampler to have had time to see it, then finishes cleanly — leaving the
// command running, which is what a `run_in_background` test run looks like.
func bashToolCleanExitScript(t *testing.T, dir string) string {
	return "#!/bin/sh\ncat >/dev/null\n" + bashToolCommandLaunch(t, dir) +
		"while [ ! -s " + dir + "/child.pid ]; do sleep 0.02; done\n" +
		"sleep 0.4\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"
}

func fastCommandSessionSampling(t *testing.T) {
	t.Helper()
	orig := commandSessionSampleInterval
	commandSessionSampleInterval = 50 * time.Millisecond
	t.Cleanup(func() { commandSessionSampleInterval = orig })
}

func workerSIDOnly(t *testing.T) {
	t.Helper()
	sessionReapWorkerSIDOnly = true
	t.Cleanup(func() { sessionReapWorkerSIDOnly = false })
}

func cleanupCommandPIDs(t *testing.T, dir string) (shell, child int) {
	t.Helper()
	shell = readPIDFile(t, filepath.Join(dir, "shell.pid"))
	child = readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() {
		_ = unix.Kill(child, unix.SIGKILL)
		_ = unix.Kill(shell, unix.SIGKILL)
	})
	return shell, child
}

func TestBashToolSession_MaxWallTime_ReapedGracefully(t *testing.T) {
	dir := t.TempDir()
	logs := captureClaudeLog(t)
	runInvocation(t, bashToolStopScript(t, dir), &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second})

	shell, child := cleanupCommandPIDs(t, dir)
	if !waitUntilDead(t, child, 5*time.Second) || !waitUntilDead(t, shell, 5*time.Second) {
		t.Fatalf("Bash-tool command session survived the max_wall_time stop (child alive=%v shell alive=%v); log:\n%s", pidAlive(child), pidAlive(shell), logs())
	}
	if _, err := os.Stat(filepath.Join(dir, "sigs")); err != nil {
		t.Errorf("child was never signalled gracefully before dying; log:\n%s", logs())
	}
	if !strings.Contains(logs(), "command session(s)") {
		t.Errorf("R5 line does not name a command session; log:\n%s", logs())
	}
}

func TestBashToolSession_MaxWallTime_WorkerSIDOnlyLeavesItRunning(t *testing.T) {
	// Neutralised twin — this PR's earlier behaviour, and the live orphans: the
	// stop reaches the worker's own SID only, and the #1798/#1814 sweeps (which
	// also filter on SID == worker PID) never see the command session.
	workerSIDOnly(t)
	dir := t.TempDir()
	runInvocation(t, bashToolStopScript(t, dir), &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second})

	shell, child := cleanupCommandPIDs(t, dir)
	time.Sleep(500 * time.Millisecond)
	if !pidAlive(child) || !pidAlive(shell) {
		t.Fatalf("worker-SID-only reap reached the command session (child alive=%v shell alive=%v); the fixture does not model the live shape", pidAlive(child), pidAlive(shell))
	}
}

func TestBashToolSession_CleanExit_ReapedThroughSampledSessions(t *testing.T) {
	fastCommandSessionSampling(t)
	dir := t.TempDir()
	logs := captureClaudeLog(t)
	runInvocation(t, bashToolCleanExitScript(t, dir), &stages.Stage{Name: "Implement", Prompt: "p"})

	shell, child := cleanupCommandPIDs(t, dir)
	if !waitUntilDead(t, child, 5*time.Second) || !waitUntilDead(t, shell, 5*time.Second) {
		t.Fatalf("command session left behind by a clean exit survived (child alive=%v shell alive=%v); log:\n%s", pidAlive(child), pidAlive(shell), logs())
	}
	if !strings.Contains(logs(), "exit=clean_exit") || !strings.Contains(logs(), "command session(s)") {
		t.Errorf("no R5 clean-exit line naming the command session; log:\n%s", logs())
	}
}

func TestBashToolSession_CleanExit_WorkerSIDOnlyLeavesItRunning(t *testing.T) {
	workerSIDOnly(t)
	fastCommandSessionSampling(t)
	dir := t.TempDir()
	runInvocation(t, bashToolCleanExitScript(t, dir), &stages.Stage{Name: "Implement", Prompt: "p"})

	shell, child := cleanupCommandPIDs(t, dir)
	time.Sleep(500 * time.Millisecond)
	if !pidAlive(child) || !pidAlive(shell) {
		t.Fatalf("worker-SID-only sweep reached the command session (child alive=%v shell alive=%v)", pidAlive(child), pidAlive(shell))
	}
}

func TestBashToolSession_BystanderInOtherSessionSurvives(t *testing.T) {
	bystander := exec.Command("sleep", "60")
	bystander.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })

	dir := t.TempDir()
	runInvocation(t, bashToolStopScript(t, dir), &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second})
	cleanupCommandPIDs(t, dir)
	if !pidAlive(bystander.Process.Pid) {
		t.Fatal("a process in an unrelated session was killed by the command-session reap")
	}
}

// startDetachedCommandSession starts the fixture worker directly (not through
// InvokeClaude) and returns its PID plus the command shell and child PIDs.
func startDetachedCommandSession(t *testing.T, dir string) (worker *exec.Cmd, shell, child int) {
	t.Helper()
	script := "#!/bin/sh\n" + bashToolCommandLaunch(t, dir) + "sleep 60\n"
	p := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	worker = exec.Command("sh", p)
	setCmdProcAttr(worker)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { defer close(reaped); _ = worker.Wait() }()
	t.Cleanup(func() { _ = unix.Kill(-worker.Process.Pid, unix.SIGKILL); <-reaped })
	shell, child = cleanupCommandPIDs(t, dir)
	if sid, _ := unix.Getsid(shell); sid != shell || shell == worker.Process.Pid {
		t.Fatalf("fixture: command shell sid=%d pid=%d worker=%d; it must lead a session of its own", sid, shell, worker.Process.Pid)
	}
	if sid, _ := unix.Getsid(child); sid != shell {
		t.Fatalf("fixture: child sid=%d, want the command shell's session %d", sid, shell)
	}
	return worker, shell, child
}

func TestCommandSessionRecords_PersistedAndPrunedAtInvocationEnd(t *testing.T) {
	useWorkerRecordsDir(t)
	dir := t.TempDir()
	worker, shell, child := startDetachedCommandSession(t, dir)
	parent := workerRecord{ID: "w-1", PID: worker.Process.Pid, Repo: "o/r", Stage: "Implement"}

	sess, err := sessionreap.Discover(worker.Process.Pid, sessionreap.Options{})
	if err != nil || len(sess) != 1 || sess[0].SID != shell {
		t.Fatalf("Discover = %+v, %v; want the command shell's session %d", sess, err, shell)
	}
	persistCommandSessions(parent, sess, 1989)
	recs, _ := allWorkerRecords()
	if len(recs) != 1 || recs[0].ParentID != "w-1" || recs[0].PID != shell || recs[0].StartToken == "" {
		t.Fatalf("records = %+v; want one command-session record linked to w-1 with a start token", recs)
	}

	// Not empty yet: finishCommandSessions keeps the record for the janitor.
	tr := sessionreap.Track(worker.Process.Pid, sessionReapOptions(1989), nil)
	tr.Sample()
	finishCommandSessions(tr, parent, 1989)
	if recs, _ = allWorkerRecords(); len(recs) != 1 {
		t.Fatalf("record for a still-running session was pruned: %+v", recs)
	}

	_ = unix.Kill(child, unix.SIGKILL)
	_ = unix.Kill(shell, unix.SIGKILL)
	if !waitUntilDead(t, child, 3*time.Second) || !waitUntilDead(t, shell, 3*time.Second) {
		t.Fatal("could not stop the fixture command session")
	}
	// The sampler only adopts sessions below a live worker, so replay the
	// observation into a fresh tracker; the session itself is now empty.
	finishCommandSessions(trackerWith(t, worker.Process.Pid, sess), parent, 1989)
	if recs, _ = allWorkerRecords(); len(recs) != 0 {
		t.Fatalf("record for an emptied session was not pruned: %+v", recs)
	}
}

// trackerWith returns a registered tracker already holding sess.
func trackerWith(t *testing.T, worker int, sess []sessionreap.Session) *sessionreap.Tracker {
	t.Helper()
	// Sampling is impossible once the command is dead, so drive a tracker whose
	// Options.Procs reports the process table as it was.
	opts := sessionReapOptions(1989)
	opts.Procs = func() ([]sessionreap.Proc, error) {
		procs := []sessionreap.Proc{{PID: worker, PPID: 1}}
		for _, s := range sess {
			procs = append(procs, sessionreap.Proc{PID: s.SID, PPID: worker})
		}
		return procs, nil
	}
	opts.Getsid = func(pid int) (int, error) {
		for _, s := range sess {
			if pid == s.SID {
				return s.SID, nil
			}
		}
		return unix.Getsid(pid)
	}
	opts.Start = func(int) string { return sess[0].Start }
	tr := sessionreap.Track(worker, opts, nil)
	tr.Sample()
	if len(tr.Sessions()) != len(sess) {
		t.Fatalf("seeded tracker holds %d sessions, want %d", len(tr.Sessions()), len(sess))
	}
	return tr
}

func TestSweepOrphanedWorkerSessions_ReapsCommandSessionOfDeadWorker(t *testing.T) {
	useWorkerRecordsDir(t)
	dir := t.TempDir()
	worker, shell, child := startDetachedCommandSession(t, dir)
	sess, _ := sessionreap.Discover(worker.Process.Pid, sessionreap.Options{})
	if len(sess) != 1 {
		t.Fatalf("Discover = %+v", sess)
	}
	// Record the dead-to-be worker (no LStart: liveness-only identity is enough
	// once the PID is gone) and its command session.
	parent := workerRecord{ID: "w-dead", PID: worker.Process.Pid, Stage: "Implement", SpawnedAt: time.Now()}
	if err := appendWorkerRecord(parent); err != nil {
		t.Fatal(err)
	}
	persistCommandSessions(parent, sess, 1989)

	// While the worker is alive the janitor must leave its command session alone.
	// (The worker record has no LStart, so classifyWorker reports it inconclusive.)
	sweepOrphanedWorkerSessions()
	if !pidAlive(child) || !pidAlive(shell) {
		t.Fatalf("janitor reaped the command session of a LIVE worker (child alive=%v shell alive=%v)", pidAlive(child), pidAlive(shell))
	}

	// The engine dies mid-invocation: the worker is gone, its command session is
	// reparented to init. The next janitor pass must find it from the record.
	_ = unix.Kill(worker.Process.Pid, unix.SIGKILL)
	if !waitUntilDead(t, worker.Process.Pid, 3*time.Second) {
		t.Fatal("worker survived SIGKILL")
	}
	_, reaped, _, _ := sweepOrphanedWorkerSessions()
	if reaped < 2 {
		t.Fatalf("janitor reaped %d members, want >= 2 (shell and child)", reaped)
	}
	if !waitUntilDead(t, child, 3*time.Second) || !waitUntilDead(t, shell, 3*time.Second) {
		t.Fatalf("orphaned command session survived the janitor (child alive=%v shell alive=%v)", pidAlive(child), pidAlive(shell))
	}
	// Two consecutive empty scans prune the command-session record.
	sweepOrphanedWorkerSessions()
	sweepOrphanedWorkerSessions()
	recs, _ := allWorkerRecords()
	for _, r := range recs {
		if r.ParentID != "" {
			t.Errorf("command-session record was not pruned after its session emptied: %+v", r)
		}
	}
}

func TestSweepOrphanedWorkerSessions_RecycledCommandLeaderIsNotTouched(t *testing.T) {
	useWorkerRecordsDir(t)
	dir := t.TempDir()
	worker, _, child := startDetachedCommandSession(t, dir)
	// A record claiming the worker's own PID is a command-session leader whose
	// start token differs: that PID is held by a different (live) process now.
	parent := workerRecord{ID: "w-gone", PID: 2147483000, SpawnedAt: time.Now()} // not alive
	if err := appendWorkerRecord(parent); err != nil {
		t.Fatal(err)
	}
	persistCommandSessions(parent, []sessionreap.Session{{SID: worker.Process.Pid, Start: "stale-token"}}, 1989)
	sweepOrphanedWorkerSessions()
	if !pidAlive(worker.Process.Pid) || !pidAlive(child) {
		t.Fatal("janitor signalled a live process whose PID merely matched a recorded command-session SID")
	}
}
