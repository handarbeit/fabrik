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

// TestAllocateRows_RepoFloorDoesNotOverflow is the regression test for a
// review finding: when repos wants fewer than minPaneRows (e.g. 1-2 watched
// repos with no provenance notes), the final floor-clamp raised repos back up
// to minPaneRows without taking the row back out of history, so the two sums
// exceeded avail. allocateRows(20, 5, 1) previously returned history=19,
// repos=3 — a sum of 22 against an avail of 20.
func TestAllocateRows_RepoFloorDoesNotOverflow(t *testing.T) {
	for _, tc := range []struct{ avail, wantHistory, wantRepos int }{
		{20, 5, 1},
		{20, 5, 0},
		{40, 30, 1},
		{100, 4, 2},
	} {
		h, r := allocateRows(tc.avail, tc.wantHistory, tc.wantRepos)
		if h+r > tc.avail {
			t.Errorf("allocateRows(%d, %d, %d) = history=%d repos=%d, sum %d > avail %d",
				tc.avail, tc.wantHistory, tc.wantRepos, h, r, h+r, tc.avail)
		}
		if r != minPaneRows {
			t.Errorf("allocateRows(%d, %d, %d): repos=%d, want the floor %d (wantRepos was below it)",
				tc.avail, tc.wantHistory, tc.wantRepos, r, minPaneRows)
		}
	}
}

// TestAllocateRows_DegradeBranchBoundedByTwo is the regression test for a
// review finding: when avail drops below 2 — reachable at ordinary terminal
// heights once the detail panel is open, since its height joins the
// never-truncated "fixed" budget — the degrade branch floors both panes to 1
// regardless of how negative avail is, overflowing the terminal by `2 -
// avail`. That's a real, understood limitation (see allocateRows' comment),
// not a fixable one without letting a pane render zero content rows, which
// SetMaxRows deliberately refuses. What this pins instead: the overflow is
// bounded at exactly 2 — never worse, no matter how far avail drops — so a
// future change can't quietly make a bad terminal size render even more rows
// than it does today.
func TestAllocateRows_DegradeBranchBoundedByTwo(t *testing.T) {
	for _, avail := range []int{1, 0, -1, -3, -10, -100} {
		h, r := allocateRows(avail, 30, 30)
		if sum := h + r; sum != 2 {
			t.Errorf("allocateRows(%d, 30, 30) = history=%d repos=%d, sum %d, want the bounded sum 2",
				avail, h, r, sum)
		}
	}
}

// TestLayout_DetailOpenShortTerminalPinnedAtItsFloor reproduces the review
// finding's exact repro (detail panel opened on a short terminal) at the
// Model level. header/active/footer/an open detail panel are never
// truncated (R1's priority), so this configuration has a higher structural
// minimum than TestLayout_VeryShortTerminalNeverOverflows' detail-closed 14
// — the open detail panel adds to the same never-truncated fixed budget.
// Below that minimum the render does overflow (same accepted category as
// heights below 14), but what this pins is that the overflow does not keep
// getting worse as the terminal keeps shrinking: allocateRows' degrade
// branch always returns a fixed sum of 2 once avail drops below it (see
// TestAllocateRows_DegradeBranchBoundedByTwo), so the total render is pinned
// at this configuration's floor no matter how far below it h drops — and at
// or above that floor, there is no overflow at all.
func TestLayout_DetailOpenShortTerminalPinnedAtItsFloor(t *testing.T) {
	seed := func(h int) Model {
		m := seedModel(t, 120, h, 15, 40, 10)
		upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = upd.(Model)
		if !m.DetailVisible() {
			t.Fatalf("height=%d: detail panel did not open", h)
		}
		return m
	}

	floor := lineCount(seed(1).View())
	for _, h := range []int{5, 10, 14} {
		if got := lineCount(seed(h).View()); got != floor {
			t.Errorf("height=%d: rendered %d rows, want the pinned floor %d (from height=1)", h, got, floor)
		}
	}

	for _, h := range []int{floor, floor + 1, floor + 5} {
		if got := lineCount(seed(h).View()); got > h {
			t.Errorf("height=%d: rendered %d rows, overflows a terminal at or above the structural floor %d", h, got, floor)
		}
	}
}

