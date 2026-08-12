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

// TestFooter_NoDropBreakdownBeforeAnyDrop is R2's "nothing to show before
// the first drop" case — an operator must be able to tell "quiet" apart
// from "actively dropping," which starts with the breakdown segment simply
// not existing until something has actually been dropped.
func TestFooter_NoDropBreakdownBeforeAnyDrop(t *testing.T) {
	var f FooterComponent
	if strings.Contains(f.View(120), "dropped") {
		t.Errorf("View() shows a drop breakdown before any DropEvent:\n%s", f.View(120))
	}
}

// TestFooter_DropBreakdownAggregatesByCategory covers R3: signature,
// unwatched, and dedupe drops must be counted and reported separately, not
// lumped into one total — asserted both via the getters and via the
// rendered text actually containing each category's own count.
func TestFooter_DropBreakdownAggregatesByCategory(t *testing.T) {
	var f FooterComponent
	for _, ev := range []DropEvent{
		{Reason: "unwatched_repo", Total: 1},
		{Reason: "unwatched_repo", Total: 2},
		{Reason: "dedupe", Total: 1},
		{Reason: "malformed_envelope", Total: 1},
	} {
		comp, _ := f.Update(ev)
		f = comp.(FooterComponent)
	}

	if got := f.DropCount("unwatched_repo"); got != 2 {
		t.Errorf(`DropCount("unwatched_repo") = %d, want 2`, got)
	}
	if got := f.DropCount("dedupe"); got != 1 {
		t.Errorf(`DropCount("dedupe") = %d, want 1`, got)
	}
	if got := f.DropCount("malformed_envelope"); got != 1 {
		t.Errorf(`DropCount("malformed_envelope") = %d, want 1`, got)
	}
	if got := f.DropCount("signature_invalid"); got != 0 {
		t.Errorf(`DropCount("signature_invalid") = %d, want 0 (never reported)`, got)
	}

	view := f.View(120)
	for _, want := range []string{"dropped: 4", "unwatched 2", "dedupe 1", "other 1", "sig 0"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() = %q, want it to contain %q", view, want)
		}
	}
}

// TestFooter_DropEventOverwritesRatherThanAccumulates guards against
// double-counting: DropEvent.Total is already the daemon's cumulative
// count (see Daemon.recordDrop), so a later, smaller-looking event for the
// same reason (e.g. after a TUI event was dropped under backpressure and
// this is the self-healing next one) must replace, not add to, the
// footer's own tally.
func TestFooter_DropEventOverwritesRatherThanAccumulates(t *testing.T) {
	var f FooterComponent
	comp, _ := f.Update(DropEvent{Reason: "dedupe", Total: 5})
	f = comp.(FooterComponent)
	comp, _ = f.Update(DropEvent{Reason: "dedupe", Total: 7})
	f = comp.(FooterComponent)

	if got := f.DropCount("dedupe"); got != 7 {
		t.Errorf(`DropCount("dedupe") = %d, want 7 (overwrite, not 12)`, got)
	}
}

// TestFooter_SignatureDriftBannerAppearsOnlyWhileActive covers R4's
// escalation surface: the banner must show while active and disappear
// after the recovery transition, not linger.
func TestFooter_SignatureDriftBannerAppearsOnlyWhileActive(t *testing.T) {
	var f FooterComponent
	if f.SignatureDriftActive() {
		t.Fatal("SignatureDriftActive() = true before any SignatureDriftEvent")
	}
	if strings.Contains(f.View(120), "SIGNATURE DRIFT") {
		t.Errorf("View() shows the drift banner before any SignatureDriftEvent:\n%s", f.View(120))
	}

	comp, _ := f.Update(SignatureDriftEvent{Active: true})
	f = comp.(FooterComponent)
	if !f.SignatureDriftActive() {
		t.Fatal("SignatureDriftActive() = false after SignatureDriftEvent{Active: true}")
	}
	if !strings.Contains(f.View(120), "SIGNATURE DRIFT") {
		t.Errorf("View() does not show the drift banner while active:\n%s", f.View(120))
	}

	comp, _ = f.Update(SignatureDriftEvent{Active: false})
	f = comp.(FooterComponent)
	if f.SignatureDriftActive() {
		t.Fatal("SignatureDriftActive() = true after SignatureDriftEvent{Active: false}")
	}
	if strings.Contains(f.View(120), "SIGNATURE DRIFT") {
		t.Errorf("View() still shows the drift banner after recovery:\n%s", f.View(120))
	}
}
