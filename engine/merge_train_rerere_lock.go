package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// rerereLockName is the lock git's rerere takes on its merge-state file.
const rerereLockName = "MERGE_RR.lock"

// mergeToleratingRerereLock runs `git merge --no-ff --no-edit ref` in the disposable
// forget-reconstruction worktree wtDir. If git refuses because a MERGE_RR.lock is
// already there (#2542 R4b, ADR 2542), it removes the lock and retries once.
//
// Removing it is sound because the worktree is created for this one reconstruction
// and every git command in it runs to completion before the next starts
// (CombinedOutput waits), so a lock found after the previous command exited is not
// held by any process of ours — it is left over. Where it came from (a killed git, or
// a rerere write the earlier replay did not clean up) is not established; the log
// line names the removal so a recurrence is visible. The retry's outcome is returned
// unchanged, so a merge that still fails keeps the existing skip-forget handling, and
// no train decision reads the result.
func (e *Engine) mergeToleratingRerereLock(ctx context.Context, wtDir, ref string) ([]byte, error) {
	run := func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", "merge", "--no-ff", "--no-edit", ref)
		cmd.Dir = wtDir
		return cmd.CombinedOutput()
	}
	out, err := run()
	if err == nil || !bytes.Contains(out, []byte(rerereLockName)) {
		return out, err
	}
	if !removeStaleRerereLock(wtDir) {
		return out, err
	}
	// The refused merge has already written the conflicted index, which blocks a
	// second merge ("unmerged files"); the worktree is disposable, so drop that state.
	for _, args := range [][]string{{"merge", "--abort"}, {"reset", "--merge"}} {
		reset := exec.CommandContext(ctx, "git", args...)
		reset.Dir = wtDir
		if reset.Run() == nil {
			break
		}
	}
	e.logf(0, "merge-train", "removed a leftover %s in the forget-reconstruction worktree and retried the merge\n", rerereLockName)
	return run()
}

// removeStaleRerereLock deletes wtDir's MERGE_RR.lock, reporting whether one was removed.
func removeStaleRerereLock(wtDir string) bool {
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = wtDir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	lock := filepath.Join(strings.TrimSpace(string(out)), rerereLockName)
	if _, statErr := os.Stat(lock); statErr != nil {
		return false
	}
	return os.Remove(lock) == nil
}

// dropRerereLockLines removes git's lock-failure lines ("fatal: Unable to create
// '…/MERGE_RR.lock': File exists." and its advice) from merge output, so an operator
// reading the skip-forget warning does not see a fatal-looking line for a lock the
// engine already tolerated (#2542 R4b).
func dropRerereLockLines(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.Contains(t, rerereLockName),
			strings.HasPrefix(t, "Another git process seems to be running"),
			strings.HasPrefix(t, "If no other git process is currently running"),
			strings.HasPrefix(t, "remove the file manually to continue"):
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
