package tui

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestFooter_SessionTotalsAccumulateFromReviewedOnly(t *testing.T) {
	var f FooterComponent
	comp, _ := f.Update(ReviewCompletedEvent{Reviewed: true, NumTurns: 5, CostUSD: 0.10})
	f = comp.(FooterComponent)
	comp, _ = f.Update(ReviewCompletedEvent{Reviewed: true, NumTurns: 3, CostUSD: 0.05})
	f = comp.(FooterComponent)
	// Skipped and errored completions must not contribute to the session total.
	comp, _ = f.Update(ReviewCompletedEvent{Skipped: true, Reason: "draft"})
	f = comp.(FooterComponent)
	comp, _ = f.Update(ReviewCompletedEvent{Err: "boom"})
	f = comp.(FooterComponent)

	if f.ReviewedCount() != 2 {
		t.Errorf("ReviewedCount() = %d, want 2", f.ReviewedCount())
	}
	if f.TotalTurns() != 8 {
		t.Errorf("TotalTurns() = %d, want 8", f.TotalTurns())
	}
	if got, want := f.TotalCostUSD(), 0.15; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("TotalCostUSD() = %v, want %v", got, want)
	}

	view := f.View(120)
	if !strings.Contains(view, "2 reviewed") || !strings.Contains(view, "8 turns") {
		t.Errorf("View() does not show session totals:\n%s", view)
	}
}

func TestFooter_RateLimitGaugeColoring(t *testing.T) {
	cases := []struct {
		name             string
		remaining, limit int
	}{
		{"healthy", 800, 1000},
		{"warning", 300, 1000},
		{"critical", 50, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f FooterComponent
			comp, _ := f.Update(RateLimitSnapshotEvent{Stats: gh.RateLimitStats{
				Limit: tc.limit, Remaining: tc.remaining, Reset: time.Now().Add(10 * time.Minute),
			}})
			f = comp.(FooterComponent)
			comp, _ = f.Update(TickEvent{At: time.Now()})
			f = comp.(FooterComponent)

			view := f.View(120)
			if !strings.Contains(view, "rest") {
				t.Errorf("View() does not show rest rate-limit gauge:\n%s", view)
			}
		})
	}
}

func TestFooter_NoRateLimitDataOmitsGauge(t *testing.T) {
	var f FooterComponent
	if strings.Contains(f.View(120), "rest") {
		t.Errorf("View() shows rate-limit gauge before any RateLimitSnapshotEvent:\n%s", f.View(120))
	}
}
