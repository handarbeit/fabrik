package tui

import tea "github.com/charmbracelet/bubbletea"

// Component is the interface implemented by each TUI pane/section. The root
// Model orchestrates layout and focus; each Component owns its own state
// and rendering. Mirrors Fabrik's tui/component.go.
type Component interface {
	Update(msg tea.Msg) (Component, tea.Cmd)
	View(width int) string
	Height() int
}
