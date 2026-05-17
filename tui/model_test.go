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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, info, "", nil, 0, false)
	if m.footer.projectInfo != info {
		t.Errorf("projectInfo = %+v, want %+v", m.footer.projectInfo, info)
	}
}

// TestInit verifies Init returns a non-nil cmd (the initial tick).
func TestInit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init() should return a non-nil cmd")
	}
}

func TestUpdate_TickEvent(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	m.focusPane = paneHistory
	m.history.history = nil

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty history pane")
	}
}

func TestUpdate_RKey_HistoryPane_ActiveIssue_SetsStatusMsg(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	m.width = 80
	m.height = 24
	next, _ := m.Update(LogEvent{IssueNumber: 0, Tag: "poll", Message: "polling now\n"})
	nm := next.(Model)
	if nm.header.statusLine != "[poll] polling now" {
		t.Errorf("statusLine = %q, want %q", nm.header.statusLine, "[poll] polling now")
	}
}

func TestUpdate_QKey_WithActiveJobs_ShowsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	// Before width is set, View should return a loading placeholder without panicking
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("expected 'Loading...' placeholder before window size, got %q", v)
	}
}

func TestView_AfterWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{BoardTitle: "Acme PM", CWD: "~/myproject"}, "", nil, 0, false)
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
			m := New(30, ProjectInfo{}, "", nil, 0, false)
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
			m := New(30, ProjectInfo{}, "", nil, 0, false)
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
			m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	SkillsStaleEvent{}.tuiEvent()
	RateLimitAlertEvent{}.tuiEvent()
}

// TestUpdate_RateLimitAlertEvent_Exhausted verifies that after receiving a
// RateLimitAlertEvent{Exhausted: true}, the alert banner becomes visible when
// graphqlStats show low remaining quota.
func TestUpdate_RateLimitAlertEvent_Exhausted(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	// First deliver stats that put ratio at 10% (below 20% threshold).
	next, _ := m.Update(PollCompletedEvent{
		GraphQLStats: RateLimitStats{Limit: 100, Remaining: 10},
	})
	m = next.(Model)

	// Trigger alert — probe failure while rate limited.
	next, _ = m.Update(RateLimitAlertEvent{Exhausted: true})
	m = next.(Model)

	if !m.alert.bannerActive {
		t.Error("expected alert.bannerActive=true after RateLimitAlertEvent{Exhausted: true}")
	}
	if !m.alert.isVisible() {
		t.Error("expected alert banner to be visible after Exhausted=true event with low quota")
	}
}

// TestUpdate_RateLimitAlertEvent_Recovered verifies that after receiving a
// RateLimitAlertEvent{Exhausted: false}, the banner's bannerActive flag is cleared.
func TestUpdate_RateLimitAlertEvent_Recovered(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	// Manually set banner active with stats still low.
	m.alert.bannerActive = true
	m.alert.graphqlStats = RateLimitStats{Limit: 100, Remaining: 10}

	// Recovery event.
	next, _ := m.Update(RateLimitAlertEvent{Exhausted: false})
	m = next.(Model)

	if m.alert.bannerActive {
		t.Error("expected alert.bannerActive=false after RateLimitAlertEvent{Exhausted: false}")
	}
}

// TestUpdateLayout_AlertBannerHeightBudget verifies that the layout height
// invariant holds when the alert banner is visible (banner takes 1 row).
func TestUpdateLayout_AlertBannerHeightBudget(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80
	const termHeight = 24

	m := New(30, ProjectInfo{}, "", nil, 0, false)
	// Set stats so banner is visible (Remaining == 0).
	m.alert.graphqlStats = RateLimitStats{Limit: 100, Remaining: 0}
	m.alert.now = time.Now()

	if !m.alert.isVisible() {
		t.Fatal("precondition: alert banner should be visible with Remaining==0")
	}
	if m.alert.Height() != 1 {
		t.Fatalf("precondition: alert Height() = %d, want 1", m.alert.Height())
	}

	// Apply window size — triggers updateLayout.
	next, _ := m.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})
	m = next.(Model)

	got := lipgloss.Height(m.View())
	if got != termHeight {
		t.Errorf("with banner visible: View() height = %d, want %d (height budget not accounting for banner)", got, termHeight)
	}
}

