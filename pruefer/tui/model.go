package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pane identifies which TUI section has focus.
type pane int

const (
	paneRepos pane = iota
	paneActive
	paneHistory
)

// Model is the bubbletea TUI model for Pruefer, structurally mirroring
// Fabrik's tui.Model (model/update/view split, header, detail pane) —
// see adrs/1114-pruefer-tui-architecture.md.
type Model struct {
	width  int
	height int

	focusPane   pane
	detailPanel bool

	header  HeaderComponent
	repos   RepoPaneComponent
	active  ActivePaneComponent
	history HistoryPaneComponent
	detail  DetailPanelComponent
	footer  FooterComponent
}

// New creates an initial TUI model. watchedRepos seeds the repos pane so
// every configured repo is visible ("never polled") before its first
// RepoPollEvent arrives. startedAt is the daemon's session start time, used
// for the header's elapsed-uptime display.
func New(watchedRepos []string, startedAt time.Time) Model {
	repos := RepoPaneComponent{focused: true}
	repos.SetWatchedRepos(watchedRepos)

	header := HeaderComponent{}
	header.SetRepoCount(len(watchedRepos))
	header.SetStartedAt(startedAt)

	return Model{
		focusPane: paneRepos,
		header:    header,
		repos:     repos,
		active:    newActivePaneComponent(),
	}
}

// Init starts the 1-second tick.
func (m Model) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return TickEvent{At: t}
	})
}

// Update handles all messages (events and tea messages).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch ev := msg.(type) {
	case tea.KeyMsg:
		switch ev.String() {
		case "ctrl+c", "q":
			return m, tea.Quit

		case "tab":
			switch m.focusPane {
			case paneRepos:
				m.focusPane = paneActive
			case paneActive:
				m.focusPane = paneHistory
			default:
				m.focusPane = paneRepos
			}
			m.syncFocus()
			return m, nil

		case "enter":
			m.detailPanel = !m.detailPanel
			m.prepareDetailItem()
			return m, nil
		}

		var cmd tea.Cmd
		switch m.focusPane {
		case paneRepos:
			var comp Component
			comp, cmd = m.repos.Update(msg)
			m.repos = comp.(RepoPaneComponent)
		case paneActive:
			var comp Component
			comp, cmd = m.active.Update(msg)
			m.active = comp.(ActivePaneComponent)
		case paneHistory:
			var comp Component
			comp, cmd = m.history.Update(msg)
			m.history = comp.(HistoryPaneComponent)
		}
		if m.detailPanel {
			m.prepareDetailItem()
		}
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = ev.Width
		m.height = ev.Height
		return m, nil

	case TickEvent:
		comp, _ := m.header.Update(msg)
		m.header = comp.(HeaderComponent)
		comp, _ = m.repos.Update(msg)
		m.repos = comp.(RepoPaneComponent)
		comp, _ = m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		comp, _ = m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		if m.detailPanel {
			m.prepareDetailItem()
		}
		return m, tickCmd()

	case RepoPollEvent:
		comp, _ := m.repos.Update(msg)
		m.repos = comp.(RepoPaneComponent)
		return m, nil

	case DerivedRepoSetEvent:
		comp, _ := m.repos.Update(msg)
		m.repos = comp.(RepoPaneComponent)
		return m, nil

	case ReviewStartedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		return m, nil

	case ReviewCompletedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		hcomp, _ := m.history.Update(msg)
		m.history = hcomp.(HistoryPaneComponent)
		fcomp, _ := m.footer.Update(msg)
		m.footer = fcomp.(FooterComponent)
		if m.detailPanel {
			m.prepareDetailItem()
		}
		return m, nil

	case RateLimitSnapshotEvent:
		comp, _ := m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil

	case DropEvent:
		comp, _ := m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil

	case SignatureDriftEvent:
		comp, _ := m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil
	}

	return m, nil
}

// syncFocus updates focused state on pane components to match m.focusPane.
func (m *Model) syncFocus() {
	m.repos.SetFocused(m.focusPane == paneRepos)
	m.active.SetFocused(m.focusPane == paneActive)
	m.history.SetFocused(m.focusPane == paneHistory)
}

// prepareDetailItem constructs the DetailItem from the focused pane's
// current selection.
func (m *Model) prepareDetailItem() {
	if m.focusPane == paneActive {
		if job := m.active.Selected(); job != nil {
			m.detail.SetItem(&DetailItem{
				Repo:     job.Repo,
				PRNumber: job.PRNumber,
				Title:    job.Title,
				IsActive: true,
				Elapsed:  m.header.now.Sub(job.StartedAt),
			})
		} else {
			m.detail.SetItem(nil)
		}
	} else if entry := m.history.Selected(); entry != nil {
		m.detail.SetItem(&DetailItem{
			Repo:        entry.Repo,
			PRNumber:    entry.PRNumber,
			Title:       entry.Title,
			Reviewed:    entry.Reviewed,
			Skipped:     entry.Skipped,
			Reason:      entry.Reason,
			Err:         entry.Err,
			NumTurns:    entry.NumTurns,
			CostUSD:     entry.CostUSD,
			Duration:    entry.Duration,
			CompletedAt: entry.CompletedAt,
		})
	} else {
		m.detail.SetItem(nil)
	}
	m.detail.SetWidth(m.width)
	m.detail.SetVisible(m.detailPanel)
}

// View renders the full TUI.
func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var sections []string
	sections = append(sections, m.header.View(m.width))
	sections = append(sections, m.repos.View(m.width))
	sections = append(sections, m.active.View(m.width))

	if m.detailPanel {
		if detail := m.detail.View(m.width); detail != "" {
			sections = append(sections, detail)
		}
	}

	sections = append(sections, m.history.View(m.width))
	sections = append(sections, m.footer.View(m.width))

	return strings.Join(sections, "\n")
}

// FocusedPaneName returns the name of the currently focused pane, for tests.
func (m Model) FocusedPaneName() string {
	switch m.focusPane {
	case paneRepos:
		return "repos"
	case paneActive:
		return "active"
	case paneHistory:
		return "history"
	}
	return ""
}

// DetailVisible reports whether the detail panel is currently shown.
func (m Model) DetailVisible() bool {
	return m.detailPanel
}
