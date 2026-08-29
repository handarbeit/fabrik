package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// seedModel builds a Model with nRepos watched repos, nDone completed reviews
// and nSkip skips, sized to w x h.
func seedModel(t *testing.T, w, h, nRepos, nDone, nSkip int) Model {
	t.Helper()
	repos := make([]string, 0, nRepos)
	for i := 0; i < nRepos; i++ {
		repos = append(repos, "verveguy/repo-"+strings.Repeat("x", i%7)+string(rune('a'+i%26)))
	}
	m := New(repos, time.Now())
	upd, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = upd.(Model)

	for i := 0; i < nDone; i++ {
		upd, _ := m.Update(ReviewCompletedEvent{
			Repo: repos[i%len(repos)], PRNumber: 100 + i, Reviewed: true,
			NumTurns: 3, CostUSD: 0.01, Duration: 2 * time.Second, CompletedAt: time.Now(),
		})
		m = upd.(Model)
	}
	for i := 0; i < nSkip; i++ {
		upd, _ := m.Update(ReviewCompletedEvent{
			Repo: repos[i%len(repos)], PRNumber: 500 + i, Skipped: true,
			Reason: "already reviewed at this head SHA", CompletedAt: time.Now(),
		})
		m = upd.(Model)
	}
	return m
}

// TestLayout_FillsTerminalHeight is the core #1674 R1 regression: before the
// fix, terminal height was captured into Model.height and never read, so the
// TUI rendered a fixed-height block regardless of window size. Asserted by
// measuring rendered rows against the simulated WindowSizeMsg rather than by
// inspecting a screenshot.
func TestLayout_FillsTerminalHeight(t *testing.T) {
	// Seed more content than any tested height can show, so "did it fill?" is
	// a question about the layout rather than about running out of rows.
	for _, h := range []int{30, 45, 62, 90} {
		m := seedModel(t, 120, h, 40, 150, 60)
		got := lineCount(m.View())
		if got > h {
			t.Errorf("height=%d: rendered %d rows, overflows the terminal", h, got)
		}
		// Small slack for inter-pane rounding. The pre-fix behavior — a
		// constant block at every height — cannot pass this.
		if h-got > 4 {
			t.Errorf("height=%d: rendered only %d rows, leaving %d unused", h, got, h-got)
		}
	}
}

// TestLayout_ResizeRelayouts pins that a later WindowSizeMsg is honored, not
// just the first.
func TestLayout_ResizeRelayouts(t *testing.T) {
	m := seedModel(t, 120, 30, 15, 40, 60)
	small := lineCount(m.View())

	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 80})
	m = upd.(Model)
	large := lineCount(m.View())

	if large <= small {
		t.Errorf("resize 30→80 did not grow the layout: %d → %d rows", small, large)
	}
}

// TestLayout_ShortTerminalDegrades covers R1's stated rule at a deliberately
// short height: both flexible panes shrink to their floor and window their
// content rather than overflowing.
func TestLayout_ShortTerminalDegrades(t *testing.T) {
	m := seedModel(t, 120, 18, 15, 40, 0)
	view := m.View()
	if got := lineCount(view); got > 18 {
		t.Fatalf("rendered %d rows into an 18-row terminal", got)
	}
	if !strings.Contains(view, "… ") {
		t.Errorf("expected a '… N more' elision at a short height:\n%s", view)
	}
}

