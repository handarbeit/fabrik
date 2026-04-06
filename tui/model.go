package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	Repo          string // "owner/repo"
	Version       string // optional version or module name of the monitored project; empty if unknown
	FabrikVersion string // fabrik binary version (e.g. "v1.2.3" or "dev(abc1234)")
}

// Model is the bubbletea TUI model for Fabrik.
type Model struct {
	// project info for the footer
	projectInfo ProjectInfo
	// poll timer
	pollInterval time.Duration
	nextPollAt   time.Time
	pollCount    int

	// active jobs keyed by "owner/repo#N" (or "#N" for single-repo)
	active map[string]*activeJob
	// activeNumToKey maps issue number → current job key for LogEvent routing.
	// When two repos have the same issue number, the last-started job wins.
	activeNumToKey map[int]string

	// completed job history (newest last)
	history []HistoryEntry

	// history viewport for scrolling
	historyVP viewport.Model

	// terminal dimensions
	width  int
	height int

	// spinner frames
	spinnerFrames []string
	spinnerIdx    int

	// now (updated on each TickEvent)
	now time.Time

	// pluginDir is the Fabrik plugin directory, passed to claude --plugin-dir
	pluginDir string

	// statusLine shows the latest poll-level log message in the header
	statusLine string

	// statusMsg is a transient user-facing message shown in the header instead
	// of statusLine. It is cleared on each TickEvent so it disappears after ~1s.
	statusMsg string

	// detailPanel controls whether the inline detail panel is shown between the
	// active and history sections in View().
	detailPanel bool

	// selection state
	focusPane    pane
	activeIdx    int  // index into sorted active issue numbers
	histIdx      int  // index into history (0 = newest)
	confirmClear bool // true when waiting for Y/N on clear-all
	confirmQuit  bool // true when waiting for q/n on quit-with-active-jobs

	// mouse double-click detection
	lastClickAt time.Time
	lastClickX  int
	lastClickY  int
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)

	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	activeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("238"))
)

