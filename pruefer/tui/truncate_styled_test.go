package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// forceColor makes lipgloss emit real ANSI escapes for the duration of the
// test. Under `go test` there is no TTY, so the default profile is Ascii and
// styles render as plain text — which is how rune-count truncation, charging
// escape bytes against the width budget, passed every existing pane test while
// truncating live rows far short of the pane edge.
func forceColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func assertLinesFit(t *testing.T, view string, width int) {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line is %d cells wide, terminal is %d: %q", w, width, ansi.Strip(l))
		}
	}
}

func TestHistoryPane_StyledRowShowsTitleAtWideTerminal(t *testing.T) {
	forceColor(t)
	var h HistoryPaneComponent
	comp, _ := h.Update(ReviewCompletedEvent{
		Repo: "verveguy/86ed", PRNumber: 1032, Reviewed: true,
		Title:    "Product owner's Inbox decisions",
		NumTurns: 5, CostUSD: 0.1712, Duration: 22 * time.Second,
		CompletedAt: time.Now(),
	})
	h = comp.(HistoryPaneComponent)

	const width = 120
	view := h.View(width)
	plain := ansi.Strip(view)
	for _, want := range []string{"$0.1712", "Product owner's Inbox decisions"} {
		if !strings.Contains(plain, want) {
			t.Errorf("row at width %d lost %q; rendered:\n%s", width, want, plain)
		}
	}
	assertLinesFit(t, view, width)
}

func TestHistoryPane_StyledRowTruncatesToVisibleWidth(t *testing.T) {
	forceColor(t)
	var h HistoryPaneComponent
	comp, _ := h.Update(ReviewCompletedEvent{
		Repo: "verveguy/86ed", PRNumber: 1032, Reviewed: true,
		Title:    strings.Repeat("very long title ", 20),
		NumTurns: 5, CostUSD: 0.17, Duration: 22 * time.Second,
		CompletedAt: time.Now(),
	})
	h = comp.(HistoryPaneComponent)

	const width = 80
	view := h.View(width)
	if !strings.Contains(ansi.Strip(view), "…") {
		t.Errorf("long row was not truncated:\n%s", ansi.Strip(view))
	}
	assertLinesFit(t, view, width)
}

func TestActivePane_StyledRowShowsTitleAtWideTerminal(t *testing.T) {
	forceColor(t)
	a := newActivePaneComponent()
	comp, _ := a.Update(ReviewStartedEvent{
		Repo: "verveguy/86ed", PRNumber: 1032,
		Title: "Product owner's Inbox decisions", StartedAt: time.Now(),
	})
	a = comp.(ActivePaneComponent)

	const width = 120
	plain := ansi.Strip(a.View(width))
	if !strings.Contains(plain, "Product owner's Inbox decisions") {
		t.Errorf("in-flight row at width %d lost its title; rendered:\n%s", width, plain)
	}
}
