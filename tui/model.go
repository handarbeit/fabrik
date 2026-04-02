package tui

import (
	"fmt"
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
	StageName   string
	Success     bool
	Duration    time.Duration
	CompletedAt time.Time
}

// activeJob tracks an in-flight worker.
type activeJob struct {
	StageName  string
	StartedAt  time.Time
	LastTag    string
	LastLine   string
}

// Model is the bubbletea TUI model for Fabrik.
type Model struct {
	// poll timer
	pollInterval  time.Duration
	nextPollAt    time.Time
	pollCount     int

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
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)

	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// New creates an initial TUI model.
// pollSeconds is the configured polling interval.
func New(pollSeconds int) Model {
	vp := viewport.New(80, 10)
	return Model{
		pollInterval:  time.Duration(pollSeconds) * time.Second,
		active:        make(map[int]*activeJob),
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
			return m, tea.Quit
		}
		// Forward key events to the history viewport for scrolling
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
			StageName: ev.StageName,
			StartedAt: ev.StartedAt,
		}
		return m, nil

	case JobCompletedEvent:
		delete(m.active, ev.IssueNumber)
		entry := HistoryEntry{
			IssueNumber: ev.IssueNumber,
			StageName:   ev.StageName,
			Success:     ev.Success,
			Duration:    ev.Duration,
			CompletedAt: ev.CompletedAt,
		}
		m.history = append(m.history, entry)
		m.updateHistoryViewport()
		return m, nil

	case LogEvent:
		// Update the last-seen tag/line for the active job, if any
		if job, ok := m.active[ev.IssueNumber]; ok {
			job.LastTag = ev.Tag
			// Trim trailing newline for display
			job.LastLine = strings.TrimRight(ev.Message, "\n")
		}
		return m, nil
	}

	return m, nil
}

// updateHistoryViewport rebuilds the viewport content from the history slice.
func (m *Model) updateHistoryViewport() {
	innerWidth := m.width - 6 // account for border + padding
	if innerWidth < 20 {
		innerWidth = 20
	}

	var lines []string
	// Show newest entries at the top.
	for i := len(m.history) - 1; i >= 0; i-- {
		h := m.history[i]
		status := successStyle.Render("✓")
		result := ""
		if !h.Success {
			status = failStyle.Render("✗")
			result = dimStyle.Render("  (failed)")
		}
		ts := dimStyle.Render(h.CompletedAt.Format("2006-01-02 15:04"))
		dur := fmtDuration(h.Duration)
		line := fmt.Sprintf("#%-5d %-12s %s %s  %s%s",
			h.IssueNumber, h.StageName, status, dur, ts, result)
		lines = append(lines, line)
	}
	m.historyVP.SetContent(strings.Join(lines, "\n"))

	// Update viewport height within the overall layout.
	historyHeight := m.height - headerHeight() - activeHeight(len(m.active)) - 4
	if historyHeight < 3 {
		historyHeight = 3
	}
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
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(timerStr) - 6
	if gap < 1 {
		gap = 1
	}
	content := title + strings.Repeat(" ", gap) + timerStr
	return borderStyle.Width(m.width - 4).Render(content)
}

func (m Model) viewActive() string {
	title := activeStyle.Render(fmt.Sprintf("In Progress (%d)", len(m.active)))

	var lines []string
	// Sort by issue number for stable display.
	nums := make([]int, 0, len(m.active))
	for n := range m.active {
		nums = append(nums, n)
	}
	sort.Ints(nums)

	spinner := m.spinnerFrames[m.spinnerIdx]
	for _, num := range nums {
		job := m.active[num]
		elapsed := fmtDuration(m.now.Sub(job.StartedAt))
		tag := ""
		if job.LastTag != "" {
			tag = dimStyle.Render(fmt.Sprintf("[%s]", job.LastTag))
		}
		msg := ""
		if job.LastLine != "" {
			// Truncate long lines to avoid wrapping
			maxMsg := m.width - 35
			if maxMsg < 0 {
				maxMsg = 0
			}
			msg = job.LastLine
			if runes := []rune(msg); len(runes) > maxMsg {
				msg = string(runes[:maxMsg]) + "…"
			}
		}
		line := fmt.Sprintf("#%-5d %-12s %s %s  %s %s",
			num, job.StageName, spinner, elapsed, tag, msg)
		lines = append(lines, line)
	}

	content := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(m.width - 4).Render(content)
}

func (m Model) viewHistory() string {
	title := dimStyle.Render(fmt.Sprintf("History (%d)", len(m.history)))
	content := title + "\n" + m.historyVP.View()
	return borderStyle.Width(m.width - 4).Render(content)
}

// headerHeight returns the approximate line height of the header pane.
func headerHeight() int {
	return 3 // border top + 1 content line + border bottom
}

// activeHeight returns the approximate line height of the active pane.
func activeHeight(n int) int {
	lines := n + 1 // title + one line per job
	if lines < 2 {
		lines = 2
	}
	return lines + 2 // +2 for border
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