// New creates an initial TUI model.
// pollSeconds is the configured polling interval.
// info provides project metadata displayed in the footer.
// pluginDir is the Fabrik plugin directory passed to claude --plugin-dir (may be empty).
func New(pollSeconds int, info ProjectInfo, pluginDir string) Model {
	vp := viewport.New(80, 10)
	return Model{
		projectInfo:    info,
		pollInterval:   time.Duration(pollSeconds) * time.Second,
		active:         make(map[string]*activeJob),
		activeNumToKey: make(map[int]string),
		history:        LoadHistory(),
		historyVP:      vp,
		spinnerFrames:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		now:            time.Now(),
		pluginDir:      pluginDir,
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
			// Always quit immediately, bypassing all confirmation dialogs.
			return m, tea.Quit

		case "q":
			if m.confirmClear {
				m.confirmClear = false
				return m, nil
			}
			if m.confirmQuit {
				return m, tea.Quit
			}
			if len(m.active) > 0 {
				m.confirmQuit = true
				return m, nil
			}
			return m, tea.Quit

		case "c":
			// Delete selected history entry
			if m.focusPane == paneHistory && len(m.history) > 0 {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					m.history = append(m.history[:realIdx], m.history[realIdx+1:]...)
					SaveHistory(m.history)
					if m.histIdx >= len(m.history) && m.histIdx > 0 {
						m.histIdx--
					}
					m.updateHistoryViewport(false)
				}
			}
			return m, nil

		case "C":
			if m.focusPane == paneHistory && len(m.history) > 0 {
				if m.confirmClear {
					// Confirmed — clear all
					m.history = nil
					m.histIdx = 0
					m.confirmClear = false
					SaveHistory(m.history)
					m.updateHistoryViewport(false)
				} else {
					// Ask for confirmation
					m.confirmClear = true
				}
			}
			return m, nil

		case "n", "N":
			if m.confirmClear {
				m.confirmClear = false
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
			// Priority: confirmClear → detailPanel → confirmQuit → trigger quit flow.
			if m.confirmClear {
				m.confirmClear = false
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
			if len(m.active) > 0 {
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
			m.updateHistoryViewport(false)
			return m, nil

		case "up", "k":
			if m.focusPane == paneActive {
				if m.activeIdx > 0 {
					m.activeIdx--
				}
			} else {
				if m.histIdx > 0 {
					m.histIdx--
					m.updateHistoryViewport(false)
				}
			}
			return m, nil

		case "down", "j":
			if m.focusPane == paneActive {
				keys := m.sortedActiveKeys()
				if m.activeIdx < len(keys)-1 {
					m.activeIdx++
				}
			} else {
				if m.histIdx < len(m.history)-1 {
					m.histIdx++
					m.updateHistoryViewport(false)
				}
			}
			return m, nil

		case "l":
			// Launch fabrik watch <issueNumber> inline via tea.ExecProcess.
			// Suspends the TUI until the user exits watch with q.
			if m.focusPane == paneActive {
				keys := m.sortedActiveKeys()
				if m.activeIdx < len(keys) {
					if job, ok := m.active[keys[m.activeIdx]]; ok {
						return m, openWatchInlineCmd(job.IssueNumber, job.Repo)
					}
				}
			} else if m.focusPane == paneHistory && len(m.history) > 0 {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					h := m.history[realIdx]
					return m, openWatchInlineCmd(h.IssueNumber, h.Repo)
				}
			}
			return m, nil

		case "enter":
			// Toggle the inline detail panel.
			m.detailPanel = !m.detailPanel
			return m, nil

		case "r":
			// Active pane: show status message — active sessions must not be interrupted.
			// History pane: resume an interactive Claude session in the issue's worktree,
			// but only when the issue is not currently active.
			if m.focusPane == paneActive {
				keys := m.sortedActiveKeys()
				if m.activeIdx < len(keys) {
					m.statusMsg = "stage in progress — use l to watch"
				}
				return m, nil
			} else if m.focusPane == paneHistory && len(m.history) > 0 {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					h := m.history[realIdx]
					if m.isActiveIssue(h) {
						m.statusMsg = "stage in progress — use l to watch"
						return m, nil
					}
					// Check worktree exists before launching resume.
					cwd, _ := os.Getwd()
					worktreePath := worktreePathForIssue(cwd, h.Repo, h.IssueNumber)
					if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
						m.statusMsg = fmt.Sprintf("no worktree for #%d", h.IssueNumber)
						return m, nil
					}
					return m, m.openResumeInlineCmd(h.Repo, h.IssueNumber, h.StageName, h.StageModel, worktreePath)
				}
			}
			return m, nil

		}
		// Forward other key events to the history viewport for scrolling
		var cmd tea.Cmd
		m.historyVP, cmd = m.historyVP.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = ev.Width
		m.height = ev.Height
		m.updateHistoryViewport(false)
		return m, nil

	case watchExitMsg:
		// fabrik watch subprocess exited — nothing to do (TUI resumes automatically).
		return m, nil

	case claudeResumeFinishedMsg:
		// claude --resume subprocess exited — nothing to do (TUI resumes automatically).
		return m, nil

	case TickEvent:
		m.now = ev.At
		m.spinnerIdx = (m.spinnerIdx + 1) % len(m.spinnerFrames)
		m.statusMsg = "" // clear transient status message each tick
		return m, tickCmd()

	case PollStartedEvent:
		m.nextPollAt = time.Now().Add(m.pollInterval)
		return m, nil

	case PollCompletedEvent:
		m.pollCount++
		m.nextPollAt = time.Now().Add(m.pollInterval)
		return m, nil

	case JobStartedEvent:
		key := activeJobKey(ev.Repo, ev.IssueNumber)
		m.active[key] = &activeJob{
			IssueNumber: ev.IssueNumber,
			Repo:        ev.Repo,
			Title:       ev.Title,
			StageName:   ev.StageName,
			IsComment:   ev.IsComment,
			StartedAt:   ev.StartedAt,
		}
		m.activeNumToKey[ev.IssueNumber] = key
		return m, nil

	case JobCompletedEvent:
		key := activeJobKey(ev.Repo, ev.IssueNumber)
		delete(m.active, key)
		if m.activeNumToKey[ev.IssueNumber] == key {
			delete(m.activeNumToKey, ev.IssueNumber)
		}
		entry := HistoryEntry{
			IssueNumber:    ev.IssueNumber,
			Repo:           ev.Repo,
			Title:          ev.Title,
			StageName:      ev.StageName,
			StageModel:     ev.StageModel,
			IsComment:      ev.IsComment,
			Success:        ev.Success,
			Completed:      ev.Completed,
			BlockedOnInput: ev.BlockedOnInput,
			Duration:       ev.Duration,
			CompletedAt:    ev.CompletedAt,
			TurnsUsed:      ev.TurnsUsed,
			MaxTurns:       ev.MaxTurns,
			CostUSD:        ev.CostUSD,
		}
		m.history = append(m.history, entry)
		SaveHistory(m.history)
		m.updateHistoryViewport(true)
		return m, nil

	case LogEvent:
		if ev.IssueNumber == 0 {
			// Poll-level message — show in header status line
			m.statusLine = fmt.Sprintf("[%s] %s", ev.Tag, strings.TrimRight(ev.Message, "\n"))
		} else if key, known := m.activeNumToKey[ev.IssueNumber]; known {
			if job, ok := m.active[key]; ok {
				job.LastTag = ev.Tag
				job.LastLine = strings.TrimRight(ev.Message, "\n")
			}
		}
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(ev)
	}

	return m, nil
}

