//go:build !windows

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/stages"
)

// sessionChildScript backgrounds, under job control (`set -m`), a child shell
// that lands in its own process group of the SAME session as the worker — the
// shape a Claude Bash-tool command has (#1989 R1). The child traps SIGINT and
// SIGTERM, recording each in a marker file, and keeps running, so a graceful
// signal is observable without the child dying from it. SIGKILL is untrappable.
func sessionChildScript(dir string) string {
	return "set -m\n" +
		"sh -c \"trap 'echo x > " + dir + "/int.mark' INT; trap 'echo x > " + dir + "/term.mark' TERM; echo \\$\\$ > " + dir + "/child.pid; while :; do sleep 0.05; done\" >/dev/null 2>&1 &\n"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// startSessionWorker starts a fake worker (session leader via setCmdProcAttr)
// that spawns sessionChildScript's child and then sleeps. Returns the worker
// command and the child's PID.
func startSessionWorker(t *testing.T, dir string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", sessionChildScript(dir)+"sleep 60\n")
	setCmdProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap the leader exactly once, in the background, so it never lingers
	// as a zombie that would look like a live session member.
	reaped := make(chan struct{})
	go func() { defer close(reaped); _ = cmd.Wait() }()
	t.Cleanup(func() {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		<-reaped
	})
	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	return cmd, childPID
}

// groupOnlyEscalate is the pre-#1989 stop path: signals only the worker's
// process group. Tests swap it into sessionEscalateFn to prove the session
// reap is load-bearing (neutralise it and the acceptance tests must fail).
func groupOnlyEscalate(sid int, _ string, sigintGrace, sigtermGrace time.Duration, _ sessionreap.Options) {
	if sigintGrace > 0 {
		_ = unix.Kill(-sid, unix.SIGINT)
		time.Sleep(sigintGrace)
	}
	if sigtermGrace > 0 {
		_ = unix.Kill(-sid, unix.SIGTERM)
		time.Sleep(sigtermGrace)
	}
	_ = unix.Kill(-sid, unix.SIGKILL)
}

func neutraliseSessionReap(t *testing.T) {
	t.Helper()
	origE, origS := sessionEscalateFn, sessionSweepFn
	sessionEscalateFn = groupOnlyEscalate
	sessionSweepFn = func(int, string, sessionreap.Options) int { return 0 }
	t.Cleanup(func() { sessionEscalateFn, sessionSweepFn = origE, origS })
}

// captureClaudeLog collects engine log lines for the duration of the test.
func captureClaudeLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	orig := claudeLogf
	claudeLogf = func(n int, tag, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf("[#%d %s] "+format, append([]any{n, tag}, args...)...))
	}
	t.Cleanup(func() { claudeLogf = orig })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "")
	}
}

// R1 (recorded against pre-#1989 code, see the R1 commit): killProcGroupGraceful
// left a same-session, separate-group child alive and never signalled it; the
// #1798/#1814 sweeps only SIGKILLed it later at invocation end. The tests
// below are the R2 acceptance tests; each has a neutralised twin asserting the
// R1 behaviour so the acceptance assertions cannot be vacuous.

func sigsReceived(dir string) bool {
	return fileExists(filepath.Join(dir, "int.mark")) || fileExists(filepath.Join(dir, "term.mark"))
}

func TestSessionReap_StopPath_ReachesSameSessionChild(t *testing.T) {
	dir := t.TempDir()
	cmd, childPID := startSessionWorker(t, dir)

	killProcGroupGraceful(cmd.Process.Pid, 1989, "r2", "max_wall_time", 300*time.Millisecond, 300*time.Millisecond)

	if !waitUntilDead(t, childPID, 3*time.Second) {
		t.Fatalf("same-session child %d survived the stop path", childPID)
	}
	if !sigsReceived(dir) {
		t.Errorf("child was SIGKILLed without ever receiving SIGINT/SIGTERM")
	}
}

func TestSessionReap_StopPath_NeutralisedGroupOnlyLeavesChild(t *testing.T) {
	neutraliseSessionReap(t)
	dir := t.TempDir()
	cmd, childPID := startSessionWorker(t, dir)

	killProcGroupGraceful(cmd.Process.Pid, 1989, "r1", "max_wall_time", 100*time.Millisecond, 100*time.Millisecond)

	if !pidAlive(childPID) {
		t.Errorf("group-only stop path unexpectedly reached the child (R1 premise / fixture broken)")
	}
	if sigsReceived(dir) {
		t.Errorf("group-only stop path delivered a graceful signal to the child")
	}
}

