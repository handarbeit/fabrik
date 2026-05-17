package tui

import (
	"fmt"
	"os"
	"os/exec"
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
	TurnsUsed   int
	MaxTurns    int // 0 means unlimited
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

// abtopFinishedMsg is returned by tea.ExecProcess when the abtop subprocess exits.
type abtopFinishedMsg struct{ Err error }

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
	Repo          string // "owner/repo" — used as fallback for OSC 8 issue links when per-entry Repo is empty
	Version       string // optional version or module name of the monitored project; empty if unknown
	FabrikVersion string // fabrik binary version (e.g. "v1.2.3" or "dev(abc1234)")
}

// Model is the bubbletea TUI model for Fabrik.
type Model struct {
	// layout
	width  int
	height int

	// focus and confirmation state
	focusPane              pane
	confirmQuit            bool
	confirmUpgrade         bool
	confirmReconcile       bool
	confirmOverwrite       bool
	overwriteTyped         string
	pendingReconcilePrompt string
	detailPanel            bool
	helpPanel              bool

	// plugin directory
	pluginDir string

	// wake channel — TUI sends to wake the engine poll loop
	wakeCh chan<- struct{}

	// components
	header  HeaderComponent
	alert   AlertBannerComponent
	active  ActivePaneComponent
	history HistoryPaneComponent
	detail  DetailPanelComponent
	help    HelpPanelComponent
	footer  FooterComponent
}

// minHistoryRows is the minimum number of rows reserved for the history pane
// when the help panel is open, ensuring history always renders.
const minHistoryRows = 5

// reconcilePromptText is the operator-facing Claude Code reconciliation prompt
// printed when the user selects [1] from the custom-workflow dialog.
const reconcilePromptText = "In .fabrik/plugin/, compare the on-disk plugin files against the embedded source at plugin/fabrik-workflows/ in the fabrik repo. Help me reconcile my local customizations with the new embedded version. Preserve my customizations where they don't conflict with the new behavior; flag conflicts for review."

// overwriteConfirmWord is the exact text the operator must type to confirm destructive overwrite.
const overwriteConfirmWord = "OVERWRITE"