// TestHelpPanelToggle verifies that pressing ? opens the help panel, pressing ?
// or esc closes it, and opening help closes the detail panel.
func TestHelpPanelToggle(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m2 := New(30, ProjectInfo{}, "", nil, 0, false)
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
	m := New(30, ProjectInfo{}, "", nil, 0, false)
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
		m := New(30, ProjectInfo{}, "", nil, 0, false)
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
			m := New(30, ProjectInfo{}, "", nil, 0, false)
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
			m := New(30, ProjectInfo{}, "", nil, 0, false)
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

// TestUpdate_WKey_SendsOnWakeCh verifies that pressing w sends on wakeCh
// and sets the status message to "waking up...".
func TestUpdate_WKey_SendsOnWakeCh(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	m := New(30, ProjectInfo{}, "", wakeCh, 0, false)
	m.width = 80
	m.height = 24
	m.header.nextPollAt = time.Now().Add(5 * time.Minute)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from w key")
	}
	if nm.header.statusMsg != "waking up..." {
		t.Errorf("statusMsg = %q, want %q", nm.header.statusMsg, "waking up...")
	}
	// The wake channel should have received a signal.
	select {
	case <-wakeCh:
		// ok
	default:
		t.Error("expected signal on wakeCh after pressing w")
	}
}

// TestUpdate_WKey_NilWakeCh_NoOp verifies that pressing w with no wakeCh is a no-op.
func TestUpdate_WKey_NilWakeCh_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)
	m.width = 80
	m.height = 24

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	nm := next.(Model)

	if nm.header.statusMsg != "" {
		t.Errorf("expected empty statusMsg with nil wakeCh, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_UKey_WhenStale_SetsConfirmUpgrade verifies that pressing u when
// skillsStaleCount > 0 sets confirmUpgrade and shows a confirmation prompt.
func TestUpdate_UKey_WhenStale_SetsConfirmUpgrade(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 3, false)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from u key (just shows prompt)")
	}
	if !nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=true after pressing u with stale skills")
	}
	if !strings.Contains(nm.header.statusMsg, "3") {
		t.Errorf("statusMsg should mention file count, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_TickEvent_ConfirmUpgrade_PromptPersists verifies that the upgrade
// confirmation prompt is re-shown after a TickEvent clears statusMsg.
func TestUpdate_TickEvent_ConfirmUpgrade_PromptPersists(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 2, false)
	m.confirmUpgrade = true
	m.header.SetStatusMsg("Upgrade 2 plugin file(s)? Active invocations pick up changes on next run. [y/N]")

	next, _ := m.Update(TickEvent{At: time.Now()})
	nm := next.(Model)

	if !nm.confirmUpgrade {
		t.Error("expected confirmUpgrade still true after tick")
	}
	if nm.header.statusMsg == "" {
		t.Error("expected prompt to be re-shown after tick cleared statusMsg")
	}
	if !strings.Contains(nm.header.statusMsg, "2") {
		t.Errorf("re-shown prompt should mention file count, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_UKey_WhenUpToDate_ShowsStatusMsg verifies that pressing u when
// skillsStaleCount == 0 shows "up to date" message without setting confirmUpgrade.
func TestUpdate_UKey_WhenUpToDate_ShowsStatusMsg(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from u key when up to date")
	}
	if nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=false when skills are up to date")
	}
	if nm.header.statusMsg != "plugin skills up to date" {
		t.Errorf("statusMsg = %q, want %q", nm.header.statusMsg, "plugin skills up to date")
	}
}

// TestUpdate_YKey_WhenConfirmUpgrade_DispatchesCmd verifies that pressing y
// when confirmUpgrade is true clears the flag and returns the upgrade cmd.
func TestUpdate_YKey_WhenConfirmUpgrade_DispatchesCmd(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 2, false)
	m.confirmUpgrade = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	nm := next.(Model)

	if cmd == nil {
		t.Error("expected non-nil cmd (upgradePluginCmd) after pressing y")
	}
	if nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=false after pressing y")
	}
}

