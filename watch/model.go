package watch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/tui"
)

// GitHubOptions holds the GitHub API configuration for the watch TUI.
type GitHubOptions struct {
	Owner        string
	Repo         string
	Client       *gh.Client
	PollInterval time.Duration
	Terminal     string
}

// issueState holds the latest GitHub-fetched state for the watched issue.
type issueState struct {
	title      string
	state      string
	labels     []string
	prNumber   int
	prState    string
	prMerged   bool
	prDraft    bool
	prHeadSHA  string
	checkRuns  []gh.CheckRun
	commentCnt int // number of comments (fetched separately)
}

// WatchModel is the bubbletea model for the fabrik watch TUI.
type WatchModel struct {
	issueNumber int
	opts        GitHubOptions
	logDir      string

	// UI state
	width  int
	height int
	vp     viewport.Model // scrollable live-output viewport
	lines  []string       // rendered lines from log follower

	// issue/PR state from GitHub
	github issueState
	// timestamp of last GitHub fetch
	lastPollAt time.Time
	pollErr    string

	// stage history for this issue from history.json
	history []tui.HistoryEntry

	// done channel — closed on quit to stop background goroutines
	done chan struct{}
}

// NewModel creates a WatchModel for the given issue number.
func NewModel(issueNumber int, opts GitHubOptions) WatchModel {
	logDir := issueLogDir(issueNumber)
	vp := viewport.New(80, 20)
	vp.SetContent("")
	return WatchModel{
		issueNumber: issueNumber,
		opts:        opts,
		logDir:      logDir,
		vp:          vp,
		done:        make(chan struct{}),
	}
}

// issueLogDir returns ~/.fabrik/logs/issue-N.
func issueLogDir(issueNumber int) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "logs", fmt.Sprintf("issue-%d", issueNumber))
}

// sessionDir returns ~/.fabrik/sessions/issue-N.
func sessionDir(issueNumber int) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "sessions", fmt.Sprintf("issue-%d", issueNumber))
}

// worktreeDir returns .fabrik/worktrees/issue-N relative to CWD.
func worktreeDir(issueNumber int) string {
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".fabrik", "worktrees", fmt.Sprintf("issue-%d", issueNumber))
}

// currentStageFromLog returns the stage name derived from the newest .log filename
// in logDir, or "" if no log files exist.
func currentStageFromLog(logDir string) string {
	path := newestLogFile(logDir)
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	// Format: <safeLabel>-<yyyyMMdd-HHmmss>-<nanos>.log
	// safeLabel is stage.Name with / \ : space replaced by -
	// First segment before the first hyphen-that-starts-a-date is the label.
	// Simplest heuristic: take everything before the first date-like segment.
	parts := strings.Split(strings.TrimSuffix(base, ".log"), "-")
	var label []string
	for _, p := range parts {
		// Date parts are all digits; stop when we hit one of length 8 (yyyyMMdd).
		if len(p) == 8 && isDigits(p) {
			break
		}
		label = append(label, p)
	}
	return strings.Join(label, "-")
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// readSessionID returns the session ID for the current stage, or "" if not found.
func readSessionID(issueNumber int, stageName string) string {
	if stageName == "" {
		return ""
	}
	path := filepath.Join(sessionDir(issueNumber), stageName+".session")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Init starts the GitHub poll ticker and the one-second UI refresh ticker.
// The log follower goroutines are launched in Run() via p.Send, which is the
// correct bubbletea pattern for goroutines that need to send messages back.
func (m WatchModel) Init() tea.Cmd {
	return tea.Batch(
		pollGitHub(),
		tickCmd(),
	)
}

// Run runs the bubbletea program with the WatchModel.
// It injects p.Send into the log follower goroutines before starting.
func Run(m WatchModel) error {
	p := tea.NewProgram(m, tea.WithAltScreen())

	// Start background goroutines that send messages via p.Send.
	StartLogFollower(m.logDir, func(msg tea.Msg) { p.Send(msg) }, m.done)

	_, err := p.Run()
	return err
}

func pollGitHub() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return GitHubPollMsg{}
	})
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return TickMsg{}
	})
}

func nextGitHubPoll(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return GitHubPollMsg{}
	})
}

