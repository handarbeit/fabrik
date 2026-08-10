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
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board", Version: "2.0.0"}, "", nil, nil, 0, false)
	m.width = 120
	footer := m.footer.View(m.width)

	// When BoardTitle is present, Version is hidden (redundant).
	for _, want := range []string{"~/projects/myapp", "My Board"} {
		if !strings.Contains(footer, want) {
			t.Errorf("viewFooter() missing %q; got: %q", want, footer)
		}
	}
	if strings.Contains(footer, "2.0.0") {
		t.Errorf("viewFooter() should hide Version when BoardTitle is present; got: %q", footer)
	}
}

func TestViewFooter_NoVersion(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board"}, "", nil, nil, 0, false)
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
	}, "", nil, nil, 0, false)
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
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board"}, "", nil, nil, 0, false)
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
	m := New(30, ProjectInfo{CWD: "~/projects/myapp", BoardTitle: "My Board"}, "", nil, nil, 0, false)
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

// TestViewFooter_RateLimitColors_ColdStartAlwaysGreen verifies that setting
// graphqlStats directly (bypassing Update, so no PollCompletedEvent sample
// has ever been observed) always renders green regardless of percentage
// remaining — the cold-start behavior required by issue #1510's Requirement
// 4 ("must not flash red on startup"). This supersedes the pre-#1510 version
// of this test, which pinned the removed percentage-only coloring.
func TestViewFooter_RateLimitColors_ColdStartAlwaysGreen(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	now := time.Now()
	reset := now.Add(10 * time.Minute)
	cases := []struct {
		name      string
		remaining int
		limit     int
	}{
		{"formerly green >50%", 2600, 5000},
		{"formerly yellow 20-50%", 1500, 5000},
		{"formerly red <20%", 900, 5000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{CWD: "~", BoardTitle: "My Board"}, "", nil, nil, 0, false)
			m.width = 120
			m.footer.now = now
			m.footer.graphqlStats = RateLimitStats{
				Limit:     tc.limit,
				Remaining: tc.remaining,
				Reset:     reset,
			}
			footer := m.footer.View(m.width)
			if !strings.Contains(footer, "42") {
				t.Errorf("expected cold-start green (42) for %d/%d; footer=%q", tc.remaining, tc.limit, footer)
			}
			if strings.Contains(footer, "214") || strings.Contains(footer, "196") {
				t.Errorf("cold start must never render yellow/red; footer=%q", footer)
			}
		})
	}
}

// TestGraphQLStyle_ReportedCase pins the false positive reported in issue
// #1510: 1980/5000 remaining, 4 minutes to reset, with poll-loop spend far
// below what's needed to exhaust the budget in that window. Must render
// green, not the old percentage logic's yellow.
func TestGraphQLStyle_ReportedCase(t *testing.T) {
	f := FooterComponent{}
	now := time.Now()

	// Warm up the estimator with a low, healthy burn rate: 5 points consumed
	// over 5 minutes (1 point/min) is orders of magnitude below what would be
	// needed to exhaust 1980 remaining in the 4 minutes left.
	f.now = now
	comp, _ := f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 1985, Reset: now.Add(9 * time.Minute)}})
	f = comp.(FooterComponent)

	f.now = now.Add(5 * time.Minute)
	comp, _ = f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 1980, Reset: now.Add(4 * time.Minute)}})
	f = comp.(FooterComponent)

	if !f.haveBurnRate {
		t.Fatal("expected a burn-rate estimate after two samples")
	}
	if got := f.graphqlStyle(); got.GetForeground() != successStyle.GetForeground() {
		t.Errorf("reported case: got style %v, want successStyle (green)", got)
	}
}

// TestGraphQLStyle_ProjectedExhaustionRed pins a genuine exhaustion
// projection: a high burn rate that will consume Remaining well before
// Reset.
func TestGraphQLStyle_ProjectedExhaustionRed(t *testing.T) {
	f := FooterComponent{}
	now := time.Now()

	f.now = now
	comp, _ := f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 2000, Reset: now.Add(11 * time.Minute)}})
	f = comp.(FooterComponent)

	// 1000 points consumed in 1 minute -> burn rate 1000/min. Projected over
	// the remaining 10 minutes (10000) vastly exceeds Remaining (1000).
	f.now = now.Add(1 * time.Minute)
	comp, _ = f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 1000, Reset: now.Add(10 * time.Minute)}})
	f = comp.(FooterComponent)

	if got := f.graphqlStyle(); got.GetForeground() != failStyle.GetForeground() {
		t.Errorf("exhaustion case: got style %v, want failStyle (red)", got)
	}
}

