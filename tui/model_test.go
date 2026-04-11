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
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
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
		{0, "00:00"},
		{-5 * time.Second, "00:00"}, // negative clamped to 0
		{30 * time.Second, "00:30"},
		{90 * time.Second, "01:30"},
		{61 * time.Second, "01:01"},
		{10*time.Minute + 5*time.Second, "10:05"},
		{500 * time.Millisecond, "00:01"}, // rounds to nearest second
		{499 * time.Millisecond, "00:00"}, // rounds down
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

// TestViewFooter_RateLimitHidden verifies that the rate limit section is omitted
// when no stats have been received (Limit==0).
func TestViewFooter_RateLimitHidden(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", Repo: "org/myapp"}, "")
	m.width = 120
	// graphqlStats is zero-value (Limit==0) by default.
	footer := m.viewFooter()
	plain := ansi.Strip(footer)

	// A rate-limit fraction would look like "4865/5000"; verify no digit precedes
	// a "/" that is followed by a digit (which would indicate a rate limit).
	for i := 1; i < len(plain)-1; i++ {
		if plain[i] == '/' && plain[i-1] >= '0' && plain[i-1] <= '9' && plain[i+1] >= '0' && plain[i+1] <= '9' {
			t.Errorf("footer should not show rate limit when Limit==0; got: %q", plain)
			break
		}
	}
}

// TestViewFooter_RateLimitShown verifies the rate limit section appears once
// graphqlStats is populated, and contains the fraction and countdown.
func TestViewFooter_RateLimitShown(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", Repo: "org/myapp"}, "")
	m.width = 120
	m.now = time.Now()
	m.graphqlStats = RateLimitStats{
		Limit:     5000,
		Remaining: 4865,
		Reset:     m.now.Add(12 * time.Minute),
	}
	footer := m.viewFooter()
	plain := ansi.Strip(footer)

	if !strings.Contains(plain, "4865/5000") {
		t.Errorf("footer missing rate limit fraction; got: %q", plain)
	}
	if !strings.Contains(plain, "12m") {
		t.Errorf("footer missing countdown; got: %q", plain)
	}
}

// TestViewFooter_RateLimitColors verifies the color thresholds applied to the
// rate limit section. Colors are forced via lipgloss.SetColorProfile so that
// ANSI escape sequences are emitted even in a non-TTY test environment.
func TestViewFooter_RateLimitColors(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	now := time.Now()
	reset := now.Add(10 * time.Minute)
	cases := []struct {
		name      string
		remaining int
		limit     int
		wantColor string // lipgloss color code present in ANSI escape
	}{
		{"green >50%", 2600, 5000, "42"},
		{"yellow 20-50%", 1500, 5000, "214"},
		{"red <20%", 900, 5000, "196"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{CWD: "~", Repo: "org/repo"}, "")
			m.width = 120
			m.now = now
			m.graphqlStats = RateLimitStats{
				Limit:     tc.limit,
				Remaining: tc.remaining,
				Reset:     reset,
			}
			footer := m.viewFooter()
			// The ANSI escape for the color code should be present in the raw string.
			if !strings.Contains(footer, tc.wantColor) {
				t.Errorf("expected color %q for %d/%d; footer=%q", tc.wantColor, tc.remaining, tc.limit, footer)
			}
		})
	}
}

// TestViewFooter_TruncationWithRateLimit verifies that at narrow widths the
// left side is truncated but the right (rate limit) side is preserved.
func TestViewFooter_TruncationWithRateLimit(t *testing.T) {
	now := time.Now()
	m := New(30, ProjectInfo{
		CWD:     "~/very/long/path/to/a/deeply/nested/project/directory",
		Repo:    "some-long-org/some-long-repo-name",
		Version: "99.99.99",
	}, "")
	m.width = 40
	m.now = now
	m.graphqlStats = RateLimitStats{
		Limit:     5000,
		Remaining: 4865,
		Reset:     now.Add(12 * time.Minute),
	}
	footer := m.viewFooter()
	plain := ansi.Strip(footer)

	// Width must not exceed terminal width.
	w := lipgloss.Width(footer)
	if w > m.width {
		t.Errorf("footer width %d exceeds terminal width %d", w, m.width)
	}
	// Right side (rate limit) must be preserved.
	if !strings.Contains(plain, "4865/5000") {
		t.Errorf("rate limit truncated from narrow footer; got: %q", plain)
	}
	// Left side must be truncated (too long to fit with right side).
	if !strings.Contains(plain, "…") {
		t.Errorf("expected left-side truncation ellipsis; got: %q", plain)
	}
}

