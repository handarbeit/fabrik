package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestFooterHeight(t *testing.T) {
	f := FooterComponent{}
	if f.Height() != 1 {
		t.Errorf("FooterComponent.Height() = %d, want 1", f.Height())
	}
}

func TestViewFooter_Content(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board", Version: "2.0.0"}, "")
	m.width = 120
	footer := m.footer.View(m.width)

	for _, want := range []string{"~/projects/myapp", "My Board", "2.0.0"} {
		if !strings.Contains(footer, want) {
			t.Errorf("viewFooter() missing %q; got: %q", want, footer)
		}
	}
}

func TestViewFooter_NoVersion(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board"}, "")
	m.width = 120
	footer := m.footer.View(m.width)

	if !strings.Contains(footer, "~/projects/myapp") {
		t.Error("footer missing CWD when version is absent")
	}
	if !strings.Contains(footer, "My Board") {
		t.Error("footer missing board title when version is absent")
	}
}

func TestViewFooter_Truncation(t *testing.T) {
	// Use a narrow terminal to force truncation.
	m := New(30, ProjectInfo{
		CWD:        "~/very/long/path/to/a/deeply/nested/project/directory",
		BoardTitle: "Some Long Board Name For Truncation Test",
		Version:    "99.99.99",
	}, "")
	m.width = 30
	footer := m.footer.View(m.width)

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
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board"}, "")
	m.width = 120
	// graphqlStats is zero-value (Limit==0) by default.
	footer := m.footer.View(m.width)
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
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board"}, "")
	m.width = 120
	m.footer.now = time.Now()
	m.footer.graphqlStats = RateLimitStats{
		Limit:     5000,
		Remaining: 4865,
		Reset:     m.footer.now.Add(12 * time.Minute),
	}
	footer := m.footer.View(m.width)
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
			m := New(30, ProjectInfo{CWD: "~", BoardTitle: "My Board"}, "")
			m.width = 120
			m.footer.now = now
			m.footer.graphqlStats = RateLimitStats{
				Limit:     tc.limit,
				Remaining: tc.remaining,
				Reset:     reset,
			}
			footer := m.footer.View(m.width)
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
		CWD:        "~/very/long/path/to/a/deeply/nested/project/directory",
		BoardTitle: "Some Long Board Name For Truncation Test",
		Version:    "99.99.99",
	}, "")
	m.width = 40
	m.footer.now = now
	m.footer.graphqlStats = RateLimitStats{
		Limit:     5000,
		Remaining: 4865,
		Reset:     now.Add(12 * time.Minute),
	}
	footer := m.footer.View(m.width)
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

// TestViewFooter_OSC8Hyperlink verifies that the board title is wrapped in an
// OSC 8 hyperlink when the terminal reports OSC 8 support.
func TestViewFooter_OSC8Hyperlink(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	m := New(30, ProjectInfo{
		CWD:        "~",
		BoardTitle: "My Board",
		BoardURL:   "https://github.com/orgs/acme/projects/1",
	}, "")
	m.width = 120
	footer := m.footer.View(m.width)

	if !strings.Contains(footer, "My Board") {
		t.Errorf("footer missing board title; got: %q", footer)
	}
	if !strings.Contains(footer, "https://github.com/orgs/acme/projects/1") {
		t.Errorf("footer missing OSC 8 hyperlink URL; got: %q", footer)
	}
}

// TestViewFooter_PlainTextNoOSC8 verifies that the board title is rendered as
// plain text (no URL) when the terminal does not support OSC 8.
func TestViewFooter_PlainTextNoOSC8(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TERM", "")
	m := New(30, ProjectInfo{
		CWD:        "~",
		BoardTitle: "My Board",
		BoardURL:   "https://github.com/orgs/acme/projects/1",
	}, "")
	m.width = 120
	footer := m.footer.View(m.width)

	if !strings.Contains(footer, "My Board") {
		t.Errorf("footer missing board title; got: %q", footer)
	}
	if strings.Contains(footer, "https://github.com/orgs/acme/projects/1") {
		t.Errorf("footer should not contain hyperlink URL in non-OSC8 terminal; got: %q", footer)
	}
}

// TestViewFooter_NoTitleSlot verifies that no separator dot is shown when
// BoardTitle is empty (footer is just CWD with no board title slot).
func TestViewFooter_NoTitleSlot(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/project"}, "")
	m.width = 120
	footer := m.footer.View(m.width)
	plain := ansi.Strip(footer)

	if !strings.Contains(plain, "~/project") {
		t.Errorf("footer missing CWD; got: %q", plain)
	}
	if strings.Contains(plain, "·") {
		t.Errorf("footer should not contain separator when board title is absent; got: %q", plain)
	}
}
