package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// HistoryEntry records a completed job for the history pane.
type HistoryEntry struct {
	IssueNumber    int
	Repo           string // "owner/repo" — empty for single-repo projects
	Title          string
	StageName      string
	StageModel     string // model configured for the stage; empty means use claude default
	IsComment      bool
	Success        bool
	Completed      bool
	BlockedOnInput bool
	Duration       time.Duration
	CompletedAt    time.Time
	TurnsUsed      int
	MaxTurns       int
	CostUSD        float64
}

// activeJob tracks an in-flight worker.
type activeJob struct {
	IssueNumber int
	Repo        string // "owner/repo" — empty for single-repo projects
	Title       string
	StageName   string
	IsComment   bool
	StartedAt   time.Time
	LastTag     string
	LastLine    string
}

// blockedIssue tracks an issue held at the dependency gate.
type blockedIssue struct {
	IssueNumber int
	Repo        string // "owner/repo" — empty for single-repo projects
	Title       string
	StageName   string
	WaitingFor  []string // e.g. ["#214", "owner/repo#215"]
}

// activeJobKey returns a unique string key for an active job.
// Format: "owner/repo#N" when Repo is non-empty, "#N" otherwise.
func activeJobKey(repo string, issueNumber int) string {
	if repo != "" {
		return fmt.Sprintf("%s#%d", repo, issueNumber)
	}
	return fmt.Sprintf("#%d", issueNumber)
}

// watchExitMsg is returned by tea.ExecProcess when the fabrik watch subprocess exits.
type watchExitMsg struct{ Err error }

// claudeResumeFinishedMsg is returned by tea.ExecProcess when the claude --resume subprocess exits.
type claudeResumeFinishedMsg struct{ Err error }

// pane identifies which TUI section has focus.
type pane int

const (
	paneActive pane = iota
	paneHistory
)

// ProjectInfo holds display metadata about the monitored project shown in the footer.
type ProjectInfo struct {
	CWD           string // display-ready CWD (home-relative or absolute)
	BoardTitle    string // GitHub Project board title; empty until startup board fetch succeeds
	BoardURL      string // https://github.com/{orgs|users}/{owner}/projects/{num}
	Version       string // optional version or module name of the monitored project; empty if unknown
	FabrikVersion string // fabrik binary version (e.g. "v1.2.3" or "dev(abc1234)")
}

// Model is the bubbletea TUI model for Fabrik.
type Model struct {
	// layout
	width  int
	height int

	// focus and confirmation state
	focusPane   pane
	confirmQuit bool
	detailPanel bool

	// plugin directory
	pluginDir string

	// components
	header  HeaderComponent
	active  ActivePaneComponent
	history HistoryPaneComponent
	detail  DetailPanelComponent
	footer  FooterComponent

	// mouse double-click detection
	lastClickAt time.Time
	lastClickX  int
	lastClickY  int
}

// New creates an initial TUI model.
// pollSeconds is the configured polling interval.
// info provides project metadata displayed in the footer.
// pluginDir is the Fabrik plugin directory passed to claude --plugin-dir (may be empty).
func New(pollSeconds int, info ProjectInfo, pluginDir string) Model {
	interval := time.Duration(pollSeconds) * time.Second
	now := time.Now()
	spinnerFrames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	active := ActivePaneComponent{
		active:         make(map[string]*activeJob),
		activeNumToKey: make(map[int]string),
		blocked:        make(map[string]*blockedIssue),
		focused:        true,
		spinnerFrames:  spinnerFrames,
		now:            now,
		pluginDir:      pluginDir,
	}

	return Model{
		focusPane: paneActive,
		pluginDir: pluginDir,
		header: HeaderComponent{
			pollInterval:  interval,
			now:           now,
			fabrikVersion: info.FabrikVersion,
		},
		active:  active,
		history: NewHistoryPaneComponent(),
		footer: FooterComponent{
			projectInfo: info,
			now:         now,
		},
	}
}

// Init starts the 1-second tick.
func (m Model) Init() tea.Cmd {
	return tickCmd()
}

