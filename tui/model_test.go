package tui

import (
	"fmt"
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
	m := New(30, ProjectInfo{}, "", "")
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
	m := New(30, ProjectInfo{}, "", "")
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
	m := New(30, ProjectInfo{}, "", "")
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
	m := New(30, ProjectInfo{}, "", "")
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
	m := New(30, ProjectInfo{}, "", "")
	next, _ := m.Update(LogEvent{IssueNumber: 999, Tag: "warn", Message: "something\n"})
	nm := next.(Model)
	if _, ok := nm.active[activeJobKey("", 999)]; ok {
		t.Error("unknown issue should not be added to active via LogEvent")
	}
}

func TestUpdate_PollStartedAndCompleted(t *testing.T) {
	m := New(30, ProjectInfo{}, "", "")
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
	m := New(30, ProjectInfo{}, "", "")
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

func TestUpdate_RKey_ActivePane_AliasesLogViewer(t *testing.T) {
	m := New(30, ProjectInfo{}, "", "")
	m.focusPane = paneActive
	key7 := activeJobKey("", 7)
	m.active[key7] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.activeNumToKey[7] = key7

	// r on an active pane item delegates to the log viewer (same as enter/l).
	// openLogViewerCmd returns nil when the log dir is empty, so we just verify
	// the key is handled (no panic) and the model is returned.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if _, ok := next.(Model); !ok {
		t.Error("expected Model returned from Update with r key in active pane")
	}
}

func TestUpdate_RKey_ActivePane_NoJobs_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "", "")
	m.focusPane = paneActive
	// No active jobs: r is a no-op

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty active pane")
	}
}

func TestUpdate_RKey_HistoryPane_WithEntry(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", "")
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", StageModel: "sonnet", Success: true},
	}

	// r on a history entry calls openResumeCmd — non-nil cmd returned
	// (openResumeCmd returns an error-message terminal cmd when worktree is missing)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("expected non-nil cmd from r key with history entry")
	}
}

func TestUpdate_RKey_HistoryPane_NoEntries_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "", "")
	m.focusPane = paneHistory
	m.history = nil

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty history pane")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"path/with spaces/dir", "'path/with spaces/dir'"},
		{"it's a test", "'it'\\''s a test'"},
		{"", "''"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestView_BeforeWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{}, "", "")
	// Before width is set, View should return a loading placeholder without panicking
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("expected 'Loading...' placeholder before window size, got %q", v)
	}
}

func TestView_AfterWindowSize(t *testing.T) {
	m := New(30, ProjectInfo{Repo: "owner/repo", CWD: "~/myproject"}, "", "")
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
	m := New(30, info, "", "")
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
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", Repo: "org/myapp", Version: "2.0.0"}, "", "")
	m.width = 120
	footer := m.viewFooter()

	for _, want := range []string{"~/projects/myapp", "org/myapp", "2.0.0"} {
		if !strings.Contains(footer, want) {
			t.Errorf("viewFooter() missing %q; got: %q", want, footer)
		}
	}
}

func TestViewFooter_NoVersion(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", Repo: "org/myapp"}, "", "")
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
	}, "", "")
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

// TestOpenTerminalCmd_UnknownID verifies that an unknown terminal ID does not
// panic and returns a non-nil tea.Cmd (the fallback path runs).
// The returned Cmd is intentionally not executed — doing so may launch GUI
// processes (osascript, terminal emulators) during go test.
func TestOpenTerminalCmd_UnknownID(t *testing.T) {
	m := New(30, ProjectInfo{}, "totally_unknown_terminal", "")
	cmd := m.openTerminalCmd("echo hello")
	if cmd == nil {
		t.Error("expected non-nil tea.Cmd for unknown terminal ID")
	}
}

// TestOpenTerminalCmd_KnownIDs verifies that known terminal IDs return non-nil Cmds.
func TestOpenTerminalCmd_KnownIDs(t *testing.T) {
	for _, id := range []string{"terminal", "iterm2", "ghostty", "kitty", "alacritty", "warp", ""} {
		m := New(30, ProjectInfo{}, id, "")
		cmd := m.openTerminalCmd("echo hello")
		if cmd == nil {
			t.Errorf("terminal %q: expected non-nil tea.Cmd", id)
		}
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
			m := New(30, ProjectInfo{}, "", "")
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

// TestDetectTerminalFromEnv is in the cmd package; here we just verify the
// terminal field is stored on the model correctly.
func TestNew_TerminalStored(t *testing.T) {
	for _, id := range []string{"", "ghostty", "iterm2"} {
		m := New(30, ProjectInfo{}, id, "")
		if m.terminal != id {
			t.Errorf("New(30, ProjectInfo{}, %q, \"\"): got terminal=%q, want %q", id, m.terminal, id)
		}
	}
}
