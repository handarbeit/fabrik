package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/handarbeit/fabrik/warnings"
)

// makeWarningEntries builds n distinct, non-dismissed entries with
// predictable keys and titles for use across the tests below.
func makeWarningEntries(n int) []warnings.Entry {
	entries := make([]warnings.Entry, n)
	for i := 0; i < n; i++ {
		entries[i] = warnings.Entry{
			Key:   fmt.Sprintf("key-%d", i),
			Title: fmt.Sprintf("warning-%d", i),
		}
	}
	return entries
}

// newTestWarningsPane builds a focused WarningsPaneComponent with n entries
// and a generous layout, ready for View()/Update().
func newTestWarningsPane(n int) WarningsPaneComponent {
	c := WarningsPaneComponent{
		entries:    makeWarningEntries(n),
		focused:    true,
		availableH: -1,
	}
	c.SetLayout(80, c.Height())
	return c
}

func TestWarningsWindowOffset(t *testing.T) {
	cases := []struct {
		name             string
		curIdx, n, rows  int
		wantOffset       int
	}{
		{"cursor within first window", 0, 10, 3, 0},
		{"cursor at end of first window", 2, 10, 3, 0},
		{"cursor just past window scrolls by one", 3, 10, 3, 1},
		{"cursor at last index anchors bottom", 9, 10, 3, 7},
		{"n fits entirely, offset always zero", 2, 3, 3, 0},
		{"windowRows zero is a no-op", 5, 10, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := warningsWindowOffset(tc.curIdx, tc.n, tc.rows)
			if got != tc.wantOffset {
				t.Errorf("warningsWindowOffset(%d, %d, %d) = %d, want %d", tc.curIdx, tc.n, tc.rows, got, tc.wantOffset)
			}
			// Invariant: curIdx must always fall inside the returned window.
			if tc.rows > 0 && tc.n > 0 {
				if tc.curIdx < got || tc.curIdx > got+tc.rows-1 {
					t.Errorf("curIdx=%d not inside window [%d, %d)", tc.curIdx, got, got+tc.rows)
				}
			}
		})
	}
}

// AC1: with N > 3 non-dismissed warnings, the rendered panel names how many
// entries are hidden.
func TestWarningsView_OverflowIndicator(t *testing.T) {
	for _, n := range []int{4, 20} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			c := newTestWarningsPane(n)
			out := ansi.Strip(c.View(80))
			want := fmt.Sprintf("+%d more", n-maxWarningRows)
			if !strings.Contains(out, want) {
				t.Errorf("View() missing overflow indicator %q; got:\n%s", want, out)
			}
		})
	}
}

// AC2: with N > 3, every entry can be brought on screen through normal
// navigation (repeated "down" presses).
func TestWarningsView_NavigationReachesLastEntry(t *testing.T) {
	n := 4
	c := newTestWarningsPane(n)
	for i := 0; i < n-1; i++ {
		next, _ := c.Update(tea.KeyMsg{Type: tea.KeyDown})
		c = next.(WarningsPaneComponent)
	}
	if c.curIdx != n-1 {
		t.Fatalf("curIdx = %d, want %d after navigating to end", c.curIdx, n-1)
	}
	out := ansi.Strip(c.View(80))
	lastTitle := fmt.Sprintf("warning-%d", n-1)
	if !strings.Contains(out, lastTitle) {
		t.Errorf("View() does not render last entry %q after navigating to it; got:\n%s", lastTitle, out)
	}
}

// AC3: when focused, the selection marker is visible in the rendered output
// for every value curIdx can take. This is the direct guard for the defect
// described in the issue and must fail against the pre-fix render loop
// (which always drew indices 0..maxWarningRows-1 regardless of curIdx).
func TestWarningsView_SelectionMarkerAlwaysVisible(t *testing.T) {
	for _, n := range []int{4, 20} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			for curIdx := 0; curIdx < n; curIdx++ {
				c := newTestWarningsPane(n)
				c.curIdx = curIdx
				out := ansi.Strip(c.View(80))
				if !strings.Contains(out, "▸ warning-"+fmt.Sprint(curIdx)) {
					t.Errorf("curIdx=%d: selection marker not found next to entry in rendered output:\n%s", curIdx, out)
				}
			}
		})
	}
}

