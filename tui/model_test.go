package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestNew(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	if m.header.pollInterval != 30*time.Second {
		t.Errorf("pollInterval = %v, want 30s", m.header.pollInterval)
	}
	if m.active.active == nil {
		t.Error("active map should be initialized")
	}
	if len(m.active.spinnerFrames) == 0 {
		t.Error("spinnerFrames should be non-empty")
	}
	if m.header.now.IsZero() {
		t.Error("now should be set")
	}
}

func TestNew_StoresProjectInfo(t *testing.T) {
	info := ProjectInfo{CWD: "~/foo", BoardTitle: "Acme Board", Version: "1.2.3"}
	m := New(30, info, "", nil)
	if m.footer.projectInfo != info {
		t.Errorf("projectInfo = %+v, want %+v", m.footer.projectInfo, info)
	}
}

// TestInit verifies Init returns a non-nil cmd (the initial tick).
func TestInit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init() should return a non-nil cmd")
	}
}

func TestUpdate_TickEvent(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.height = 24
	initial := m.active.spinnerIdx
	at := time.Now()

	next, cmd := m.Update(TickEvent{At: at})
	nm := next.(Model)

	if nm.header.now != at {
		t.Error("now not updated from TickEvent")
	}
	if nm.active.spinnerIdx != (initial+1)%len(m.active.spinnerFrames) {
		t.Errorf("spinnerIdx = %d, want %d", nm.active.spinnerIdx, (initial+1)%len(m.active.spinnerFrames))
	}
	if cmd == nil {
		t.Error("expected non-nil cmd (next tick) from TickEvent")
	}
}

func TestUpdate_PollStartedAndCompleted(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	before := time.Now()

	next, _ := m.Update(PollStartedEvent{Owner: "o", Repo: "r", Project: 1})
	nm := next.(Model)
	if !nm.header.nextPollAt.After(before) {
		t.Error("nextPollAt should be in the future after PollStartedEvent")
	}

	// PollCompletedEvent updates header timer (no pollCount on Model anymore).
	next2, _ := nm.Update(PollCompletedEvent{ItemCount: 5, Dispatched: 2})
	nm2 := next2.(Model)
	if !nm2.header.nextPollAt.After(before) {
		t.Error("nextPollAt should be in the future after PollCompletedEvent")
	}
}

func TestUpdate_QuitKey(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Error("expected quit cmd from 'q' key")
	}
	// Execute the command and check it's tea.Quit
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestUpdate_TabKey_SwitchesPanes verifies tab toggles focus between panes.
func TestUpdate_TabKey_SwitchesPanes(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.height = 24
	if m.focusPane != paneActive {
		t.Fatal("expected initial pane to be active")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	nm := next.(Model)
	if nm.focusPane != paneHistory {
		t.Error("expected pane to switch to history after tab")
	}
	if cmd != nil {
		t.Error("expected nil cmd from tab")
	}
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyTab})
	nm2 := next2.(Model)
	if nm2.focusPane != paneActive {
		t.Error("expected pane to switch back to active after second tab")
	}
}

func TestUpdate_RKey_HistoryPane_WithEntry_MissingWorktree(t *testing.T) {
	redirectHistory(t)
	// Chdir to a temp dir so worktree path won't accidentally exist.
	t.Chdir(t.TempDir())
	m := New(30, ProjectInfo{}, "", nil)
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", StageModel: "sonnet", Success: true},
	}

	// r on a history entry when worktree is missing: nil cmd, statusMsg set.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd when worktree is missing")
	}
	nm := next.(Model)
	if nm.header.statusMsg == "" {
		t.Error("expected statusMsg to be set when worktree is missing")
	}
}

