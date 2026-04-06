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

func TestMain(m *testing.M) {
	// Redirect history to a temp location so tests don't clobber real history.
	// This runs before all tests in the package.
	m.Run()
}

func redirectHistory(t *testing.T) {
	t.Helper()
	old := HistoryPathOverride
	HistoryPathOverride = filepath.Join(t.TempDir(), "history.json")
	t.Cleanup(func() { HistoryPathOverride = old })
}

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0:00"},
		{-5 * time.Second, "0:00"}, // negative clamped to 0
		{30 * time.Second, "0:30"},
		{90 * time.Second, "1:30"},
		{61 * time.Second, "1:01"},
		{10*time.Minute + 5*time.Second, "10:05"},
		{500 * time.Millisecond, "0:01"}, // rounds to nearest second
		{499 * time.Millisecond, "0:00"}, // rounds down
	}
	for _, tt := range tests {
		got := fmtDuration(tt.d)
		if got != tt.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestHeaderHeight(t *testing.T) {
	h := headerHeight()
	if h != 1 {
		t.Errorf("headerHeight() = %d, want 1", h)
	}
}

func TestActiveHeight(t *testing.T) {
	tests := []struct {
		n    int
		want int
	}{
		{0, 4}, // min 2 lines + 2 border
		{1, 4}, // 1 job + title + 2 border
		{2, 5}, // 2 jobs + title + 2 border
		{5, 8},
	}
	for _, tt := range tests {
		got := activeHeight(tt.n)
		if got != tt.want {
			t.Errorf("activeHeight(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

func TestNew(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	if m.pollInterval != 30*time.Second {
		t.Errorf("pollInterval = %v, want 30s", m.pollInterval)
	}
	if m.active == nil {
		t.Error("active map should be initialized")
	}
	if len(m.spinnerFrames) == 0 {
		t.Error("spinnerFrames should be non-empty")
	}
	if m.now.IsZero() {
		t.Error("now should be set")
	}
}

func TestUpdate_TickEvent(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	initial := m.spinnerIdx
	at := time.Now()

	next, cmd := m.Update(TickEvent{At: at})
	nm := next.(Model)

	if nm.now != at {
		t.Error("now not updated from TickEvent")
	}
	if nm.spinnerIdx != (initial+1)%len(m.spinnerFrames) {
		t.Errorf("spinnerIdx = %d, want %d", nm.spinnerIdx, (initial+1)%len(m.spinnerFrames))
	}
	if cmd == nil {
		t.Error("expected non-nil cmd (next tick) from TickEvent")
	}
}

func TestUpdate_JobStartedAndCompleted(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	start := time.Now()

	// JobStartedEvent adds to active
	next, _ := m.Update(JobStartedEvent{IssueNumber: 42, StageName: "Implement", StartedAt: start})
	nm := next.(Model)
	key42 := activeJobKey("", 42)
	if _, ok := nm.active[key42]; !ok {
		t.Fatal("expected issue 42 in active after JobStartedEvent")
	}
	if nm.active[key42].StageName != "Implement" {
		t.Errorf("StageName = %q", nm.active[key42].StageName)
	}

	// JobCompletedEvent removes from active and adds to history
	end := start.Add(2 * time.Minute)
	next2, _ := nm.Update(JobCompletedEvent{
		IssueNumber: 42,
		StageName:   "Implement",
		Success:     true,
		Duration:    2 * time.Minute,
		CompletedAt: end,
	})
	nm2 := next2.(Model)

	if _, ok := nm2.active[key42]; ok {
		t.Error("expected issue 42 removed from active after JobCompletedEvent")
	}
	if len(nm2.history) != 1 {
		t.Fatalf("history len = %d, want 1", len(nm2.history))
	}
	h := nm2.history[0]
	if h.IssueNumber != 42 || h.StageName != "Implement" || !h.Success {
		t.Errorf("history entry = %+v", h)
	}
	if h.Duration != 2*time.Minute {
		t.Errorf("duration = %v, want 2m", h.Duration)
	}
}

func TestUpdate_LogEvent_UpdatesActiveJob(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	key7 := activeJobKey("", 7)
	m.active[key7] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.activeNumToKey[7] = key7

	next, _ := m.Update(LogEvent{IssueNumber: 7, Tag: "claude", Message: "running prompt\n"})
	nm := next.(Model)

	job, ok := nm.active[key7]
	if !ok {
		t.Fatal("issue 7 missing from active")
	}
	if job.LastTag != "claude" {
		t.Errorf("LastTag = %q, want claude", job.LastTag)
	}
	if job.LastLine != "running prompt" {
		t.Errorf("LastLine = %q, want 'running prompt' (trailing newline stripped)", job.LastLine)
	}
}

func TestUpdate_LogEvent_UnknownIssue(t *testing.T) {
	// LogEvent for an issue not in active map should not panic
	m := New(30, ProjectInfo{}, "")
	next, _ := m.Update(LogEvent{IssueNumber: 999, Tag: "warn", Message: "something\n"})
	nm := next.(Model)
	if _, ok := nm.active[activeJobKey("", 999)]; ok {
		t.Error("unknown issue should not be added to active via LogEvent")
	}
}

func TestUpdate_PollStartedAndCompleted(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	before := time.Now()

	next, _ := m.Update(PollStartedEvent{Owner: "o", Repo: "r", Project: 1})
	nm := next.(Model)
	if !nm.nextPollAt.After(before) {
		t.Error("nextPollAt should be in the future after PollStartedEvent")
	}

	next2, _ := nm.Update(PollCompletedEvent{ItemCount: 5, Dispatched: 2})
	nm2 := next2.(Model)
	if nm2.pollCount != 1 {
		t.Errorf("pollCount = %d, want 1", nm2.pollCount)
	}
}

func TestUpdate_QuitKey(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
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

func TestUpdate_RKey_ActivePane_SetsStatusMsg(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneActive
	key7 := activeJobKey("", 7)
	m.active[key7] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.activeNumToKey[7] = key7

	// r on an active pane item sets statusMsg and returns no cmd.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key in active pane")
	}
	nm := next.(Model)
	if nm.statusMsg == "" {
		t.Error("expected statusMsg to be set after r key in active pane")
	}
}

func TestUpdate_RKey_ActivePane_NoJobs_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneActive
	// No active jobs: r is a no-op

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty active pane")
	}
}

func TestUpdate_RKey_HistoryPane_WithEntry_MissingWorktree(t *testing.T) {
	redirectHistory(t)
	// Chdir to a temp dir so worktree path won't accidentally exist.
	t.Chdir(t.TempDir())
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", StageModel: "sonnet", Success: true},
	}

	// r on a history entry when worktree is missing: nil cmd, statusMsg set.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd when worktree is missing")
	}
	nm := next.(Model)
	if nm.statusMsg == "" {
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
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", StageModel: "sonnet", Success: true},
	}

	// r on a history entry with worktree present: returns non-nil tea.ExecProcess cmd.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("expected non-nil cmd (tea.ExecProcess) from r key with worktree present")
	}
}