// TestUpdate_NKey_CancelsConfirmUpgrade verifies that pressing n when
// confirmUpgrade is true clears the flag and status message.
func TestUpdate_NKey_CancelsConfirmUpgrade(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 2, false)
	m.confirmUpgrade = true
	m.header.statusMsg = "Upgrade 2 plugin file(s)? [y/N]"

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from n key canceling upgrade")
	}
	if nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=false after pressing n")
	}
	if nm.header.statusMsg != "" {
		t.Errorf("expected empty statusMsg after cancel, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_EscKey_CancelsConfirmUpgrade verifies that pressing esc when
// confirmUpgrade is true clears the flag and status message.
func TestUpdate_EscKey_CancelsConfirmUpgrade(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 2, false)
	m.confirmUpgrade = true
	m.header.statusMsg = "Upgrade 2 plugin file(s)? [y/N]"

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from esc key canceling upgrade")
	}
	if nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=false after pressing esc")
	}
	if nm.header.statusMsg != "" {
		t.Errorf("expected empty statusMsg after cancel, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_New_WithSkillsStaleCount initializes with a stale count and
// verifies the header badge field is set.
func TestUpdate_New_WithSkillsStaleCount(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 5, false)
	if m.header.skillsStaleCount != 5 {
		t.Errorf("skillsStaleCount = %d, want 5", m.header.skillsStaleCount)
	}
}

// TestUpdate_SkillsStaleEvent updates the header badge count.
func TestUpdate_SkillsStaleEvent(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, false)

	next, cmd := m.Update(SkillsStaleEvent{Count: 4})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from SkillsStaleEvent")
	}
	if nm.header.skillsStaleCount != 4 {
		t.Errorf("skillsStaleCount = %d, want 4", nm.header.skillsStaleCount)
	}
}

// TestUpdate_PluginUpgradeResultMsg_Success verifies a successful upgrade clears
// the badge and shows a success message.
func TestUpdate_PluginUpgradeResultMsg_Success(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 3, false)
	m.confirmUpgrade = true

	next, _ := m.Update(pluginUpgradeResultMsg{Wrote: 3, Err: nil})
	nm := next.(Model)

	if nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=false after successful upgrade")
	}
	if nm.header.skillsStaleCount != 0 {
		t.Errorf("skillsStaleCount = %d, want 0 after upgrade", nm.header.skillsStaleCount)
	}
	if !strings.Contains(nm.header.statusMsg, "3") {
		t.Errorf("statusMsg should mention count, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_PluginUpgradeResultMsg_Error verifies a failed upgrade retains the
// badge and shows an error message.
func TestUpdate_PluginUpgradeResultMsg_Error(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 2, false)
	m.confirmUpgrade = true

	next, _ := m.Update(pluginUpgradeResultMsg{Wrote: 0, Err: fmt.Errorf("disk full")})
	nm := next.(Model)

	if nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=false after upgrade attempt")
	}
	if nm.header.skillsStaleCount != 2 {
		t.Errorf("skillsStaleCount = %d, want 2 (badge retained on error)", nm.header.skillsStaleCount)
	}
	if !strings.Contains(nm.header.statusMsg, "disk full") {
		t.Errorf("statusMsg should contain error, got %q", nm.header.statusMsg)
	}
}

