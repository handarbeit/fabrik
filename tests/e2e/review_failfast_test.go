//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestReviewFailFastDecision unit-tests reviewFailFastDue's boundary
// conditions (handarbeit/fabrik#1396, R3/R4). It requires no live bed:
//
//	go test -tags e2e -run TestReviewFailFastDecision -v ./tests/e2e/
func TestReviewFailFastDecision(t *testing.T) {
	window := 10 * time.Minute
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		readyAt     time.Time
		reviewCount int
		now         time.Time
		want        bool
	}{
		{
			name:        "not yet ready (zero readyAt) never fires, however long",
			readyAt:     time.Time{},
			reviewCount: 0,
			now:         now,
			want:        false,
		},
		{
			name:        "ready but has a review never fires, however long",
			readyAt:     now.Add(-24 * time.Hour),
			reviewCount: 1,
			now:         now,
			want:        false,
		},
		{
			name:        "ready with a DISMISSED-or-any review still never fires (author/state agnostic)",
			readyAt:     now.Add(-24 * time.Hour),
			reviewCount: 3,
			now:         now,
			want:        false,
		},
		{
			name:        "just before the window elapses does not fire",
			readyAt:     now.Add(-window + time.Second),
			reviewCount: 0,
			now:         now,
			want:        false,
		},
		{
			name:        "exactly at the window fires (>=)",
			readyAt:     now.Add(-window),
			reviewCount: 0,
			now:         now,
			want:        true,
		},
		{
			name:        "past the window with zero reviews fires",
			readyAt:     now.Add(-window - time.Minute),
			reviewCount: 0,
			now:         now,
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewFailFastDue(tt.readyAt, tt.reviewCount, tt.now, window)
			if got != tt.want {
				t.Errorf("reviewFailFastDue(readyAt=%v, reviewCount=%d, now=%v, window=%v) = %v, want %v",
					tt.readyAt, tt.reviewCount, tt.now, window, got, tt.want)
			}
		})
	}
}

// TestReviewFailFastWindowDefault confirms the documented default and the
// E2E_REVIEW_FAILFAST_WINDOW override, mirroring pollBase()'s own test
// convention (t.Setenv auto-restores).
func TestReviewFailFastWindowDefault(t *testing.T) {
	if got := reviewFailFastWindow(); got != defaultReviewFailFastWindow {
		t.Errorf("reviewFailFastWindow() = %v, want default %v", got, defaultReviewFailFastWindow)
	}

	t.Setenv("E2E_REVIEW_FAILFAST_WINDOW", "5m")
	if got := reviewFailFastWindow(); got != 5*time.Minute {
		t.Errorf("reviewFailFastWindow() with override = %v, want 5m", got)
	}

	t.Setenv("E2E_REVIEW_FAILFAST_WINDOW", "not-a-duration")
	if got := reviewFailFastWindow(); got != defaultReviewFailFastWindow {
		t.Errorf("reviewFailFastWindow() with invalid override = %v, want default %v", got, defaultReviewFailFastWindow)
	}
}
