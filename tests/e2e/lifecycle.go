//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Fabrik test-bed lifecycle management. Until now the harness assumed an
// externally-started Fabrik (AssertFabrikRunning skips if it isn't up). The
// restart-safety scenario needs to actually stop and start the bed to exercise
// reconstructTrainState against durable artifacts with an empty in-memory map
// (the definition of a restart). These helpers own that lifecycle.
//
// Design constraints honored:
//   - The started process is DETACHED (new process group, released, stdio to
//     /dev/null) so it survives the test process exiting — the bed is a persistent
//     instance, and later tests / manual use expect it up.
//   - It launches WITHOUT --auto-upgrade (a train-capable dev binary must not be
//     reverted to a release mid-suite) and WITH GITHUB_TOKEN stripped from the
//     child env, so Fabrik resolves its identity from the bed's own .env
//     (FABRIK_TOKEN = @arbeithand) instead of an ambient token.

// fabrikLockPath returns the bed's lock file path.
func fabrikLockPath(env *Env) string {
	return filepath.Join(env.FabrikTestDir, ".fabrik", "fabrik.lock")
}

// lockedPID reads the bed lock and returns the pid, or 0 if no live-locked Fabrik.
func lockedPID(env *Env) int {
	contents, err := os.ReadFile(fabrikLockPath(env))
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(contents)), "%d", &pid); err != nil || pid <= 0 {
		return 0
	}
	if syscallSignalZero(pid) != nil {
		return 0
	}
	return pid
}

// StopFabrikTestBed stops the running bed (SIGTERM to the locked pid) and waits
// for the lock to clear (graceful shutdown unlinks it). No-op if not running.
func StopFabrikTestBed(t *testing.T, env *Env) {
	t.Helper()
	pid := lockedPID(env)
	if pid == 0 {
		t.Logf("StopFabrikTestBed: no running bed (no live lock) — nothing to stop")
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		if serr := p.Signal(syscall.SIGTERM); serr != nil {
			t.Fatalf("SIGTERM pid %d: %v", pid, serr)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if lockedPID(env) == 0 {
			t.Logf("StopFabrikTestBed: bed pid %d stopped, lock cleared", pid)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bed pid %d did not release lock within 30s", pid)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// StartFabrikTestBed launches a fresh detached bed from the bed's own binary and
// waits for it to acquire the lock. No-op if already running.
func StartFabrikTestBed(t *testing.T, env *Env) {
	t.Helper()
	if lockedPID(env) != 0 {
		t.Logf("StartFabrikTestBed: bed already running — nothing to start")
		return
	}
	bin := filepath.Join(env.FabrikTestDir, "fabrik")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("bed binary not found at %s: %v", bin, err)
	}

	cmd := exec.Command(bin, "-notui")
	cmd.Dir = env.FabrikTestDir
	// Strip GITHUB_TOKEN so Fabrik uses FABRIK_TOKEN (@arbeithand) from the bed's
	// .env — an ambient token must not hijack the bed's identity.
	cmd.Env = stripEnv(os.Environ(), "GITHUB_TOKEN")
	// Detach: new process group + /dev/null stdio so the child outlives the test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bed Fabrik (%s): %v", bin, err)
	}
	// Release so the test never becomes its reaper; the OS reparents it on exit.
	_ = cmd.Process.Release()

	deadline := time.Now().Add(40 * time.Second)
	for {
		if pid := lockedPID(env); pid != 0 {
			t.Logf("StartFabrikTestBed: bed up (pid %d)", pid)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bed Fabrik did not acquire lock within 40s of launch")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// RestartFabrikTestBed stops then starts the bed, simulating a process restart
// with an empty in-memory state map. Registers a cleanup that guarantees the bed
// is left running even if the test fails mid-restart.
func RestartFabrikTestBed(t *testing.T, env *Env) {
	t.Helper()
	t.Cleanup(func() { StartFabrikTestBed(t, env) }) // ensure the bed is up at test end
	StopFabrikTestBed(t, env)
	StartFabrikTestBed(t, env)
}

// stripEnv returns environ with any KEY=... entry for key removed.
func stripEnv(environ []string, key string) []string {
	pfx := key + "="
	out := environ[:0:0]
	for _, kv := range environ {
		if strings.HasPrefix(kv, pfx) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
