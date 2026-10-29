package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Replay-equivalence rule names, logged by the reset sites (#2517, FR-005).
const (
	ruleReplayEquivalent = "replay-equivalent (patch-ids differ, net content identical)"
	rulePatchID          = "patch-id"
)

// replayEquivalent reports whether subject is `other` replayed onto subject's own base
// with nothing else added (#2517, ADR 2517): replaying other's net change onto subject's
// merge-base with baseRef yields exactly subject's tree, and subject carries no merge
// commit that other lacks.
//
// It is the content-based fallback for the patch-id comparison in holdsNothingBeyond. A
// patch-id hashes a hunk's context lines, so a clean rebase that merely shifts a commit's
// context onto a moved base gives it a new id and the commit reads as new work. The tree
// of the replay does not depend on context, so it still compares equal.
//
//	B_s = merge-base(subject, baseRef), B_o = merge-base(other, baseRef)
//	git merge-tree --write-tree --merge-base=B_o B_s other   ==  subject^{tree}
//
// The merge-tree call needs no worktree and writes only unreferenced tree objects. Every
// problem (no merge-base, an unknown option on old git, a conflict, any other error) is
// "not equivalent" with a reason, never equivalence: the caller then behaves exactly as it
// did before this rule existed. The reason is for logging only; it deliberately never
// contains "failed", so callers that read that word as git doubt are unaffected.
func replayEquivalent(wtDir, subject, other, baseRef string) (bool, string) {
	bs, err := gitOutputIn(wtDir, "merge-base", subject, baseRef)
	if err != nil || bs == "" {
		return false, "no merge-base of " + subject + " with the base"
	}
	bo, err := gitOutputIn(wtDir, "merge-base", other, baseRef)
	if err != nil || bo == "" {
		return false, "no merge-base of " + other + " with the base"
	}
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--merge-base="+bo, bs, other)
	cmd.Dir = wtDir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, "replaying onto the local base conflicts"
		}
		return false, fmt.Sprintf("replay unavailable: %v", err)
	}
	replayed := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if replayed == "" {
		return false, "replay produced no tree"
	}
	tree, err := gitOutputIn(wtDir, "rev-parse", subject+"^{tree}")
	if err != nil {
		return false, "cannot resolve the tree of " + subject
	}
	if replayed != tree {
		return false, "the replayed tree differs from the local tree"
	}
	merges, err := gitOutputIn(wtDir, "rev-list", "--merges", "--count", subject, "^"+other, "^"+baseRef)
	if err != nil {
		return false, "cannot count merge commits"
	}
	if n, convErr := strconv.Atoi(merges); convErr != nil || n != 0 {
		return false, "local-only merge commit"
	}
	return true, ""
}

// subsumedByReplay is replayEquivalent behind the sync/catch-up seam
// (SetReplayEquivalenceDisabledForTest). Called with wm.mu held.
func (wm *WorktreeManager) subsumedByReplay(wtDir, subject, other, baseRef string) (bool, string) {
	if wm.replayEquivalenceDisabled.Load() {
		return false, "replay equivalence disabled"
	}
	return replayEquivalent(wtDir, subject, other, baseRef)
}

// SetCommentRebaseRestoreContentCheckDisabledForTest neutralises the content-based
// fallback in RestoreUnpushedRebase on every registered worktree manager (the FR-001
// non-vacuity seam, #2517).
func (e *Engine) SetCommentRebaseRestoreContentCheckDisabledForTest(disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restoreContentCheckDisabledForTest = disabled
	for _, wm := range e.worktreeManagers {
		wm.restoreContentCheckDisabled.Store(disabled)
	}
}

// SetReplayEquivalenceDisabledForTest neutralises the content-based fallback in the
// issue-branch sync and the catch-up pure-rebase test on every registered worktree
// manager (the FR-002 non-vacuity seam, #2517).
func (e *Engine) SetReplayEquivalenceDisabledForTest(disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replayEquivalenceDisabledForTest = disabled
	for _, wm := range e.worktreeManagers {
		wm.replayEquivalenceDisabled.Store(disabled)
	}
}
