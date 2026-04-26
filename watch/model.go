package watch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// GitHubOptions holds the GitHub API configuration for the watch TUI.
type GitHubOptions struct {
	Owner        string
	Repo         string
	Client       *gh.Client
	PollInterval time.Duration
	PluginDir    string
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
	pluginDir   string

	// UI state
	width          int
	height         int
	vp             viewport.Model // scrollable live-output viewport
	lines          []string       // rendered lines from log follower (live stage buffer)
	currentLogPath string         // log path of the currently displayed historical tab (empty on live tab)

	// Tab bar state
	stageTabs      []stageTab
	selectedTabIdx int
	stageOrder     map[string]int // stage name -> pipeline order (from stages YAML)

	// Turn counter for the live stage (resets on new log file / invocation change)
	turnsUsed              int
	cachedEffectiveMaxTurns int            // cached result of effectiveMaxTurns(); updated on NewLogFileMsg/GitHubPollMsg
	stageMaxTurns          map[string]int  // stage name -> configured max_turns (0 = unlimited)

	// Transient status message (shown in status bar, cleared on TickMsg)
	statusMsg string

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
// stagesDir is the path to the stages YAML directory; used to build the
// pipeline order map for tab sorting. If stagesDir is empty or LoadAll fails,
// tabs fall back to chronological ordering.
func NewModel(issueNumber int, opts GitHubOptions, stagesDir string) WatchModel {
	logDir := issueLogDir(opts.Owner, opts.Repo, issueNumber)
	vp := viewport.New(80, 20)
	vp.SetContent("")

	stageOrder := buildStageOrder(stagesDir)
	stageMaxTurns := buildStageMaxTurns(stagesDir)
	tabs := buildStageTabs(logDir, stageOrder)
	selectedTabIdx := liveTabIdx(tabs)

	return WatchModel{
		issueNumber:    issueNumber,
		opts:           opts,
		logDir:         logDir,
		pluginDir:      opts.PluginDir,
		vp:             vp,
		stageTabs:      tabs,
		selectedTabIdx: selectedTabIdx,
		stageOrder:     stageOrder,
		stageMaxTurns:  stageMaxTurns,
		done:           make(chan struct{}),
	}
}

// buildStageOrder loads stage configs from stagesDir and returns a map of
// stage name → pipeline order value. Returns an empty map on any error,
// enabling graceful fallback to chronological tab ordering.
func buildStageOrder(stagesDir string) map[string]int {
	order := make(map[string]int)
	if stagesDir == "" {
		return order
	}
	loaded, err := stages.LoadAll(stagesDir)
	if err != nil {
		return order
	}
	for _, s := range loaded {
		order[s.Name] = s.Order
	}
	return order
}

// buildStageMaxTurns loads stage configs from stagesDir and returns a map of
// stage name → max_turns value. Returns an empty map on any error.
func buildStageMaxTurns(stagesDir string) map[string]int {
	m := make(map[string]int)
	if stagesDir == "" {
		return m
	}
	loaded, err := stages.LoadAll(stagesDir)
	if err != nil {
		return m
	}
	for _, s := range loaded {
		m[s.Name] = s.MaxTurns
	}
	return m
}

// logFileCountForStage counts .log files in logDir whose stage label matches stageName.
func logFileCountForStage(logDir, stageName string) int {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") &&
			stageNameFromFilename(e.Name()) == stageName {
			count++
		}
	}
	return count
}

// effectiveMaxTurns returns the effective turn budget for the currently live stage.
// Applies 2× multiplier when fabrik:extend-turns label is present and this is the
// first invocation for the stage (one log file = not yet in extension loop).
// Returns 0 when the stage has no configured limit or for comment-review invocations
// (which use their own smaller budget not tracked in stageMaxTurns).
func (m *WatchModel) effectiveMaxTurns() int {
	stageName := currentStageFromLog(m.logDir)
	if stageName == "" {
		return 0
	}
	// Comment-review invocations use comment_max_turns (not stage.MaxTurns); show turn N without denominator.
	if strings.HasSuffix(stageName, "-comment-review") {
		return 0
	}
	base, ok := m.stageMaxTurns[stageName]
	if !ok || base == 0 {
		return 0
	}
	for _, lbl := range m.github.labels {
		if lbl == "fabrik:extend-turns" {
			if logFileCountForStage(m.logDir, stageName) == 1 {
				return 2 * base
			}
			break
		}
	}
	return base
}

// liveTabIdx returns the index of the live tab in tabs, or 0 if none.
func liveTabIdx(tabs []stageTab) int {
	for i, t := range tabs {
		if t.IsLive {
			return i
		}
	}
	return 0
}

// mergeTabSelection rebuilds the tab list and preserves the selected tab by label.
// If the user was on the live tab (or the label no longer exists), returns the new live tab index.
func mergeTabSelection(newTabs []stageTab, oldTabs []stageTab, oldIdx int) int {
	// Determine whether the old selection was the live tab.
	wasLive := oldIdx >= len(oldTabs) || (len(oldTabs) > 0 && oldTabs[oldIdx].IsLive)
	if wasLive || oldIdx >= len(oldTabs) {
		return liveTabIdx(newTabs)
	}
	// Try to find the old label in the new list.
	oldLabel := oldTabs[oldIdx].Label
	for i, t := range newTabs {
		if t.Label == oldLabel {
			return i
		}
	}
	return liveTabIdx(newTabs)
}

