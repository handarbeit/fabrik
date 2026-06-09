package watch

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// newTestModel creates a minimal WatchModel for key-handler tests.
// It avoids filesystem calls by using an empty logDir and no stagesDir.
func newTestModel() WatchModel {
	asp := new(atomic.Value)
	asp.Store("")
	m := WatchModel{
		issueNumber:    1,
		done:           make(chan struct{}),
		activeStagePtr: asp,
	}
	return m
}

// isDone returns true if the done channel is closed.
func isDone(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestUpdate_EscapeKey_Quits verifies that the Escape key quits the watch TUI.
func TestUpdate_EscapeKey_Quits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from Escape key")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
	if !isDone(m.done) {
		t.Error("expected done channel to be closed after Escape key")
	}
}

// TestUpdate_QKey_Quits verifies that q quits the watch TUI (unchanged behavior).
func TestUpdate_QKey_Quits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from q key")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestWrapContent verifies the wrapContent helper wraps long lines at the given width.
func TestWrapContent(t *testing.T) {
	// Zero / negative width: return unchanged.
	in := "hello world this is a very long line that should not be wrapped"
	if got := wrapContent(in, 0); got != in {
		t.Errorf("wrapContent(width=0) should return input unchanged; got %q", got)
	}

	// Width >= len(content): no wrap needed.
	short := "hello world"
	if got := wrapContent(short, 80); got != short {
		t.Errorf("wrapContent(short, 80) = %q; want %q", got, short)
	}

	// Width < len(content): result must contain a newline.
	wrapped := wrapContent("hello world foo bar", 10)
	if !strings.Contains(wrapped, "\n") {
		t.Errorf("wrapContent should insert newline for long line; got %q", wrapped)
	}

	// Each resulting line must be <= width runes.
	for _, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
		if len([]rune(line)) > 10 {
			t.Errorf("wrapped line %q exceeds width 10", line)
		}
	}
}

// TestUpdate_WindowSizeMsg_RewrapsContent verifies that a WindowSizeMsg re-wraps
// live content at the new viewport width.
func TestUpdate_WindowSizeMsg_RewrapsContent(t *testing.T) {
	m := newTestModel()
	m.vp = viewport.New(80, 20)
	// Add a long line to the live buffer.
	longLine := strings.Repeat("word ", 30) + "\n"
	m.lines = []string{longLine}
	m.vp.SetContent(longLine)

	// Shrink the terminal width to 40.
	model, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	wm := model.(WatchModel)
	content := wm.vp.View()
	for _, line := range strings.Split(content, "\n") {
		visible := len([]rune(line))
		if visible > 42 { // viewport adds a couple chars of padding; allow slack
			t.Errorf("after resize to 40, line %q has %d runes (expected <= 42)", line, visible)
		}
	}
}

// TestUpdate_CtrlC_Quits verifies that ctrl+c quits the watch TUI (unchanged behavior).
func TestUpdate_CtrlC_Quits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from ctrl+c key")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestUpdate_TurnCountMsg_UpdatesTurnsUsed verifies that TurnCountMsg sets turnsUsed.
func TestUpdate_TurnCountMsg_UpdatesTurnsUsed(t *testing.T) {
	m := newTestModel()
	model, _ := m.Update(TurnCountMsg{TurnsUsed: 7})
	wm := model.(WatchModel)
	if wm.turnsUsed != 7 {
		t.Errorf("turnsUsed = %d, want 7", wm.turnsUsed)
	}

	// Subsequent message updates the count.
	model2, _ := wm.Update(TurnCountMsg{TurnsUsed: 12})
	wm2 := model2.(WatchModel)
	if wm2.turnsUsed != 12 {
		t.Errorf("turnsUsed = %d after second TurnCountMsg, want 12", wm2.turnsUsed)
	}
}

// TestUpdate_NewLogFileMsg_ResetsTurnsUsed verifies that NewLogFileMsg resets turnsUsed.
func TestUpdate_NewLogFileMsg_ResetsTurnsUsed(t *testing.T) {
	m := newTestModel()
	m.turnsUsed = 23
	model, _ := m.Update(NewLogFileMsg{Path: "/tmp/fake-Research-20260101-100000-000000000.log"})
	wm := model.(WatchModel)
	if wm.turnsUsed != 0 {
		t.Errorf("turnsUsed = %d after NewLogFileMsg, want 0", wm.turnsUsed)
	}
}

// TestView_TurnCounter_WithDenominator verifies that View shows "turn N/M" when
// a live stage tab is present and cachedEffectiveMaxTurns > 0.
func TestView_TurnCounter_WithDenominator(t *testing.T) {
	m := newTestModel()
	m.vp = viewport.New(80, 20)
	m.stageTabs = []stageTab{{Label: "Research", IsLive: true}}
	m.selectedTabIdx = 0
	m.turnsUsed = 5
	m.cachedEffectiveMaxTurns = 50

	view := m.View()
	if !strings.Contains(view, "turn 5/50") {
		t.Errorf("expected 'turn 5/50' in view, got:\n%s", view)
	}
}