// runInvocation runs InvokeClaude against a fake claude script with the given
// stage and short grace windows; returns when it does.
func runInvocation(t *testing.T, script string, stage *stages.Stage) {
	t.Helper()
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()
	origScan := descendantScanInterval
	descendantScanInterval = 50 * time.Millisecond
	defer func() { descendantScanInterval = origScan }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = InvokeClaude(context.Background(), stage, gh.ProjectItem{Number: 1989, Title: "session reap"}, nil, false, t.TempDir(),
			InvokeOptions{SigIntGrace: 300 * time.Millisecond, SigTermGrace: 300 * time.Millisecond})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("InvokeClaude did not return")
	}
}

func stopScript(dir string) string {
	return "#!/bin/sh\ncat >/dev/null\n" + sessionChildScript(dir) + "sleep 60\n"
}

func cleanExitScript(dir string) string {
	return "#!/bin/sh\ncat >/dev/null\n" + sessionChildScript(dir) +
		"while [ ! -s " + dir + "/child.pid ]; do sleep 0.02; done\n" +
		"printf '%s\\n' '" + fakeClaudeCompleteJSON + "'\n"
}

func TestSessionReap_MaxWallTime_ChildGetsGracefulSignalsAndDies(t *testing.T) {
	dir := t.TempDir()
	logs := captureClaudeLog(t)
	runInvocation(t, stopScript(dir), &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second})

	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Fatalf("child survived max_wall_time stop; log:\n%s", logs())
	}
	if !sigsReceived(dir) {
		t.Errorf("child never saw SIGINT/SIGTERM on the max_wall_time stop path; log:\n%s", logs())
	}
	if !strings.Contains(logs(), "signalled") || !strings.Contains(logs(), "member(s)") {
		t.Errorf("R5 line missing; log:\n%s", logs())
	}
}

func TestSessionReap_MaxWallTime_NeutralisedChildOnlySIGKILLedLater(t *testing.T) {
	neutraliseSessionReap(t)
	dir := t.TempDir()
	runInvocation(t, stopScript(dir), &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second})

	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	// R1's middle outcome: gone only because the #1798/#1814 sweeps SIGKILL it
	// at invocation end — and it never saw a graceful signal.
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Errorf("child survived even the invocation-end sweeps")
	}
	if sigsReceived(dir) {
		t.Errorf("group-only stop path delivered a graceful signal")
	}
}

func TestSessionReap_CleanExit_PostExitSweepReapsAndLogs(t *testing.T) {
	dir := t.TempDir()
	logs := captureClaudeLog(t)
	runInvocation(t, cleanExitScript(dir), &stages.Stage{Name: "Implement", Prompt: "p"})

	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	if !waitUntilDead(t, childPID, 5*time.Second) {
		t.Fatalf("child left behind by a clean exit survived; log:\n%s", logs())
	}
	if !strings.Contains(logs(), "session reap: signalled") || !strings.Contains(logs(), "exit=clean_exit") {
		t.Errorf("no R5 clean-exit line from the post-exit session sweep; log:\n%s", logs())
	}
}

func TestSessionReap_CleanExit_NeutralisedHasNoSessionSweepLine(t *testing.T) {
	neutraliseSessionReap(t)
	dir := t.TempDir()
	logs := captureClaudeLog(t)
	runInvocation(t, cleanExitScript(dir), &stages.Stage{Name: "Implement", Prompt: "p"})

	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	waitUntilDead(t, childPID, 5*time.Second)
	if strings.Contains(logs(), "session reap: signalled") {
		t.Errorf("neutralised sweep still logged a session reap")
	}
}

// R4 end-to-end: a process in another session survives a max_wall_time stop.
func TestSessionReap_MaxWallTime_BystanderInOtherSessionSurvives(t *testing.T) {
	bystander := exec.Command("sleep", "60")
	bystander.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })

	dir := t.TempDir()
	runInvocation(t, stopScript(dir), &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second})
	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	if !pidAlive(bystander.Process.Pid) {
		t.Fatal("process in another session was killed by the worker's session reap")
	}
}