// issueLogDir returns the log directory for an issue, namespaced by repo when
// owner and repo are non-empty (multi-repo mode).
//   - single-repo: <cwd>/.fabrik/logs/issue-N/
//   - multi-repo:  <cwd>/.fabrik/logs/<owner>-<repo>/issue-N/
func issueLogDir(owner, repo string, issueNumber int) string {
	cwd, _ := os.Getwd()
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	if owner == "" || repo == "" {
		return filepath.Join(cwd, ".fabrik", "logs", issuePart)
	}
	return filepath.Join(cwd, ".fabrik", "logs", owner+"-"+repo, issuePart)
}

// sessionDir returns the session directory for an issue, namespaced by repo when
// owner and repo are non-empty (multi-repo mode).
//   - single-repo: <cwd>/.fabrik/sessions/issue-N/
//   - multi-repo:  <cwd>/.fabrik/sessions/<owner>-<repo>/issue-N/
func sessionDir(owner, repo string, issueNumber int) string {
	cwd, _ := os.Getwd()
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	if owner == "" || repo == "" {
		return filepath.Join(cwd, ".fabrik", "sessions", issuePart)
	}
	return filepath.Join(cwd, ".fabrik", "sessions", owner+"-"+repo, issuePart)
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
	return stageNameFromFilename(filepath.Base(path))
}

