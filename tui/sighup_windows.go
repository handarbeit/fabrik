//go:build windows

package tui

import tea "github.com/charmbracelet/bubbletea"

// sendSighupCmd is a no-op on Windows: SIGHUP is not a Windows signal.
func sendSighupCmd() tea.Cmd {
	return nil
}
