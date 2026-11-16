package pruefer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #2470 R5: Pruefer's worktree handling has no dependency on a Fabrik
// workspace root. Each review clones into a fresh os.MkdirTemp directory and
// never reads <bed>/.fabrik/worktrees, so a relocated workspace_root cannot
// orphan or mis-match anything of Pruefer's. This pins the finding.
func TestPrueferWorktreeIsIndependentOfTheFabrikWorkspace(t *testing.T) {
	src, err := os.ReadFile("worktree.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `os.MkdirTemp("", `) {
		t.Error("Pruefer review worktrees must live in a per-review temp directory")
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		for _, banned := range []string{"internal/workspace", ".fabrik/worktrees", `"worktrees"`} {
			if strings.Contains(string(data), banned) {
				t.Errorf("%s references %s: Pruefer must not depend on the Fabrik workspace layout", f, banned)
			}
		}
	}
}
