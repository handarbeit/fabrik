package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// activeReview tracks one in-flight review, mirroring tui/active.go's
// activeJob shape (StartedAt for elapsed-time display).
type activeReview struct {
	Repo      string
	PRNumber  int
	Title     string
	StartedAt time.Time
}

// activeReviewKey mirrors tui/active.go's activeJobKey pattern: reviews are
// keyed "owner/repo#N" rather than by repo alone, since Pruefer's single
// shared concurrency semaphore can run reviews from multiple watched repos
// at once.
func activeReviewKey(repo string, prNumber int) string {
	return fmt.Sprintf("%s#%d", repo, prNumber)
}

// ActivePaneComponent manages the in-flight-reviews pane.
type ActivePaneComponent struct {
	active        map[string]*activeReview
	idx           int
	focused       bool
	spinnerFrames []string
	spinnerIdx    int
	now           time.Time
}

// newActivePaneComponent returns an ActivePaneComponent ready to receive
// events (its map and spinner frames initialized).
func newActivePaneComponent() ActivePaneComponent {
	return ActivePaneComponent{
		active:        make(map[string]*activeReview),
		spinnerFrames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
}

func (a ActivePaneComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		a.now = ev.At
		if len(a.spinnerFrames) > 0 {
			a.spinnerIdx = (a.spinnerIdx + 1) % len(a.spinnerFrames)
		}

	case ReviewStartedEvent:
		if a.active == nil {
			a.active = make(map[string]*activeReview)
		}
		a.active[activeReviewKey(ev.Repo, ev.PRNumber)] = &activeReview{
			Repo: ev.Repo, PRNumber: ev.PRNumber, Title: ev.Title, StartedAt: ev.StartedAt,
		}

	case ReviewCompletedEvent:
		delete(a.active, activeReviewKey(ev.Repo, ev.PRNumber))
		if keys := a.sortedKeys(); a.idx >= len(keys) && a.idx > 0 {
			a.idx = len(keys) - 1
		}

	case tea.KeyMsg:
		if !a.focused {
			return a, nil
		}
		switch ev.String() {
		case "up", "k":
			if a.idx > 0 {
				a.idx--
			}
		case "down", "j":
			if a.idx < len(a.sortedKeys())-1 {
				a.idx++
			}
		}
	}
	return a, nil
}

func (a ActivePaneComponent) sortedKeys() []string {
	keys := make([]string, 0, len(a.active))
	for k := range a.active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a ActivePaneComponent) View(width int) string {
	focusIndicator := " "
	if a.focused {
		focusIndicator = "▸"
	}
	title := activeStyle.Render(fmt.Sprintf("%s In-Flight Reviews (%d)", focusIndicator, len(a.active)))

	maxWidth := width - 6
	if maxWidth < 20 {
		maxWidth = 20
	}
	keys := a.sortedKeys()
	spinner := ""
	if len(a.spinnerFrames) > 0 {
		spinner = a.spinnerFrames[a.spinnerIdx]
	}
	var lines []string
	for idx, key := range keys {
		job := a.active[key]
		elapsed := fmtDuration(a.now.Sub(job.StartedAt))
		titleStr := ""
		if job.Title != "" {
			titleStr = " " + dimStyle.Render(job.Title)
		}
		line := fmt.Sprintf("%s %-30s %s%s", spinner, key, elapsed, titleStr)
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		if a.focused && idx == a.idx {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, dimStyle.Render("no reviews in flight"))
	}
	content := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

func (a ActivePaneComponent) Height() int {
	n := len(a.active)
	if n == 0 {
		n = 1
	}
	return n + 3
}

// SetFocused updates the focused state.
func (a *ActivePaneComponent) SetFocused(f bool) {
	a.focused = f
}

// Selected returns the currently selected in-flight review, or nil if none.
func (a ActivePaneComponent) Selected() *activeReview {
	keys := a.sortedKeys()
	if a.idx < len(keys) {
		return a.active[keys[a.idx]]
	}
	return nil
}

// Count returns the number of in-flight reviews.
func (a ActivePaneComponent) Count() int {
	return len(a.active)
}