// New creates an initial TUI model.
// pollSeconds is the configured polling interval.
// info provides project metadata displayed in the footer.
// pluginDir is the Fabrik plugin directory passed to claude --plugin-dir (may be empty).
// wakeCh is an optional channel the TUI sends on to wake the engine poll loop (may be nil).
// skillsStaleCount is the number of plugin skill files that differ from embedded; 0 means up to date.
// customWorkflow is true when the three-way plugin comparison detects operator customizations.
func New(pollSeconds int, info ProjectInfo, pluginDir string, wakeCh chan struct{}, skillsStaleCount int, customWorkflow bool) Model {
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
		defaultRepo:    info.Repo,
	}

	return Model{
		focusPane: paneActive,
		pluginDir: pluginDir,
		wakeCh:    wakeCh,
		header: HeaderComponent{
			pollInterval:     interval,
			now:              now,
			fabrikVersion:    info.FabrikVersion,
			skillsStaleCount: skillsStaleCount,
			customWorkflow:   customWorkflow,
		},
		alert:   AlertBannerComponent{now: now},
		active:  active,
		history: NewHistoryPaneComponent(info.Repo),
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
		// When help panel is open, suppress most keybindings.
		// Scroll keys are forwarded to the help viewport; ctrl+c still quits.
		if m.helpPanel {
			switch ev.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "?", "esc":
				m.helpPanel = false
				m.help.SetVisible(false)
				m.updateLayout(false)
				return m, nil
			default:
				var cmd tea.Cmd
				m.help.vp, cmd = m.help.vp.Update(msg)
				return m, cmd
			}
		}

		// When confirmOverwrite is active, collect OVERWRITE typed characters.
		if m.confirmOverwrite {
			switch ev.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.confirmOverwrite = false
				m.overwriteTyped = ""
				m.header.SetStatusMsg("")
				return m, nil
			case "backspace", "ctrl+h", "delete":
				if len(m.overwriteTyped) > 0 {
					runes := []rune(m.overwriteTyped)
					m.overwriteTyped = string(runes[:len(runes)-1])
				}
				return m, nil
			default:
				ch := ev.String()
				if len([]rune(ch)) == 1 && len([]rune(m.overwriteTyped)) < len(overwriteConfirmWord) {
					m.overwriteTyped += ch
				}
				if m.overwriteTyped == overwriteConfirmWord {
					m.confirmOverwrite = false
					m.overwriteTyped = ""
					return m, upgradePluginCmd(m.pluginDir)
				}
				if len([]rune(m.overwriteTyped)) == len(overwriteConfirmWord) && m.overwriteTyped != overwriteConfirmWord {
					// Full word typed but wrong — clear and cancel.
					m.confirmOverwrite = false
					m.overwriteTyped = ""
					m.header.SetStatusMsg("")
				}
				return m, nil
			}
		}

		switch ev.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "?":
			m.helpPanel = true
			m.detailPanel = false
			m.help.SetVisible(true)
			m.updateLayout(false)
			m.help.ScrollToTop()
			return m, nil

		case "q":
			if m.history.ConfirmClear() {
				m.history.SetConfirmClear(false)
				m.updateLayout(false)
				return m, nil
			}
			if m.confirmQuit {
				return m, tea.Quit
			}
			if m.active.ActiveCount() > 0 {
				m.confirmQuit = true
				m.updateLayout(false)
				return m, nil
			}
			return m, tea.Quit

		case "n", "N":
			if m.confirmReconcile {
				m.confirmReconcile = false
				m.header.SetStatusMsg("")
				return m, nil
			}
			if m.confirmOverwrite {
				m.confirmOverwrite = false
				m.overwriteTyped = ""
				m.header.SetStatusMsg("")
				return m, nil
			}
			if m.confirmUpgrade {
				m.confirmUpgrade = false
				m.header.SetStatusMsg("")
				return m, nil
			}
			if m.history.ConfirmClear() {
				m.history.SetConfirmClear(false)
				m.updateLayout(false)
				return m, nil
			}
			if m.confirmQuit {
				m.confirmQuit = false
				m.updateLayout(false)
				return m, nil
			}
			if m.detailPanel {
				m.detailPanel = false
				m.updateLayout(false)
				return m, nil
			}

		case "esc":
			if m.confirmReconcile {
				m.confirmReconcile = false
				m.header.SetStatusMsg("")
				return m, nil
			}
			if m.confirmOverwrite {
				m.confirmOverwrite = false
				m.overwriteTyped = ""
				m.header.SetStatusMsg("")
				return m, nil
			}
			if m.confirmUpgrade {
				m.confirmUpgrade = false
				m.header.SetStatusMsg("")
				return m, nil
			}
			if m.history.ConfirmClear() {
				m.history.SetConfirmClear(false)
				m.updateLayout(false)
				return m, nil
			}
			if m.detailPanel {
				m.detailPanel = false
				m.updateLayout(false)
				return m, nil
			}
			if m.confirmQuit {
				m.confirmQuit = false
				m.updateLayout(false)
				return m, nil
			}
			if m.active.ActiveCount() > 0 {
				m.confirmQuit = true
				m.updateLayout(false)
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
			m.updateLayout(false)
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

		case "w":
			if m.wakeCh != nil {
				select {
				case m.wakeCh <- struct{}{}:
				default:
				}
				m.header.SetStatusMsg("waking up...")
				m.header.nextPollAt = time.Now()
			}
			return m, nil

		case "ctrl+r":
			m.header.SetStatusMsg("refreshing…")
			return m, sendSighupCmd()

		case "u":
			if m.header.customWorkflow {
				m.confirmReconcile = true
				m.header.SetStatusMsg("[1] Reconcile via Claude Code  [2] Overwrite (destructive)  [3] Cancel")
			} else if m.header.skillsStaleCount > 0 {
				m.confirmUpgrade = true
				m.header.SetStatusMsg(fmt.Sprintf(
					"Upgrade %d plugin file(s)? Active invocations pick up changes on next run. [y/N]",
					m.header.skillsStaleCount,
				))
			} else {
				m.header.SetStatusMsg("plugin skills up to date")
			}
			return m, nil

		case "1":
			if m.confirmReconcile {
				m.confirmReconcile = false
				m.pendingReconcilePrompt = reconcilePromptText
				return m, tea.Quit
			}
			return m, nil

		case "2":
			if m.confirmReconcile {
				m.confirmReconcile = false
				m.confirmOverwrite = true
				m.overwriteTyped = ""
				m.header.SetStatusMsg("This will discard your customizations. Type 'OVERWRITE' to confirm.")
				return m, nil
			}
			return m, nil

		case "3":
			if m.confirmReconcile {
				m.confirmReconcile = false
				m.header.SetStatusMsg("")
				return m, nil
			}
			return m, nil

		case "y", "Y":
			if m.confirmUpgrade {
				m.confirmUpgrade = false
				return m, upgradePluginCmd(m.pluginDir)
			}
			// Not confirming — forward to history viewport for scrolling.
			var cmd tea.Cmd
			m.history.historyVP, cmd = m.history.historyVP.Update(msg)
			return m, cmd

		case "a":
			if _, err := exec.LookPath("abtop"); err != nil {
				m.header.SetStatusMsg("abtop not found in PATH — install from github.com/graykode/abtop")
				return m, nil
			}
			return m, openAbtopInlineCmd()

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

	case abtopFinishedMsg:
		if ev.Err != nil {
			m.header.SetStatusMsg(fmt.Sprintf("abtop error: %v", ev.Err))
		}
		return m, nil

	case TickEvent:
		// Fan out to all components
		comp, _ := m.header.Update(msg)
		m.header = comp.(HeaderComponent)
		// TickEvent clears statusMsg; re-show active confirmation prompts so
		// they remain visible until the user responds.
		if m.confirmReconcile {
			m.header.SetStatusMsg("[1] Reconcile via Claude Code  [2] Overwrite (destructive)  [3] Cancel")
		} else if m.confirmOverwrite {
			m.header.SetStatusMsg("This will discard your customizations. Type 'OVERWRITE' to confirm.")
		} else if m.confirmUpgrade {
			m.header.SetStatusMsg(fmt.Sprintf(
				"Upgrade %d plugin file(s)? Active invocations pick up changes on next run. [y/N]",
				m.header.skillsStaleCount,
			))
		}
		comp, _ = m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		comp, _ = m.alert.Update(msg)
		m.alert = comp.(AlertBannerComponent)
		comp, _ = m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, tickCmd()

	case ProjectMetaEvent:
		comp, _ := m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil

	case WebhookStatusEvent:
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
		comp, _ = m.alert.Update(msg)
		m.alert = comp.(AlertBannerComponent)
		m.updateLayout(false)
		comp, _ = m.footer.Update(msg)
		m.footer = comp.(FooterComponent)
		return m, nil

	case RateLimitAlertEvent:
		comp, _ := m.alert.Update(msg)
		m.alert = comp.(AlertBannerComponent)
		m.updateLayout(false)
		return m, nil

	case JobStartedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		m.updateLayout(false)
		return m, nil

	case IssueBlockedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		m.updateLayout(false)
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

	case TurnProgressEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		return m, nil

	case StageChangedEvent:
		comp, _ := m.active.Update(msg)
		m.active = comp.(ActivePaneComponent)
		return m, nil

	case SkillsStaleEvent:
		m.header.SetSkillsStaleCount(ev.Count)
		if ev.Count == 0 {
			m.header.SetCustomWorkflow(false)
		}
		return m, nil

	case CustomWorkflowEvent:
		m.header.SetCustomWorkflow(true)
		m.header.SetSkillsStaleCount(0)
		return m, nil

	case pluginUpgradeResultMsg:
		m.confirmUpgrade = false
		m.confirmOverwrite = false
		m.overwriteTyped = ""
		if ev.Err != nil {
			m.header.SetStatusMsg(fmt.Sprintf("plugin upgrade failed: %v", ev.Err))
		} else {
			m.header.SetSkillsStaleCount(0)
			m.header.SetCustomWorkflow(false)
			m.header.SetStatusMsg(fmt.Sprintf("Plugin skills upgraded: %d file(s)", ev.Wrote))
		}
		return m, nil

	}

	return m, nil
}