// TestAllocateRows_ServesChangingPaneFirst pins R1's documented priority: the
// changing pane (Completed Reviews) is served before the static one.
func TestAllocateRows_ServesChangingPaneFirst(t *testing.T) {
	// Plenty of space: repos gets exactly what it wants and the surplus goes
	// to the changing pane, so the terminal is filled rather than padded.
	if h, r := allocateRows(40, 10, 15); r != 15 || h+r != 40 {
		t.Errorf("ample: got history=%d repos=%d, want repos=15 and the surplus to history (total 40)", h, r)
	}
	// Contended: history keeps its content, repos yields to the floor.
	if h, r := allocateRows(20, 30, 30); h != 20-minPaneRows || r != minPaneRows {
		t.Errorf("contended: got history=%d repos=%d, want %d/%d", h, r, 20-minPaneRows, minPaneRows)
	}
	// History needs little: the surplus goes to repos, not wasted.
	if h, r := allocateRows(20, 2, 30); r <= minPaneRows {
		t.Errorf("history-light: repos got %d, expected the surplus (history=%d)", r, h)
	}
	// Too short for both floors: still split, never zero or negative.
	h, r := allocateRows(4, 30, 30)
	if h < 1 || r < 1 || h+r > 4 {
		t.Errorf("cramped: got history=%d repos=%d for avail=4", h, r)
	}
}

// TestLayout_PanelOrder pins R4: the changing panes sit above the static
// Watched Repos list.
func TestLayout_PanelOrder(t *testing.T) {
	m := seedModel(t, 120, 60, 5, 3, 2)
	view := m.View()

	iActive := strings.Index(view, "In-Flight Reviews")
	iHistory := strings.Index(view, "Completed Reviews")
	iRepos := strings.Index(view, "Watched Repos")
	if iActive < 0 || iHistory < 0 || iRepos < 0 {
		t.Fatalf("missing a pane: active=%d history=%d repos=%d\n%s", iActive, iHistory, iRepos, view)
	}
	if !(iActive < iHistory && iHistory < iRepos) {
		t.Errorf("panel order is active=%d history=%d repos=%d; want active < history < repos", iActive, iHistory, iRepos)
	}
}

// TestHistoryPane_ColumnsAlign pins R3: with repo names of widely varying
// length, the timestamp column starts at the same offset on every row.
func TestHistoryPane_ColumnsAlign(t *testing.T) {
	var h HistoryPaneComponent
	for i, repo := range []string{
		"verveguy/liminis-context-graph",
		"verveguy/86ed",
		"handarbeit/fabrik",
		"verveguy/liminis-remarkable",
	} {
		comp, _ := h.Update(ReviewCompletedEvent{
			Repo: repo, PRNumber: 100 + i, Reviewed: true,
			Duration: time.Second, CompletedAt: time.Date(2026, 8, 29, 12, 34, 56, 0, time.UTC),
		})
		h = comp.(HistoryPaneComponent)
	}

	var offsets []int
	for _, line := range strings.Split(stripANSI(h.View(140)), "\n") {
		if idx := strings.Index(line, "12:34:56"); idx >= 0 {
			offsets = append(offsets, idx)
		}
	}
	if len(offsets) < 4 {
		t.Fatalf("expected 4 timestamped rows, found %d", len(offsets))
	}
	for _, o := range offsets[1:] {
		if o != offsets[0] {
			t.Errorf("timestamp column is ragged: offsets %v", offsets)
			break
		}
	}
}

// TestElideMiddle_PreservesPRNumber: the tail of a repo#PR key identifies the
// PR, so elision must come out of the middle, not the end.
func TestElideMiddle_PreservesPRNumber(t *testing.T) {
	got := elideMiddle("verveguy/liminis-context-graph#286", 20)
	if len([]rune(got)) != 20 {
		t.Errorf("elideMiddle length = %d, want 20 (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "286") {
		t.Errorf("elideMiddle(%q) = %q, dropped the PR number", "…#286", got)
	}
	if !strings.HasPrefix(got, "verveguy") {
		t.Errorf("elideMiddle dropped the owner: %q", got)
	}
	if s := "short#1"; elideMiddle(s, 20) != s {
		t.Errorf("elideMiddle shortened a string that already fits")
	}
}

// ansiRE matches SGR escape sequences, which pad the raw string but occupy no
// terminal columns — column-offset assertions must ignore them.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