func TestUpdate_RKey_HistoryPane_WithWorktree(t *testing.T) {
	redirectHistory(t)
	// Create a temp project dir with the worktree directory.
	dir := t.TempDir()
	worktree := filepath.Join(dir, ".fabrik", "worktrees", "issue-42")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	m := New(30, ProjectInfo{}, "", nil)
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", StageModel: "sonnet", Success: true},
	}

	// r on a history entry with worktree present: returns non-nil tea.ExecProcess cmd.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("expected non-nil cmd (tea.ExecProcess) from r key with worktree present")
	}
}

func TestUpdate_RKey_HistoryPane_NoEntries_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.focusPane = paneHistory
	m.history.history = nil

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty history pane")
	}
}

func TestUpdate_RKey_HistoryPane_ActiveIssue_SetsStatusMsg(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
	m.focusPane = paneHistory
	// Add issue 42 to both active map and history.
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}
	m.history.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", Success: true},
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd when issue is active")
	}
	nm := next.(Model)
	if nm.header.statusMsg == "" {
		t.Error("expected statusMsg to be set when issue is actively running")
	}
}

func TestUpdate_LogEvent_IssueZero_StatusLine(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.height = 24
	next, _ := m.Update(LogEvent{IssueNumber: 0, Tag: "poll", Message: "polling now\n"})
	nm := next.(Model)
	if nm.header.statusLine != "[poll] polling now" {
		t.Errorf("statusLine = %q, want %q", nm.header.statusLine, "[poll] polling now")
	}
}

func TestUpdate_QKey_WithActiveJobs_ShowsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Error("expected nil cmd (no quit yet) when q pressed with active jobs")
	}
	nm := next.(Model)
	if !nm.confirmQuit {
		t.Error("expected confirmQuit=true after q with active jobs")
	}
}

func TestUpdate_QKey_WhenConfirmQuit_Quits(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.confirmQuit = true
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected non-nil cmd (quit) when q pressed while confirmQuit=true")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestUpdate_NKey_CancelsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.confirmQuit = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if cmd != nil {
		t.Error("expected nil cmd after n cancels confirmQuit")
	}
	nm := next.(Model)
	if nm.confirmQuit {
		t.Error("expected confirmQuit=false after n key")
	}
}

func TestUpdate_EscKey_WithActiveJobs_ShowsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("expected nil cmd (no quit yet) when Escape pressed with active jobs")
	}
	nm := next.(Model)
	if !nm.confirmQuit {
		t.Error("expected confirmQuit=true after Escape with active jobs")
	}
}

func TestUpdate_EscKey_WhenConfirmQuit_Cancels(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.confirmQuit = true
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("expected nil cmd after Escape cancels confirmQuit")
	}
	nm := next.(Model)
	if nm.confirmQuit {
		t.Error("expected confirmQuit=false after Escape key while confirmQuit=true")
	}
}

func TestUpdate_CtrlC_BypassesConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.confirmQuit = true
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected non-nil cmd (quit) from ctrl+c even when confirmQuit=true")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestView_BeforeWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	// Before width is set, View should return a loading placeholder without panicking
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("expected 'Loading...' placeholder before window size, got %q", v)
	}
}

func TestView_AfterWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{BoardTitle: "Acme PM", CWD: "~/myproject"}, "", nil)
	m.width = 80
	m.height = 24
	m.header.nextPollAt = time.Now().Add(30 * time.Second)

	v := m.View()
	if strings.Contains(v, "Loading") {
		t.Error("should not show loading after window size is set")
	}
	if !strings.Contains(v, "fabrik") {
		t.Error("header should contain 'fabrik'")
	}
	if !strings.Contains(v, "In Progress") {
		t.Error("should show In Progress pane")
	}
	if !strings.Contains(v, "History") {
		t.Error("should show History pane")
	}
	if !strings.Contains(v, "Acme PM") {
		t.Error("footer should contain board title")
	}
	if !strings.Contains(v, "~/myproject") {
		t.Error("footer should contain CWD")
	}
}

