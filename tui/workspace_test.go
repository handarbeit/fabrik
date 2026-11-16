package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/handarbeit/fabrik/internal/workspace"
)

// #2470: the TUI resolves worktrees and sessions through the layout it was
// given, so "resume" and "watch" find work under a relocated root.
func TestTUIPathsFollowWorkspaceRoot(t *testing.T) {
	bed := t.TempDir()
	root := filepath.Join(t.TempDir(), "ws")
	ws := workspace.Layout{Bed: bed, Root: root}

	if got, want := worktreePathForIssue(ws, "o/r", 6), filepath.Join(root, "worktrees", "o-r", "issue-6"); got != want {
		t.Errorf("worktreePathForIssue = %s, want %s", got, want)
	}
	if got, want := worktreePathForIssue(workspace.Default(bed), "o/r", 6), filepath.Join(bed, ".fabrik", "worktrees", "o-r", "issue-6"); got != want {
		t.Errorf("default worktreePathForIssue = %s, want %s", got, want)
	}
	dir := filepath.Join(root, "sessions", "o-r", "issue-6")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Plan.session"), []byte("sess_y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tuiReadSessionID(ws, "o/r", 6, "Plan"); got != "sess_y" {
		t.Errorf("tuiReadSessionID = %q", got)
	}
	m := New(5, ProjectInfo{Workspace: ws}, "", nil, nil, 0, false)
	if m.ws.Root != root || m.active.ws.Root != root || m.history.ws.Root != root {
		t.Errorf("New must hand the layout to the model and its panes: %+v %+v %+v", m.ws, m.active.ws, m.history.ws)
	}
}
