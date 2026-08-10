package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// graphqlProjectionYellowMargin is the fraction of Remaining at which the
// GraphQL budget indicator turns yellow: projected consumption to reset
// exceeding this fraction of Remaining is "approaching" exhaustion.
// Deliberately a separate constant from engine/backoff.go's ADR-028
// rateLimitBackoffThreshold/rateLimitHealthyThreshold — those gate
// poll-interval backoff (a behavioral concern); this gates only the
// footer's display color, and the two must stay independently tunable.
// See issue #1510.
const graphqlProjectionYellowMargin = 0.75

// graphqlSample is a single (time, Remaining) observation of the GraphQL
// rate-limit budget, taken from a PollCompletedEvent.
type graphqlSample struct {
	at        time.Time
	remaining int
}

// FooterComponent renders the bottom status bar: project info, rate limit stats,
// and (when webhook mode is active) the webhook health indicator.
type FooterComponent struct {
	projectInfo   ProjectInfo
	graphqlStats  RateLimitStats
	now           time.Time
	webhookState  string         // "", "starting_up", "healthy", "unhealthy"
	webhookCounts map[string]int // per-type event counts

	// GraphQL burn-rate estimation state (#1510). haveSample/lastSample track
	// the most recent PollCompletedEvent observation; haveBurnRate/burnRatePerMin
	// hold the estimate derived from the two most recent samples. Kept entirely
	// in-component (rather than widening RateLimitStats) — see ADR discussion
	// in issue #1510's Plan stage.
	haveSample     bool
	lastSample     graphqlSample
	haveBurnRate   bool
	burnRatePerMin float64
}

func (f FooterComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		f.now = ev.At
	case PollCompletedEvent:
		if ev.GraphQLStats.Limit > 0 {
			f = f.observeGraphQLSample(ev.GraphQLStats)
		}
	case ProjectMetaEvent:
		f.projectInfo.BoardTitle = ev.BoardTitle
		f.projectInfo.BoardURL = ev.BoardURL
	case WebhookStatusEvent:
		f.webhookState = ev.State
		if ev.EventCounts != nil {
			f.webhookCounts = ev.EventCounts
		}
	}
	return f, nil
}

// observeGraphQLSample records a new GraphQL rate-limit observation and updates
// the burn-rate estimate from it. The estimate is a simple two-point delta
// against the immediately-prior sample (Δpoints/Δwall-clock-minutes) — no
// rolling window or smoothing, per issue #1510's Plan.
//
// A Remaining increase between consecutive samples is treated as a window
// reset: the stale sample is discarded and the estimator restarts (a
// cold-start-like gap of one cycle) rather than compute a negative rate.
// Sample timestamps use f.now (updated once per second via TickEvent) rather
// than time.Now(), keeping the estimator deterministic and testable via
// direct field assignment.
func (f FooterComponent) observeGraphQLSample(stats RateLimitStats) FooterComponent {
	f.graphqlStats = stats

	if f.now.IsZero() {
		// No TickEvent has landed yet; treat this as a fresh baseline rather
		// than compute a bogus rate against a zero timestamp. Harmless: cold
		// start already renders green.
		f.lastSample = graphqlSample{at: f.now, remaining: stats.Remaining}
		f.haveSample = true
		f.haveBurnRate = false
		return f
	}

	if !f.haveSample {
		f.lastSample = graphqlSample{at: f.now, remaining: stats.Remaining}
		f.haveSample = true
		f.haveBurnRate = false
		return f
	}

	prev := f.lastSample
	if stats.Remaining > prev.remaining {
		// Window reset: Remaining jumped back up. Discard the stale sample
		// and restart the estimator rather than emit a negative burn rate.
		f.lastSample = graphqlSample{at: f.now, remaining: stats.Remaining}
		f.haveBurnRate = false
		return f
	}

	elapsed := f.now.Sub(prev.at)
	if elapsed <= 0 {
		// Non-advancing or out-of-order timestamp; keep the prior estimate
		// rather than divide by a non-positive duration, but still advance
		// the baseline so the next sample compares against this one.
		f.lastSample = graphqlSample{at: f.now, remaining: stats.Remaining}
		return f
	}

	pointsConsumed := prev.remaining - stats.Remaining // >= 0, reset case handled above
	f.burnRatePerMin = float64(pointsConsumed) / elapsed.Minutes()
	f.haveBurnRate = true
	f.lastSample = graphqlSample{at: f.now, remaining: stats.Remaining}
	return f
}

// graphqlStyle selects the display style for the GraphQL budget indicator by
// projecting the estimated burn rate forward to the window reset and
// comparing it against Remaining, rather than coloring on raw percentage
// remaining alone (issue #1510).
//
// Cold start (no burn-rate estimate yet) and an idle/zero burn rate both
// render green by construction — never a fallback to percentage-based
// coloring, which would reintroduce the false alarm this issue exists to fix.
func (f FooterComponent) graphqlStyle() lipgloss.Style {
	minutesToReset := f.graphqlStats.Reset.Sub(f.now).Minutes()
	if !f.haveBurnRate || f.burnRatePerMin <= 0 || minutesToReset <= 0 {
		return successStyle
	}
	projected := f.burnRatePerMin * minutesToReset
	remaining := float64(f.graphqlStats.Remaining)
	switch {
	case projected > remaining:
		return failStyle
	case projected > graphqlProjectionYellowMargin*remaining:
		return activeStyle
	default:
		return successStyle
	}
}