// AC4: Fix/Dismiss/enter must act on the same entry the operator can see
// selected. SelectedEntry() is what model.go's f/F handler and warnings.go's
// own d/D/enter handlers act on; this asserts it always matches whichever
// entry is rendered with the selection marker.
func TestWarningsView_SelectedEntryMatchesRenderedSelection(t *testing.T) {
	n := 4
	for curIdx := 0; curIdx < n; curIdx++ {
		t.Run(fmt.Sprintf("curIdx=%d", curIdx), func(t *testing.T) {
			c := newTestWarningsPane(n)
			c.curIdx = curIdx

			selected := c.SelectedEntry()
			if selected == nil {
				t.Fatalf("SelectedEntry() returned nil for curIdx=%d", curIdx)
			}

			out := ansi.Strip(c.View(80))
			renderedKey := ""
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "▸ ") {
					for i := 0; i < n; i++ {
						if strings.Contains(line, fmt.Sprintf("warning-%d", i)) {
							renderedKey = fmt.Sprintf("key-%d", i)
						}
					}
				}
			}
			if renderedKey == "" {
				t.Fatalf("no rendered selection line found for curIdx=%d in:\n%s", curIdx, out)
			}
			if renderedKey != selected.Key {
				t.Errorf("SelectedEntry().Key = %q, but rendered selection marker is on %q", selected.Key, renderedKey)
			}
		})
	}
}

// AC5: with exactly 3 warnings, rendered output is byte-for-byte unchanged
// from today's behavior — the fix must be a no-op in the non-overflow case.
// Captured with ANSI color stripped for determinism across TTY environments.
func TestWarningsView_ThreeWarnings_Unchanged(t *testing.T) {
	c := newTestWarningsPane(3)
	got := ansi.Strip(c.View(80))

	want := "╭────────────────────────────────────────────────────────────────────────────╮\n" +
		"│ ▸ Warnings (3)  [F]ix  [D]ismiss  [enter] detail                           │\n" +
		"│ ▸ warning-0                                                                │\n" +
		"│   warning-1                                                                │\n" +
		"│   warning-2                                                                │\n" +
		"╰────────────────────────────────────────────────────────────────────────────╯"

	if got != want {
		t.Errorf("View() with 3 warnings changed.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// AC6: panel height stays within availableH for N = 1, 3, 4, 20, with and
// without expandedDetail. This is the direct cross-check for R5: Height()
// and View() must never disagree about the number of lines rendered.
func TestWarningsView_HeightMatchesRenderedLines(t *testing.T) {
	for _, n := range []int{1, 3, 4, 20} {
		for _, expanded := range []bool{false, true} {
			t.Run(fmt.Sprintf("n=%d/expanded=%v", n, expanded), func(t *testing.T) {
				c := newTestWarningsPane(n)
				if expanded {
					c.entries[0].Detail = "line one\nline two\nline three"
					c.expandedDetail = true
				}
				h := c.Height()
				c.SetLayout(80, h)
				out := c.View(80)
				gotLines := len(strings.Split(out, "\n"))
				if gotLines != h {
					t.Errorf("Height() = %d, but View() rendered %d lines:\n%s", h, gotLines, out)
				}
				if h > c.availableH {
					t.Errorf("Height() = %d exceeds availableH = %d", h, c.availableH)
				}
			})
		}
	}
}

// AC7: with dismissed entries present and showDismissed toggled both ways,
// AC1-AC4 still hold — the fix must operate on the visible slice.
func TestWarningsView_WithDismissedEntries(t *testing.T) {
	build := func(showDismissed bool) WarningsPaneComponent {
		entries := makeWarningEntries(5)
		entries[4].Dismissed = true // 4 non-dismissed, 1 dismissed
		c := WarningsPaneComponent{
			entries:       entries,
			focused:       true,
			showDismissed: showDismissed,
			availableH:    -1,
		}
		c.SetLayout(80, c.Height())
		return c
	}

	for _, showDismissed := range []bool{false, true} {
		t.Run(fmt.Sprintf("showDismissed=%v", showDismissed), func(t *testing.T) {
			c := build(showDismissed)
			visible := c.visibleEntries()
			n := len(visible)

			// AC1: overflow indicator present when visible count exceeds the cap.
			out := ansi.Strip(c.View(80))
			if n > maxWarningRows {
				want := fmt.Sprintf("+%d more", n-maxWarningRows)
				if !strings.Contains(out, want) {
					t.Errorf("missing overflow indicator %q; got:\n%s", want, out)
				}
			}

			// AC2/AC3/AC4: navigate to the last visible entry and confirm it's
			// rendered, selected, and matches SelectedEntry().
			for i := 0; i < n-1; i++ {
				next, _ := c.Update(tea.KeyMsg{Type: tea.KeyDown})
				c = next.(WarningsPaneComponent)
			}
			if c.curIdx != n-1 {
				t.Fatalf("curIdx = %d, want %d", c.curIdx, n-1)
			}
			out = ansi.Strip(c.View(80))
			lastEntry := visible[n-1]
			if !strings.Contains(out, "▸") {
				t.Fatalf("no selection marker rendered:\n%s", out)
			}
			if !strings.Contains(out, lastEntry.Title) {
				t.Errorf("last visible entry %q not rendered after navigation:\n%s", lastEntry.Title, out)
			}
			selected := c.SelectedEntry()
			if selected == nil || selected.Key != lastEntry.Key {
				t.Errorf("SelectedEntry() = %+v, want key %q", selected, lastEntry.Key)
			}
		})
	}
}
