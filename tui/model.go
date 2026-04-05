package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HistoryEntry records a completed job for the history pane.
type HistoryEntry struct {
	IssueNumber int
	Repo        string // "owner/repo" — empty for single-repo projects
	Title       string
	StageName   string
	StageModel  string // model configured for the stage; empty means use claude default
	IsComment   bool
	Success        bool
	Completed      bool
	BlockedOnInput bool
	Duration    time.Duration
	CompletedAt time.Time
	TurnsUsed   int
	MaxTurns    int
	CostUSD     float64
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

// pane identifies which TUI section has focus.
type pane int

const (
	paneActive pane = iota
	paneHistory
)

// ProjectInfo holds display metadata about the monitored project shown in the footer.
type ProjectInfo struct {
	CWD     string // display-ready CWD (home-relative or absolute)
	Repo    string // "owner/repo"
	Version string // optional version or module name; empty if unknown
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

	// terminal is the resolved terminal emulator identifier (e.g. "terminal",
	// "iterm2", "ghostty", "kitty", "alacritty", "warp"). Empty string means
	// use the platform default.
	terminal string

	// selection state
	focusPane    pane
	activeIdx    int  // index into sorted active issue numbers
	histIdx      int  // index into history (0 = newest)
	confirmClear bool // true when waiting for Y/N on clear-all
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
// terminal is the resolved terminal emulator identifier; empty string uses the platform default.
// pluginDir is the Fabrik plugin directory passed to claude --plugin-dir (may be empty).
func New(pollSeconds int, info ProjectInfo, terminal string, pluginDir string) Model {
	vp := viewport.New(80, 10)
	return Model{
		projectInfo:    info,
		pollInterval:   time.Duration(pollSeconds) * time.Second,
		active:         make(map[string]*activeJob),
		activeNumToKey: make(map[int]string),
		history:        LoadHistory(),
		historyVP:     vp,
		spinnerFrames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		now:           time.Now(),
		terminal:      terminal,
		pluginDir:     pluginDir,
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
		case "ctrl+c", "q":
			if m.confirmClear {
				m.confirmClear = false
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
					m.updateHistoryViewport()
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
					m.updateHistoryViewport()
				} else {
					// Ask for confirmation
					m.confirmClear = true
				}
			}
			return m, nil

		case "n", "N", "escape":
			if m.confirmClear {
				m.confirmClear = false
				return m, nil
			}

		case "tab":
			if m.focusPane == paneActive {
				m.focusPane = paneHistory
			} else {
				m.focusPane = paneActive
			}
			m.updateHistoryViewport()
			return m, nil

		case "up", "k":
			if m.focusPane == paneActive {
				if m.activeIdx > 0 {
					m.activeIdx--
				}
			} else {
				if m.histIdx > 0 {
					m.histIdx--
					m.updateHistoryViewport()
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
					m.updateHistoryViewport()
				}
			}
			return m, nil

		case "enter", "l":
			// Open latest log file for selected job (active or history pane)
			if m.focusPane == paneActive {
				keys := m.sortedActiveKeys()
				if m.activeIdx < len(keys) {
					if job, ok := m.active[keys[m.activeIdx]]; ok {
						logDir := logDirForJob(job)
						return m, m.openLogViewerCmd(logDir)
					}
				}
			} else if m.focusPane == paneHistory && len(m.history) > 0 {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					h := m.history[realIdx]
					logDir := logDirForHistory(h)
					return m, m.openLogViewerCmd(logDir)
				}
			}
			return m, nil

		case "r":
			// Active pane: open log viewer (active sessions must not be interrupted)
			// History pane: open interactive resume session in the issue's worktree
			if m.focusPane == paneActive {
				keys := m.sortedActiveKeys()
				if m.activeIdx < len(keys) {
					if job, ok := m.active[keys[m.activeIdx]]; ok {
						logDir := logDirForJob(job)
						return m, m.openLogViewerCmd(logDir)
					}
				}
			} else if m.focusPane == paneHistory && len(m.history) > 0 {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					h := m.history[realIdx]
					return m, m.openResumeCmd(h.IssueNumber, h.StageName, h.StageModel)
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
		m.updateHistoryViewport()
		return m, nil

	case TickEvent:
		m.now = ev.At
		m.spinnerIdx = (m.spinnerIdx + 1) % len(m.spinnerFrames)
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
			IssueNumber: ev.IssueNumber,
			Repo:        ev.Repo,
			Title:       ev.Title,
			StageName:   ev.StageName,
			StageModel:  ev.StageModel,
			IsComment:   ev.IsComment,
			Success:        ev.Success,
			Completed:      ev.Completed,
			BlockedOnInput: ev.BlockedOnInput,
			Duration:    ev.Duration,
			CompletedAt: ev.CompletedAt,
			TurnsUsed:   ev.TurnsUsed,
			MaxTurns:    ev.MaxTurns,
			CostUSD:     ev.CostUSD,
		}
		m.history = append(m.history, entry)
		SaveHistory(m.history)
		m.updateHistoryViewport()
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
	}

	return m, nil
}