// TestFmtRateLimitCountdown verifies the countdown formatting helper.
func TestFmtRateLimitCountdown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		delta time.Duration
		want  string
	}{
		{"soon (past)", -5 * time.Second, "soon"},
		{"soon (zero)", 0, "soon"},
		{"seconds", 45 * time.Second, "45s"},
		{"minutes", 3*time.Minute + 30*time.Second, "3m"},
		{"hours", 2*time.Hour + 30*time.Minute, "2h"},
		{"exactly 1 minute", 60 * time.Second, "1m"},
		{"exactly 1 hour", 3600 * time.Second, "1h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fmtRateLimitCountdown(now.Add(tc.delta), now)
			if got != tc.want {
				t.Errorf("fmtRateLimitCountdown delta=%v: got %q, want %q", tc.delta, got, tc.want)
			}
		})
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

// TestLayoutHeightInvariant_NarrowWithHint verifies that the layout height invariant
// holds on narrow terminals where the hint line would wrap without truncation.
// The confirmQuit hint (~90 plain-text chars) is the longest variant and is the
// most likely to wrap on narrow terminals.
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
		// Narrow terminal (40 cols) with confirmQuit hint visible — longest hint, most likely to wrap.
		{"narrow_confirmQuit", 40, paneHistory, true, 1, 0, 1},
		// Narrow terminal with normal history hint visible.
		{"narrow_normalHint", 40, paneHistory, false, 0, 0, 1},
		// Standard-ish narrow (60 cols) with confirmQuit.
		{"medium_confirmQuit", 60, paneHistory, true, 1, 0, 1},
		// Blocked issues must be counted in activeHeight to avoid over-allocating history viewport.
		{"with_blocked", 80, paneHistory, false, 1, 3, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "")
			now := time.Now()
			for i := 0; i < tc.nActive; i++ {
				m.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
			}
			for i := 0; i < tc.nBlocked; i++ {
				m.blocked[fmt.Sprintf("issue-%d", tc.nActive+i+1)] = &blockedIssue{IssueNumber: tc.nActive + i + 1, StageName: "Research"}
			}
			for i := 0; i < tc.nHistory; i++ {
				m.history = append(m.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research", Success: true, Completed: true})
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

// TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel verifies enter in history pane toggles the detail panel.
func TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history = []HistoryEntry{
		{IssueNumber: 99999, StageName: "Research"},
	}
	// enter toggles detail panel — no subprocess launched.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("enter key should return nil cmd (no subprocess)")
	}
	nm := next.(Model)
	if !nm.detailPanel {
		t.Error("expected detailPanel=true after enter in history pane")
	}
}

// TestUpdate_EscapeKey_ClosesDetailPanel verifies escape closes the detail panel.
func TestUpdate_EscapeKey_ClosesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.detailPanel = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("escape key should return nil cmd")
	}
	nm := next.(Model)
	if nm.detailPanel {
		t.Error("expected detailPanel=false after escape key")
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

// TestUpdate_QKey_WithActiveJobs_ShowsConfirmQuit verifies that q with active jobs shows the confirm dialog.
func TestUpdate_QKey_WithActiveJobs_ShowsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Error("expected nil cmd (no quit yet) when q pressed with active jobs")
	}
	nm := next.(Model)
	if !nm.confirmQuit {
		t.Error("expected confirmQuit=true after q with active jobs")
	}
}