// TestView_TurnCounter_Unlimited verifies "turn N" (no denominator) when
// cachedEffectiveMaxTurns == 0 (unlimited stage).
func TestView_TurnCounter_Unlimited(t *testing.T) {
	m := newTestModel()
	m.vp = viewport.New(80, 20)
	m.stageTabs = []stageTab{{Label: "Research", IsLive: true}}
	m.selectedTabIdx = 0
	m.turnsUsed = 3
	m.cachedEffectiveMaxTurns = 0

	view := m.View()
	if !strings.Contains(view, "turn 3") {
		t.Errorf("expected 'turn 3' in view, got:\n%s", view)
	}
	if strings.Contains(view, "turn 3/") {
		t.Errorf("expected no denominator for unlimited stage, got:\n%s", view)
	}
}

// TestView_TurnCounter_Hidden_WhenNoTurns verifies turn counter is absent when turnsUsed == 0.
func TestView_TurnCounter_Hidden_WhenNoTurns(t *testing.T) {
	m := newTestModel()
	m.vp = viewport.New(80, 20)
	m.stageTabs = []stageTab{{Label: "Research", IsLive: true}}
	m.selectedTabIdx = 0
	m.turnsUsed = 0
	m.cachedEffectiveMaxTurns = 50

	view := m.View()
	if strings.Contains(view, "turn") {
		t.Errorf("expected no turn counter when turnsUsed=0, got:\n%s", view)
	}
}

// TestActiveStageFromLabels verifies the helper that extracts the in-progress stage from labels.
func TestActiveStageFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"single in-progress", []string{"stage:Review:in_progress"}, "Review"},
		{"multiple labels, one in-progress", []string{"fabrik:yolo", "stage:Implement:in_progress", "stage:Research:complete"}, "Implement"},
		{"no in-progress label", []string{"stage:Research:complete", "fabrik:yolo"}, ""},
		{"empty labels", []string{}, ""},
		{"nil labels", nil, ""},
		{"stage prefix but no in_progress suffix", []string{"stage:Review:complete"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activeStageFromLabels(tt.labels)
			if got != tt.want {
				t.Errorf("activeStageFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
			}
		})
	}
}

// TestWorktreeDir verifies that worktreeDir builds the correct path for multi-repo
// and legacy single-repo layouts.
func TestWorktreeDir(t *testing.T) {
	cwd, _ := os.Getwd()
	tests := []struct {
		name        string
		owner, repo string
		issueNumber int
		wantSuffix  string
	}{
		{
			"multi-repo",
			"myowner", "myrepo", 42,
			filepath.Join(cwd, ".fabrik", "worktrees", "myowner-myrepo", "issue-42"),
		},
		{
			"empty owner falls back to bare",
			"", "myrepo", 42,
			filepath.Join(cwd, ".fabrik", "worktrees", "issue-42"),
		},
		{
			"empty repo falls back to bare",
			"myowner", "", 42,
			filepath.Join(cwd, ".fabrik", "worktrees", "issue-42"),
		},
		{
			"both empty falls back to bare",
			"", "", 99,
			filepath.Join(cwd, ".fabrik", "worktrees", "issue-99"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := worktreeDir(tt.owner, tt.repo, tt.issueNumber)
			if got != tt.wantSuffix {
				t.Errorf("worktreeDir(%q, %q, %d) = %q, want %q",
					tt.owner, tt.repo, tt.issueNumber, got, tt.wantSuffix)
			}
		})
	}
}

// TestTopKeyHints verifies that topKeyHints returns all expected key labels
// and does not end with a newline.
func TestTopKeyHints(t *testing.T) {
	got := topKeyHints()

	for _, want := range []string{"q quit", "↑↓ scroll", "←→ tabs", "G bottom", "g top", "i resume claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("topKeyHints() missing %q; got %q", want, got)
		}
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("topKeyHints() must not end with a newline; got %q", got)
	}
}