// handleMouse processes a tea.MouseMsg: forwards all mouse events to the history
// viewport (which handles wheel scroll internally), then performs hit-testing for
// left-click events to update selection state and detect double-clicks.
func (m Model) handleMouse(ev tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Forward all mouse events to the history viewport so wheel-scroll works.
	var cmd tea.Cmd
	m.historyVP, cmd = m.historyVP.Update(ev)

	if ev.Button != tea.MouseButtonLeft || ev.Action != tea.MouseActionPress {
		return m, cmd
	}

	// Compute layout Y positions for hit-testing.
	nActive := len(m.active)
	activeH := activeHeight(nActive)

	// histTopY: Y where the history pane border starts.
	// Active pane occupies Y=1..activeH (inclusive). Detail panel may follow.
	histTopY := 1 + activeH
	if m.detailPanel {
		if detail := m.viewDetail(); detail != "" {
			histTopY += strings.Count(detail, "\n") + 1
		}
	}

	clickY := ev.Y

	// Detect double-click: same cell within 300ms.
	isDoubleClick := time.Since(m.lastClickAt) < 300*time.Millisecond &&
		ev.X == m.lastClickX && ev.Y == m.lastClickY
	m.lastClickAt = time.Now()
	m.lastClickX = ev.X
	m.lastClickY = ev.Y

	switch {
	case clickY >= 1 && clickY <= activeH:
		// Click anywhere in the active pane (borders, title, or content rows).
		m.focusPane = paneActive

		// Content rows start at Y=3 (border-top at Y=1, title at Y=2).
		// Border-bottom is at Y=activeH; don't treat it as a row selection.
		if clickY >= 3 && clickY < activeH {
			keys := m.sortedActiveKeys()
			nKeys := len(keys)
			maxLines := activeH - 3
			start := 0
			hasEllipsis := false
			if nKeys > maxLines && maxLines > 0 {
				start = m.activeIdx - maxLines/2
				if start < 0 {
					start = 0
				}
				if start+maxLines > nKeys {
					start = max(nKeys-maxLines, 0)
				}
				// Mirror viewActive(): when ellipsis is shown, the last content
				// row is the ellipsis indicator and must not be treated as a job.
				if start > 0 || start+maxLines < nKeys {
					maxLines--
					hasEllipsis = true
				}
			}
			visibleRow := clickY - 3
			if hasEllipsis && visibleRow == maxLines {
				// Clicked the "… N more" ellipsis row; focus the pane but don't select a job.
				break
			}
			actualIdx := start + visibleRow
			if actualIdx >= 0 && actualIdx < nKeys {
				m.activeIdx = actualIdx
				if isDoubleClick {
					if job, ok := m.active[keys[actualIdx]]; ok {
						return m, openWatchInlineCmd(job.IssueNumber, job.Repo)
					}
				}
			}
		}

	case clickY >= histTopY && clickY <= histTopY+1:
		// Click on history pane border-top or title row → switch focus.
		m.focusPane = paneHistory
		m.updateHistoryViewport(false)

	case clickY >= histTopY+2:
		// Click on history viewport content.
		visibleRow := clickY - (histTopY + 2)
		newHistIdx := visibleRow + m.historyVP.YOffset
		if newHistIdx >= 0 && newHistIdx < len(m.history) {
			m.focusPane = paneHistory
			m.histIdx = newHistIdx
			m.updateHistoryViewport(false)
			if isDoubleClick {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					h := m.history[realIdx]
					return m, openWatchInlineCmd(h.IssueNumber, h.Repo)
				}
			}
		}
	}

	return m, cmd
}