// TestGraphQLStyle_ProjectionBoundary pins the exact comparison operators at
// both thresholds: strictly-greater-than in both cases, so a projection
// exactly equal to a threshold stays in the lower (safer) band.
func TestGraphQLStyle_ProjectionBoundary(t *testing.T) {
	// remaining=1000, burnRate=100 pts/min => projected = 100 * minutes.
	// graphqlProjectionYellowMargin (0.75) puts the yellow threshold at 750.
	cases := []struct {
		name    string
		minutes float64 // minutes to reset
		want    lipgloss.Style
	}{
		{"just under yellow margin -> green", 7.49, successStyle},                       // projected 749 < 750
		{"exactly at yellow margin -> green (not strictly greater)", 7.5, successStyle}, // projected 750 == 750
		{"just over yellow margin -> yellow", 7.51, activeStyle},                        // projected 751 > 750
		{"exactly at remaining -> yellow (not strictly greater)", 10.0, activeStyle},    // projected 1000 == 1000
		{"just over remaining -> red", 10.01, failStyle},                                // projected 1001 > 1000
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := FooterComponent{
				now:            now,
				haveBurnRate:   true,
				burnRatePerMin: 100,
				graphqlStats: RateLimitStats{
					Limit:     5000,
					Remaining: 1000,
					Reset:     now.Add(time.Duration(tc.minutes * float64(time.Minute))),
				},
			}
			if got := f.graphqlStyle(); got.GetForeground() != tc.want.GetForeground() {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestGraphQLStyle_WindowReset verifies that a Remaining increase between
// consecutive samples is treated as a window reset: the estimator discards
// the stale sample (rendering green, cold-start-like) rather than computing
// a negative burn rate, and a subsequent post-reset sample produces a fresh,
// correct estimate.
func TestGraphQLStyle_WindowReset(t *testing.T) {
	f := FooterComponent{}
	now := time.Now()

	// Establish a high burn rate pre-reset.
	f.now = now
	comp, _ := f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 2000, Reset: now.Add(11 * time.Minute)}})
	f = comp.(FooterComponent)
	f.now = now.Add(1 * time.Minute)
	comp, _ = f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 500, Reset: now.Add(10 * time.Minute)}})
	f = comp.(FooterComponent)
	if got := f.graphqlStyle(); got.GetForeground() != failStyle.GetForeground() {
		t.Fatalf("pre-reset sanity check: got %v, want failStyle", got)
	}

	// Window resets: Remaining jumps back up to Limit.
	f.now = now.Add(2 * time.Minute)
	comp, _ = f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 5000, Reset: now.Add(70 * time.Minute)}})
	f = comp.(FooterComponent)
	if f.haveBurnRate {
		t.Error("window reset should discard the burn-rate estimate")
	}
	if got := f.graphqlStyle(); got.GetForeground() != successStyle.GetForeground() {
		t.Errorf("immediately after window reset: got %v, want successStyle (green)", got)
	}

	// A subsequent post-reset sample computes a fresh rate correctly (not
	// poisoned by the pre-reset delta).
	f.now = now.Add(3 * time.Minute)
	comp, _ = f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 4990, Reset: now.Add(69 * time.Minute)}})
	f = comp.(FooterComponent)
	if !f.haveBurnRate {
		t.Fatal("expected a fresh burn-rate estimate after the post-reset sample")
	}
	if f.burnRatePerMin != 10 {
		t.Errorf("expected fresh burn rate of 10 pts/min, got %v", f.burnRatePerMin)
	}
	if got := f.graphqlStyle(); got.GetForeground() != successStyle.GetForeground() {
		t.Errorf("healthy post-reset burn rate: got %v, want successStyle (green)", got)
	}
}

// TestGraphQLStyle_IdleZeroBurn verifies an idle engine (Remaining unchanged
// across samples) renders green no matter how low Remaining has fallen.
func TestGraphQLStyle_IdleZeroBurn(t *testing.T) {
	f := FooterComponent{}
	now := time.Now()

	f.now = now
	comp, _ := f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 50, Reset: now.Add(11 * time.Minute)}})
	f = comp.(FooterComponent)
	f.now = now.Add(5 * time.Minute)
	comp, _ = f.Update(PollCompletedEvent{GraphQLStats: RateLimitStats{Limit: 5000, Remaining: 50, Reset: now.Add(6 * time.Minute)}})
	f = comp.(FooterComponent)

	if f.burnRatePerMin != 0 {
		t.Fatalf("expected zero burn rate for an idle engine, got %v", f.burnRatePerMin)
	}
	if got := f.graphqlStyle(); got.GetForeground() != successStyle.GetForeground() {
		t.Errorf("idle engine at low Remaining: got %v, want successStyle (green)", got)
	}
}

