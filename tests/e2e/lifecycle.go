//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/pollctl"
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
// the gate runner's own StartBed (tests/gate/bed.go) already uses for bed
// startup — rather than deriving a value from the engine's own
// --drain-deadline config, which would couple this test harness to engine
// internals for a precision gain that a fixed-but-generous timeout plus
// real diagnostics (bedDiagnostics below) doesn't need.
const bedLifecycleTimeout = 90 * time.Second

// bedForceQuitTimeout bounds how long StopFabrikTestBed waits after the
// second (force-quit) SIGTERM.
const bedForceQuitTimeout = 30 * time.Second

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
	forced := false
	lastLog := start
	for {
		if lockedPID(env) == 0 {
			t.Logf("StopFabrikTestBed: bed pid %d stopped, lock cleared after %s", pid, time.Since(start).Round(time.Second))
			return
		}
		now := time.Now()
		if now.After(deadline) && !forced {
			// The first SIGTERM starts a graceful drain, which waits for every
			// in-flight worker — a Claude stage can run for many minutes (a
			// leftover item from an earlier scenario was enough to overrun 90s
			// in the 0.0.83 gate). Do what an operator does and send the second
			// signal, which the engine treats as force-quit (engine/poll.go's
			// second-signal listener); restart recovery then cleans up the
			// interrupted worker's state.
			forced = true
			t.Logf("StopFabrikTestBed: pid %d still draining after %s — sending a second SIGTERM (force-quit)", pid, bedLifecycleTimeout)
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGTERM)
			}
			deadline = now.Add(bedForceQuitTimeout)
		} else if now.After(deadline) {
			t.Fatalf("bed pid %d did not release lock within %s plus %s after force-quit — bed diagnostics:\n%s",
				pid, bedLifecycleTimeout, bedForceQuitTimeout, bedDiagnostics(env, pid))
		}
		if now.Sub(lastLog) >= bedLifecycleLogEvery {
			t.Logf("StopFabrikTestBed: still waiting for pid %d to release lock (%s elapsed)", pid, now.Sub(start).Round(time.Second))
			lastLog = now
		}
		time.Sleep(bedLifecyclePollInterval)
	}
}

// defaultBedPollSeconds is the bed engine's poll cadence for e2e runs, passed
// explicitly as -poll rather than left to the engine's own 30s default.
//
// The bed and this suite share one 5,000/hour GraphQL budget, and the engine is
// the larger consumer: measured from its own inline rateLimit telemetry during
// the v0.0.82 gate, ~1,320 points/hour while a leg is active (~310/hour idle,
// where the idle-upgrade backoff already stretches the cadence). Over a ~70
// minute leg that is ~1,500 points before the suite's own calls are counted,
// which is what put the off leg into the engine's 20% backoff and invalidated
// it. Halving the cadence halves that share.
//
// Passed as a flag, not left to FABRIK_POLL in the bed's .env: the flag is
// visible in `ps`, travels with the repo rather than with one machine's
// untracked .env, and cannot be silently lost when the bed is re-provisioned.
// cmd/root.go only consults FABRIK_POLL when -poll is still at its default, so
// an explicit flag deliberately wins over the .env.
//
// Override with E2E_BED_POLL_SECONDS, mirroring E2E_POLL_INTERVAL's convention
// — e.g. to restore the 30s cadence for a single scenario in isolation, where
// budget is not a constraint and detection latency matters more.
const defaultBedPollSeconds = "60"

func bedPollSeconds() string {
	if s := os.Getenv("E2E_BED_POLL_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return s
		}
	}
	return defaultBedPollSeconds
}

// markInheritedFDsCloseOnExec marks every descriptor above stdio close-on-exec,
// so processes this test spawns from here on do not inherit descriptors this
// test itself inherited — most importantly the gate runner's JSON pipe (see the call
// site in StartFabrikTestBed for the full #1694 history).
//
// Marking a descriptor close-on-exec does not affect this process's own use of
// it: the flag is consulted only at exec(2). fds we do not own are left
// readable and writable here and simply stop crossing an exec boundary, which
// is the correct default for all of them — nothing this suite spawns has any
// business holding the runner's descriptors.
//
// The scan is bounded by RLIMIT_NOFILE (capped, since that can be enormous or
// unlimited). fcntl on an unopened descriptor returns EBADF, which is the
// expected result for most of the range and is deliberately ignored — there is
// no portable way to enumerate only the open ones, and the syscall is cheap.
func markInheritedFDsCloseOnExec() {
	max := 4096
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil {
		if cur := int(rl.Cur); cur > 0 && cur < max {
			max = cur
		}
	}
	for fd := 3; fd < max; fd++ {
		syscall.CloseOnExec(fd)
	}
}

