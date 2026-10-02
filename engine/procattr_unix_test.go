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
)

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !isProcessAlive(pid)
}

// The normal post-exit state (#1957 R1): the worker leader has exited and been
// reaped, a grandchild in its group lives on, and killProcGroup must still
// clean it up.
func TestKillProcGroup_PostExitGrandchildIsKilled(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
	setCmdProcAttr(cmd)
	out, err := cmd.Output() // Wait() reaps the leader
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("grandchild pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	if isProcessAlive(cmd.Process.Pid) {
		t.Fatal("fixture: leader still alive after Wait")
	}
	killProcGroup(cmd, 42, "test")
	if !waitGone(grandchild, 3*time.Second) {
		t.Fatal("post-exit grandchild survived killProcGroup")
	}
}

// A stored PID whose live holder does not lead its own group — the shape of a
// recycled PID — must not be group-signalled (#1957 R1).
func TestKillProcGroup_StaleStoredPIDIsSkipped(t *testing.T) {
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

	// A cmd whose recorded PID is now held by an unrelated live process.
	stale := &exec.Cmd{Process: &os.Process{Pid: bystander}}
	killProcGroup(stale, 42, "test")
	time.Sleep(200 * time.Millisecond)
	if !isProcessAlive(bystander) {
		t.Fatal("killProcGroup signalled a stored PID it does not own")
	}
}

func TestKillProcGroup_CatastrophicPIDsRefused(t *testing.T) {
	for _, pid := range []int{-1, 0, 1, syscall.Getpgrp()} {
		// Must return without signalling anything (the test process surviving
		// is the assertion for the own-group case).
		killProcGroup(&exec.Cmd{Process: &os.Process{Pid: pid}}, 42, "test")
	}
}