// TestUpdate_CustomWorkflowEvent verifies that CustomWorkflowEvent sets the
// customWorkflow badge and clears skillsStaleCount.
func TestUpdate_CustomWorkflowEvent(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 4, false)

	next, cmd := m.Update(CustomWorkflowEvent{})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from CustomWorkflowEvent")
	}
	if !nm.header.customWorkflow {
		t.Error("expected customWorkflow=true after CustomWorkflowEvent")
	}
	if nm.header.skillsStaleCount != 0 {
		t.Errorf("skillsStaleCount = %d, want 0 after CustomWorkflowEvent", nm.header.skillsStaleCount)
	}
}

// TestUpdate_New_WithCustomWorkflow verifies customWorkflow=true is wired from New.
func TestUpdate_New_WithCustomWorkflow(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	if !m.header.customWorkflow {
		t.Error("expected customWorkflow=true when New called with true")
	}
}

// TestUKey_CustomWorkflow verifies pressing u when customWorkflow=true enters
// confirmReconcile and shows the 3-option status message.
func TestUKey_CustomWorkflow(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from u key entering confirmReconcile")
	}
	if !nm.confirmReconcile {
		t.Error("expected confirmReconcile=true after u key with customWorkflow")
	}
	if !strings.Contains(nm.header.statusMsg, "[1]") {
		t.Errorf("statusMsg should show 3-option dialog, got %q", nm.header.statusMsg)
	}
	if !strings.Contains(nm.header.statusMsg, "[2]") {
		t.Errorf("statusMsg should show overwrite option, got %q", nm.header.statusMsg)
	}
	if !strings.Contains(nm.header.statusMsg, "[3]") {
		t.Errorf("statusMsg should show cancel option, got %q", nm.header.statusMsg)
	}
}

// TestUKey_CustomWorkflow_Key3_Cancels verifies [3] cancels confirmReconcile.
func TestUKey_CustomWorkflow_Key3_Cancels(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmReconcile = true
	m.header.SetStatusMsg("[1] Reconcile  [2] Overwrite  [3] Cancel")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from [3] cancel")
	}
	if nm.confirmReconcile {
		t.Error("expected confirmReconcile=false after [3]")
	}
	if nm.header.statusMsg != "" {
		t.Errorf("expected empty statusMsg after cancel, got %q", nm.header.statusMsg)
	}
}

// TestUKey_CustomWorkflow_Key2_EntersConfirmOverwrite verifies [2] transitions
// to confirmOverwrite state with the OVERWRITE prompt.
func TestUKey_CustomWorkflow_Key2_EntersConfirmOverwrite(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmReconcile = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from [2]")
	}
	if nm.confirmReconcile {
		t.Error("expected confirmReconcile=false after [2]")
	}
	if !nm.confirmOverwrite {
		t.Error("expected confirmOverwrite=true after [2]")
	}
	if !strings.Contains(nm.header.statusMsg, "OVERWRITE") {
		t.Errorf("statusMsg should prompt for OVERWRITE, got %q", nm.header.statusMsg)
	}
}

// TestUKey_OverwriteConfirm verifies that typing OVERWRITE one character at a
// time triggers the upgrade cmd.
func TestUKey_OverwriteConfirm(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmOverwrite = true

	word := "OVERWRITE"
	var lastModel Model
	var lastCmd tea.Cmd
	for i, ch := range word {
		var next tea.Model
		next, lastCmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		m = next.(Model)
		if i < len(word)-1 {
			if lastCmd != nil {
				t.Errorf("expected nil cmd after char %d (%c), got non-nil", i, ch)
			}
			if !m.confirmOverwrite {
				t.Errorf("expected confirmOverwrite still true after char %d (%c)", i, ch)
			}
		}
	}
	lastModel = m

	// After typing full OVERWRITE: confirmOverwrite cleared, cmd returned.
	if lastModel.confirmOverwrite {
		t.Error("expected confirmOverwrite=false after full OVERWRITE typed")
	}
	if lastCmd == nil {
		t.Error("expected non-nil cmd (upgrade) after OVERWRITE typed correctly")
	}
}