// updateHistoryViewport rebuilds the viewport content from the history slice.
// Uses a pointer receiver so it can be called on the addressable local copy
// in Update(); the mutations are included in the returned model value.
// If scrollToTop is true, the viewport is scrolled to the top (newest entry);
// otherwise the viewport is adjusted so histIdx remains visible.
func (m *Model) updateHistoryViewport(scrollToTop bool) {
	innerWidth := max(m.width-6, 20) // account for border + padding

	var lines []string
	// Show newest entries at the top.
	for i := len(m.history) - 1; i >= 0; i-- {
		h := m.history[i]
		var status, result string
		if !h.Success {
			status = failStyle.Render("✗")
			result = dimStyle.Render("  (error)")
		} else if h.BlockedOnInput {
			status = activeStyle.Render("?")
			result = dimStyle.Render("  (input needed)")
		} else if !h.Completed {
			status = dimStyle.Render("↻")
			result = dimStyle.Render("  (retry)")
		} else {
			status = successStyle.Render("✓")
		}
		ts := dimStyle.Render(h.CompletedAt.Format("2006-01-02 15:04"))
		dur := fmtDuration(h.Duration)
		stats := ""
		if h.TurnsUsed > 0 || h.CostUSD > 0 {
			parts := []string{}
			if h.MaxTurns > 0 {
				parts = append(parts, fmt.Sprintf("%d/%d turns", h.TurnsUsed, h.MaxTurns))
			} else if h.TurnsUsed > 0 {
				parts = append(parts, fmt.Sprintf("%d turns", h.TurnsUsed))
			}
			if h.CostUSD > 0 {
				parts = append(parts, fmt.Sprintf("$%.2f", h.CostUSD))
			}
			stats = dimStyle.Render("  " + strings.Join(parts, " "))
		}
		titleStr := ""
		if h.Title != "" {
			maxTitle := max(innerWidth-60, 10)
			t := h.Title
			if runes := []rune(t); len(runes) > maxTitle {
				t = string(runes[:maxTitle]) + "…"
			}
			titleStr = "  " + dimStyle.Render(t)
		}
		stageDisplay := h.StageName
		if h.IsComment {
			stageDisplay += " 💬"
		}
		// Pad stage column manually — emoji width differs from byte count
		stagePad := 12 - lipgloss.Width(stageDisplay)
		if stagePad > 0 {
			stageDisplay += strings.Repeat(" ", stagePad)
		}
		line := fmt.Sprintf("#%-5d %s %s %s  %s%s%s%s",
			h.IssueNumber, stageDisplay, status, dur, ts, stats, result, titleStr)
		displayIdx := len(m.history) - 1 - i // 0-based index matching histIdx
		if m.focusPane == paneHistory && displayIdx == m.histIdx {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}
	m.historyVP.SetContent(strings.Join(lines, "\n"))

	// Update viewport height within the overall layout.
	// Minimum 1 (not 3) so the history pane can shrink on small terminals
	// without pushing header/footer off screen. viewport.Model panics on
	// non-positive Height, so the floor of 1 is both a correctness and
	// safety requirement.
	historyHeight := max(m.height-headerHeight()-activeHeight(len(m.active))-footerHeight()-3, 1)
	m.historyVP.Width = innerWidth
	m.historyVP.Height = historyHeight

	if scrollToTop {
		// New entry arrived — scroll to top so newest is visible.
		m.historyVP.GotoTop()
	} else {
		// Keep histIdx visible: clamp YOffset so histIdx falls within
		// [YOffset, YOffset+Height-1]. SetYOffset clamps internally.
		if m.histIdx < m.historyVP.YOffset {
			m.historyVP.SetYOffset(m.histIdx)
		} else if m.histIdx > m.historyVP.YOffset+m.historyVP.Height-1 {
			m.historyVP.SetYOffset(m.histIdx - m.historyVP.Height + 1)
		}
	}
}

// View renders the full TUI.
func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var sections []string

	// ── Header pane ──
	sections = append(sections, m.viewHeader())

	// ── In-Progress pane ──
	sections = append(sections, m.viewActive())

	// ── Detail panel (toggled by enter) ──
	if m.detailPanel {
		if detail := m.viewDetail(); detail != "" {
			sections = append(sections, detail)
		}
	}

	// ── History pane ──
	sections = append(sections, m.viewHistory())

	// ── Footer ──
	sections = append(sections, m.viewFooter())

	return strings.Join(sections, "\n")
}

