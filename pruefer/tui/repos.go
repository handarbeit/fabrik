package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// repoStatus is the per-watched-repo poll status tracked by
// RepoPaneComponent — last poll time, PR count found, and last error,
// combined per the spec's "poll status" open question.
type repoStatus struct {
	Repo       string
	LastPollAt time.Time
	PRCount    int
	LastErr    string
}

// RepoPaneComponent renders the watched-repositories pane.
type RepoPaneComponent struct {
	repos   map[string]*repoStatus
	order   []string // insertion order, seeded from Config.WatchedRepos
	now     time.Time
	focused bool
}

func (r RepoPaneComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		r.now = ev.At
	case RepoPollEvent:
		if r.repos == nil {
			r.repos = make(map[string]*repoStatus)
		}
		st, ok := r.repos[ev.Repo]
		if !ok {
			st = &repoStatus{Repo: ev.Repo}
			r.repos[ev.Repo] = st
			r.order = append(r.order, ev.Repo)
		}
		st.LastPollAt = ev.At
		st.PRCount = ev.PRCount
		st.LastErr = ev.Err
	}
	return r, nil
}

func (r RepoPaneComponent) View(width int) string {
	focusIndicator := " "
	if r.focused {
		focusIndicator = "▸"
	}
	title := activeStyle.Render(fmt.Sprintf("%s Watched Repos (%d)", focusIndicator, len(r.order)))

	maxWidth := width - 6
	if maxWidth < 20 {
		maxWidth = 20
	}
	var lines []string
	for _, repo := range r.order {
		st := r.repos[repo]
		age := "never polled"
		if !st.LastPollAt.IsZero() {
			age = fmtDuration(r.now.Sub(st.LastPollAt)) + " ago"
		}
		line := fmt.Sprintf("%-30s %2d PR(s)  polled %s", repo, st.PRCount, age)
		if st.LastErr != "" {
			line += "  " + failStyle.Render("error: "+st.LastErr)
		}
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, dimStyle.Render("no repos configured"))
	}
	content := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

func (r RepoPaneComponent) Height() int {
	n := len(r.order)
	if n == 0 {
		n = 1
	}
	return n + 3
}

// SetFocused updates the focused state.
func (r *RepoPaneComponent) SetFocused(f bool) {
	r.focused = f
}

// SetWatchedRepos seeds the pane with the configured repo list in order, so
// every watched repo is visible ("never polled") before its first
// RepoPollEvent arrives.
func (r *RepoPaneComponent) SetWatchedRepos(repos []string) {
	if r.repos == nil {
		r.repos = make(map[string]*repoStatus)
	}
	for _, repo := range repos {
		if _, ok := r.repos[repo]; !ok {
			r.repos[repo] = &repoStatus{Repo: repo}
			r.order = append(r.order, repo)
		}
	}
}
