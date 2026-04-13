package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// TestUpdate_JK_HistoryPane verifies j/k navigation in the history pane.
func TestUpdate_JK_HistoryPane(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research"},
		{IssueNumber: 2, StageName: "Plan"},
		{IssueNumber: 3, StageName: "Implement"},
	}

	// j increments histIdx
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	nm := next.(Model)
	if nm.history.histIdx != 1 {
		t.Errorf("histIdx = %d after j, want 1", nm.history.histIdx)
	}
	// k decrements histIdx
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	nm2 := next2.(Model)
	if nm2.history.histIdx != 0 {
		t.Errorf("histIdx = %d after k, want 0", nm2.history.histIdx)
	}
	// k at 0 is a no-op
	next3, _ := nm2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	nm3 := next3.(Model)
	if nm3.history.histIdx != 0 {
		t.Errorf("histIdx = %d after k at 0, want 0", nm3.history.histIdx)
	}
}

// TestUpdate_CKey_DeletesHistoryEntry verifies c removes the selected entry.
func TestUpdate_CKey_DeletesHistoryEntry(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research"},
		{IssueNumber: 2, StageName: "Plan"},
	}
	// histIdx=0 → realIdx = len-1-0 = 1 → deletes entry at index 1 (IssueNumber 2)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	nm := next.(Model)
	if len(nm.history.History()) != 1 {
		t.Errorf("history len = %d after c, want 1", len(nm.history.History()))
	}
	if nm.history.History()[0].IssueNumber != 1 {
		t.Errorf("remaining entry IssueNumber = %d, want 1", nm.history.History()[0].IssueNumber)
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
	const numEntries = 30
	for i := 0; i < numEntries; i++ {
		m.history.history = append(m.history.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research"})
	}
	// Initialise viewport with scroll-to-visible (histIdx=0 at top).
	m.updateLayout(false)
	if m.history.YOffset() != 0 {
		t.Fatalf("initial YOffset = %d, want 0", m.history.YOffset())
	}

	// Navigate down far enough to push histIdx below the visible area.
	vpHeight := m.history.VPHeight()
	if vpHeight < 1 {
		t.Fatalf("viewport height = %d, expected > 0", vpHeight)
	}

	cur := m
	for i := 0; i < vpHeight; i++ {
		next, _ := cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		cur = next.(Model)
	}
	// histIdx is now == vpHeight (one past the last visible line when YOffset was 0).
	if cur.history.HistIdx() != vpHeight {
		t.Fatalf("histIdx = %d after %d j presses, want %d", cur.history.HistIdx(), vpHeight, vpHeight)
	}
	if cur.history.YOffset() == 0 {
		t.Errorf("YOffset still 0 after navigating below visible area; want > 0")
	}
	if cur.history.HistIdx() > cur.history.YOffset()+cur.history.VPHeight()-1 {
		t.Errorf("histIdx %d not visible: YOffset=%d Height=%d",
			cur.history.HistIdx(), cur.history.YOffset(), cur.history.VPHeight())
	}

	// Navigate back up past the top of the current viewport.
	savedOffset := cur.history.YOffset()
	for i := 0; i < vpHeight; i++ {
		next, _ := cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		cur = next.(Model)
	}
	if cur.history.YOffset() >= savedOffset {
		t.Errorf("YOffset %d did not retreat from %d after navigating up", cur.history.YOffset(), savedOffset)
	}
	if cur.history.HistIdx() < cur.history.YOffset() {
		t.Errorf("histIdx %d above viewport: YOffset=%d", cur.history.HistIdx(), cur.history.YOffset())
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
		m.history.history = append(m.history.history, HistoryEntry{IssueNumber: i + 1, StageName: "Research"})
	}
	m.updateLayout(false)
	m.history.SetHistIdx(10)
	m.updateLayout(false)
	if m.history.YOffset() == 0 {
		// Force a non-zero offset to make the assertion meaningful.
		m.history.historyVP.SetYOffset(5)
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
	if nm.history.YOffset() != 0 {
		t.Errorf("YOffset = %d after JobCompletedEvent, want 0", nm.history.YOffset())
	}
}

// TestUpdate_CapitalC_ClearAll verifies two C presses clear all history.
func TestUpdate_CapitalC_ClearAll(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research"},
		{IssueNumber: 2, StageName: "Plan"},
	}
	// First C sets confirmClear
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
	nm := next.(Model)
	if !nm.history.ConfirmClear() {
		t.Error("expected confirmClear=true after first C")
	}
	if len(nm.history.History()) != 2 {
		t.Error("history should not be cleared after first C")
	}
	// Second C confirms and clears
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
	nm2 := next2.(Model)
	if nm2.history.ConfirmClear() {
		t.Error("expected confirmClear=false after confirmed C")
	}
	if len(nm2.history.History()) != 0 {
		t.Errorf("history len = %d after confirmed clear, want 0", len(nm2.history.History()))
	}
}

// TestUpdate_QuitDuringConfirmClear_Cancels verifies q cancels confirmClear state.
func TestUpdate_QuitDuringConfirmClear_Cancels(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.history.SetConfirmClear(true)
	m.focusPane = paneHistory
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	nm := next.(Model)
	if nm.history.ConfirmClear() {
		t.Error("expected confirmClear=false after q during confirmation")
	}
	if cmd != nil {
		t.Error("expected nil cmd (no quit) when q pressed during confirm")
	}
}

// TestUpdate_NKey_CancelsConfirmClear verifies n cancels the clear confirmation.
func TestUpdate_NKey_CancelsConfirmClear(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.history.SetConfirmClear(true)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	nm := next.(Model)
	if nm.history.ConfirmClear() {
		t.Error("expected confirmClear=false after n")
	}
}

// TestViewHistory_ConfirmClear verifies the confirmation prompt is shown.
func TestViewHistory_ConfirmClear(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{{IssueNumber: 1, StageName: "Research"}}
	m.history.SetConfirmClear(true)
	view := m.history.View(m.width)
	if !strings.Contains(view, "Clear all history") {
		t.Errorf("expected confirmation text in viewHistory, got: %q", view)
	}
}

// TestViewHistory_IsComment verifies the 💬 emoji appears for comment history entries.
func TestViewHistory_IsComment(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.history.history = []HistoryEntry{{IssueNumber: 5, StageName: "Implement", IsComment: true, Success: true}}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	view := m.history.View(m.width)
	if !strings.Contains(view, "💬") {
		t.Errorf("expected 💬 in viewHistory for IsComment entry, got: %q", view)
	}
}

// TestViewHistory_ConfirmQuit verifies the quit confirmation prompt is shown in viewHistory.
func TestViewHistory_ConfirmQuit(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Implement", StartedAt: time.Now()}
	m.confirmQuit = true
	m.syncFocus()
	m.updateLayout(false)

	view := m.history.View(m.width)
	if !strings.Contains(view, "Quit Fabrik?") {
		t.Errorf("expected quit confirmation text in viewHistory, got: %q", view)
	}
	if !strings.Contains(view, "1 jobs") {
		t.Errorf("expected job count in viewHistory confirmation, got: %q", view)
	}
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
	if len(m.history.History()) != 1 {
		t.Fatalf("after first event: history len = %d, want 1", len(m.history.History()))
	}

	// Second completion for the same (issue, stage) — should replace, not append.
	next, _ = m.Update(JobCompletedEvent{
		IssueNumber: 10,
		StageName:   "Research",
		Success:     true,
		CompletedAt: t2,
	})
	m = next.(Model)

	if len(m.history.History()) != 1 {
		t.Fatalf("after duplicate event: history len = %d, want 1 (duplicate should replace)", len(m.history.History()))
	}
	if !m.history.History()[0].Success {
		t.Error("history entry should be the most recent (Success: true), but got the older one")
	}
	if !m.history.History()[0].CompletedAt.Equal(t2) {
		t.Errorf("history entry CompletedAt = %v, want %v", m.history.History()[0].CompletedAt, t2)
	}

	// A different stage on the same issue must still be appended independently.
	next, _ = m.Update(JobCompletedEvent{
		IssueNumber: 10,
		StageName:   "Plan",
		Success:     true,
		CompletedAt: t2,
	})
	m = next.(Model)
	if len(m.history.History()) != 2 {
		t.Fatalf("after Plan event: history len = %d, want 2 (different stage, not a duplicate)", len(m.history.History()))
	}
}
