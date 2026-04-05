package tui

import (
	"fmt"
	"os"
	"os/exec"
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
	Title       string
	StageName   string
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
	Title     string
	StageName string
	IsComment bool
	StartedAt time.Time
	LastTag   string
	LastLine  string
}

// pane identifies which TUI section has focus.
type pane int

const (
	paneActive pane = iota
	paneHistory
)

// Model is the bubbletea TUI model for Fabrik.
type Model struct {
	// poll timer
	pollInterval time.Duration
	nextPollAt   time.Time
	pollCount    int

	// active jobs (keyed by issue number)
	active map[int]*activeJob

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

	// statusLine shows the latest poll-level log message in the header
	statusLine string

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
func New(pollSeconds int) Model {
	vp := viewport.New(80, 10)
	return Model{
		pollInterval:  time.Duration(pollSeconds) * time.Second,
		active:        make(map[int]*activeJob),
		history:       LoadHistory(),
		historyVP:     vp,
		spinnerFrames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		now:           time.Now(),
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
				nums := m.sortedActiveNums()
				if m.activeIdx < len(nums)-1 {
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
				nums := m.sortedActiveNums()
				if m.activeIdx < len(nums) {
					num := nums[m.activeIdx]
					if _, ok := m.active[num]; ok {
						logDir := fmt.Sprintf("%s/.fabrik/logs/issue-%d", homeDir(), num)
						return m, openLogViewerCmd(logDir)
					}
				}
			} else if m.focusPane == paneHistory && len(m.history) > 0 {
				realIdx := len(m.history) - 1 - m.histIdx
				if realIdx >= 0 && realIdx < len(m.history) {
					h := m.history[realIdx]
					logDir := fmt.Sprintf("%s/.fabrik/logs/issue-%d", homeDir(), h.IssueNumber)
					return m, openLogViewerCmd(logDir)
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
		m.active[ev.IssueNumber] = &activeJob{
			Title:     ev.Title,
			StageName: ev.StageName,
			IsComment: ev.IsComment,
			StartedAt: ev.StartedAt,
		}
		return m, nil

	case JobCompletedEvent:
		delete(m.active, ev.IssueNumber)
		entry := HistoryEntry{
			IssueNumber: ev.IssueNumber,
			Title:       ev.Title,
			StageName:   ev.StageName,
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
		} else if job, ok := m.active[ev.IssueNumber]; ok {
			job.LastTag = ev.Tag
			job.LastLine = strings.TrimRight(ev.Message, "\n")
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
	historyHeight := max(m.height-headerHeight()-activeHeight(len(m.active))-4, 3)
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
	nums := m.sortedActiveNums()

	spinner := m.spinnerFrames[m.spinnerIdx]
	for idx, num := range nums {
		job := m.active[num]
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
			num, stageDisplay, spinner, elapsed, titleStr, tag, msg)
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

	hint := ""
	if m.focusPane == paneActive && len(m.active) > 0 {
		hint = dimStyle.Render("  [enter/l]ogs  [tab] history")
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
		hint = dimStyle.Render("  [l]ogs  [c]lear entry  [C]lear all  [tab] in-progress")
	}
	content := title + hint + "\n" + m.historyVP.View()
	return borderStyle.Width(m.width - 4).Render(content)
}

// headerHeight returns the approximate line height of the header pane.
func headerHeight() int {
	return 1
}

// activeHeight returns the approximate line height of the active pane.
func activeHeight(n int) int {
	return max(n+1, 2) + 2 // title + one line per job (min 2) + border
}

// sortedActiveNums returns issue numbers from the active map in sorted order.
func (m Model) sortedActiveNums() []int {
	nums := make([]int, 0, len(m.active))
	for n := range m.active {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

// openLogViewerCmd returns a tea.Cmd that opens a terminal showing the most
// recent log file in the given directory, piped through the stream filter
// for human-readable output.
func openLogViewerCmd(logDir string) tea.Cmd {
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
	if strings.HasSuffix(latest, ".json") {
		return openTerminalCmd(fmt.Sprintf(
			"cd %s && cat %s | %s _stream-filter | less -R",
			logDir, latest, fabrikBin))
	}
	return openTerminalCmd(fmt.Sprintf("cd %s && less +F %s", logDir, latest))
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
// the given command. On macOS it uses osascript to open Terminal.app; on Linux
// it tries common terminal emulators.
func openTerminalCmd(name string, args ...string) tea.Cmd {
	return func() tea.Msg {
		cmdStr := name
		for _, a := range args {
			cmdStr += " " + a
		}

		var cmd *exec.Cmd
		if runtime.GOOS == "darwin" {
			script := fmt.Sprintf(`tell application "Terminal"
activate
do script "%s"
end tell`, strings.ReplaceAll(cmdStr, `"`, `\"`))
			cmd = exec.Command("osascript", "-e", script)
		} else {
			// Try common Linux terminals
			for _, term := range []string{"gnome-terminal", "xterm", "konsole"} {
				if _, err := exec.LookPath(term); err == nil {
					if term == "gnome-terminal" {
						cmd = exec.Command(term, "--", "sh", "-c", cmdStr)
					} else {
						cmd = exec.Command(term, "-e", cmdStr)
					}
					break
				}
			}
			if cmd == nil {
				return nil // no terminal found
			}
		}
		_ = cmd.Start()
		return nil
	}
}