// updateHistoryViewport rebuilds the viewport content from the history slice.
// Uses a pointer receiver so it can be called on the addressable local copy
// in Update(); the mutations are included in the returned model value.
func (m *Model) updateHistoryViewport() {
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
	historyHeight := max(m.height-headerHeight()-activeHeight(len(m.active))-footerHeight()-4, 3)
	m.historyVP.Width = innerWidth
	m.historyVP.Height = historyHeight
	// Scroll to top (newest) on update.
	m.historyVP.GotoTop()
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
	timerStr := dimStyle.Render(timer)

	status := ""
	if m.statusLine != "" {
		status = "  " + dimStyle.Render(m.statusLine)
	}

	// title + status on left, timer on right, single line
	left := title + status
	leftWidth := lipgloss.Width(left)
	timerWidth := lipgloss.Width(timerStr)
	available := m.width - 1 // leading space
	if leftWidth+timerWidth+1 > available {
		// Truncate status to fit
		maxStatus := max(available-lipgloss.Width(title)-timerWidth-3, 0)
		if maxStatus > 0 && m.statusLine != "" {
			s := m.statusLine
			if runes := []rune(s); len(runes) > maxStatus {
				s = string(runes[:maxStatus]) + "…"
			}
			status = "  " + dimStyle.Render(s)
		} else {
			status = ""
		}
		left = title + status
		leftWidth = lipgloss.Width(left)
	}
	gap := max(m.width-leftWidth-timerWidth-4, 1)
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
		lines = lines[start : start+maxLines]
		if start > 0 || start+maxLines < len(keys) {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … %d more", len(keys)-maxLines)))
		}
	}

	hint := ""
	if m.focusPane == paneActive && len(m.active) > 0 {
		hint = dimStyle.Render("  [r/enter/l]ogs  [tab] history")
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
	} else if m.focusPane == paneHistory && len(m.history) > 0 {
		hint = dimStyle.Render("  [r]esume  [l]ogs  [c]lear entry  [C]lear all  [tab] in-progress")
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
	return min(max(n+1, 2)+2, 10) // title + one line per job (min 2) + border, max 10
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

// logDirForJob returns the log directory for an active job.
func logDirForJob(job *activeJob) string {
	return logDirForIssue(job.Repo, job.IssueNumber)
}

// logDirForHistory returns the log directory for a history entry.
func logDirForHistory(h HistoryEntry) string {
	return logDirForIssue(h.Repo, h.IssueNumber)
}

// logDirForIssue returns ~/.fabrik/logs/<owner>-<repo>/issue-N/ in multi-repo
// mode, or ~/.fabrik/logs/issue-N/ in single-repo mode (empty repo).
func logDirForIssue(repo string, issueNumber int) string {
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	if repo == "" {
		return fmt.Sprintf("%s/.fabrik/logs/%s", homeDir(), issuePart)
	}
	// Sanitize "owner/repo" → "owner-repo" for use as a directory name.
	repoPart := strings.ReplaceAll(repo, "/", "-")
	return fmt.Sprintf("%s/.fabrik/logs/%s/%s", homeDir(), repoPart, issuePart)
}

// openLogViewerCmd returns a tea.Cmd that opens a terminal showing the most
// recent log file in the given directory, piped through the stream filter
// for human-readable output.
func (m Model) openLogViewerCmd(logDir string) tea.Cmd {
	if _, err := os.Stat(logDir); err != nil {
		return nil
	}
	entries, err := os.ReadDir(logDir)
	if err != nil || len(entries) == 0 {
		return nil
	}
	// Sort by modification time (most recent last) — filenames sort
	// alphabetically by stage name, not by time.
	sort.Slice(entries, func(i, j int) bool {
		fi, _ := entries[i].Info()
		fj, _ := entries[j].Info()
		if fi == nil || fj == nil {
			return entries[i].Name() < entries[j].Name()
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	latest := entries[len(entries)-1].Name()

	// Resolve fabrik binary for the stream filter
	fabrikBin, err := os.Executable()
	if err != nil {
		fabrikBin = "fabrik"
	}

	// For JSON files, pipe through the stream filter; for other files, use less.
	// Pass as a single string — openTerminalCmd sends it to Terminal.app's do script.
	// Shell-quote path components so directories or binaries with spaces work correctly.
	if strings.HasSuffix(latest, ".json") {
		return m.openTerminalCmd(fmt.Sprintf(
			"cd %s && cat %s | %s _stream-filter | less -R",
			shellQuote(logDir), shellQuote(latest), shellQuote(fabrikBin)))
	}
	return m.openTerminalCmd(fmt.Sprintf("cd %s && less +F %s", shellQuote(logDir), shellQuote(latest)))
}

// shellQuote wraps s in single quotes and escapes any embedded single quotes,
// making it safe to embed in a shell command string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tuiReadSessionID reads the Claude session ID for a given issue and stage.
// The path logic mirrors engine.ReadSessionID — keep in sync if either changes.
func tuiReadSessionID(issueNumber int, stageName string) string {
	home, _ := os.UserHomeDir()
	base := filepath.Base(stageName)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "default"
	}
	path := filepath.Join(home, ".fabrik", "sessions",
		fmt.Sprintf("issue-%d", issueNumber), base+".session")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// openResumeCmd returns a tea.Cmd that opens a new terminal window running an
// interactive Claude session in the issue's worktree. If a session file exists
// for the given stage, --resume <id> is passed; otherwise a fresh session starts.
// If the worktree directory does not exist, an error terminal window is opened.
func (m Model) openResumeCmd(issueNumber int, stageName, stageModel string) tea.Cmd {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	worktreeDir := filepath.Join(cwd, ".fabrik", "worktrees", fmt.Sprintf("issue-%d", issueNumber))
	if _, statErr := os.Stat(worktreeDir); statErr != nil {
		if os.IsNotExist(statErr) {
			return m.openTerminalCmd(fmt.Sprintf("echo 'Worktree for issue #%d not found (%s). The issue has not been processed yet.' && read", issueNumber, worktreeDir))
		}
		return m.openTerminalCmd(fmt.Sprintf("echo 'Failed to access worktree for issue #%d (%s): %v' && read", issueNumber, worktreeDir, statErr))
	}

	// Build the command with properly shell-quoted arguments to handle paths
	// with spaces or shell metacharacters.
	parts := []string{"claude"}
	sessionID := tuiReadSessionID(issueNumber, stageName)
	if sessionID != "" {
		parts = append(parts, "--resume", shellQuote(sessionID))
	}
	if stageModel != "" {
		parts = append(parts, "--model", shellQuote(stageModel))
	}
	if m.pluginDir != "" {
		parts = append(parts, "--plugin-dir", shellQuote(m.pluginDir))
	}

	// Build: cd <worktreeDir> && claude [--resume <id>] [--model <m>] [--plugin-dir <d>]
	cmdStr := fmt.Sprintf("cd %s && %s", shellQuote(worktreeDir), strings.Join(parts, " "))
	return m.openTerminalCmd(cmdStr)
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

// homeDir returns the user's home directory.
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// openTerminalCmd returns a tea.Cmd that opens a new terminal window running
// cmdStr as a shell command. It dispatches to the terminal specified by
// m.terminal; an empty string or unknown ID falls back to the platform default.
func (m Model) openTerminalCmd(cmdStr string) tea.Cmd {
	return func() tea.Msg {
		var cmd *exec.Cmd

		switch m.terminal {
		case "iterm2":
			// iTerm2 v3+ AppleScript API; macOS only. Fall back to Linux default on other platforms.
			if runtime.GOOS == "darwin" {
				script := fmt.Sprintf(`tell application "iTerm"
	activate
	create window with default profile command "%s"
end tell`, strings.ReplaceAll(cmdStr, `"`, `\"`))
				cmd = exec.Command("osascript", "-e", script)
			} else {
				cmd = linuxFallbackTerminal(cmdStr)
			}

		case "ghostty":
			if runtime.GOOS == "darwin" {
				// Ghostty binary cannot launch the GUI directly on macOS; use open.
				cmd = exec.Command("open", "-na", "Ghostty.app", "--args", "-e", "sh", "-c", cmdStr)
			} else {
				cmd = exec.Command("ghostty", "-e", "sh", "-c", cmdStr)
			}

		case "kitty":
			cmd = exec.Command("kitty", "sh", "-c", cmdStr)

		case "alacritty":
			cmd = exec.Command("alacritty", "-e", "sh", "-c", cmdStr)

		case "warp":
			// Warp does not support passing a command via -e; macOS only.
			if runtime.GOOS == "darwin" {
				fmt.Fprintf(os.Stderr, "[warn] Warp terminal does not support opening with a command; log viewer unavailable\n")
				cmd = exec.Command("open", "-a", "Warp")
			} else {
				cmd = linuxFallbackTerminal(cmdStr)
			}

		case "terminal", "":
			// Terminal.app on macOS or platform default on Linux.
			if runtime.GOOS == "darwin" {
				script := fmt.Sprintf(`tell application "Terminal"
activate
do script "%s"
end tell`, strings.ReplaceAll(cmdStr, `"`, `\"`))
				cmd = exec.Command("osascript", "-e", script)
			} else {
				cmd = linuxFallbackTerminal(cmdStr)
			}

		default:
			// Unknown terminal ID — warn and fall through to platform default.
			fmt.Fprintf(os.Stderr, "[warn] unknown terminal %q; falling back to platform default\n", m.terminal)
			if runtime.GOOS == "darwin" {
				script := fmt.Sprintf(`tell application "Terminal"
activate
do script "%s"
end tell`, strings.ReplaceAll(cmdStr, `"`, `\"`))
				cmd = exec.Command("osascript", "-e", script)
			} else {
				cmd = linuxFallbackTerminal(cmdStr)
			}
		}

		if cmd == nil {
			return nil
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to launch terminal %q: %v\n", m.terminal, err)
		}
		return nil
	}
}

// linuxFallbackTerminal returns an *exec.Cmd for the first available terminal
// emulator found in PATH (gnome-terminal, xterm, konsole). Returns nil if none found.
func linuxFallbackTerminal(cmdStr string) *exec.Cmd {
	for _, term := range []string{"gnome-terminal", "xterm", "konsole"} {
		if _, err := exec.LookPath(term); err == nil {
			if term == "gnome-terminal" {
				return exec.Command(term, "--", "sh", "-c", cmdStr)
			}
			return exec.Command(term, "-e", "sh", "-c", cmdStr)
		}
	}
	return nil
}
