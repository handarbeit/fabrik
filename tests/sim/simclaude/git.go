package simclaude

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
)

// invocationSeq gives every commitFile call a unique number, so repeated
// invocations of the same script for the same stage (retries, comment
// re-review) never produce an empty "nothing to commit" diff — real Claude
// invocations always vary their output in some way; a scripted one needs a
// deliberate source of variation to stay realistic.
var invocationSeq atomic.Uint64

// commitStageMarker writes and commits a small per-stage marker file inside
// workDir — the default, unscripted behavior R1 specifies. relPath is
// namespaced under .simclaude/ so it can never collide with a real file a
// scenario's own script writes.
func commitStageMarker(workDir, stageName string, issueNumber int) error {
	seq := invocationSeq.Add(1)
	relPath := filepath.Join(".simclaude", stageName+".md")
	content := fmt.Sprintf("# %s\n\nsimclaude default script — issue #%d, invocation %d\n", stageName, issueNumber, seq)
	message := fmt.Sprintf("chore(simclaude): %s stage work for issue #%d (invocation %d)", stageName, issueNumber, seq)
	return commitFile(workDir, relPath, content, message)
}

// commitFile writes content to relPath (relative to workDir) and commits it
// with the real git binary. A local, invocation-scoped user.name/user.email
// is passed via -c so this works even when the harness's local git origin
// has no configured committer identity — mirroring
// engine.setCommitterIdentity's role for production worktrees, but supplied
// per-call rather than relying on repo-level config being set up first.
func commitFile(workDir, relPath, content, message string) error {
	full := filepath.Join(workDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("simclaude: creating %s: %w", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("simclaude: writing %s: %w", full, err)
	}
	if out, err := runGit(workDir, "add", relPath); err != nil {
		return fmt.Errorf("simclaude: git add %s: %s: %w", relPath, out, err)
	}
	if out, err := runGit(workDir,
		"-c", "user.name=simclaude",
		"-c", "user.email=simclaude@fabrik.test",
		"commit", "-m", message,
	); err != nil {
		return fmt.Errorf("simclaude: git commit: %s: %w", out, err)
	}
	return nil
}

func runGit(workDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	return cmd.CombinedOutput()
}
