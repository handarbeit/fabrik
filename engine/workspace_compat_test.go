package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/handarbeit/fabrik/internal/workspace"
)

// Test-only cwd-rooted helpers for the in-bed default layout. Production code
// resolves paths through a workspace.Layout (#2470); keeping these out of the
// production build means no production site can call them by mistake.

func cwdLayout() workspace.Layout {
	cwd, _ := os.Getwd()
	return workspace.Default(cwd)
}

// SessionDir is the default-layout session directory of an issue.
func SessionDir(issueNumber int) string {
	return filepath.Join(cwdLayout().Dir(workspace.Sessions), fmt.Sprintf("issue-%d", issueNumber))
}

// LogDir is the default-layout log directory of an issue.
func LogDir(issueNumber int) string {
	return filepath.Join(cwdLayout().Dir(workspace.Logs), fmt.Sprintf("issue-%d", issueNumber))
}

func sessionFile(issueNumber int, stageName string) string {
	return filepath.Join(SessionDir(issueNumber), sanitizeStageName(stageName)+".session")
}
