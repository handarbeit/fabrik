package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// FooterComponent renders the bottom status bar: the running session-total
// cost/turns (accumulated in-memory across every reviewed PR, reset on
// daemon restart — matching Fabrik's own session-scoped TUI totals, see
// adrs/1114-pruefer-tui-architecture.md) and the GitHub REST API rate-limit
// gauge (github.Client.RateLimitStats()'s "rest" return value — Pruefer
// issues no GraphQL requests, so "graphql" would always read zero).
type FooterComponent struct {
	reviewedCount int
	totalTurns    int
	totalCostUSD  float64
	restStats     rateLimitStats
	now           time.Time
}

// rateLimitStats mirrors github.RateLimitStats's fields needed for display,
// avoiding importing the whole github package into this file (events.go
// already does, for RateLimitSnapshotEvent — this local type just narrows
// what footer.go itself reads from it).
type rateLimitStats struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

func (f FooterComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		f.now = ev.At
	case ReviewCompletedEvent:
		if ev.Reviewed {
			f.reviewedCount++
			f.totalTurns += ev.NumTurns
			f.totalCostUSD += ev.CostUSD
		}
	case RateLimitSnapshotEvent:
		f.restStats = rateLimitStats{Limit: ev.Stats.Limit, Remaining: ev.Stats.Remaining, Reset: ev.Stats.Reset}
	}
	return f, nil
}

func (f FooterComponent) View(width int) string {
	leftPlain := fmt.Sprintf("session: %d reviewed  ·  %d turns  ·  $%.4f", f.reviewedCount, f.totalTurns, f.totalCostUSD)
	left := dimStyle.Render(leftPlain)

	var right string
	if f.restStats.Limit > 0 {
		countdown := fmtRateLimitCountdown(f.restStats.Reset, f.now)
		plain := fmt.Sprintf("rest %d/%d  %s", f.restStats.Remaining, f.restStats.Limit, countdown)
		pct := float64(f.restStats.Remaining) / float64(f.restStats.Limit)
		var style lipgloss.Style
		switch {
		case pct > 0.5:
			style = successStyle
		case pct > 0.2:
			style = activeStyle
		default:
			style = failStyle
		}
		right = style.Render(plain)
	}

	maxWidth := width - 2
	if maxWidth < 1 {
		maxWidth = 1
	}
	if right == "" {
		if lipgloss.Width(left) > maxWidth {
			runes := []rune(leftPlain)
			for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > maxWidth {
				runes = runes[:len(runes)-1]
			}
			left = dimStyle.Render(string(runes) + "…")
		}
		return left
	}

	rightWidth := lipgloss.Width(right)
	gap := maxWidth - lipgloss.Width(left) - rightWidth
	if gap < 1 {
		availLeft := maxWidth - rightWidth - 1
		if availLeft < 0 {
			availLeft = 0
		}
		runes := []rune(leftPlain)
		for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > availLeft {
			runes = runes[:len(runes)-1]
		}
		if len(runes) == 0 {
			left = ""
		} else {
			left = dimStyle.Render(string(runes) + "…")
		}
		gap = maxWidth - lipgloss.Width(left) - rightWidth
		if gap < 0 {
			gap = 0
		}
	}
	return left + strings.Repeat(" ", gap) + right
}

func (f FooterComponent) Height() int {
	return 1
}

// TotalCostUSD returns the accumulated session-total review cost.
func (f FooterComponent) TotalCostUSD() float64 {
	return f.totalCostUSD
}

// TotalTurns returns the accumulated session-total review turns.
func (f FooterComponent) TotalTurns() int {
	return f.totalTurns
}

// ReviewedCount returns the accumulated session-total number of reviewed PRs.
func (f FooterComponent) ReviewedCount() int {
	return f.reviewedCount
}
