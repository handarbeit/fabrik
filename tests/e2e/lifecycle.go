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

// bedLifecycleTimeout bounds both StopFabrikTestBed's wait for the lock to
// clear and StartFabrikTestBed's wait for the lock to be acquired (#1677,
// REQ2). Previously these were hardcoded at 30s/40s respectively — fine
// against an empty bed, but #1677's own incident showed a bed with enough
// accumulated state (stale board labels, a large tests/sim session/log
// backlog) can make the engine's own startup janitors and shutdown drain
// consume close to their full budget, leaving those short, fixed waits with
// no margin. 90s is deliberately generous — mirroring the 90s convention
// scripts/e2e/run.sh's own preflight_bed_start already uses for bed
// startup — rather than deriving a value from the engine's own
// --drain-deadline config, which would couple this test harness to engine
// internals for a precision gain that a fixed-but-generous timeout plus
// real diagnostics (bedDiagnostics below) doesn't need.
const bedLifecycleTimeout = 90 * time.Second

// bedLifecyclePollInterval is how often StopFabrikTestBed/StartFabrikTestBed
// re-check the lock and log progress while waiting.
const bedLifecyclePollInterval = 500 * time.Millisecond

// bedLifecycleLogEvery caps how often progress is logged while polling —
// every 500ms would flood test output over a 90s wait.
const bedLifecycleLogEvery = 10 * time.Second

// bedDiagnostics reports what the bed appears to be doing, for inclusion in
// a StopFabrikTestBed/StartFabrikTestBed timeout failure (#1677, REQ2): the
// issue's own evidence showed session-janitor/log-janitor/transient-label
// scan activity in fabrik.log at the moment of a lock-timeout failure, so a
// bare "did not release lock within 30s" message discarded exactly the
// information needed to diagnose a recurrence. Reports process liveness
// (kill -0 equivalent) for pid, if pid > 0, plus a tail of the bed's own
// fabrik.log.
func bedDiagnostics(env *Env, pid int) string {
	var b strings.Builder
	if pid > 0 {
		if err := syscallSignalZero(pid); err != nil {
			fmt.Fprintf(&b, "process pid %d: not alive (%v)\n", pid, err)
		} else {
			fmt.Fprintf(&b, "process pid %d: alive\n", pid)
		}
	} else {
		b.WriteString("process pid: unknown (not yet captured)\n")
	}
	fmt.Fprintf(&b, "tail of %s:\n%s", env.LogPath, tailFile(env.LogPath, 20))
	return b.String()
}

// tailFile returns the last n lines of path, or a placeholder describing why
// it couldn't be read (missing file, empty file) — never an error, since
// this is diagnostic-only output attached to an already-failing test.
func tailFile(path string, n int) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("  (could not read %s: %v)\n", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return "  (empty)\n"
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
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
	start := time.Now()
	deadline := start.Add(bedLifecycleTimeout)
	lastLog := start
	for {
		if lockedPID(env) == 0 {
			t.Logf("StopFabrikTestBed: bed pid %d stopped, lock cleared after %s", pid, time.Since(start).Round(time.Second))
			return
		}
		now := time.Now()
		if now.After(deadline) {
			t.Fatalf("bed pid %d did not release lock within %s — bed diagnostics:\n%s",
				pid, bedLifecycleTimeout, bedDiagnostics(env, pid))
		}
		if now.Sub(lastLog) >= bedLifecycleLogEvery {
			t.Logf("StopFabrikTestBed: still waiting for pid %d to release lock (%s elapsed)", pid, now.Sub(start).Round(time.Second))
			lastLog = now
		}
		time.Sleep(bedLifecyclePollInterval)
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
	launchedPID := cmd.Process.Pid
	// Release so the test never becomes its reaper; the OS reparents it on exit.
	_ = cmd.Process.Release()

	start := time.Now()
	deadline := start.Add(bedLifecycleTimeout)
	lastLog := start
	for {
		if pid := lockedPID(env); pid != 0 {
			t.Logf("StartFabrikTestBed: bed up (pid %d) after %s", pid, time.Since(start).Round(time.Second))
			return
		}
		now := time.Now()
		if now.After(deadline) {
			t.Fatalf("bed Fabrik did not acquire lock within %s of launch (launched pid %d) — bed diagnostics:\n%s",
				bedLifecycleTimeout, launchedPID, bedDiagnostics(env, launchedPID))
		}
		if now.Sub(lastLog) >= bedLifecycleLogEvery {
			t.Logf("StartFabrikTestBed: still waiting for launched pid %d to acquire lock (%s elapsed)", launchedPID, now.Sub(start).Round(time.Second))
			lastLog = now
		}
		time.Sleep(bedLifecyclePollInterval)
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
