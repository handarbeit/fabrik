package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/handarbeit/fabrik/internal/workspace"
)

// #2470: the watcher reads worktrees, sessions and logs from the configured
// workspace root, not from <cwd>/.fabrik.
func TestWatchPathsFollowWorkspaceRoot(t *testing.T) {
	bed := t.TempDir()
	root := filepath.Join(t.TempDir(), "ws")
	ws := workspace.Layout{Bed: bed, Root: root}

	if got, want := worktreeDir(ws, "o", "r", 4), filepath.Join(root, "worktrees", "o-r", "issue-4"); got != want {
		t.Errorf("worktreeDir = %s, want %s", got, want)
	}
	if got, want := worktreeDir(ws, "", "", 4), filepath.Join(root, "worktrees", "issue-4"); got != want {
		t.Errorf("flat worktreeDir = %s, want %s", got, want)
	}
	if got, want := issueLogDir(ws, "o", "r", 4), filepath.Join(root, "logs", "o-r", "issue-4"); got != want {
		t.Errorf("issueLogDir = %s, want %s", got, want)
	}
	sess := sessionDir(ws, "o", "r", 4)
	if want := filepath.Join(root, "sessions", "o-r", "issue-4"); sess != want {
		t.Errorf("sessionDir = %s, want %s", sess, want)
	}
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "Plan.session"), []byte("sess_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readSessionID(ws, "o", "r", 4, "Plan"); got != "sess_x" {
		t.Errorf("readSessionID = %q", got)
	}
	if got := readSessionID(workspace.Default(bed), "o", "r", 4, "Plan"); got != "" {
		t.Errorf("the default layout must not see the relocated session, got %q", got)
	}
}