func (m Model) viewHeader() string {
	var timer string
	remaining := time.Until(m.nextPollAt)
	if remaining <= 0 {
		timer = "polling..."
	} else {
		timer = fmt.Sprintf("poll in %s", fmtDuration(remaining))
	}

	title := titleStyle.Render("fabrik")
	if m.projectInfo.FabrikVersion != "" {
		title = title + " " + dimStyle.Render(m.projectInfo.FabrikVersion)
	}
	timerStr := dimStyle.Render(timer)

	// statusMsg is transient (cleared each tick); it takes priority over statusLine.
	displayStatus := m.statusMsg
	if displayStatus == "" {
		displayStatus = m.statusLine
	}

	status := ""
	if displayStatus != "" {
		status = "  " + dimStyle.Render(displayStatus)
	}

	// title + status on left, timer on right, single line
	left := title + status
	leftWidth := lipgloss.Width(left)
	timerWidth := lipgloss.Width(timerStr)
	available := m.width - 1 // leading space
	if leftWidth+timerWidth > available {
		// Truncate status to fit. The rendered structure is:
		//   " " + title + "  " + dimStyle(s+"…") + gap + timerStr
		// where "  " (2 chars) is the prefix and "…" (1 char) is the suffix → 3 chars overhead.
		maxStatus := max(available-lipgloss.Width(title)-timerWidth-3, 0)
		if maxStatus > 0 && displayStatus != "" {
			s := displayStatus
			// Shrink using display width (lipgloss.Width strips ANSI), matching viewFooter pattern.
			for lipgloss.Width(s) > maxStatus {
				runes := []rune(s)
				if len(runes) == 0 {
					break
				}
				s = string(runes[:len(runes)-1])
			}
			s += "…"
			status = "  " + dimStyle.Render(s)
		} else {
			status = ""
		}
		left = title + status
		leftWidth = lipgloss.Width(left)
	}
	// gap min=0: when header is exactly full-width after truncation no padding is needed.
	gap := max(m.width-1-leftWidth-timerWidth, 0)
	return " " + left + strings.Repeat(" ", gap) + timerStr
}

