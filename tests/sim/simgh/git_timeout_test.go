//go:build !windows

package simgh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubHangingGit writes a fake `git` onto a fresh PATH-only directory that
// sleeps far longer than any timeout this file sets, then prepends it to
// PATH for the duration of the test. It never actually runs `git` for real,
// so this needs no repo fixture -- the point is only to prove a hung
// fork+exec+wait is bounded, not to exercise real git semantics.
func stubHangingGit(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nsleep 5\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub git: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

// shrinkGitCommandTimeout swaps gitCommandTimeout for the duration of the
// test so a deliberately-wedged stub git times out in milliseconds rather
// than gitCommandTimeout's real 30s default.
func shrinkGitCommandTimeout(t *testing.T) {
	t.Helper()
	orig := gitCommandTimeout
	gitCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitCommandTimeout = orig })
}

// assertTimeoutDiagnostic checks the R4 requirement: the error must name
// both the wedged command and its directory, not just report a generic
// failure -- the whole point being that a reader doesn't need a goroutine
// dump to find the culprit (#1624's original complaint).
func assertTimeoutDiagnostic(t *testing.T, err error, dir string, wantArg string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "timed out") {
		t.Errorf("error %q does not mention a timeout", msg)
	}
	if !strings.Contains(msg, wantArg) {
		t.Errorf("error %q does not name the git subcommand %q", msg, wantArg)
	}
	if !strings.Contains(msg, dir) {
		t.Errorf("error %q does not name the directory %q", msg, dir)
	}
}

// TestRunGit_Timeout proves AC1 for runGit: a git invocation that hangs
// after starting is killed and reported via a timeout diagnostic, rather
// than hanging until the caller (and ultimately the whole suite) gives up.
//
// Non-vacuousness (AC1's "proved... by removing the context timeout and
// observing the hang return") was confirmed manually during implementation:
// temporarily reverting runGit to a plain exec.Command with no context
// caused this test to hang for the stub's full 5s sleep instead of failing
// fast at ~200ms.
func TestRunGit_Timeout(t *testing.T) {
	stubHangingGit(t)
	shrinkGitCommandTimeout(t)

	dir := t.TempDir()
	_, err := runGit(dir, "status")
	assertTimeoutDiagnostic(t, err, dir, "status")
}

// TestRunGitAllowFail_Timeout proves the same for runGitAllowFail, whose
// normal contract (a non-zero exit is a non-error "ok=false") must not
// swallow a timeout the same way -- a timeout is not "a meaningful non-zero
// exit", it's the subprocess never answering at all.
func TestRunGitAllowFail_Timeout(t *testing.T) {
	stubHangingGit(t)
	shrinkGitCommandTimeout(t)

	dir := t.TempDir()
	_, ok, err := runGitAllowFail(dir, "merge", "--no-ff", "deadbeef")
	if ok {
		t.Error("expected ok=false on timeout")
	}
	assertTimeoutDiagnostic(t, err, dir, "merge")
}

// TestRunGitStdin_Timeout proves the same for runGitStdin -- the third,
// less-used sibling that initBare calls first for every scenario (via
// SeedRepo), and so the one whose hang would strand gitMu earliest.
func TestRunGitStdin_Timeout(t *testing.T) {
	stubHangingGit(t)
	shrinkGitCommandTimeout(t)

	dir := t.TempDir()
	_, err := runGitStdin(dir, "", "mktree")
	assertTimeoutDiagnostic(t, err, dir, "mktree")
}
