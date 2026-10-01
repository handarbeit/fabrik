//go:build !windows

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	gh "github.com/handarbeit/fabrik/github"
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
	t.Cleanup(func() {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		_ = cmd.Wait()
	})
	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	return cmd, childPID
}

// TestR1_StopPath_GroupScopedKillMissesSameSessionChild is the #1989 R1
// reproduction against the stop-path primitive in isolation (no
// invocation-end sweeps): a child in a separate process group of the worker's
// session receives neither SIGINT nor SIGTERM from killProcGroupGraceful, and
// survives it.
func TestR1_StopPath_GroupScopedKillMissesSameSessionChild(t *testing.T) {
	dir := t.TempDir()
	cmd, childPID := startSessionWorker(t, dir)

	sid, err := unix.Getsid(childPID)
	if err != nil || sid != cmd.Process.Pid {
		t.Fatalf("child sid=%d err=%v, want worker pid %d", sid, err, cmd.Process.Pid)
	}
	pgid, _ := unix.Getpgid(childPID)
	if pgid == cmd.Process.Pid {
		t.Fatalf("child pgid == worker pgid %d; fixture failed to separate the group", pgid)
	}

	killProcGroupGraceful(cmd.Process.Pid, 1989, "r1", "max_wall_time", 200*time.Millisecond, 200*time.Millisecond)
	_ = cmd.Wait()

	t.Logf("R1 outcome: child alive after stop path=%v, SIGINT received=%v, SIGTERM received=%v",
		pidAlive(childPID), fileExists(filepath.Join(dir, "int.mark")), fileExists(filepath.Join(dir, "term.mark")))
	if !pidAlive(childPID) {
		t.Errorf("R1: expected the same-session child to survive the group-scoped stop path")
	}
	if fileExists(filepath.Join(dir, "int.mark")) || fileExists(filepath.Join(dir, "term.mark")) {
		t.Errorf("R1: child unexpectedly received a graceful signal from the group-scoped kill")
	}
}

// TestR1_MaxWallTime_InvocationEndSweepCatchesChild runs the real stop path
// (max_wall_time through InvokeClaude) and records what the existing
// invocation-end sweeps (#1798/#1814) do with the same child.
func TestR1_MaxWallTime_InvocationEndSweepCatchesChild(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	binDir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" + sessionChildScript(dir) + "sleep 60\n"
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

	stage := &stages.Stage{Name: "Implement", Prompt: "p", MaxWallTime: time.Second}
	issue := gh.ProjectItem{Number: 1989, Title: "R1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = InvokeClaude(context.Background(), stage, issue, nil, false, t.TempDir(),
			InvokeOptions{SigIntGrace: 300 * time.Millisecond, SigTermGrace: 300 * time.Millisecond})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("InvokeClaude did not return")
	}

	childPID := readPIDFile(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = unix.Kill(childPID, unix.SIGKILL) })
	dead := waitUntilDead(t, childPID, 5*time.Second)
	t.Logf("R1 outcome: child dead after InvokeClaude=%v, SIGINT received=%v, SIGTERM received=%v",
		dead, fileExists(filepath.Join(dir, "int.mark")), fileExists(filepath.Join(dir, "term.mark")))
	if !dead {
		t.Errorf("child survived even the invocation-end sweeps")
	}
	if strings.Contains(os.Getenv("R1_STRICT"), "1") && (fileExists(filepath.Join(dir, "int.mark")) || fileExists(filepath.Join(dir, "term.mark"))) {
		t.Errorf("child received a graceful signal")
	}
}
