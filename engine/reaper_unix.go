//go:build !windows

package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// reapWorktreeProcesses enumerates every live process whose cwd is wtDir or a
// subdirectory of it and sends SIGKILL to each. It runs immediately before a
// worktree directory is removed, complementing the PGID-scoped killProcGroup:
// a descendant that called setsid() (e.g. a dev server backgrounded via
// Claude Code's background-bash tool) leaves the worker's process group and
// survives killProcGroup's kill(-PGID, SIGKILL), but still has its cwd rooted
// in the worktree — which this reaper catches instead.
//
// This is a package-level function, not a WorktreeManager method, so the
// worktree janitor's os.RemoveAll fallback (which runs when no WorktreeManager
// is available for the repo) can call it too.
//
// Enumeration and kill failures are always non-fatal: they are logged via logf
// as warnings, and the caller proceeds with its own removal (git worktree
// remove / os.RemoveAll) regardless of outcome here.
func reapWorktreeProcesses(wtDir string, issueNumber int, logf func(int, string, string, ...any)) {
	if wtDir == "" {
		return
	}
	// Resolve symlinks so wtDir matches the resolved cwd paths reported by
	// /proc/*/cwd (Linux) and lsof's -Fn field (macOS) — both report the real
	// path, e.g. macOS resolves /tmp → /private/tmp and /var → /private/var,
	// which would otherwise defeat isUnderWorktreeDir's prefix match.
	if resolved, err := filepath.EvalSymlinks(wtDir); err == nil {
		wtDir = resolved
	}
	switch runtime.GOOS {
	case "linux":
		reapWorktreeProcessesLinux(wtDir, issueNumber, logf)
	case "darwin":
		reapWorktreeProcessesDarwin(wtDir, issueNumber, logf)
	default:
		// Other Unix-likes: no known-good enumeration primitive. Non-fatal no-op.
	}
}

// isUnderWorktreeDir reports whether path is wtDir itself or a descendant of it.
// Both inputs are expected to already be absolute, clean paths (callers resolve
// wtDir once via filepath.Clean and cwd values come pre-cleaned from readlink/lsof).
func isUnderWorktreeDir(path, wtDir string) bool {
	if path == "" || wtDir == "" {
		return false
	}
	path = filepath.Clean(path)
	wtDir = filepath.Clean(wtDir)
	if path == wtDir {
		return true
	}
	return strings.HasPrefix(path, wtDir+string(filepath.Separator))
}

// killWorktreeProcess sends SIGKILL to pid, skipping the engine's own process,
// treating ESRCH (already exited — a natural PID-reuse/race outcome) as
// silent, and logging unexpected errors as warnings via logf. On success, logs
// the kill using the existing "[#N kill] ..." convention so it is auditable
// alongside killProcGroup's grandchild-cleanup log line.
func killWorktreeProcess(pid int, comm, wtDir string, issueNumber int, logf func(int, string, string, ...any)) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if comm == "" {
		comm = "?"
	}
	logf(issueNumber, "kill", "sending SIGKILL to PID %d (%s) rooted in %s (worktree cwd cleanup)\n", pid, comm, wtDir)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		logf(issueNumber, "kill", "warn: could not kill PID %d (%s): %v\n", pid, comm, err)
	}
}

// reapWorktreeProcessesLinux scans /proc/*/cwd symlinks for processes whose
// cwd is rooted under wtDir. Immediately before killing a match, it re-reads
// /proc/<pid>/cwd to close the TOCTOU window where the pid could have been
// reused for an unrelated process between enumeration and kill.
func reapWorktreeProcessesLinux(wtDir string, issueNumber int, logf func(int, string, string, ...any)) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		logf(issueNumber, "kill", "warn: worktree process reap: could not read /proc: %v\n", err)
		return
	}
	wtDir = filepath.Clean(wtDir)
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil {
			continue // not a pid directory
		}
		cwd, readErr := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if readErr != nil {
			// Process exited between ReadDir and Readlink, or we lack permission
			// (e.g. a different user's process) — both are expected and non-fatal.
			continue
		}
		// The kernel appends " (deleted)" to the readlink target when the cwd's
		// underlying directory has been unlinked (e.g. a crashed prior run left
		// a process whose worktree was partially removed) — strip it before
		// matching, or the prefix check silently fails on exactly the orphans
		// this reaper exists to catch.
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if !isUnderWorktreeDir(cwd, wtDir) {
			continue
		}

		// TOCTOU re-check: re-read cwd immediately before killing to shrink the
		// window in which this pid could have been recycled for a different,
		// unrelated process since the check above.
		cwd2, readErr2 := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if readErr2 != nil {
			continue
		}
		cwd2 = strings.TrimSuffix(cwd2, " (deleted)")
		if !isUnderWorktreeDir(cwd2, wtDir) {
			continue
		}

		comm := readComm(pid)
		killWorktreeProcess(pid, comm, wtDir, issueNumber, logf)
	}
}

// readComm returns the command name for pid from /proc/<pid>/comm, or "" if
// unavailable. Best-effort only — used for log readability.
func readComm(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// reapWorktreeProcessesDarwin enumerates processes whose cwd file descriptor
// is rooted under wtDir via `lsof -a -d cwd -n -P -Fpcn`. This inspects only
// each process's cwd fd (not every open fd, unlike `lsof +D`), which keeps
// the call fast and non-hanging on a synchronous teardown path even for a
// node_modules-heavy worktree.
//
// macOS has no cheap per-pid re-check primitive equivalent to /proc, so unlike
// the Linux path this does not re-verify cwd immediately before killing —
// the small PID-reuse TOCTOU window here is accepted, not mitigated.
func reapWorktreeProcessesDarwin(wtDir string, issueNumber int, logf func(int, string, string, ...any)) {
	wtDir = filepath.Clean(wtDir)
	cmd := exec.Command("lsof", "-a", "-d", "cwd", "-n", "-P", "-Fpcn")
	out, err := cmd.Output()
	if err != nil {
		// lsof exits non-zero when it finds no matching processes at all, which
		// is a normal empty result rather than a failure — only warn when there
		// is no output to work with (e.g. lsof missing, permission failure).
		if len(out) == 0 {
			logf(issueNumber, "kill", "warn: worktree process reap: lsof failed: %v\n", err)
			return
		}
	}

	var pid int
	var comm string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			// Start of a new process record — flush is not needed since fields
			// for a given pid arrive in p, c, n order.
			pid, _ = strconv.Atoi(line[1:])
			comm = ""
		case 'c':
			comm = line[1:]
		case 'n':
			cwd := line[1:]
			if pid > 0 && isUnderWorktreeDir(cwd, wtDir) {
				killWorktreeProcess(pid, comm, wtDir, issueNumber, logf)
			}
			pid = 0
		}
	}
}