// Update handles all bubbletea messages.
func (m WatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch ev := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = ev.Width
		m.height = ev.Height
		m.vp.Width = ev.Width
		m.vp.Height = m.viewportHeight()
		return m, nil

	case tea.KeyMsg:
		switch ev.String() {
		case "ctrl+c", "q":
			close(m.done)
			return m, tea.Quit

		case "up", "k":
			m.vp.ScrollUp(1)
			return m, nil

		case "down", "j":
			m.vp.ScrollDown(1)
			return m, nil

		case "G":
			m.vp.GotoBottom()
			return m, nil

		case "g":
			m.vp.GotoTop()
			return m, nil

		case "i":
			return m, m.openClaudeCmd()
		}

	case LogLineMsg:
		m.lines = append(m.lines, ev.Text)
		m.vp.SetContent(strings.Join(m.lines, ""))
		m.vp.GotoBottom()
		return m, nil

	case NewLogFileMsg:
		// Stage transition: clear the viewport and start fresh for new stage.
		m.lines = nil
		m.vp.SetContent("")
		return m, nil

	case GitHubPollMsg:
		m.fetchGitHub()
		return m, nextGitHubPoll(m.opts.PollInterval)

	case TickMsg:
		return m, tickCmd()
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// fetchGitHub refreshes GitHub state synchronously. For a TUI this runs on the
// main goroutine; the operations are fast REST calls and the poll interval is
// long (30s default), so blocking is acceptable.
func (m *WatchModel) fetchGitHub() {
	// Always refresh history — it doesn't require a GitHub client.
	m.history = issueHistory(m.issueNumber)

	if m.opts.Client == nil || m.opts.Owner == "" || m.opts.Repo == "" {
		return
	}
	issue, err := m.opts.Client.FetchIssue(m.opts.Owner, m.opts.Repo, m.issueNumber)
	if err != nil {
		m.pollErr = fmt.Sprintf("GitHub: %v", err)
		return
	}
	m.pollErr = ""
	m.github.title = issue.Title
	m.github.state = issue.State
	m.github.labels = issue.Labels
	m.commentCnt = issue.Comments

	// Discover the linked PR by convention (branch fabrik/issue-N).
	// Cache the PR number once found; re-fetch details on each poll.
	if m.github.prNumber == 0 {
		pr, err := m.opts.Client.FetchLinkedPR(m.opts.Owner, m.opts.Repo, m.issueNumber)
		if err == nil && pr != nil {
			m.github.prNumber = pr.Number
			m.github.prState = pr.State
			m.github.prMerged = pr.Merged
			m.github.prDraft = pr.Draft
			m.github.prHeadSHA = pr.HeadSHA
		}
	} else {
		pr, err := m.opts.Client.FetchPRDetails(m.opts.Owner, m.opts.Repo, m.github.prNumber)
		if err == nil {
			m.github.prState = pr.State
			m.github.prMerged = pr.Merged
			m.github.prDraft = pr.Draft
			m.github.prHeadSHA = pr.HeadSHA
		}
	}

	if m.github.prHeadSHA != "" {
		runs, err := m.opts.Client.FetchCheckRuns(m.opts.Owner, m.opts.Repo, m.github.prHeadSHA)
		if err == nil {
			m.github.checkRuns = runs
		}
	}

	m.lastPollAt = time.Now()
}

// issueHistory loads history entries for a specific issue.
func issueHistory(issueNumber int) []tui.HistoryEntry {
	all := tui.LoadHistory()
	var out []tui.HistoryEntry
	for _, e := range all {
		if e.IssueNumber == issueNumber {
			out = append(out, e)
		}
	}
	return out
}

// viewportHeight returns the number of lines available for the live-output viewport.
func (m WatchModel) viewportHeight() int {
	// Header: ~3 lines, status bar: ~1 line, history: ~5 lines, padding: 2
	reserved := 11
	if m.height > reserved+5 {
		return m.height - reserved
	}
	return 5
}

// View renders the TUI.
func (m WatchModel) View() string {
	var b strings.Builder

	// ── Header ──
	title := m.github.title
	if title == "" {
		title = fmt.Sprintf("issue #%d", m.issueNumber)
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("  fabrik watch  #%d: %s", m.issueNumber, title)))
	b.WriteString("\n")

	// Labels row
	if len(m.github.labels) > 0 {
		b.WriteString(dimStyle.Render("  " + strings.Join(m.github.labels, "  ")))
		b.WriteString("\n")
	}

	// PR/CI row
	b.WriteString(m.prStatusLine())
	b.WriteString("\n\n")

	// ── Live output viewport ──
	b.WriteString(sectionStyle.Render("Live output"))
	b.WriteString("\n")
	b.WriteString(m.vp.View())
	b.WriteString("\n\n")

	// ── Stage history ──
	b.WriteString(sectionStyle.Render("Stage history"))
	b.WriteString("\n")
	if len(m.history) == 0 {
		b.WriteString(dimStyle.Render("  (no history — run engine with --tui to record)"))
	} else {
		// Show last 5 entries
		start := len(m.history) - 5
		if start < 0 {
			start = 0
		}
		for _, e := range m.history[start:] {
			status := successStyle.Render("✓")
			if !e.Success {
				status = failStyle.Render("✗")
			}
			b.WriteString(fmt.Sprintf("  %s  %-12s  %s  $%.4f\n",
				status, e.StageName, e.Duration.Round(time.Second), e.CostUSD))
		}
	}
	b.WriteString("\n")

	// ── Status bar ──
	b.WriteString(m.statusBar())

	return b.String()
}