// bedGitConfigIsolationEnv returns the git-config isolation env for a bed
// launch, matching the gate runner's StartBed / BedStartCmd in tests/gate/bed.go
// (#1756/R5; the runner is untagged and cannot import this package, so the two
// launch sites are independent and pinned by TestBedStartCmdContracts): its
// GIT_CONFIG_GLOBAL points at the credential-only config the runner writes to
// .fabrik/git-config-isolated, so the operator's url.*.insteadOf rewrite
// cannot silently move the bed's HTTPS git onto SSH. The runner sets these only
// in its own launch, so a bed restarted here (every train-mode
// switch) would otherwise run on the operator's real ~/.gitconfig — which
// is how an App-mode leg could pass over SSH while HTTPS (#1846) was never
// exercised. Missing file (a go test run outside the gate runner) → no isolation,
// logged.
func bedGitConfigIsolationEnv(t *testing.T, env *Env) []string {
	t.Helper()
	path := filepath.Join(env.FabrikTestDir, ".fabrik", "git-config-isolated")
	if _, err := os.Stat(path); err != nil {
		t.Logf("StartFabrikTestBed: no isolated git config at %s (%v) — bed inherits the operator's git config", path, err)
		return nil
	}
	return []string{"GIT_CONFIG_GLOBAL=" + path, "GIT_CONFIG_NOSYSTEM=1"}
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

	// Launch contract shared with the gate runner's StartBed (tests/gate/bed.go,
	// BedStartCmd): `-notui -poll <secs>` with no --auto-upgrade, the isolated
	// gitconfig (bedGitConfigIsolationEnv), and no ambient FABRIK_GITHUB_APP_*.
	// The runner is untagged and cannot import this package, so the two launch
	// sites are independent — keep them in agreement (the runner's half is pinned
	// by TestBedStartCmdContracts).
	cmd := exec.Command(bin, "-notui", "-poll", bedPollSeconds())
	cmd.Dir = env.FabrikTestDir
	// Strip GITHUB_TOKEN so Fabrik uses FABRIK_TOKEN (@arbeithand) from the bed's
	// .env — an ambient token must not hijack the bed's identity.
	cmd.Env = stripEnv(os.Environ(), "GITHUB_TOKEN")
	// Auth mode comes from the bed's own .env only (#1861, applied by
	// TestSwitchTrainMode) — an ambient FABRIK_GITHUB_APP_* would win over
	// it, since .env loading never overrides an already-set variable.
	for _, k := range bedAppAuthEnvKeys {
		cmd.Env = stripEnv(cmd.Env, k)
	}
	cmd.Env = append(cmd.Env, bedGitConfigIsolationEnv(t, env)...)
	// The bed-only poll hold/trigger seam (#1978, ADR-1978) — the same entry the
	// gate runner's BedStartCmd adds, so a restarted bed keeps it. Enabled but
	// released it is a normal free-running bed.
	cmd.Env = append(cmd.Env, pollctl.Env(env.FabrikTestDir))
	// Red-singleton auto-repair is off on the bed (#2045): the live red-singleton test
	// asserts the ADR-1545 pause; the repair path is covered by its sim twin. The same
	// entry the gate runner's BedStartCmd adds.
	cmd.Env = append(cmd.Env, "FABRIK_MAX_TRAIN_AUTO_REPAIR_ATTEMPTS=0")
	// The overlap-aware batch filter is off on the bed (#2047): "**" ignores every path,
	// so the live conflict-resolution tests can still batch members writing one file.
	cmd.Env = append(cmd.Env, "FABRIK_MERGE_TRAIN_OVERLAP_IGNORE=**")
	// Detach: new process group + /dev/null stdio so the child outlives the test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	// Stdout/stderr go to bed-run.log, like the gate runner's own launch
	// (tests/gate/bed.go StartBed), so the startup banner — including the identity
	// line verifyBedAuthIdentity checks (#1861) — is readable after a
	// harness restart too. Appended, not truncated: the gate runner truncates once at
	// the start of a run, and every restart after that (mode switches,
	// TestMergeTrainRestartSafety) keeps its predecessors' output for
	// post-mortems. verifyBedAuthIdentity reads only the latest startup.
	// Falls back to /dev/null if the file can't open.
	if out, err := os.OpenFile(bedRunLogPath(env), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		fmt.Fprintf(out, "\n=== StartFabrikTestBed %s ===\n", time.Now().UTC().Format(time.RFC3339))
		cmd.Stdout, cmd.Stderr = out, out
		defer out.Close()
	} else {
		t.Logf("StartFabrikTestBed: cannot open %s (%v) — bed stdout discarded", bedRunLogPath(env), err)
	}
	// Setting stdio to /dev/null covers fds 0-2 only. Every OTHER descriptor
	// this test process inherited is still passed to the child: os/exec sets
	// up ProcAttr.Files and ExtraFiles but does not close arbitrary inherited
	// fds, and descriptors created by a parent shell are not close-on-exec.
	//
	// That leaked the gate runner's output pipe into a daemon designed to
	// outlive the run, and wedged the gate (#1694). The (then bash) runner fed
	// `go test`'s output through a FIFO and afterwards waited for the consumer
	// to drain, which requires EOF, which requires every write end to be
	// closed. `go test` inherits the descriptor, the test binary inherits it, and
	// this detached bed inherited it too and then held it open indefinitely.
	// (The Go runner copies the pipe itself and bounds that wait — see
	// tests/gate/exec.go's WaitDelay — but this fix at the source stays.)
	// Confirmed by lsof against a bed still running hours after its run:
	//
	//   fabrik 13897 bpja 3w FIFO ... /tmp.xHMuUtjYKA/fifo
	//
	// The consumer therefore never saw EOF, the post-suite wait blocked, and
	// the #1676 watchdog aborted the script — silently skipping the isolated
	// runaway-guard leg that should have run next. It only ever bit the "on"
	// leg because that is where the bed is (re)started from inside a `go test`
	// invocation rather than by the gate runner before the pipe exists.
	markInheritedFDsCloseOnExec()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bed Fabrik (%s): %v", bin, err)
	}
	launchedPID := cmd.Process.Pid
	// Reap the bed when it exits. The OS reparents a child only when its PARENT
	// exits, so a bed started from inside the suite binary (a mid-suite restart,
	// e.g. TestMergeTrainColdCacheBaseMember) that later exits stays a zombie
	// until the whole suite ends — and a zombie answers kill -0, so lockedPID
	// reported it alive: a later stop "timed out", the next start found the bed
	// "already running", and every remaining scenario ran with no working bed
	// (0.0.83 gate run 3). Waiting in a goroutine reaps it the moment it exits
	// without blocking the test; the bed still outlives the test if the suite
	// ends first (it is then reparented to init).
	go func() { _ = cmd.Wait() }()

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