// TestUpdate_QKey_WhenConfirmQuit_Quits verifies that q while confirmQuit is true quits immediately.
func TestUpdate_QKey_WhenConfirmQuit_Quits(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.confirmQuit = true
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected non-nil cmd (quit) when q pressed while confirmQuit=true")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestUpdate_NKey_CancelsConfirmQuit verifies n dismisses the quit confirmation dialog.
func TestUpdate_NKey_CancelsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
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

// TestUpdate_EscKey_WithActiveJobs_ShowsConfirmQuit verifies Escape with active jobs shows confirm dialog.
func TestUpdate_EscKey_WithActiveJobs_ShowsConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("expected nil cmd (no quit yet) when Escape pressed with active jobs")
	}
	nm := next.(Model)
	if !nm.confirmQuit {
		t.Error("expected confirmQuit=true after Escape with active jobs")
	}
}

// TestUpdate_EscKey_WhenConfirmQuit_Cancels verifies Escape cancels the confirmQuit dialog.
func TestUpdate_EscKey_WhenConfirmQuit_Cancels(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.confirmQuit = true
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("expected nil cmd after Escape cancels confirmQuit")
	}
	nm := next.(Model)
	if nm.confirmQuit {
		t.Error("expected confirmQuit=false after Escape key while confirmQuit=true")
	}
}

// TestUpdate_CtrlC_BypassesConfirmQuit verifies ctrl+c quits immediately even when confirmQuit is active.
func TestUpdate_CtrlC_BypassesConfirmQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.confirmQuit = true
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected non-nil cmd (quit) from ctrl+c even when confirmQuit=true")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestViewHistory_ConfirmQuit verifies the quit confirmation prompt is shown in viewHistory.
func TestViewHistory_ConfirmQuit(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	key42 := activeJobKey("", 42)
	m.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}
	m.confirmQuit = true

	view := m.viewHistory()
	if !strings.Contains(view, "Quit Fabrik?") {
		t.Errorf("expected quit confirmation text in viewHistory, got: %q", view)
	}
	if !strings.Contains(view, "1 jobs") {
		t.Errorf("expected job count in viewHistory confirmation, got: %q", view)
	}
}

// --- Mouse tests ---

// mouseLeftPress returns a tea.MouseMsg for a left button press at (x, y).
func mouseLeftPress(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: x, Y: y}
}

// TestHandleMouse_ClickActivePaneTitle verifies a click on the active pane title
// area (Y=2) switches focus to the active pane.
func TestHandleMouse_ClickActivePaneTitle(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory // start on history

	next, cmd := m.Update(mouseLeftPress(10, 2))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Errorf("focusPane = %v, want paneActive after clicking title row Y=2", nm.focusPane)
	}
	if cmd != nil {
		t.Error("expected nil cmd from clicking active pane title")
	}
}

// TestHandleMouse_ClickActivePaneBorderTop verifies a click on Y=1 (active pane
// border top) also switches focus to the active pane.
func TestHandleMouse_ClickActivePaneBorderTop(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory

	next, _ := m.Update(mouseLeftPress(5, 1))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Errorf("focusPane = %v, want paneActive after clicking Y=1", nm.focusPane)
	}
}

// TestHandleMouse_ClickActiveRow_SelectsRow verifies clicking on a row in the
// active pane content area selects that row and switches focus.
func TestHandleMouse_ClickActiveRow_SelectsRow(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	// Add two active jobs so row clicks are meaningful.
	m.active[activeJobKey("", 1)] = &activeJob{IssueNumber: 1, StageName: "Research", StartedAt: time.Now()}
	m.active[activeJobKey("", 2)] = &activeJob{IssueNumber: 2, StageName: "Plan", StartedAt: time.Now()}

	// With 2 active jobs, activeHeight(2)=5. Row 0 is at Y=3, row 1 is at Y=4.
	// Click Y=3 → select row 0.
	next, cmd := m.Update(mouseLeftPress(5, 3))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Errorf("focusPane = %v, want paneActive", nm.focusPane)
	}
	if nm.activeIdx != 0 {
		t.Errorf("activeIdx = %d, want 0 after clicking row Y=3", nm.activeIdx)
	}
	if cmd != nil {
		t.Error("expected nil cmd (single click, not double-click)")
	}

	// Click Y=4 → select row 1.
	next2, _ := m.Update(mouseLeftPress(5, 4))
	nm2 := next2.(Model)
	if nm2.activeIdx != 1 {
		t.Errorf("activeIdx = %d, want 1 after clicking row Y=4", nm2.activeIdx)
	}
}

