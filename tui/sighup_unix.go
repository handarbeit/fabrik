//go:build !windows

package tui

import (
	"os"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// sendSighupCmd returns a tea.Cmd that sends SIGHUP to the current process,
// triggering the engine's drain-and-re-exec restart path.
func sendSighupCmd() tea.Cmd {
	return func() tea.Msg {
		syscall.Kill(os.Getpid(), syscall.SIGHUP) //nolint:errcheck
		return nil
	}
}