// TestLayoutHeightInvariant verifies that the total rendered height of View()
// equals m.height for various numbers of active jobs.
func TestLayoutHeightInvariant(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80
	const termHeight = 24

	for _, n := range []int{0, 1, 7, 8, 15} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			m := New(30, ProjectInfo{}, "", nil)
			// Add n active jobs.
			now := time.Now()
			for i := 0; i < n; i++ {
				m.active.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
			}
			// Apply window size — this triggers updateLayout.
			next, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
			m = next.(Model)

			got := lipgloss.Height(m.View())
			if got != termHeight {
				t.Errorf("n=%d: View() height = %d, want %d (header/footer pushed off screen)", n, got, termHeight)
			}
		})
	}
}

// TestLayoutHeightInvariant_SmallTerminal verifies that View() height equals
// the terminal height on small terminals.
func TestLayoutHeightInvariant_SmallTerminal(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80

	cases := []struct {
		termHeight int
		nActive    int
	}{
		{16, 7},
		{17, 7},
		{14, 5},
		{10, 0},
		{9, 0},
		{8, 0},
		{7, 0}, // availableHistoryH = 1 (trimmed)
		{6, 0}, // availableHistoryH = 0 (history omitted)
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("h=%d,n=%d", tc.termHeight, tc.nActive), func(t *testing.T) {
			m := New(30, ProjectInfo{}, "", nil)
			now := time.Now()
			for i := 0; i < tc.nActive; i++ {
				m.active.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
			}
			next, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: tc.termHeight})
			m = next.(Model)

			got := lipgloss.Height(m.View())
			if got != tc.termHeight {
				t.Errorf("h=%d,n=%d: View() height = %d, want %d (header/footer pushed off screen)",
					tc.termHeight, tc.nActive, got, tc.termHeight)
			}
		})
	}
}

// TestLayoutHeightInvariant_NarrowWithHint verifies that the layout height invariant
// holds on narrow terminals where the hint line would wrap without truncation.
func TestLayoutHeightInvariant_NarrowWithHint(t *testing.T) {
	redirectHistory(t)

	const termHeight = 24

	cases := []struct {
		name        string
		termWidth   int
		focusPane   pane
		confirmQuit bool
		nActive     int
		nBlocked    int
		nHistory    int
	}{
		{"narrow_confirmQuit", 40, paneHistory, true, 1, 0, 1},
		{"narrow_normalHint", 40, paneHistory, false, 0, 0, 1},
		{"medium_confirmQuit", 60, paneHistory, true, 1, 0, 1},
		{"with_blocked", 80, paneHistory, false, 1, 3, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "", nil)
			now := time.Now()
			for i := 0; i < tc.nActive; i++ {
				m.active.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
			}
			for i := 0; i < tc.nBlocked; i++ {
				m.active.blocked[fmt.Sprintf("issue-%d", tc.nActive+i+1)] = &blockedIssue{IssueNumber: tc.nActive + i + 1, StageName: "Research"}
			}
			for i := 0; i < tc.nHistory; i++ {
				m.history.history = append(m.history.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research", Success: true, Completed: true})
			}
			m.focusPane = tc.focusPane
			m.confirmQuit = tc.confirmQuit

			next, _ := m.Update(tea.WindowSizeMsg{Width: tc.termWidth, Height: termHeight})
			m = next.(Model)

			got := lipgloss.Height(m.View())
			if got != termHeight {
				t.Errorf("%s: View() height = %d, want %d (footer pushed off screen)", tc.name, got, termHeight)
			}
		})
	}
}

// TestTuiEventMethods exercises the tuiEvent() no-op methods on each event type
// to achieve coverage of these trivial interface satisfiers.
func TestTuiEventMethods(t *testing.T) {
	LogEvent{}.tuiEvent()
	PollStartedEvent{}.tuiEvent()
	PollCompletedEvent{}.tuiEvent()
	JobStartedEvent{}.tuiEvent()
	JobCompletedEvent{}.tuiEvent()
	TickEvent{}.tuiEvent()
}