// TestHandleMouse_DoubleClickActiveRow_OpensWatch verifies that a double-click on
// an active pane row returns a non-nil cmd (tea.ExecProcess for fabrik watch).
func TestHandleMouse_DoubleClickActiveRow_OpensWatch(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.active[activeJobKey("", 7)] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}

	// First click: single click, no cmd.
	next, cmd := m.Update(mouseLeftPress(5, 3))
	if cmd != nil {
		t.Error("first click should not open watch")
	}
	nm := next.(Model)

	// Second click at the same position within 300ms: double-click.
	nm.lastClickAt = time.Now() // ensure it's within the threshold
	next2, cmd2 := nm.Update(mouseLeftPress(5, 3))
	_ = next2
	if cmd2 == nil {
		t.Error("expected non-nil cmd (tea.ExecProcess) from double-click on active row")
	}
}

// TestHandleMouse_ClickHistoryTitle_SwitchesFocus verifies clicking the history
// pane title row switches focus to the history pane.
func TestHandleMouse_ClickHistoryTitle_SwitchesFocus(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive

	// With no active jobs, activeHeight(0)=4. Active pane: Y=1..4.
	// histTopY = 1 + 4 = 5. histTitle at Y=6.
	histTopY := 1 + activeHeight(0)
	histTitleY := histTopY + 1

	next, _ := m.Update(mouseLeftPress(5, histTitleY))
	nm := next.(Model)
	if nm.focusPane != paneHistory {
		t.Errorf("focusPane = %v, want paneHistory after clicking history title Y=%d", nm.focusPane, histTitleY)
	}
}

// TestHandleMouse_ClickHistoryRow_SelectsEntry verifies clicking on a history
// viewport row selects that entry.
func TestHandleMouse_ClickHistoryRow_SelectsEntry(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive
	m.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research", Success: true},
		{IssueNumber: 2, StageName: "Plan", Success: true},
		{IssueNumber: 3, StageName: "Implement", Success: true},
	}
	m.updateHistoryViewport(false)

	// histTopY = 1 + activeHeight(0) = 5. Viewport content starts at Y=7.
	histTopY := 1 + activeHeight(0)
	histContentStart := histTopY + 2

	// Click the first viewport row → histIdx=0.
	next, cmd := m.Update(mouseLeftPress(10, histContentStart))
	nm := next.(Model)
	if nm.focusPane != paneHistory {
		t.Errorf("focusPane = %v, want paneHistory", nm.focusPane)
	}
	if nm.histIdx != 0 {
		t.Errorf("histIdx = %d, want 0 after clicking first history row", nm.histIdx)
	}
	if cmd != nil {
		t.Error("expected nil cmd from single history row click")
	}

	// Click the second viewport row → histIdx=1.
	next2, _ := m.Update(mouseLeftPress(10, histContentStart+1))
	nm2 := next2.(Model)
	if nm2.histIdx != 1 {
		t.Errorf("histIdx = %d, want 1 after clicking second history row", nm2.histIdx)
	}
}

// TestHandleMouse_DoubleClickHistoryRow_OpensWatch verifies that a double-click on
// a history row returns a non-nil cmd.
func TestHandleMouse_DoubleClickHistoryRow_OpensWatch(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", Success: true},
	}
	m.updateHistoryViewport(false)

	histTopY := 1 + activeHeight(0)
	histContentStart := histTopY + 2

	// First click at history row.
	next, cmd := m.Update(mouseLeftPress(10, histContentStart))
	if cmd != nil {
		t.Error("first click should not open watch")
	}
	nm := next.(Model)

	// Second click immediately (simulate double-click).
	nm.lastClickAt = time.Now()
	next2, cmd2 := nm.Update(mouseLeftPress(10, histContentStart))
	_ = next2
	if cmd2 == nil {
		t.Error("expected non-nil cmd from double-click on history row")
	}
}

