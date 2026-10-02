//go:build !windows

package engine

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// The default killFn runs against wm.currentCmd, which is never cleared after
// the subprocess exits. Once its PID is held by an unrelated live process, the
// stored-PID group kill must send nothing (#1957).
func TestWebhookKillFn_StaleCurrentCmdSendsNothing(t *testing.T) {
	wm, _ := newTestWebhookManager(t)

	starter := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
	setCmdProcAttr(starter)
	out, err := starter.Output()
	if err != nil {
		t.Fatal(err)
	}
	bystander, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(bystander, syscall.SIGKILL) })

	wm.killFn(&exec.Cmd{Process: &os.Process{Pid: bystander}})
	time.Sleep(200 * time.Millisecond)
	if !isProcessAlive(bystander) {
		t.Fatal("killFn signalled a stored PID that no longer names the subprocess")
	}
}

// A live subprocess whose start token was recorded at spawn is still killed.
func TestWebhookKillFn_LiveRecordedSubprocessIsKilled(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	cmd := exec.Command("sleep", "60")
	setCmdProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	sessionreap.RecordStart(pid, sessionreap.Options{})
	t.Cleanup(func() { forgetWebhookStart(cmd) })

	wm.killFn(cmd)
	deadline := time.Now().Add(3 * time.Second)
	for isProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if isProcessAlive(pid) {
		t.Fatal("recorded subprocess survived killFn")
	}
}

// Replacing currentCmd must drop the previous subprocess's start token, but not
// when the new subprocess was handed the same PID: its token was recorded at
// spawn, before the supersede, and forgetting by PID would wipe it (#1957).
func TestForgetSupersededWebhookStart(t *testing.T) {
	const prevPID, otherPID = 4242001, 4242002
	mk := func(pid int) *exec.Cmd { return &exec.Cmd{Process: &os.Process{Pid: pid}} }
	record := func(pid int, tok string) {
		sessionreap.RecordStart(pid, sessionreap.Options{Start: func(int) string { return tok }})
		t.Cleanup(func() { sessionreap.ForgetStart(pid) })
	}

	record(prevPID, "old")
	record(otherPID, "new")
	forgetSupersededWebhookStart(mk(prevPID), mk(otherPID))
	if got := sessionreap.OwnerOf(prevPID).Start; got != "" {
		t.Errorf("previous subprocess's token not forgotten: %q", got)
	}
	if got := sessionreap.OwnerOf(otherPID).Start; got != "new" {
		t.Errorf("new subprocess's token lost: %q", got)
	}

	// Recycled PID: prev and next share a PID, so the fresh token must survive.
	record(prevPID, "fresh")
	forgetSupersededWebhookStart(mk(prevPID), mk(prevPID))
	if got := sessionreap.OwnerOf(prevPID).Start; got != "fresh" {
		t.Errorf("token for a recycled PID was wiped: %q", got)
	}
}