// webhookIndicator returns the rendered webhook health indicator string, or "" when
// webhook mode is disabled.
func (f FooterComponent) webhookIndicator() string {
	total := 0
	for _, n := range f.webhookCounts {
		total += n
	}
	switch f.webhookState {
	case "healthy":
		return successStyle.Render(fmt.Sprintf("● webhook (%d)", total))
	case "starting_up":
		return infoStyle.Render(fmt.Sprintf("○ webhook (%d)", total))
	case "unhealthy":
		return activeStyle.Render(fmt.Sprintf("◌ webhook (%d)", total))
	default:
		return ""
	}
}

// supportsOSC8 returns true when the terminal is known to support OSC 8 hyperlinks.
// Detection uses environment variables because no universal runtime query exists.
func supportsOSC8() bool {
	prog := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	switch prog {
	case "iterm.app", "ghostty", "wezterm", "kitty":
		return true
	}
	if strings.Contains(strings.ToLower(os.Getenv("TERM")), "kitty") {
		return true
	}
	return false
}

// renderWithOSC8 applies dimStyle to plain text but preserves OSC 8 hyperlinks.
// Renders the entire string in dim first, then replaces the plain title with
// the OSC 8 hyperlinked version so SGR codes flow through naturally.
func renderWithOSC8(plain, title, boardURL string) string {
	if title == "" || boardURL == "" || !supportsOSC8() {
		return dimStyle.Render(plain)
	}
	idx := strings.Index(plain, title)
	if idx < 0 {
		return dimStyle.Render(plain)
	}
	styled := dimStyle.Render(plain)
	link := termenv.Hyperlink(boardURL, title)
	return strings.Replace(styled, title, link, 1)
}

// injectIssueLink replaces the first occurrence of "#NNN" in line with an OSC 8
// hyperlink pointing to the GitHub issue page. Returns line unchanged when the
// terminal does not support OSC 8, repo is empty, or issueNum is zero.
// Must be called AFTER all lipgloss Render() calls — lipgloss strips OSC 8 sequences.
func injectIssueLink(line, repo string, issueNum int) string {
	if !supportsOSC8() || repo == "" || issueNum == 0 {
		return line
	}
	url := fmt.Sprintf("https://github.com/%s/issues/%d", repo, issueNum)
	issueText := fmt.Sprintf("#%d", issueNum)
	link := termenv.Hyperlink(url, issueText)
	return strings.Replace(line, issueText, link, 1)
}

func (f FooterComponent) View(width int) string {
	// Assemble left side: CWD [· BoardTitle] [· version]
	parts := []string{}
	if f.projectInfo.CWD != "" {
		parts = append(parts, f.projectInfo.CWD)
	}
	if f.projectInfo.BoardTitle != "" {
		parts = append(parts, f.projectInfo.BoardTitle)
	} else if f.projectInfo.Version != "" {
		parts = append(parts, f.projectInfo.Version)
	}
	leftPlain := strings.Join(parts, " · ")

	var rightParts []string
	if whInd := f.webhookIndicator(); whInd != "" {
		rightParts = append(rightParts, whInd)
	}
	if f.graphqlStats.Limit > 0 {
		countdown := fmtRateLimitCountdown(f.graphqlStats.Reset, f.now)
		plain := fmt.Sprintf("%d/%d  %s", f.graphqlStats.Remaining, f.graphqlStats.Limit, countdown)
		rightParts = append(rightParts, f.graphqlStyle().Render(plain))
	}
	var rightStr string
	if len(rightParts) > 0 {
		rightStr = strings.Join(rightParts, "  ")
	}

	maxWidth := width - 2 // leave 1 char margin on each side
	if maxWidth < 1 {
		maxWidth = 1
	}

	if rightStr == "" {
		footer := renderWithOSC8(leftPlain, f.projectInfo.BoardTitle, f.projectInfo.BoardURL)
		if lipgloss.Width(footer) > maxWidth {
			runes := []rune(leftPlain)
			for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > maxWidth {
				runes = runes[:len(runes)-1]
			}
			footer = renderWithOSC8(string(runes)+"…", f.projectInfo.BoardTitle, f.projectInfo.BoardURL)
		}
		return footer
	}

	rightWidth := lipgloss.Width(rightStr)
	leftRendered := renderWithOSC8(leftPlain, f.projectInfo.BoardTitle, f.projectInfo.BoardURL)
	gap := maxWidth - lipgloss.Width(leftRendered) - rightWidth

	// Centre the Claude account in the gap when it fits with at least one
	// space of separation on each side. It is deliberately the first thing
	// dropped when the terminal narrows: the left (cwd/board) and right
	// (webhook/rate-limit) segments are operational state, while this is
	// standing context that the startup notice also records.
	if account := f.projectInfo.ClaudeAccount; account != "" && gap > 0 {
		rendered := dimStyle.Render(account)
		if w := lipgloss.Width(rendered); gap >= w+2 {
			leftPad := (gap - w) / 2
			return leftRendered + strings.Repeat(" ", leftPad) + rendered +
				strings.Repeat(" ", gap-w-leftPad) + rightStr
		}
	}
	if gap < 1 {
		availLeft := maxWidth - rightWidth - 1
		if availLeft < 0 {
			availLeft = 0
		}
		runes := []rune(leftPlain)
		if availLeft == 0 {
			leftRendered = ""
		} else {
			for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > availLeft {
				runes = runes[:len(runes)-1]
			}
			if len(runes) == 0 {
				leftRendered = ""
			} else {
				leftRendered = renderWithOSC8(string(runes)+"…", f.projectInfo.BoardTitle, f.projectInfo.BoardURL)
			}
		}
		gap = maxWidth - lipgloss.Width(leftRendered) - rightWidth
		if gap < 0 {
			gap = 0
		}
	}
	return leftRendered + strings.Repeat(" ", gap) + rightStr
}

func (f FooterComponent) Height() int {
	return 1
}