func (m Model) viewActive() string {
	focusIndicator := " "
	if m.focusPane == paneActive {
		focusIndicator = "▸"
	}
	title := activeStyle.Render(fmt.Sprintf("%s In Progress (%d)", focusIndicator, len(m.active)))

	var lines []string
	keys := m.sortedActiveKeys()

	spinner := m.spinnerFrames[m.spinnerIdx]
	for idx, key := range keys {
		job := m.active[key]
		elapsed := fmtDuration(m.now.Sub(job.StartedAt))
		tag := ""
		if job.LastTag != "" {
			tag = dimStyle.Render(fmt.Sprintf("[%s]", job.LastTag))
		}
		msg := ""
		if job.LastLine != "" {
			msg = job.LastLine
		}
		titleStr := ""
		if job.Title != "" {
			titleStr = dimStyle.Render(job.Title) + " "
		}
		stageDisplay := job.StageName
		if job.IsComment {
			stageDisplay += " 💬"
		}
		stagePad := 12 - lipgloss.Width(stageDisplay)
		if stagePad > 0 {
			stageDisplay += strings.Repeat(" ", stagePad)
		}
		line := fmt.Sprintf("#%-5d %s %s %s  %s%s %s",
			job.IssueNumber, stageDisplay, spinner, elapsed, titleStr, tag, msg)
		// Truncate to terminal width to prevent wrapping
		maxWidth := max(m.width-6, 20) // account for border + padding
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		if m.focusPane == paneActive && idx == m.activeIdx {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}

	// Cap visible lines to fit within activeHeight
	maxLines := activeHeight(len(m.active)) - 3 // subtract title + border
	if len(lines) > maxLines && maxLines > 0 {
		// Show lines around the selected index
		start := m.activeIdx - maxLines/2
		if start < 0 {
			start = 0
		}
		if start+maxLines > len(lines) {
			start = max(len(lines)-maxLines, 0)
		}
		if start > 0 || start+maxLines < len(keys) {
			maxLines--
		}
		lines = lines[start : start+maxLines]
		if start > 0 || start+maxLines < len(keys) {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … %d more", len(keys)-maxLines)))
		}
	}

	hint := ""
	if m.focusPane == paneActive && len(m.active) > 0 {
		hint = dimStyle.Render("  [l] watch  [enter] details  [tab] history")
	}
	content := title + hint + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(m.width - 4).Render(content)
}

func (m Model) viewHistory() string {
	focusIndicator := " "
	if m.focusPane == paneHistory {
		focusIndicator = "▸"
	}
	title := dimStyle.Render(fmt.Sprintf("%s History (%d)", focusIndicator, len(m.history)))
	hint := ""
	if m.confirmClear {
		hint = failStyle.Render("  Clear all history? [C]onfirm / [n]o")
	} else if m.confirmQuit {
		hint = failStyle.Render(fmt.Sprintf("  Quit Fabrik? %d jobs still in progress \u2014 they will be interrupted.  [q] Quit anyway   [n/Escape] Cancel", len(m.active)))
	} else if m.focusPane == paneHistory && len(m.history) > 0 {
		hint = dimStyle.Render("  [r]esume  [l] watch  [enter] details  [c]lear  [C]lear all  [tab] in-progress")
	}
	content := title + hint + "\n" + m.historyVP.View()
	return borderStyle.Width(m.width - 4).Render(content)
}

func (m Model) viewFooter() string {
	// Assemble: CWD · owner/repo [· version]
	parts := []string{}
	if m.projectInfo.CWD != "" {
		parts = append(parts, m.projectInfo.CWD)
	}
	if m.projectInfo.Repo != "" {
		parts = append(parts, m.projectInfo.Repo)
	}
	if m.projectInfo.Version != "" {
		parts = append(parts, m.projectInfo.Version)
	}
	footer := dimStyle.Render(strings.Join(parts, " · "))

	// Truncate to terminal width so the footer never wraps.
	maxWidth := m.width - 1
	if maxWidth < 1 {
		maxWidth = 1
	}
	if lipgloss.Width(footer) > maxWidth {
		// Re-render with truncated plain text.
		plain := strings.Join(parts, " · ")
		runes := []rune(plain)
		// Binary search is overkill; shrink until it fits.
		for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > maxWidth {
			runes = runes[:len(runes)-1]
		}
		footer = dimStyle.Render(string(runes) + "…")
	}
	return footer
}

// headerHeight returns the approximate line height of the header pane.
func headerHeight() int {
	return 1
}

// footerHeight returns the height of the footer pane (one persistent line).
func footerHeight() int {
	return 1
}

// activeHeight returns the approximate line height of the active pane.
// Capped to avoid pushing history off screen on small terminals.
func activeHeight(n int) int {
	base := max(n+1, 2) + 2
	if base > 10 {
		return 11
	}
	return base
}

// sortedActiveKeys returns job keys from the active map in sorted order.
// Keys have the form "owner/repo#N" or "#N" (single-repo).
func (m Model) sortedActiveKeys() []string {
	keys := make([]string, 0, len(m.active))
	for k := range m.active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tuiReadSessionID reads the Claude session ID for a given repo, issue and stage.
// The path logic mirrors engine.ReadSessionID — keep in sync if either changes.
func tuiReadSessionID(repo string, issueNumber int, stageName string) string {
	home, _ := os.UserHomeDir()
	base := filepath.Base(stageName)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "default"
	}
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	var sessDir string
	if repo != "" {
		repoPart := strings.ReplaceAll(repo, "/", "-")
		sessDir = filepath.Join(home, ".fabrik", "sessions", repoPart, issuePart)
	} else {
		sessDir = filepath.Join(home, ".fabrik", "sessions", issuePart)
	}
	data, err := os.ReadFile(filepath.Join(sessDir, base+".session"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}


// fmtDuration formats a duration as MM:SS.
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// openWatchInlineCmd returns a tea.Cmd that suspends the TUI and launches
// "fabrik watch <issueNumber>" in the current terminal via tea.ExecProcess.
// The TUI is restored automatically when the user exits watch with q.
// In multi-repo mode, --owner and --repo flags are passed explicitly so the
// child process watches the correct repository.
func openWatchInlineCmd(issueNumber int, repo string) tea.Cmd {
	fabrikBin, err := os.Executable()
	if err != nil {
		fabrikBin = "fabrik"
	}
	args := []string{"watch"}
	if repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 {
			args = append(args, "--owner", parts[0], "--repo", parts[1])
		}
	}
	args = append(args, strconv.Itoa(issueNumber))
	cmd := exec.Command(fabrikBin, args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return watchExitMsg{Err: err}
	})
}

// openResumeInlineCmd returns a tea.Cmd that suspends the TUI and launches an
// interactive Claude session in the issue's worktree via tea.ExecProcess.
// worktreePath must already be verified to exist by the caller.
// If a session file exists for the given stage, --resume <id> is passed;
// otherwise a fresh session starts.
func (m Model) openResumeInlineCmd(repo string, issueNumber int, stageName, stageModel, worktreePath string) tea.Cmd {
	args := []string{}
	sessionID := tuiReadSessionID(repo, issueNumber, stageName)
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if stageModel != "" {
		args = append(args, "--model", stageModel)
	}
	if m.pluginDir != "" {
		args = append(args, "--plugin-dir", m.pluginDir)
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = worktreePath
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return claudeResumeFinishedMsg{Err: err}
	})
}