// prStatusLine renders the PR and CI check summary.
func (m WatchModel) prStatusLine() string {
	if m.github.prNumber == 0 {
		if m.opts.Owner == "" {
			return dimStyle.Render("  PR: (no --owner/--repo configured)")
		}
		return dimStyle.Render("  PR: (none linked)")
	}
	prLabel := fmt.Sprintf("PR #%d", m.github.prNumber)
	if m.github.prMerged {
		prLabel += " ✓ merged"
	} else if m.github.prState == "closed" {
		prLabel += " closed"
	} else if m.github.prDraft {
		prLabel += " (draft)"
	} else {
		prLabel += " open"
	}

	checks := m.checkRunSummary()
	if checks != "" {
		return dimStyle.Render("  " + prLabel + "  |  " + checks)
	}
	return dimStyle.Render("  " + prLabel)
}

// checkRunSummary returns a compact CI status string like "✓4 ✗1 ⏳2".
func (m WatchModel) checkRunSummary() string {
	if len(m.github.checkRuns) == 0 {
		return ""
	}
	pass, fail, running := 0, 0, 0
	for _, cr := range m.github.checkRuns {
		switch cr.Status {
		case "completed":
			if cr.Conclusion == "success" || cr.Conclusion == "neutral" || cr.Conclusion == "skipped" {
				pass++
			} else {
				fail++
			}
		default:
			running++
		}
	}
	var parts []string
	if pass > 0 {
		parts = append(parts, successStyle.Render(fmt.Sprintf("✓%d", pass)))
	}
	if fail > 0 {
		parts = append(parts, failStyle.Render(fmt.Sprintf("✗%d", fail)))
	}
	if running > 0 {
		parts = append(parts, activeStyle.Render(fmt.Sprintf("⏳%d", running)))
	}
	return strings.Join(parts, " ")
}

// statusBar renders the bottom status bar.
func (m WatchModel) statusBar() string {
	keys := dimStyle.Render("q quit  ↑↓ scroll  G bottom  g top  i open claude")
	var pollInfo string
	if !m.lastPollAt.IsZero() {
		ago := time.Since(m.lastPollAt).Round(time.Second)
		pollInfo = dimStyle.Render(fmt.Sprintf("  polled %s ago", ago))
	}
	if m.pollErr != "" {
		pollInfo = failStyle.Render("  " + m.pollErr)
	}
	return keys + pollInfo
}

// openClaudeCmd returns a tea.Cmd that opens a new terminal window running
// claude --resume SESSION_ID in the issue's worktree.
func (m WatchModel) openClaudeCmd() tea.Cmd {
	return func() tea.Msg {
		stageName := currentStageFromLog(m.logDir)
		sessionID := readSessionID(m.issueNumber, stageName)
		wt := worktreeDir(m.issueNumber)

		var claudeArgs string
		if sessionID != "" {
			claudeArgs = fmt.Sprintf("--resume %s", shellQuote(sessionID))
		}
		cmdStr := fmt.Sprintf("cd %s && claude %s", shellQuote(wt), claudeArgs)

		openTerminal(m.opts.Terminal, cmdStr)
		return nil
	}
}

// openTerminal opens a new terminal window running cmdStr.
func openTerminal(terminal, cmdStr string) {
	var cmd *exec.Cmd
	switch terminal {
	case "iterm2":
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
			cmd = exec.Command("open", "-na", "Ghostty.app", "--args", "-e", "sh", "-c", cmdStr)
		} else {
			cmd = exec.Command("ghostty", "-e", "sh", "-c", cmdStr)
		}
	case "kitty":
		cmd = exec.Command("kitty", "sh", "-c", cmdStr)
	case "alacritty":
		cmd = exec.Command("alacritty", "-e", "sh", "-c", cmdStr)
	case "warp":
		if runtime.GOOS == "darwin" {
			cmd = exec.Command("open", "-a", "Warp")
		} else {
			cmd = linuxFallbackTerminal(cmdStr)
		}
	default:
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
	if cmd != nil {
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to launch terminal: %v\n", err)
		}
	}
}

// linuxFallbackTerminal returns an exec.Cmd for the first available terminal emulator.
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

// shellQuote wraps s in single quotes and escapes embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Styles
var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)