// TestLayout_VeryShortTerminalNeverOverflows is the regression test for a
// review finding: SetMaxRows unconditionally floored its input to
// minPaneRows, defeating allocateRows' dedicated "too short for both floors"
// branch — which deliberately returns values below minPaneRows so
// history+repos still sums to what's available. Before the fix, Model.View()
// rendered a constant 18 rows regardless of terminal height once it dropped
// below ~18 rows, overflowing every height in this table.
//
// The range tested is [14, 17]: with 0 in-flight reviews, header(1) +
// active(4) + footer(1) + 2*paneChrome(6) = 12 non-content rows, plus the
// 1-row-each floor allocateRows' degrade branch still guarantees history and
// repos, puts the hard structural minimum this design can render into at 14
// — header/active/footer/detail are documented as never truncated (R1's
// stated priority), so a shorter terminal cannot avoid overflowing no matter
// what history/repos do, and isn't this test's concern. 14-17 is exactly the
// window the old constant-18 bug overflowed but a correctly degrading layout
// should not.
func TestLayout_VeryShortTerminalNeverOverflows(t *testing.T) {
	for _, h := range []int{14, 15, 16, 17} {
		m := seedModel(t, 120, h, 15, 40, 0)
		if got := lineCount(m.View()); got > h {
			t.Errorf("height=%d: rendered %d rows, overflows the terminal", h, got)
		}
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

// TestHistoryPane_SelectionTracksVisibleRows is the regression test for the
// review finding on #1675: h.idx is a position within the rendered (visible)
// list, so navigation bounds and Selected() must resolve against
// visibleEntries(). Before the fix they used h.entries, so the highlighted row
// and the detail panel disagreed as soon as one skip was filtered out.
func TestHistoryPane_SelectionTracksVisibleRows(t *testing.T) {
	var h HistoryPaneComponent
	// Oldest first: real1, skip1, real2 — exactly the reviewer's repro.
	for _, ev := range []ReviewCompletedEvent{
		{Repo: "o/r", PRNumber: 1, Reviewed: true, CompletedAt: time.Now()},
		{Repo: "o/r", PRNumber: 2, Skipped: true, Reason: "draft", CompletedAt: time.Now()},
		{Repo: "o/r", PRNumber: 3, Reviewed: true, CompletedAt: time.Now()},
	} {
		comp, _ := h.Update(ev)
		h = comp.(HistoryPaneComponent)
	}
	h.SetFocused(true)

	// idx=0 is the newest visible entry: PR 3.
	if got := h.Selected(); got == nil || got.PRNumber != 3 {
		t.Fatalf("idx=0 selected %v, want PR 3", got)
	}
	// One step down is the next visible entry: PR 1, not the filtered skip.
	comp, _ := h.Update(tea.KeyMsg{Type: tea.KeyDown})
	h = comp.(HistoryPaneComponent)
	if got := h.Selected(); got == nil || got.PRNumber != 1 {
		t.Fatalf("after one 'down', selected %v, want PR 1 (the skip must be stepped over)", got)
	}
	// Navigation must stop at the last *visible* row, not run on through the
	// filtered entries.
	for i := 0; i < 5; i++ {
		comp, _ := h.Update(tea.KeyMsg{Type: tea.KeyDown})
		h = comp.(HistoryPaneComponent)
	}
	if got := h.Selected(); got == nil || got.PRNumber != 1 {
		t.Fatalf("selection ran past the last visible row: %v", got)
	}
}

// TestPaneHeightMatchesRender pins the second review finding: with a granted
// budget both flexible panes pad to it, so Height() must report the padded
// height rather than the entry count.
func TestPaneHeightMatchesRender(t *testing.T) {
	var h HistoryPaneComponent
	comp, _ := h.Update(ReviewCompletedEvent{Repo: "o/r", PRNumber: 1, Reviewed: true, CompletedAt: time.Now()})
	h = comp.(HistoryPaneComponent)
	h.SetMaxRows(20)
	if got, want := h.Height(), lineCount(h.View(120)); got != want {
		t.Errorf("history Height() = %d, but View() renders %d rows", got, want)
	}

	var r RepoPaneComponent
	r.SetWatchedRepos([]string{"o/a", "o/b"})
	r.SetMaxRows(15)
	if got, want := r.Height(), lineCount(r.View(120)); got != want {
		t.Errorf("repos Height() = %d, but View() renders %d rows", got, want)
	}
}
