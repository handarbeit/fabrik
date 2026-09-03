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

// minPaneRows is the floor a flexible pane keeps even when the terminal is too
// short to satisfy both. Below this a pane conveys nothing, so shrinking
// further only wastes the rows.
const minPaneRows = 3

// paneChrome is the non-content height every bordered pane costs: a title row
// plus the top and bottom border.
const paneChrome = 3

// allocateRows divides the height left over after the fixed panes between the
// two flexible ones (#1674 R1).
//
// The rule, stated so it is a decision rather than an accident: **the changing
// pane is served first.** Completed Reviews gets what its content needs up to
// the available space; Watched Repos takes the remainder. Watched Repos is
// static and grows with the repo count, so letting it win would push the panes
// an operator is actually watching off the screen — which is the defect this
// addresses. Both keep minPaneRows, and whichever cannot fit its content
// windows with a "… N more" line.
//
// avail is content rows available to both panes combined (chrome already
// deducted). wantHistory/wantRepos are their unconstrained content heights.
func allocateRows(avail, wantHistory, wantRepos int) (history, repos int) {
	if avail < 2*minPaneRows {
		// Too short to satisfy both floors: split what there is, still giving
		// the changing pane the larger half.
		history = avail / 2
		if history < 1 {
			history = 1
		}
		repos = avail - history
		if repos < 1 {
			repos = 1
		}
		return history, repos
	}
	history = wantHistory
	if history > avail-minPaneRows {
		history = avail - minPaneRows
	}
	if history < minPaneRows {
		history = minPaneRows
	}
	repos = avail - history
	// Hand back anything Completed Reviews does not need.
	if repos > wantRepos {
		repos = wantRepos
		if extra := avail - repos - history; extra > 0 {
			history += extra
		}
	}
	if repos < minPaneRows {
		repos = minPaneRows
	}
	return history, repos
}

// View renders the full TUI.
//
// Panel order (#1674 R4): the panes that change — In-Flight and Completed —
// sit above the static Watched Repos list, which grows with the repo count and
// previously pushed them down the screen.
func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	header := m.header.View(m.width)
	active := m.active.View(m.width)
	footer := m.footer.View(m.width)

	detail := ""
	if m.detailPanel {
		detail = m.detail.View(m.width)
	}

	// Budget the remaining height between the two flexible panes. m.height is
	// 0 until the first WindowSizeMsg, in which case each pane falls back to
	// its own default and the layout behaves as it did before #1674.
	if m.height > 0 {
		fixed := lineCount(header) + lineCount(active) + lineCount(footer)
		if detail != "" {
			fixed += lineCount(detail)
		}
		avail := m.height - fixed - 2*paneChrome
		wantHistory := len(m.history.visibleEntries())
		if wantHistory == 0 {
			wantHistory = 1
		}
		wantRepos := len(m.repos.order) + len(m.repos.provenanceNotes())
		if wantRepos == 0 {
			wantRepos = 1
		}
		hRows, rRows := allocateRows(avail, wantHistory, wantRepos)
		m.history.SetMaxRows(hRows)
		m.repos.SetMaxRows(rRows)
	}

	var sections []string
	sections = append(sections, header)
	sections = append(sections, active)
	if detail != "" {
		sections = append(sections, detail)
	}
	sections = append(sections, m.history.View(m.width))
	sections = append(sections, m.repos.View(m.width))
	sections = append(sections, footer)

	return strings.Join(sections, "\n")
}

// lineCount returns the rendered height of a section in terminal rows.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
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