// TestBottomStatusLine covers the four key behaviors of the bottom info line.
func TestBottomStatusLine(t *testing.T) {
	// (a) statusMsg set → transient message replaces session/poll content.
	t.Run("statusMsg", func(t *testing.T) {
		m := newTestModel()
		m.statusMsg = "stage is active"
		got := m.bottomStatusLine(200)
		if !strings.Contains(got, "stage is active") {
			t.Errorf("expected statusMsg text, got %q", got)
		}
		if strings.Contains(got, "session") {
			t.Errorf("statusMsg should replace session info, got %q", got)
		}
	})

	// (d) no session file → "no session yet".
	t.Run("no session", func(t *testing.T) {
		m := newTestModel()
		// logDir is empty → currentStageFromLog("") returns "" → readSessionID returns "".
		got := m.bottomStatusLine(200)
		if !strings.Contains(got, "no session yet") {
			t.Errorf("expected 'no session yet', got %q", got)
		}
	})

	// (b) session present + wide terminal → UUID and poll suffix both visible.
	// (c) session present + narrow terminal → UUID present, poll suffix dropped.
	t.Run("session present width handling", func(t *testing.T) {
		// Use a temporary directory as CWD so session files don't pollute the repo.
		tmpDir := t.TempDir()
		oldCwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatalf("chdir tmpDir: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chdir(oldCwd); err != nil {
				t.Errorf("restore cwd: %v", err)
			}
		})

		// Create a fake log file so currentStageFromLog returns a stage name.
		logDir := filepath.Join(tmpDir, "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Fatalf("mkdir log dir: %v", err)
		}
		writeLog(t, logDir, "Validate-20260101-120000-000000000.log")

		// Write the session file at the cwd-relative path that readSessionID expects.
		uuid := "dd9fe2fd-132f-44ed-9c3a-6b7c2b46cc30"
		sessDir := filepath.Join(tmpDir, ".fabrik", "sessions", "issue-88888")
		if err := os.MkdirAll(sessDir, 0o755); err != nil {
			t.Fatalf("mkdir session dir: %v", err)
		}
		sessionFile := filepath.Join(sessDir, "Validate.session")
		if err := os.WriteFile(sessionFile, []byte(uuid+"\n"), 0o644); err != nil {
			t.Fatalf("write session file: %v", err)
		}

		m := newTestModel()
		m.issueNumber = 88888
		m.logDir = logDir
		m.lastPollAt = time.Now()

		// (b) wide terminal — full UUID and poll suffix should both appear.
		gotWide := m.bottomStatusLine(200)
		if !strings.Contains(gotWide, uuid) {
			t.Errorf("wide: expected UUID in output, got %q", gotWide)
		}
		if !strings.Contains(gotWide, "polled") {
			t.Errorf("wide: expected poll suffix, got %q", gotWide)
		}

		// (c) narrow terminal (50 cols): "session: "+UUID = 45 visible chars;
		// any poll suffix pushes past 50, so it must be dropped.
		gotNarrow := m.bottomStatusLine(50)
		if !strings.Contains(gotNarrow, uuid) {
			t.Errorf("narrow: expected UUID preserved, got %q", gotNarrow)
		}
		if strings.Contains(gotNarrow, "polled") {
			t.Errorf("narrow: expected poll suffix to be dropped, got %q", gotNarrow)
		}
	})
}

// TestUpdate_NewLogFileMsg_LabelOverride is the primary regression test: when an
// Implement-comment-review log has a newer timestamp than a Review log but GitHub
// labels say "stage:Review:in_progress", the Review tab must be IsLive after
// processing a NewLogFileMsg.
func TestUpdate_NewLogFileMsg_LabelOverride(t *testing.T) {
	dir := t.TempDir()

	// Write an Implement log, an Implement-comment-review log (newest), and a Review log.
	writeLog(t, dir, "Implement-20260101-120000-000000000.log")
	writeLog(t, dir, "Review-20260101-180000-000000000.log")
	writeLog(t, dir, "Implement-comment-review-20260101-190000-000000000.log") // newest timestamp

	asp := new(atomic.Value)
	asp.Store("")
	m := WatchModel{
		issueNumber: 99,
		logDir:      dir,
		done:        make(chan struct{}),
		stageOrder: map[string]int{
			"Implement": 3,
			"Review":    4,
		},
		github: issueState{
			labels: []string{"stage:Review:in_progress"},
		},
		activeStagePtr: asp,
	}

	model, _ := m.Update(NewLogFileMsg{Path: dir + "/Implement-comment-review-20260101-190000-000000000.log"})
	wm := model.(WatchModel)

	var reviewLive, implementLive bool
	for _, tab := range wm.stageTabs {
		if tab.Label == "Review" {
			reviewLive = tab.IsLive
		}
		if tab.Label == "Implement" {
			implementLive = tab.IsLive
		}
	}
	if !reviewLive {
		t.Error("Review tab must be IsLive (activeStage=Review from labels), even though Implement-comment-review log is newer")
	}
	if implementLive {
		t.Error("Implement tab must NOT be IsLive when Review is the active stage")
	}
	// Selected tab should be the live (Review) tab.
	if wm.selectedTabIdx >= len(wm.stageTabs) || !wm.stageTabs[wm.selectedTabIdx].IsLive {
		t.Errorf("selectedTabIdx=%d does not point to live tab", wm.selectedTabIdx)
	}
}