// TestHelpPanelToggle verifies that pressing ? opens the help panel, pressing ?
// or esc closes it, and opening help closes the detail panel.
func TestHelpPanelToggle(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.height = 24

	// Pressing ? opens help panel.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	nm := next.(Model)
	if cmd != nil {
		t.Error("expected nil cmd from ? key")
	}
	if !nm.helpPanel {
		t.Error("expected helpPanel=true after ? key")
	}

	// Pressing ? again closes help panel.
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	nm2 := next2.(Model)
	if nm2.helpPanel {
		t.Error("expected helpPanel=false after second ? key")
	}

	// Pressing esc while help is open closes help panel.
	next3, _ := nm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm3 := next3.(Model)
	if nm3.helpPanel {
		t.Error("expected helpPanel=false after esc while help open")
	}

	// Pressing ? while detail panel is open closes detail and opens help.
	m2 := New(30, ProjectInfo{}, "", nil)
	m2.width = 80
	m2.height = 24
	m2.detailPanel = true
	next4, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	nm4 := next4.(Model)
	if !nm4.helpPanel {
		t.Error("expected helpPanel=true after ? with detail open")
	}
	if nm4.detailPanel {
		t.Error("expected detailPanel=false after ? with detail open")
	}
}

// TestHelpPanelSuppressKeys verifies that when the help panel is open, keys that
// would normally change model state (tab, enter, r, l) are suppressed.
func TestHelpPanelSuppressKeys(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.height = 24
	// Open help panel.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	m = next.(Model)
	if !m.helpPanel {
		t.Fatal("precondition: help panel should be open")
	}

	initialFocusPane := m.focusPane
	initialDetailPanel := m.detailPanel

	// tab should not switch panes.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	nm := next.(Model)
	if nm.focusPane != initialFocusPane {
		t.Error("tab should be suppressed while help is open (focusPane changed)")
	}

	// enter should not toggle detail panel.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm = next.(Model)
	if nm.detailPanel != initialDetailPanel {
		t.Error("enter should be suppressed while help is open (detailPanel changed)")
	}

	// r should be suppressed (nil cmd, no status message change).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	nm = next.(Model)
	if nm.focusPane != initialFocusPane {
		t.Error("r should be suppressed while help is open (focusPane changed)")
	}

	// q should be suppressed (no quit).
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		// The viewport Update forwards q to the viewport which returns nil cmd.
		// If somehow a quit cmd is returned, that's a bug.
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); ok {
			t.Error("q should be suppressed while help is open (got quit cmd)")
		}
	}
}

