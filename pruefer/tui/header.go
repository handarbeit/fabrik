package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// HeaderComponent renders the top title bar: program name, watched-repo
// count, and elapsed session time.
type HeaderComponent struct {
	repoCount int
	startedAt time.Time
	now       time.Time
}

func (h HeaderComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	if ev, ok := msg.(TickEvent); ok {
		h.now = ev.At
	}
	return h, nil
}

func (h HeaderComponent) View(width int) string {
	title := titleStyle.Render("Pruefer")
	elapsed := fmtDuration(h.now.Sub(h.startedAt))
	info := dimStyle.Render(fmt.Sprintf("watching %d repo(s)  ·  up %s", h.repoCount, elapsed))
	return title + "  " + info
}

func (h HeaderComponent) Height() int {
	return 1
}

// SetRepoCount records the number of watched repos, shown in the header.
func (h *HeaderComponent) SetRepoCount(n int) {
	h.repoCount = n
}

// SetStartedAt records the daemon's session start time, used to compute the
// elapsed-uptime display.
func (h *HeaderComponent) SetStartedAt(t time.Time) {
	h.startedAt = t
	h.now = t
}