// worktreePathForIssue returns the absolute path to the issue's git worktree.
// In single-repo mode: <rootDir>/.fabrik/worktrees/issue-N
// In multi-repo mode:  <rootDir>/.fabrik/worktrees/<owner>-<repo>/issue-N
func worktreePathForIssue(rootDir, repo string, issueNumber int) string {
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	if repo != "" {
		repoPart := strings.ReplaceAll(repo, "/", "-")
		return filepath.Join(rootDir, ".fabrik", "worktrees", repoPart, issuePart)
	}
	return filepath.Join(rootDir, ".fabrik", "worktrees", issuePart)
}

// isActiveIssue reports whether the history entry's issue is currently being
// processed (present in the active jobs map). Works correctly in multi-repo mode.
func (m Model) isActiveIssue(h HistoryEntry) bool {
	key := activeJobKey(h.Repo, h.IssueNumber)
	_, ok := m.active[key]
	return ok
}

// viewDetail renders an inline detail panel for the currently selected item.
// Active pane: shows in-flight job metadata.
// History pane: shows completed job metadata.
// Returns an empty string when there is nothing to display.
func (m Model) viewDetail() string {
	var lines []string

	if m.focusPane == paneActive {
		keys := m.sortedActiveKeys()
		if m.activeIdx < len(keys) {
			if job, ok := m.active[keys[m.activeIdx]]; ok {
				elapsed := fmtDuration(m.now.Sub(job.StartedAt))
				lines = append(lines, fmt.Sprintf("Issue:   #%d", job.IssueNumber))
				if job.Title != "" {
					lines = append(lines, fmt.Sprintf("Title:   %s", job.Title))
				}
				lines = append(lines, fmt.Sprintf("Stage:   %s", job.StageName))
				lines = append(lines, fmt.Sprintf("Elapsed: %s", elapsed))
			}
		}
	} else if m.focusPane == paneHistory && len(m.history) > 0 {
		realIdx := len(m.history) - 1 - m.histIdx
		if realIdx >= 0 && realIdx < len(m.history) {
			h := m.history[realIdx]
			statusStr := "success"
			if !h.Success {
				statusStr = "error"
			} else if h.BlockedOnInput {
				statusStr = "awaiting input"
			} else if !h.Completed {
				statusStr = "incomplete"
			}
			lines = append(lines, fmt.Sprintf("Issue:    #%d", h.IssueNumber))
			if h.Title != "" {
				lines = append(lines, fmt.Sprintf("Title:    %s", h.Title))
			}
			lines = append(lines, fmt.Sprintf("Stage:    %s", h.StageName))
			if h.StageModel != "" {
				lines = append(lines, fmt.Sprintf("Model:    %s", h.StageModel))
			}
			lines = append(lines, fmt.Sprintf("Status:   %s", statusStr))
			lines = append(lines, fmt.Sprintf("Duration: %s", fmtDuration(h.Duration)))
			if h.TurnsUsed > 0 {
				if h.MaxTurns > 0 {
					lines = append(lines, fmt.Sprintf("Turns:    %d/%d", h.TurnsUsed, h.MaxTurns))
				} else {
					lines = append(lines, fmt.Sprintf("Turns:    %d", h.TurnsUsed))
				}
			}
			if h.CostUSD > 0 {
				lines = append(lines, fmt.Sprintf("Cost:     $%.4f", h.CostUSD))
			}
			if !h.CompletedAt.IsZero() {
				lines = append(lines, fmt.Sprintf("At:       %s", h.CompletedAt.Format("2006-01-02 15:04:05")))
			}
		}
	}

	if len(lines) == 0 {
		return ""
	}

	titleLine := dimStyle.Render("Detail") + "  " + dimStyle.Render("[esc] close")
	content := titleLine + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(m.width - 4).Render(content)
}