func TestUpdate_RKey_HistoryPane_NoEntries_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneHistory
	m.history = nil

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty history pane")
	}
}


func TestView_BeforeWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	// Before width is set, View should return a loading placeholder without panicking
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("expected 'Loading...' placeholder before window size, got %q", v)
	}
}

func TestView_AfterWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{Repo: "owner/repo", CWD: "~/myproject"}, "")
	m.width = 80
	m.height = 24
	m.nextPollAt = time.Now().Add(30 * time.Second)

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
	if !strings.Contains(v, "owner/repo") {
		t.Error("footer should contain repo")
	}
	if !strings.Contains(v, "~/myproject") {
		t.Error("footer should contain CWD")
	}
}

func TestNew_StoresProjectInfo(t *testing.T) {
	info := ProjectInfo{CWD: "~/foo", Repo: "org/bar", Version: "1.2.3"}
	m := New(30, info, "")
	if m.projectInfo != info {
		t.Errorf("projectInfo = %+v, want %+v", m.projectInfo, info)
	}
}

func TestFooterHeight(t *testing.T) {
	if footerHeight() != 1 {
		t.Errorf("footerHeight() = %d, want 1", footerHeight())
	}
}

func TestViewFooter_Content(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", Repo: "org/myapp", Version: "2.0.0"}, "")
	m.width = 120
	footer := m.viewFooter()

	for _, want := range []string{"~/projects/myapp", "org/myapp", "2.0.0"} {
		if !strings.Contains(footer, want) {
			t.Errorf("viewFooter() missing %q; got: %q", want, footer)
		}
	}
}

func TestViewFooter_NoVersion(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", Repo: "org/myapp"}, "")
	m.width = 120
	footer := m.viewFooter()

	if !strings.Contains(footer, "~/projects/myapp") {
		t.Error("footer missing CWD when version is absent")
	}
	if !strings.Contains(footer, "org/myapp") {
		t.Error("footer missing repo when version is absent")
	}
}