// tickCmd schedules a TickEvent one second from now.
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
		case "ctrl+c":
			return m, tea.Quit

		case "q":
			if m.history.ConfirmClear() {
				m.history.SetConfirmClear(false)
				return m, nil
			}
			if m.confirmQuit {
				return m, tea.Quit
			}
			if m.active.ActiveCount() > 0 {
				m.confirmQuit = true
				return m, nil
			}
			return m, tea.Quit

		case "n", "N":
			if m.history.ConfirmClear() {
				m.history.SetConfirmClear(false)
				return m, nil
			}
			if m.confirmQuit {
				m.confirmQuit = false
				return m, nil
			}
			if m.detailPanel {
				m.detailPanel = false
				return m, nil
			}

		case "esc":
			if m.history.ConfirmClear() {
				m.history.SetConfirmClear(false)
				return m, nil
			}
			if m.detailPanel {
				m.detailPanel = false
				return m, nil
			}
			if m.confirmQuit {
				m.confirmQuit = false
				return m, nil
			}
			if m.active.ActiveCount() > 0 {
				m.confirmQuit = true
				return m, nil
			}
			return m, tea.Quit

		case "tab":
			if m.focusPane == paneActive {
				m.focusPane = paneHistory
			} else {
				m.focusPane = paneActive
			}
			m.syncFocus()
			m.updateLayout(false)
			return m, nil

		case "enter":
			m.detailPanel = !m.detailPanel
			return m, nil

		case "r":
			if m.focusPane == paneActive {
				if m.active.SelectedJob() != nil {
					m.header.SetStatusMsg("stage in progress — use l to watch")
				}
				return m, nil
			} else if m.focusPane == paneHistory {
				entry := m.history.SelectedEntry()
				if entry == nil {
					return m, nil
				}
				if isActiveIssue(m.active.active, *entry) {
					m.header.SetStatusMsg("stage in progress — use l to watch")
					return m, nil
				}
				cwd, _ := os.Getwd()
				worktreePath := worktreePathForIssue(cwd, entry.Repo, entry.IssueNumber)
				if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
					m.header.SetStatusMsg(fmt.Sprintf("no worktree for #%d", entry.IssueNumber))
					return m, nil
				}
				return m, openResumeInlineCmd(m.pluginDir, entry.Repo, entry.IssueNumber, entry.StageName, entry.StageModel, worktreePath)
			}
			return m, nil

		case "c", "C":
			if m.focusPane == paneHistory {
				comp, cmd := m.history.Update(msg)
				m.history = comp.(HistoryPaneComponent)
				m.updateLayout(false)
				return m, cmd
			}
			return m, nil

		case "up", "k", "down", "j":
			if m.focusPane == paneActive {
				comp, cmd := m.active.Update(msg)
				m.active = comp.(ActivePaneComponent)
				return m, cmd
			}
			comp, cmd := m.history.Update(msg)
			m.history = comp.(HistoryPaneComponent)
			m.updateLayout(false)
			m.history.ScrollToVisible()
			return m, cmd

		case "l":
			if m.focusPane == paneActive {
				comp, cmd := m.active.Update(msg)
				m.active = comp.(ActivePaneComponent)
				return m, cmd
			}
			if m.focusPane == paneHistory {
				comp, cmd := m.history.Update(msg)
				m.history = comp.(HistoryPaneComponent)
				return m, cmd
			}
			return m, nil
		}
		// Forward other key events to the history viewport for scrolling
		var cmd tea.Cmd
		m.history.historyVP, cmd = m.history.historyVP.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = ev.Width
		m.height = ev.Height
		m.updateLayout(false)
		return m, nil

	case watchExitMsg:
		return m, nil

	case claudeResumeFinishedMsg:
		return m, nil

	case TickEvent:
		// Fan out to all components
		comp, _ := m.header.Update(msg)
		m.header = comp.(HeaderComponent)
		comp, _ = m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		comp, _ = m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, tickCmd()

	case ProjectMetaEvent:
		comp, _ := m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil

	case PollStartedEvent:
		comp, _ := m.header.Update(msg)
		m.header = comp.(HeaderComponent)
		return m, nil

	case PollCompletedEvent:
		comp, _ := m.header.Update(msg)
		m.header = comp.(HeaderComponent)
		comp, _ = m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil

	case JobStartedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		return m, nil

	case IssueBlockedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		return m, nil

	case JobCompletedEvent:
		// Fan out to both active (remove job) and history (add entry)
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		hcomp, _ := m.history.Update(msg)
		m.history = hcomp.(HistoryPaneComponent)
		m.updateLayout(true)
		return m, nil

	case LogEvent:
		if ev.IssueNumber == 0 {
			comp, _ := m.header.Update(msg)
			m.header = comp.(HeaderComponent)
		} else {
			comp, _ := m.active.Update(msg)
			m.active = comp.(ActivePaneComponent)
		}
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(ev)
	}

	return m, nil
}