// syncFocus updates focused state on pane components to match m.focusPane.
func (m *Model) syncFocus() {
	m.active.SetFocused(m.focusPane == paneActive)
	m.history.SetFocused(m.focusPane == paneHistory)
}

// updateLayout recomputes the history and help viewport dimensions and content.
func (m *Model) updateLayout(scrollToTop bool) {
	activeH := m.active.Height()
	detailH := 0
	if m.detailPanel {
		m.prepareDetailItem()
		detailH = m.detail.Height()
	}
	totalAvail := m.height - m.header.Height() - m.alert.Height() - activeH - detailH - m.footer.Height()

	helpH := 0
	if m.helpPanel {
		helpTarget := max(totalAvail-minHistoryRows, 5)
		if helpTarget > totalAvail {
			helpTarget = max(totalAvail, 0)
		}
		m.help.SetLayout(m.width, helpTarget)
		helpH = m.help.Height()
	}

	availableHistoryH := max(totalAvail-helpH, 0)
	m.history.SetLayout(m.width, availableHistoryH, m.confirmQuit, m.active.ActiveCount())
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
				TurnsUsed:   job.TurnsUsed,
				MaxTurns:    job.MaxTurns,
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
	if alertView := m.alert.View(m.width); alertView != "" {
		sections = append(sections, alertView)
	}
	sections = append(sections, m.active.View(m.width))

	if m.detailPanel {
		m.prepareDetailItem()
		if detail := m.detail.View(m.width); detail != "" {
			sections = append(sections, detail)
		}
	}

	if m.helpPanel {
		if help := m.help.View(m.width); help != "" {
			sections = append(sections, help)
		}
	}

	if histView := m.history.View(m.width); histView != "" {
		sections = append(sections, histView)
	}
	sections = append(sections, m.footer.View(m.width))

	return strings.Join(sections, "\n")
}

// viewHeader, viewActive, viewHistory, viewFooter, viewDetail are in their
// respective component files (header.go, active.go, history.go, footer.go, detail.go).

// PendingReconcilePrompt returns the reconciliation prompt text set when the
// user selects [1] from the custom-workflow dialog, or an empty string if none.
// The caller (runTUI) should print this to stderr after p.Run() returns.
func (m Model) PendingReconcilePrompt() string {
	return m.pendingReconcilePrompt
}