func TestViewFooter_Truncation(t *testing.T) {
	// Use a narrow terminal to force truncation.
	m := New(30, ProjectInfo{
		CWD:     "~/very/long/path/to/a/deeply/nested/project/directory",
		Repo:    "some-long-org/some-long-repo-name",
		Version: "99.99.99",
	}, "")
	m.width = 30
	footer := m.viewFooter()

	// Footer must not exceed terminal width (lipgloss.Width excludes ANSI escapes).
	w := lipgloss.Width(footer)
	if w > m.width {
		t.Errorf("footer width %d exceeds terminal width %d", w, m.width)
	}
	// Must contain truncation indicator when content is long.
	if !strings.Contains(footer, "…") {
		t.Errorf("expected truncation ellipsis in narrow footer; got: %q", footer)
	}
}


// TestLayoutHeightInvariant verifies that the total rendered height of View()
// equals m.height for various numbers of active jobs. This ensures the header
// and footer are never pushed off screen when In Progress fills up.
func TestLayoutHeightInvariant(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80
	const termHeight = 24

	for _, n := range []int{0, 1, 7, 8, 15} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			m := New(30, ProjectInfo{}, "")
			// Add n active jobs.
			now := time.Now()
			for i := 0; i < n; i++ {
				m.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
			}
			// Apply window size — this triggers updateHistoryViewport().
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
// the terminal height on terminals where the old history floor of 3 caused
// overflow but the new floor of 1 fixes it.
//
// The invariant is satisfiable only when termHeight ≥ activeHeight(n)+5+1
// (at least 1 history viewport line fits). Each case below satisfies that
// condition with exactly 1 or 2 lines left for history — the exact range
// where the old floor(3) forced overflow but floor(1) does not.
func TestLayoutHeightInvariant_SmallTerminal(t *testing.T) {
	redirectHistory(t)

	const termWidth = 80

	cases := []struct {
		termHeight int
		nActive    int
	}{
		// activeHeight(7)=10; termHeight=16: residual=1; old floor(3) → overflow, new floor(1) → exact
		{16, 7},
		// activeHeight(7)=10; termHeight=17: residual=2; old floor(3) → overflow, new floor(1) → exact
		{17, 7},
		// activeHeight(5)=8; termHeight=14: residual=1; old floor(3) → overflow, new floor(1) → exact
		{14, 5},
		// activeHeight(0)=4; termHeight=10: residual=1; old floor(3) → overflow, new floor(1) → exact
		{10, 0},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("h=%d,n=%d", tc.termHeight, tc.nActive), func(t *testing.T) {
			m := New(30, ProjectInfo{}, "")
			now := time.Now()
			for i := 0; i < tc.nActive; i++ {
				m.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
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

// TestViewHeader_TimerAlwaysVisible verifies that when the status message is
// long enough to trigger truncation, the timer string still appears in the output.
func TestViewHeader_TimerAlwaysVisible(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 60
	m.nextPollAt = time.Now().Add(90 * time.Second)

	// Status long enough to overflow without truncation.
	m.statusLine = "this is a very long status message that should be truncated by the header renderer"

	header := m.viewHeader()

	// The timer prefix must be visible.
	if !strings.Contains(header, "poll in") {
		t.Errorf("viewHeader() does not contain timer; got: %q", header)
	}
}

// TestViewHeader_WidthNeverExceedsTerminal verifies that lipgloss.Width of
// viewHeader() never exceeds m.width for various status line lengths.
func TestViewHeader_WidthNeverExceedsTerminal(t *testing.T) {
	cases := []struct {
		name       string
		width      int
		statusLine string
	}{
		{"empty status", 80, ""},
		{"short status", 80, "syncing"},
		{"boundary status", 80, strings.Repeat("x", 60)},
		{"very long status", 80, strings.Repeat("x", 200)},
		{"narrow terminal empty", 30, ""},
		{"narrow terminal long", 30, strings.Repeat("x", 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "")
			m.width = tc.width
			m.nextPollAt = time.Now().Add(90 * time.Second)
			m.statusLine = tc.statusLine

			header := m.viewHeader()
			w := lipgloss.Width(header)
			if w > tc.width {
				t.Errorf("viewHeader() width %d exceeds terminal width %d; status=%q",
					w, tc.width, tc.statusLine)
			}
		})
	}
}


// TestInit verifies Init returns a non-nil cmd (the initial tick).
func TestInit(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init() should return a non-nil cmd")
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

// TestLoadHistory_MalformedJSON verifies LoadHistory returns nil on bad JSON.
func TestLoadHistory_MalformedJSON(t *testing.T) {
	redirectHistory(t)
	if err := os.WriteFile(HistoryPathOverride, []byte("not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	entries := LoadHistory()
	if entries != nil {
		t.Error("expected nil from LoadHistory with malformed JSON")
	}
}

// TestLoadHistory_RoundTrip verifies history entries survive a save/load cycle.
func TestLoadHistory_RoundTrip(t *testing.T) {
	redirectHistory(t)
	entries := []HistoryEntry{
		{IssueNumber: 1, StageName: "Research", Success: true},
		{IssueNumber: 2, StageName: "Implement", Success: false, IsComment: true},
	}
	SaveHistory(entries)
	loaded := LoadHistory()
	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}
	if loaded[0].IssueNumber != 1 || loaded[1].IsComment != true {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}
}

// TestUpdate_TabKey_SwitchesPanes verifies tab toggles focus between panes.
func TestUpdate_TabKey_SwitchesPanes(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
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

// TestUpdate_JK_HistoryPane verifies j/k navigation in the history pane.
func TestUpdate_JK_HistoryPane(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research"},
		{IssueNumber: 2, StageName: "Plan"},
		{IssueNumber: 3, StageName: "Implement"},
	}

	// j increments histIdx
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	nm := next.(Model)
	if nm.histIdx != 1 {
		t.Errorf("histIdx = %d after j, want 1", nm.histIdx)
	}
	// k decrements histIdx
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	nm2 := next2.(Model)
	if nm2.histIdx != 0 {
		t.Errorf("histIdx = %d after k, want 0", nm2.histIdx)
	}
	// k at 0 is a no-op
	next3, _ := nm2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	nm3 := next3.(Model)
	if nm3.histIdx != 0 {
		t.Errorf("histIdx = %d after k at 0, want 0", nm3.histIdx)
	}
}

// TestUpdate_JK_ActivePane verifies j/k navigation in the active pane.
func TestUpdate_JK_ActivePane(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.active[activeJobKey("", 1)] = &activeJob{StageName: "Research", StartedAt: time.Now()}
	m.active[activeJobKey("", 2)] = &activeJob{StageName: "Plan", StartedAt: time.Now()}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	nm := next.(Model)
	if nm.activeIdx != 1 {
		t.Errorf("activeIdx = %d after j, want 1", nm.activeIdx)
	}
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	nm2 := next2.(Model)
	if nm2.activeIdx != 0 {
		t.Errorf("activeIdx = %d after k, want 0", nm2.activeIdx)
	}
}

// TestUpdate_CKey_DeletesHistoryEntry verifies c removes the selected entry.
func TestUpdate_CKey_DeletesHistoryEntry(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research"},
		{IssueNumber: 2, StageName: "Plan"},
	}
	// histIdx=0 → realIdx = len-1-0 = 1 → deletes entry at index 1 (IssueNumber 2)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	nm := next.(Model)
	if len(nm.history) != 1 {
		t.Errorf("history len = %d after c, want 1", len(nm.history))
	}
	if nm.history[0].IssueNumber != 1 {
		t.Errorf("remaining entry IssueNumber = %d, want 1", nm.history[0].IssueNumber)
	}
}

// TestUpdate_CKey_EmptyHistory_NoOp verifies c is a no-op with no history.
func TestUpdate_CKey_EmptyHistory_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneHistory
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if cmd != nil {
		t.Error("expected nil cmd from c with empty history")
	}
}

// TestUpdate_ScrollToVisible verifies that navigating down past the visible
// viewport area scrolls the YOffset down, and navigating back up scrolls it up.
func TestUpdate_ScrollToVisible(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory

	// Fill history with enough entries to exceed viewport height.
	// historyHeight = max(24 - 1 - activeHeight(0) - 1 - 3, 3) = max(24-1-2-1-3, 3) = 17
	const numEntries = 30
	for i := 0; i < numEntries; i++ {
		m.history = append(m.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research"})
	}
	// Initialise viewport with scroll-to-visible (histIdx=0 at top).
	m.updateHistoryViewport(false)
	if m.historyVP.YOffset != 0 {
		t.Fatalf("initial YOffset = %d, want 0", m.historyVP.YOffset)
	}

	// Navigate down far enough to push histIdx below the visible area.
	// viewport height reported by historyVP.Height after updateHistoryViewport.
	vpHeight := m.historyVP.Height
	if vpHeight < 1 {
		t.Fatalf("viewport height = %d, expected > 0", vpHeight)
	}

	cur := m
	for i := 0; i < vpHeight; i++ {
		next, _ := cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		cur = next.(Model)
	}
	// histIdx is now == vpHeight (one past the last visible line when YOffset was 0).
	if cur.histIdx != vpHeight {
		t.Fatalf("histIdx = %d after %d j presses, want %d", cur.histIdx, vpHeight, vpHeight)
	}
	if cur.historyVP.YOffset == 0 {
		t.Errorf("YOffset still 0 after navigating below visible area; want > 0")
	}
	if cur.histIdx > cur.historyVP.YOffset+cur.historyVP.Height-1 {
		t.Errorf("histIdx %d not visible: YOffset=%d Height=%d",
			cur.histIdx, cur.historyVP.YOffset, cur.historyVP.Height)
	}

	// Navigate back up past the top of the current viewport.
	savedOffset := cur.historyVP.YOffset
	for i := 0; i < vpHeight; i++ {
		next, _ := cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		cur = next.(Model)
	}
	if cur.historyVP.YOffset >= savedOffset {
		t.Errorf("YOffset %d did not retreat from %d after navigating up", cur.historyVP.YOffset, savedOffset)
	}
	if cur.histIdx < cur.historyVP.YOffset {
		t.Errorf("histIdx %d above viewport: YOffset=%d", cur.histIdx, cur.historyVP.YOffset)
	}
}

// TestUpdate_JobCompletedScrollsToTop verifies that receiving a JobCompletedEvent
// resets the viewport to the top regardless of the current selection position.
func TestUpdate_JobCompletedScrollsToTop(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory

	// Pre-populate history and navigate the selection down.
	for i := 0; i < 20; i++ {
		m.history = append(m.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research"})
	}
	m.updateHistoryViewport(false)
	m.histIdx = 10
	m.updateHistoryViewport(false)
	if m.historyVP.YOffset == 0 {
		// Force a non-zero offset to make the assertion meaningful.
		m.historyVP.SetYOffset(5)
	}

	// Fire a JobCompletedEvent — should scroll back to top.
	next, _ := m.Update(JobCompletedEvent{
		IssueNumber: 99,
		StageName:   "Implement",
		Success:     true,
		Completed:   true,
		CompletedAt: time.Now(),
	})
	nm := next.(Model)
	if nm.historyVP.YOffset != 0 {
		t.Errorf("YOffset = %d after JobCompletedEvent, want 0", nm.historyVP.YOffset)
	}
}

// TestUpdate_CapitalC_ClearAll verifies two C presses clear all history.
func TestUpdate_CapitalC_ClearAll(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research"},
		{IssueNumber: 2, StageName: "Plan"},
	}
	// First C sets confirmClear
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
	nm := next.(Model)
	if !nm.confirmClear {
		t.Error("expected confirmClear=true after first C")
	}
	if len(nm.history) != 2 {
		t.Error("history should not be cleared after first C")
	}
	// Second C confirms and clears
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
	nm2 := next2.(Model)
	if nm2.confirmClear {
		t.Error("expected confirmClear=false after confirmed C")
	}
	if len(nm2.history) != 0 {
		t.Errorf("history len = %d after confirmed clear, want 0", len(nm2.history))
	}
}

// TestUpdate_QuitDuringConfirmClear_Cancels verifies q cancels confirmClear state.
func TestUpdate_QuitDuringConfirmClear_Cancels(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.confirmClear = true
	m.focusPane = paneHistory
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	nm := next.(Model)
	if nm.confirmClear {
		t.Error("expected confirmClear=false after q during confirmation")
	}
	if cmd != nil {
		t.Error("expected nil cmd (no quit) when q pressed during confirm")
	}
}

// TestUpdate_NKey_CancelsConfirmClear verifies n cancels the clear confirmation.
func TestUpdate_NKey_CancelsConfirmClear(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.confirmClear = true
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	nm := next.(Model)
	if nm.confirmClear {
		t.Error("expected confirmClear=false after n")
	}
}

// TestUpdate_EnterL_HistoryPane_NoLogDir verifies enter returns nil when log dir is missing.
func TestUpdate_EnterL_HistoryPane_NoLogDir(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 99999, StageName: "Research"},
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("expected nil cmd from enter when log dir doesn't exist")
	}
}

// TestUpdate_EnterKey_ActivePane_TogglesDetailPanel verifies enter in active pane toggles the detail panel.
func TestUpdate_EnterKey_ActivePane_TogglesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive
	m.active[activeJobKey("", 99999)] = &activeJob{StageName: "Research", StartedAt: time.Now()}

	// First enter: panel opens.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(Model)
	if cmd != nil {
		t.Error("enter key should return nil cmd (no subprocess)")
	}
	if !nm.detailPanel {
		t.Error("expected detailPanel=true after first enter")
	}

	// Second enter: panel closes.
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm2 := next2.(Model)
	if nm2.detailPanel {
		t.Error("expected detailPanel=false after second enter")
	}
}

// TestUpdate_LogEvent_IssueZero_StatusLine verifies poll-level log events update statusLine.
func TestUpdate_LogEvent_IssueZero_StatusLine(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	next, _ := m.Update(LogEvent{IssueNumber: 0, Tag: "poll", Message: "polling now\n"})
	nm := next.(Model)
	if nm.statusLine != "[poll] polling now" {
		t.Errorf("statusLine = %q, want %q", nm.statusLine, "[poll] polling now")
	}
}

// TestTuiReadSessionID_NotFound verifies empty string for a missing session file.
func TestTuiReadSessionID_NotFound(t *testing.T) {
	id := tuiReadSessionID("", 99999, "SomeStageThatDoesNotExist")
	if id != "" {
		t.Errorf("expected empty string for missing session, got %q", id)
	}
}

// TestUpdate_LKey_Returns_WatchCmd verifies the l key returns a non-nil tea.ExecProcess cmd.
func TestUpdate_LKey_Returns_WatchCmd(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneActive
	m.active[activeJobKey("", 7)] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.activeNumToKey[7] = activeJobKey("", 7)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if cmd == nil {
		t.Error("expected non-nil cmd (tea.ExecProcess) from l key with active job")
	}
}

// TestViewHistory_ConfirmClear verifies the confirmation prompt is shown.
func TestViewHistory_ConfirmClear(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history = []HistoryEntry{{IssueNumber: 1, StageName: "Research"}}
	m.confirmClear = true
	view := m.viewHistory()
	if !strings.Contains(view, "Clear all history") {
		t.Errorf("expected confirmation text in viewHistory, got: %q", view)
	}
}

// TestViewActive_IsComment verifies the 💬 emoji appears for comment jobs.
func TestViewActive_IsComment(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.active[activeJobKey("", 5)] = &activeJob{StageName: "Implement", IsComment: true, StartedAt: time.Now()}
	view := m.viewActive()
	if !strings.Contains(view, "💬") {
		t.Errorf("expected 💬 in viewActive for IsComment job, got: %q", view)
	}
}

// TestViewHistory_IsComment verifies the 💬 emoji appears for comment history entries.
func TestViewHistory_IsComment(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.history = []HistoryEntry{{IssueNumber: 5, StageName: "Implement", IsComment: true, Success: true}}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	view := m.viewHistory()
	if !strings.Contains(view, "💬") {
		t.Errorf("expected 💬 in viewHistory for IsComment entry, got: %q", view)
	}
}

// TestViewHeader_WithStatusLine verifies statusLine content appears in the header.
func TestViewHeader_WithStatusLine(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.statusLine = "some status"
	header := m.viewHeader()
	if !strings.Contains(header, "some status") {
		t.Errorf("header missing statusLine, got: %q", header)
	}
}

// TestViewHeader_StatusLineTruncation verifies narrow headers truncate statusLine without panic.
func TestViewHeader_StatusLineTruncation(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 25
	m.statusLine = "a very very very very very very long status message"
	header := m.viewHeader()
	w := lipgloss.Width(header)
	if w > m.width+5 {
		t.Errorf("header too wide: %d > %d", w, m.width)
	}
}

// TestUpdate_RKey_HistoryPane_ActiveIssue_SetsStatusMsg verifies that r on a history entry
// for an issue that is currently active shows a status message and returns no cmd.
func TestUpdate_RKey_HistoryPane_ActiveIssue_SetsStatusMsg(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneHistory
	// Add issue 42 to both active map and history.
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}
	m.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", Success: true},
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd when issue is active")
	}
	nm := next.(Model)
	if nm.statusMsg == "" {
		t.Error("expected statusMsg to be set when issue is actively running")
	}
}