// handleMouse processes mouse events: forwards wheel scroll to the history
// viewport, then performs hit-testing using component Height() returns.
func (m Model) handleMouse(ev tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Forward all mouse events to the history viewport so wheel-scroll works.
	cmd := m.history.ForwardMouseEvent(ev)

	if ev.Button != tea.MouseButtonLeft || ev.Action != tea.MouseActionPress {
		return m, cmd
	}

	// Compute layout Y positions from component Height() returns.
	headerH := m.header.Height()
	activeH := m.active.Height()

	detailH := 0
	if m.detailPanel {
		m.prepareDetailItem()
		detailH = m.detail.Height()
	}

	activeStart := headerH
	activeEnd := activeStart + activeH
	detailEnd := activeEnd + detailH
	histStart := detailEnd

	clickY := ev.Y

	// Detect double-click: same cell within 300ms.
	isDoubleClick := time.Since(m.lastClickAt) < 300*time.Millisecond &&
		ev.X == m.lastClickX && ev.Y == m.lastClickY
	m.lastClickAt = time.Now()
	m.lastClickX = ev.X
	m.lastClickY = ev.Y

	switch {
	case clickY >= activeStart && clickY < activeEnd:
		m.focusPane = paneActive
		m.syncFocus()
		localY := clickY - activeStart
		m.active.HandleClick(ev.X, localY)

		// Double-click on active content row opens watch
		if isDoubleClick && localY >= 2 && localY < activeH-1 {
			if job := m.active.SelectedJob(); job != nil {
				return m, openWatchInlineCmd(job.IssueNumber, job.Repo)
			}
		}

	case clickY >= histStart && clickY <= histStart+1:
		m.focusPane = paneHistory
		m.syncFocus()
		m.updateLayout(false)

	case clickY >= histStart+2:
		m.focusPane = paneHistory
		m.syncFocus()
		localY := clickY - histStart
		m.history.HandleClick(ev.X, localY)
		m.updateLayout(false)

		if isDoubleClick {
			if entry := m.history.SelectedEntry(); entry != nil {
				return m, openWatchInlineCmd(entry.IssueNumber, entry.Repo)
			}
		}
	}

	return m, cmd
}

// syncFocus updates focused state on pane components to match m.focusPane.
func (m *Model) syncFocus() {
	m.active.SetFocused(m.focusPane == paneActive)
	m.history.SetFocused(m.focusPane == paneHistory)
}

// updateLayout recomputes the history viewport dimensions and content.
func (m *Model) updateLayout(scrollToTop bool) {
	activeH := m.active.Height()
	detailH := 0
	if m.detailPanel {
		m.prepareDetailItem()
		detailH = m.detail.Height()
	}
	availableHeight := m.height - m.header.Height() - activeH - detailH - m.footer.Height()
	m.history.SetLayout(m.width, availableHeight, m.confirmQuit, m.active.ActiveCount())
	if scrollToTop {
		m.history.ScrollToTop()
	} else {
		m.history.ScrollToVisible()
	}
}

// prepareDetailItem constructs the DetailItem from the focused pane's selection.
func (m *Model) prepareDetailItem() {
	if m.focusPane == paneActive {
		if job := m.active.SelectedJob(); job != nil {
			m.detail.SetItem(&DetailItem{
				IssueNumber: job.IssueNumber,
				Title:       job.Title,
				StageName:   job.StageName,
				IsActive:    true,
				Elapsed:     m.header.now.Sub(job.StartedAt),
			})
		} else {
			m.detail.SetItem(nil)
		}
	} else if entry := m.history.SelectedEntry(); entry != nil {
		m.detail.SetItem(&DetailItem{
			IssueNumber:    entry.IssueNumber,
			Title:          entry.Title,
			StageName:      entry.StageName,
			StageModel:     entry.StageModel,
			Success:        entry.Success,
			Completed:      entry.Completed,
			BlockedOnInput: entry.BlockedOnInput,
			Duration:       entry.Duration,
			TurnsUsed:      entry.TurnsUsed,
			MaxTurns:       entry.MaxTurns,
			CostUSD:        entry.CostUSD,
			CompletedAt:    entry.CompletedAt,
		})
	} else {
		m.detail.SetItem(nil)
	}
	m.detail.SetVisible(m.detailPanel)
}

// View renders the full TUI.
func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var sections []string

	sections = append(sections, m.header.View(m.width))
	sections = append(sections, m.active.View(m.width))

	if m.detailPanel {
		m.prepareDetailItem()
		if detail := m.detail.View(m.width); detail != "" {
			sections = append(sections, detail)
		}
	}

	sections = append(sections, m.history.View(m.width))
	sections = append(sections, m.footer.View(m.width))

	return strings.Join(sections, "\n")
}
