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
	// InstallationID is the installation that granted this repo, populated
	// by the most recent DerivedRepoSetEvent (#1641/R4) — zero until then
	// (e.g. before the daemon's first derivation completes).
	InstallationID int64
}

// RepoPaneComponent renders the watched-repositories pane.
type RepoPaneComponent struct {
	repos   map[string]*repoStatus
	order   []string // insertion order, seeded from Config.WatchedRepos
	now     time.Time
	focused bool

	// The remaining fields are populated by the most recent
	// DerivedRepoSetEvent (#1641/R4) — the installation-derived set's own
	// provenance, distinct from any individual repo's poll status above.
	installations []DerivedInstallationSummary
	filteredOut   []string
	truncated     bool
	capped        bool
	capApplied    int
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
	case DerivedRepoSetEvent:
		if r.repos == nil {
			r.repos = make(map[string]*repoStatus)
		}
		wanted := make(map[string]bool, len(ev.Repos))
		for _, entry := range ev.Repos {
			wanted[entry.Repo] = true
			st, ok := r.repos[entry.Repo]
			if !ok {
				st = &repoStatus{Repo: entry.Repo}
				r.repos[entry.Repo] = st
				r.order = append(r.order, entry.Repo)
			}
			st.InstallationID = entry.InstallationID
		}
		// Drop repos the derived set no longer grants/includes — an
		// installation lost, or newly excluded by a watched_repos filter
		// change — preserving poll status for every repo still present.
		newOrder := make([]string, 0, len(r.order))
		for _, repo := range r.order {
			if wanted[repo] {
				newOrder = append(newOrder, repo)
				continue
			}
			delete(r.repos, repo)
		}
		r.order = newOrder
		r.installations = ev.Installations
		r.filteredOut = ev.FilteredOut
		r.truncated = ev.Truncated
		r.capped = ev.Capped
		r.capApplied = ev.CapApplied
	}
	return r, nil
}

func (r RepoPaneComponent) View(width int) string {
	focusIndicator := " "
	if r.focused {
		focusIndicator = "▸"
	}
	titleText := fmt.Sprintf("%s Watched Repos (%d)", focusIndicator, len(r.order))
	if n := len(r.installations); n > 0 {
		titleText += fmt.Sprintf(" — %d installation(s)", n)
	}
	title := activeStyle.Render(titleText)

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
		provenance := ""
		if st.InstallationID != 0 {
			provenance = fmt.Sprintf(" [installation %d]", st.InstallationID)
		}
		line := fmt.Sprintf("%-30s %2d PR(s)  polled %s%s", repo, st.PRCount, age, provenance)
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
	for _, note := range r.provenanceNotes() {
		lines = append(lines, note)
	}
	content := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

// provenanceNotes renders the derived set's provenance/warning lines (R4):
// a truncated-enumeration warning, a max_derived_repos cap warning, and a
// count of watched_repos entries not covered by any installation. Absent
// (no DerivedRepoSetEvent received yet, or nothing to report) yields no
// lines — this pane must never claim provenance it hasn't actually been
// told.
func (r RepoPaneComponent) provenanceNotes() []string {
	var notes []string
	if r.truncated {
		notes = append(notes, failStyle.Render("⚠ pagination ceiling hit while enumerating installations/repos — the derived set may be incomplete"))
	}
	if r.capped {
		notes = append(notes, failStyle.Render(fmt.Sprintf("⚠ capped at %d repos (max_derived_repos) — raise the cap or narrow watched_repos to review the rest", r.capApplied)))
	}
	if len(r.filteredOut) > 0 {
		notes = append(notes, dimStyle.Render(fmt.Sprintf("%d watched_repos entr(ies) not covered by any installation: %s", len(r.filteredOut), strings.Join(r.filteredOut, ", "))))
	}
	return notes
}

func (r RepoPaneComponent) Height() int {
	n := len(r.order)
	if n == 0 {
		n = 1
	}
	return n + 3 + len(r.provenanceNotes())
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