// TestUKey_OverwriteConfirm_WrongWord verifies a wrong word clears confirmOverwrite.
func TestUKey_OverwriteConfirm_WrongWord(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmOverwrite = true

	// Type a different 9-char word.
	wrong := "OVERWRITE" // length 9; let's type OVERWRITX (wrong last char)
	word := []rune("OVERWRITX")
	for _, ch := range word {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		m = next.(Model)
	}

	_ = wrong
	if m.confirmOverwrite {
		t.Error("expected confirmOverwrite=false after wrong full word")
	}
	if m.overwriteTyped != "" {
		t.Errorf("expected overwriteTyped cleared, got %q", m.overwriteTyped)
	}
}

// TestUKey_OverwriteConfirm_Backspace verifies backspace removes last character.
func TestUKey_OverwriteConfirm_Backspace(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmOverwrite = true

	// Type "OVE" then backspace.
	for _, ch := range "OVE" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		m = next.(Model)
	}
	if m.overwriteTyped != "OVE" {
		t.Fatalf("expected overwriteTyped=OVE, got %q", m.overwriteTyped)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(Model)
	if m.overwriteTyped != "OV" {
		t.Errorf("expected overwriteTyped=OV after backspace, got %q", m.overwriteTyped)
	}
	if !m.confirmOverwrite {
		t.Error("expected confirmOverwrite=true after backspace")
	}
}

// TestUKey_OverwriteConfirm_Esc verifies esc cancels confirmOverwrite.
func TestUKey_OverwriteConfirm_Esc(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmOverwrite = true
	m.overwriteTyped = "OVER"

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from esc cancel of overwrite")
	}
	if nm.confirmOverwrite {
		t.Error("expected confirmOverwrite=false after esc")
	}
	if nm.overwriteTyped != "" {
		t.Errorf("expected overwriteTyped cleared, got %q", nm.overwriteTyped)
	}
}

// TestUpdate_SkillsStaleEvent_Zero_ClearsCustomWorkflow verifies that
// SkillsStaleEvent{Count:0} clears the customWorkflow badge.
func TestUpdate_SkillsStaleEvent_Zero_ClearsCustomWorkflow(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true) // customWorkflow=true

	next, _ := m.Update(SkillsStaleEvent{Count: 0})
	nm := next.(Model)

	if nm.header.customWorkflow {
		t.Error("expected customWorkflow=false after SkillsStaleEvent{Count:0}")
	}
}

// TestUpdate_TickEvent_ConfirmReconcile_PromptPersists verifies the reconcile
// dialog prompt is re-shown after a TickEvent clears statusMsg.
func TestUpdate_TickEvent_ConfirmReconcile_PromptPersists(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true)
	m.confirmReconcile = true
	m.header.SetStatusMsg("[1] Reconcile  [2] Overwrite  [3] Cancel")

	next, _ := m.Update(TickEvent{At: time.Now()})
	nm := next.(Model)

	if !nm.confirmReconcile {
		t.Error("expected confirmReconcile still true after tick")
	}
	if nm.header.statusMsg == "" {
		t.Error("expected dialog prompt to be re-shown after tick")
	}
}

// TestUpdate_PluginUpgradeResultMsg_ClearsCustomWorkflow verifies a successful
// upgrade also clears the customWorkflow badge.
func TestUpdate_PluginUpgradeResultMsg_ClearsCustomWorkflow(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, 0, true) // customWorkflow=true
	m.confirmOverwrite = true

	next, _ := m.Update(pluginUpgradeResultMsg{Wrote: 5, Err: nil})
	nm := next.(Model)

	if nm.header.customWorkflow {
		t.Error("expected customWorkflow=false after successful upgrade")
	}
	if nm.confirmOverwrite {
		t.Error("expected confirmOverwrite=false after successful upgrade")
	}
}