// readSessionID returns the session ID for the current stage, or "" if not found.
func readSessionID(owner, repo string, issueNumber int, stageName string) string {
	if stageName == "" {
		return ""
	}
	path := filepath.Join(sessionDir(owner, repo, issueNumber), stageName+".session")
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

// wrapContent wraps s to fit within width columns, ANSI-escape-sequence-aware.
// Prefers word breaks (whitespace / hyphen); falls back to hard wrap for
// unbreakable tokens. Returns s unchanged when width < 1.
func wrapContent(s string, width int) string {
	if width < 1 {
		return s
	}
	return ansi.Wrap(s, width, "")
}

// switchToTab updates the viewport content when the user selects a different tab.
func (m *WatchModel) switchToTab(idx int) {
	if idx < 0 || idx >= len(m.stageTabs) {
		return
	}
	tab := m.stageTabs[idx]
	if tab.IsLive {
		m.currentLogPath = ""
		m.vp.SetContent(wrapContent(strings.Join(m.lines, ""), m.vp.Width))
		m.vp.GotoBottom()
	} else {
		m.currentLogPath = tab.LogPath
		m.vp.SetContent(wrapContent(renderLogFile(tab.LogPath), m.vp.Width))
	}
}

// Update handles all bubbletea messages.
func (m WatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch ev := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = ev.Width
		m.height = ev.Height
		m.vp.Width = ev.Width
		m.vp.Height = m.viewportHeight()
		// Re-wrap content at the new width.
		if m.currentLogPath != "" {
			m.vp.SetContent(wrapContent(renderLogFile(m.currentLogPath), m.vp.Width))
		} else {
			m.vp.SetContent(wrapContent(strings.Join(m.lines, ""), m.vp.Width))
		}
		return m, nil

	case tea.KeyMsg:
		switch ev.String() {
		case "ctrl+c", "q", "esc":
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

		case "left", "h":
			if m.selectedTabIdx > 0 {
				m.selectedTabIdx--
				m.switchToTab(m.selectedTabIdx)
			}
			return m, nil

		case "right", "l":
			if m.selectedTabIdx < len(m.stageTabs)-1 {
				m.selectedTabIdx++
				m.switchToTab(m.selectedTabIdx)
			}
			return m, nil

		case "i":
			// Guard: block if stage is currently active.
			stageName := currentStageFromLog(m.logDir)
			inProgressLabel := fmt.Sprintf("stage:%s:in_progress", stageName)
			for _, lbl := range m.github.labels {
				if lbl == inProgressLabel {
					return m, func() tea.Msg {
						return StatusMsgMsg{Text: "session is active — wait for stage to complete"}
					}
				}
			}
			return m, m.openClaudeInlineCmd()
		}

	case LogLineMsg:
		m.lines = append(m.lines, ev.Text)
		// Update viewport if live tab is selected, or if no tabs exist yet (startup race).
		isLive := len(m.stageTabs) == 0 || (m.selectedTabIdx < len(m.stageTabs) && m.stageTabs[m.selectedTabIdx].IsLive)
		if isLive {
			m.vp.SetContent(wrapContent(strings.Join(m.lines, ""), m.vp.Width))
			m.vp.GotoBottom()
		}
		return m, nil

	case TurnCountMsg:
		m.turnsUsed = ev.TurnsUsed
		return m, nil

	case NewLogFileMsg:
		// Stage transition: clear live buffer, rebuild tabs, move to live tab.
		m.lines = nil
		m.currentLogPath = ""
		m.vp.SetContent("")
		m.turnsUsed = 0
		newTabs := buildStageTabs(m.logDir, m.stageOrder)
		m.stageTabs = newTabs
		m.selectedTabIdx = liveTabIdx(newTabs)
		m.cachedEffectiveMaxTurns = m.effectiveMaxTurns()
		return m, nil

	case GitHubPollMsg:
		m.fetchGitHub()
		// Rebuild tabs; preserve selection by label.
		newTabs := buildStageTabs(m.logDir, m.stageOrder)
		m.selectedTabIdx = mergeTabSelection(newTabs, m.stageTabs, m.selectedTabIdx)
		m.stageTabs = newTabs
		m.cachedEffectiveMaxTurns = m.effectiveMaxTurns()
		return m, nextGitHubPoll(m.opts.PollInterval)

	case TickMsg:
		m.statusMsg = ""
		return m, tickCmd()

	case ClaudeFinishedMsg:
		// TUI has been restored automatically by tea.ExecProcess; nothing to do.
		return m, nil

	case StatusMsgMsg:
		m.statusMsg = ev.Text
		return m, nil

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
	m.github.commentCnt = issue.Comments

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
	// Overhead when labels row and tab bar are both shown (common runtime case):
	//   header + \n: 2, labels + \n: 2, prLine + \n: 2, tabBar + \n: 2,
	//   \n after viewport: 1, statusBar: 1 → 10 total.
	reserved := 10
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

	// PR/CI row (also shows live turn counter when a stage is active)
	prLine := m.prStatusLine()
	if m.github.commentCnt > 0 {
		prLine += dimStyle.Render(fmt.Sprintf("  |  %d comments", m.github.commentCnt))
	}
	isLive := len(m.stageTabs) == 0 || (m.selectedTabIdx < len(m.stageTabs) && m.stageTabs[m.selectedTabIdx].IsLive)
	if isLive && m.turnsUsed > 0 {
		if m.cachedEffectiveMaxTurns > 0 {
			prLine += dimStyle.Render(fmt.Sprintf("  |  turn %d/%d", m.turnsUsed, m.cachedEffectiveMaxTurns))
		} else {
			prLine += dimStyle.Render(fmt.Sprintf("  |  turn %d", m.turnsUsed))
		}
	}
	b.WriteString(prLine)
	b.WriteString("\n")

	// ── Tab bar ──
	if len(m.stageTabs) > 0 {
		b.WriteString(m.tabBar())
		b.WriteString("\n")
	}

	// ── Viewport ──
	b.WriteString(m.vp.View())
	b.WriteString("\n")

	// ── Status bar ──
	b.WriteString(m.statusBar())

	return b.String()
}

// tabBar renders the stage tab bar.
// Live (currently-running) tabs are shown in orange; the selected tab is bold.
// Four cases: live+selected → bold orange, live+not-selected → orange,
// not-live+selected → bold blue, not-live+not-selected → dim.
func (m WatchModel) tabBar() string {
	var parts []string
	for i, t := range m.stageTabs {
		label := t.Label
		if t.IsLive {
			label = "● " + label
		}
		tab := fmt.Sprintf("[ %s ]", label)
		isSelected := i == m.selectedTabIdx
		var style lipgloss.Style
		switch {
		case t.IsLive && isSelected:
			style = activeSectionStyle
		case t.IsLive:
			style = activeStyle
		case isSelected:
			style = sectionStyle
		default:
			style = dimStyle
		}
		parts = append(parts, style.Render(tab))
	}
	return strings.Join(parts, " ")
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
	keys := dimStyle.Render("q quit  ↑↓ scroll  ←→ tabs  G bottom  g top  i resume claude")

	// Session ID for current stage.
	sessionID := readSessionID(m.opts.Owner, m.opts.Repo, m.issueNumber, currentStageFromLog(m.logDir))
	if sessionID != "" {
		keys += dimStyle.Render("  |  session: " + sessionID)
	}

	// Transient status message takes precedence over poll info.
	if m.statusMsg != "" {
		return keys + "  " + failStyle.Render(m.statusMsg)
	}

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

// openClaudeInlineCmd returns a tea.Cmd that suspends the TUI and launches
// claude --resume <session-id> inline in the current terminal via tea.ExecProcess.
// When Claude exits, the TUI resumes and ClaudeFinishedMsg is sent.
func (m WatchModel) openClaudeInlineCmd() tea.Cmd {
	stageName := currentStageFromLog(m.logDir)
	sessionID := readSessionID(m.opts.Owner, m.opts.Repo, m.issueNumber, stageName)
	wt := worktreeDir(m.issueNumber)

	var args []string
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if m.pluginDir != "" {
		args = append(args, "--plugin-dir", m.pluginDir)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = wt

	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return ClaudeFinishedMsg{Err: err}
	})
}

// Styles
var (
	headerStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	sectionStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	activeSectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	dimStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	activeStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)