// TestLayoutHeightInvariant_ConfirmClear verifies the height invariant holds when
// the confirm-clear prompt is shown and then dismissed via n, esc, or q.
func TestLayoutHeightInvariant_ConfirmClear(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80
	const termHeight = 24

	setup := func(t *testing.T) Model {
		t.Helper()
		m := New(30, ProjectInfo{}, "", nil)
		for i := 0; i < 3; i++ {
			m.history.history = append(m.history.history, HistoryEntry{
				IssueNumber: i + 1, StageName: "Research", Success: true, Completed: true,
			})
		}
		m.focusPane = paneHistory
		m.syncFocus()
		next, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
		return next.(Model)
	}

	t.Run("shown", func(t *testing.T) {
		m := setup(t)
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
		m = next.(Model)
		if !m.history.ConfirmClear() {
			t.Fatal("precondition: confirmClear should be true after C key")
		}
		got := lipgloss.Height(m.View())
		if got != termHeight {
			t.Errorf("confirm-clear shown: View() height = %d, want %d", got, termHeight)
		}
	})

	t.Run("dismissed_n", func(t *testing.T) {
		m := setup(t)
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
		m = next.(Model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		m = next.(Model)
		if m.history.ConfirmClear() {
			t.Fatal("precondition: confirmClear should be false after n key")
		}
		got := lipgloss.Height(m.View())
		if got != termHeight {
			t.Errorf("confirm-clear dismissed (n): View() height = %d, want %d", got, termHeight)
		}
	})

	t.Run("dismissed_esc", func(t *testing.T) {
		m := setup(t)
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
		m = next.(Model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = next.(Model)
		if m.history.ConfirmClear() {
			t.Fatal("precondition: confirmClear should be false after esc key")
		}
		got := lipgloss.Height(m.View())
		if got != termHeight {
			t.Errorf("confirm-clear dismissed (esc): View() height = %d, want %d", got, termHeight)
		}
	})

	t.Run("dismissed_q", func(t *testing.T) {
		m := setup(t)
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
		m = next.(Model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		m = next.(Model)
		if m.history.ConfirmClear() {
			t.Fatal("precondition: confirmClear should be false after q key")
		}
		got := lipgloss.Height(m.View())
		if got != termHeight {
			t.Errorf("confirm-clear dismissed (q): View() height = %d, want %d", got, termHeight)
		}
	})
}

// TestLayoutHeightInvariant_DetailPanel verifies the height invariant holds when
// the detail panel is opened and closed.
func TestLayoutHeightInvariant_DetailPanel(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80
	const termHeight = 24

	cases := []struct {
		name    string
		nActive int
		nHist   int
		focused pane
	}{
		{"no_jobs_history_focused", 0, 3, paneHistory},
		{"with_active_job_active_focused", 1, 0, paneActive},
		{"with_active_and_history", 2, 3, paneHistory},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "", nil)
			now := time.Now()
			for i := 0; i < tc.nActive; i++ {
				key := fmt.Sprintf("issue-%d", i+1)
				m.active.active[key] = &activeJob{IssueNumber: i + 1, StageName: "Research", StartedAt: now}
			}
			for i := 0; i < tc.nHist; i++ {
				m.history.history = append(m.history.history, HistoryEntry{
					IssueNumber: i + 1, StageName: "Research", Success: true, Completed: true,
				})
			}
			m.focusPane = tc.focused
			m.syncFocus()
			next, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
			m = next.(Model)

			// Open detail panel.
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = next.(Model)
			if !m.detailPanel {
				t.Fatal("precondition: detailPanel should be true after enter")
			}
			got := lipgloss.Height(m.View())
			if got != termHeight {
				t.Errorf("detail panel open: View() height = %d, want %d", got, termHeight)
			}

			// Close detail panel.
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = next.(Model)
			if m.detailPanel {
				t.Fatal("precondition: detailPanel should be false after second enter")
			}
			got = lipgloss.Height(m.View())
			if got != termHeight {
				t.Errorf("detail panel closed: View() height = %d, want %d", got, termHeight)
			}
		})
	}
}

// TestLayoutHeightInvariant_WithHelp verifies that the total rendered height of View()
// equals m.height when the help panel is open at a standard terminal size.
func TestLayoutHeightInvariant_WithHelp(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80
	const termHeight = 24

	cases := []struct {
		name    string
		nActive int
		focused pane
		nHist   int
	}{
		{"active_focus_no_history", 0, paneActive, 0},
		{"active_focus_with_history", 0, paneActive, 3},
		{"history_focus_with_history", 0, paneHistory, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "", nil)
			now := time.Now()
			for i := 0; i < tc.nActive; i++ {
				m.active.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
			}
			for i := 0; i < tc.nHist; i++ {
				m.history.history = append(m.history.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research", Success: true, Completed: true})
			}
			m.focusPane = tc.focused
			m.syncFocus()

			// Apply window size first.
			next, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
			m = next.(Model)

			// Open help panel.
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
			m = next.(Model)
			if !m.helpPanel {
				t.Fatal("precondition: help panel should be open")
			}

			got := lipgloss.Height(m.View())
			if got != termHeight {
				t.Errorf("%s: View() height with help = %d, want %d", tc.name, got, termHeight)
			}
		})
	}
}
