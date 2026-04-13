package tui

import tea "github.com/charmbracelet/bubbletea"

// Component is the interface implemented by each TUI pane/section.
// The root Model orchestrates layout and focus; each Component owns
// its own state and rendering. Hit-testing (HandleClick) is implemented
// as a pointer-receiver method on each component, not part of this
// interface, since it needs to mutate state directly.
type Component interface {
	Update(msg tea.Msg) (Component, tea.Cmd)
	View(width int) string
	Height() int
}