// TestGraphQLStyle_ResetAtOrBehindNow verifies no panic/division issue and a
// green result when Reset is at or behind now, even with an established
// non-zero burn rate.
func TestGraphQLStyle_ResetAtOrBehindNow(t *testing.T) {
	now := time.Now()
	f := FooterComponent{
		now:            now,
		haveBurnRate:   true,
		burnRatePerMin: 100,
		graphqlStats: RateLimitStats{
			Limit:     5000,
			Remaining: 10,
			Reset:     now.Add(-1 * time.Minute), // behind now
		},
	}
	if got := f.graphqlStyle(); got.GetForeground() != successStyle.GetForeground() {
		t.Errorf("Reset behind now: got %v, want successStyle (green)", got)
	}

	f.graphqlStats.Reset = now // exactly at now
	if got := f.graphqlStyle(); got.GetForeground() != successStyle.GetForeground() {
		t.Errorf("Reset at now: got %v, want successStyle (green)", got)
	}
}

// TestGraphQLStyle_LimitZero verifies no division-by-zero panic when Limit
// is zero (the pre-existing Limit > 0 gate in Update/View covers this, but
// graphqlStyle itself must also not divide by Limit anywhere).
func TestGraphQLStyle_LimitZero(t *testing.T) {
	f := FooterComponent{
		now: time.Now(),
		graphqlStats: RateLimitStats{
			Limit:     0,
			Remaining: 0,
			Reset:     time.Time{},
		},
	}
	// Must not panic.
	_ = f.graphqlStyle()
}

// TestViewFooter_TruncationWithRateLimit verifies that at narrow widths the
// left side is truncated but the right (rate limit) side is preserved.
func TestViewFooter_TruncationWithRateLimit(t *testing.T) {
	now := time.Now()
	m := New(30, ProjectInfo{
		CWD:        "~/very/long/path/to/a/deeply/nested/project/directory",
		BoardTitle: "Some Long Board Name For Truncation Test",
		Version:    "99.99.99",
	}, "", nil, nil, 0, false)
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
	}, "", nil, nil, 0, false)
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
	}, "", nil, nil, 0, false)
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
	m := New(30, ProjectInfo{CWD: "~/project"}, "", nil, nil, 0, false)
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

// The footer must name the account, not just the profile directory: the two
// are independent, so "which account is this instance billing?" is otherwise
// unanswerable from the UI.
func TestViewFooter_ShowsClaudeAccount(t *testing.T) {
	m := New(30, ProjectInfo{CWD: "~/dev/fabrik", ClaudeAccount: "someone@example.com"}, "", nil, nil, 0, false)
	m.width = 120
	m.footer.graphqlStats = RateLimitStats{Remaining: 4000, Limit: 5000}

	footer := ansi.Strip(m.footer.View(m.width))
	if !strings.Contains(footer, "someone@example.com") {
		t.Errorf("footer must name the logged-in account; got: %q", footer)
	}
	// Left and right segments must survive alongside it.
	for _, want := range []string{"~/dev/fabrik", "4000/5000"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer lost %q when the account was added; got: %q", want, footer)
		}
	}
	if lipgloss.Width(m.footer.View(m.width)) > m.width {
		t.Errorf("footer exceeded width %d: %q", m.width, footer)
	}
}

// The account is standing context, not operational state, so it is the first
// thing dropped when the terminal is too narrow — never at the cost of the
// cwd or the rate-limit indicator, and never by overflowing the line.
func TestViewFooter_ClaudeAccountDroppedWhenNarrow(t *testing.T) {
	for _, width := range []int{40, 50, 60} {
		m := New(30, ProjectInfo{CWD: "~/dev/fabrik", ClaudeAccount: "a-very-long-account-name@example.com"}, "", nil, nil, 0, false)
		m.width = width
		m.footer.graphqlStats = RateLimitStats{Remaining: 4000, Limit: 5000}

		rendered := m.footer.View(m.width)
		footer := ansi.Strip(rendered)
		if strings.Contains(footer, "a-very-long-account-name@example.com") {
			t.Errorf("width %d: account should be dropped rather than crowd the line; got: %q", width, footer)
		}
		if lipgloss.Width(rendered) > width {
			t.Errorf("width %d: footer overflowed: %q (%d cols)", width, footer, lipgloss.Width(rendered))
		}
	}
}

// An unset or unreadable account leaves the footer byte-for-byte as before.
func TestViewFooter_NoAccountUnchanged(t *testing.T) {
	withAcct := New(30, ProjectInfo{CWD: "~/dev/fabrik"}, "", nil, nil, 0, false)
	withAcct.width = 120
	withAcct.footer.graphqlStats = RateLimitStats{Remaining: 4000, Limit: 5000}
	got := withAcct.footer.View(withAcct.width)

	if strings.Contains(ansi.Strip(got), "  logged in") {
		t.Errorf("unset account must render nothing extra; got: %q", ansi.Strip(got))
	}
	if lipgloss.Width(got) > withAcct.width {
		t.Errorf("footer overflowed: %q", ansi.Strip(got))
	}
}