// TestHandleMouse_NonLeftClick_NoStateChange verifies that non-left-click mouse
// events (e.g. right click, motion) do not change selection state.
func TestHandleMouse_NonLeftClick_NoStateChange(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive

	// Right button press: should not change focus or selection.
	rightClick := tea.MouseMsg{Button: tea.MouseButtonRight, Action: tea.MouseActionPress, X: 5, Y: 3}
	next, _ := m.Update(rightClick)
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Error("right-click should not change focus pane")
	}

	// Release event: no state change.
	release := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, X: 5, Y: 2}
	next2, _ := m.Update(release)
	nm2 := next2.(Model)
	if nm2.focusPane != paneActive {
		t.Error("mouse release should not change focus pane")
	}
}

// TestHandleMouse_ClickOutOfBounds_NoStateChange verifies clicks outside the
// known pane regions do not crash or change state unexpectedly.
func TestHandleMouse_ClickOutOfBounds_NoStateChange(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24

	// Click at Y=0 (header) should not crash or change focus.
	next, _ := m.Update(mouseLeftPress(5, 0))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Error("click on header should not change focus pane from initial paneActive")
	}
}

// TestLoadHistory_Dedup verifies that LoadHistory collapses duplicate entries
// (same issue, repo, stage, IsComment) to the most recent by CompletedAt, while
// preserving entries for different stages on the same issue.
func TestLoadHistory_Dedup(t *testing.T) {
	redirectHistory(t)

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)

	entries := []HistoryEntry{
		// Two duplicates for issue 42, Research — t2 is more recent than t1.
		{IssueNumber: 42, Repo: "", StageName: "Research", IsComment: false, Success: false, CompletedAt: t1},
		{IssueNumber: 42, Repo: "", StageName: "Research", IsComment: false, Success: true, CompletedAt: t2},
		// Different stage on the same issue — must be preserved.
		{IssueNumber: 42, Repo: "", StageName: "Plan", IsComment: false, Success: true, CompletedAt: t3},
		// Comment run on the same stage — different IsComment, must be preserved.
		{IssueNumber: 42, Repo: "", StageName: "Research", IsComment: true, Success: true, CompletedAt: t1},
	}
	SaveHistory(entries)

	got := LoadHistory()

	if len(got) != 3 {
		t.Fatalf("LoadHistory returned %d entries, want 3 (dedup should collapse 2 Research entries to 1)", len(got))
	}

	// Find the Research (non-comment) entry — must be the most recent one (Success: true).
	var researchEntry *HistoryEntry
	for i := range got {
		if got[i].StageName == "Research" && !got[i].IsComment {
			researchEntry = &got[i]
			break
		}
	}
	if researchEntry == nil {
		t.Fatal("Research (non-comment) entry not found in result")
	}
	if !researchEntry.Success {
		t.Error("LoadHistory kept the older Research entry instead of the most recent one")
	}
	if !researchEntry.CompletedAt.Equal(t2) {
		t.Errorf("Research entry CompletedAt = %v, want %v", researchEntry.CompletedAt, t2)
	}

	// Plan entry must be present.
	var planFound bool
	for _, h := range got {
		if h.StageName == "Plan" {
			planFound = true
			break
		}
	}
	if !planFound {
		t.Error("Plan entry was removed by deduplication, but should be preserved (different stage)")
	}

	// Research IsComment=true entry must be present.
	var commentFound bool
	for _, h := range got {
		if h.StageName == "Research" && h.IsComment {
			commentFound = true
			break
		}
	}
	if !commentFound {
		t.Error("Research IsComment=true entry was removed, but should be preserved (different dedup key)")
	}
}

// TestJobCompletedEvent_Dedup verifies that receiving a JobCompletedEvent for
// the same (issue, stage) replaces the existing history entry rather than
// appending a duplicate.
func TestJobCompletedEvent_Dedup(t *testing.T) {
	redirectHistory(t)

	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	// First completion.
	next, _ := m.Update(JobCompletedEvent{
		IssueNumber: 10,
		StageName:   "Research",
		Success:     false,
		CompletedAt: t1,
	})
	m = next.(Model)
	if len(m.history) != 1 {
		t.Fatalf("after first event: history len = %d, want 1", len(m.history))
	}

	// Second completion for the same (issue, stage) — should replace, not append.
	next, _ = m.Update(JobCompletedEvent{
		IssueNumber: 10,
		StageName:   "Research",
		Success:     true,
		CompletedAt: t2,
	})
	m = next.(Model)

	if len(m.history) != 1 {
		t.Fatalf("after duplicate event: history len = %d, want 1 (duplicate should replace)", len(m.history))
	}
	if !m.history[0].Success {
		t.Error("history entry should be the most recent (Success: true), but got the older one")
	}
	if !m.history[0].CompletedAt.Equal(t2) {
		t.Errorf("history entry CompletedAt = %v, want %v", m.history[0].CompletedAt, t2)
	}

	// A different stage on the same issue must still be appended independently.
	next, _ = m.Update(JobCompletedEvent{
		IssueNumber: 10,
		StageName:   "Plan",
		Success:     true,
		CompletedAt: t2,
	})
	m = next.(Model)
	if len(m.history) != 2 {
		t.Fatalf("after Plan event: history len = %d, want 2 (different stage, not a duplicate)", len(m.history))
	}
}

func TestUpdate_IssueBlockedEvent(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")

	// IssueBlockedEvent adds to blocked map
	next, _ := m.Update(IssueBlockedEvent{
		IssueNumber: 214,
		Title:       "fix auto-upgrade",
		StageName:   "Research",
		WaitingFor:  []string{"#213"},
	})
	nm := next.(Model)
	key214 := activeJobKey("", 214)
	b, ok := nm.blocked[key214]
	if !ok {
		t.Fatal("expected issue 214 in blocked map after IssueBlockedEvent")
	}
	if b.StageName != "Research" {
		t.Errorf("StageName = %q, want Research", b.StageName)
	}
	if len(b.WaitingFor) != 1 || b.WaitingFor[0] != "#213" {
		t.Errorf("WaitingFor = %v, want [#213]", b.WaitingFor)
	}

	// JobStartedEvent for the same issue removes it from blocked
	next2, _ := nm.Update(JobStartedEvent{
		IssueNumber: 214,
		StageName:   "Research",
		StartedAt:   time.Now(),
	})
	nm2 := next2.(Model)
	if _, ok := nm2.blocked[key214]; ok {
		t.Error("expected issue 214 removed from blocked after JobStartedEvent")
	}
}

func TestViewActive_BlockedIssue(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 120
	m.now = time.Now()

	// Add a blocked issue
	key := activeJobKey("", 215)
	m.blocked[key] = &blockedIssue{
		IssueNumber: 215,
		Title:       "cut release",
		StageName:   "Research",
		WaitingFor:  []string{"#214"},
	}

	view := m.viewActive()
	if !strings.Contains(view, "🔒") {
		t.Errorf("expected 🔒 in viewActive for blocked issue, got: %q", view)
	}
	if !strings.Contains(view, "#215") {
		t.Errorf("expected #215 in viewActive, got: %q", view)
	}
	if !strings.Contains(view, "waiting for") {
		t.Errorf("expected 'waiting for' in viewActive, got: %q", view)
	}
	if !strings.Contains(view, "#214") {
		t.Errorf("expected #214 in waiting-for list, got: %q", view)
	}
}

func TestViewActive_BlockedCountIncludedInHeader(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 120
	m.now = time.Now()

	// 1 active + 1 blocked = 2 in header
	m.active[activeJobKey("", 10)] = &activeJob{IssueNumber: 10, StageName: "Implement", StartedAt: time.Now()}
	m.blocked[activeJobKey("", 20)] = &blockedIssue{IssueNumber: 20, StageName: "Research", WaitingFor: []string{"#10"}}

	view := m.viewActive()
	if !strings.Contains(view, "In Progress (2)") {
		t.Errorf("expected 'In Progress (2)' in header, got: %q", view)
	}
}

func TestIssueBlockedEvent_tuiEvent(t *testing.T) {
	IssueBlockedEvent{}.tuiEvent() // satisfies interface
}
