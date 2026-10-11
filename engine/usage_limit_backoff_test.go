package engine

import (
	"sync"
	"testing"
	"time"
)

func TestClaudeUsageLimitedUntilTime(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	if _, ok := e.claudeUsageLimitedUntilTime(now); ok {
		t.Fatal("expected not usage limited before any activation")
	}

	e.activateClaudeUsageLimit(1, nil, now)
	deadline := now.Add(claudeUsageLimitFallbackBackoff)

	if got, ok := e.claudeUsageLimitedUntilTime(now); !ok || !got.Equal(deadline) {
		t.Fatalf("claudeUsageLimitedUntilTime = (%v, %v), want (%v, true)", got, ok, deadline)
	}

	// Once now reaches the deadline, usage-limit hold has lazily cleared.
	if _, ok := e.claudeUsageLimitedUntilTime(deadline); ok {
		t.Fatal("expected usage-limit hold to have lazily expired at the deadline")
	}
}

func TestActivateClaudeUsageLimit_ExtendsNotShortens(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	// First activation: fallback deadline (now + 1h).
	e.activateClaudeUsageLimit(1, nil, now)
	first := now.Add(claudeUsageLimitFallbackBackoff)

	// Second activation with an earlier fallback base — should NOT shorten.
	e.activateClaudeUsageLimit(2, nil, now.Add(-30*time.Minute))
	if got, ok := e.claudeUsageLimitedUntilTime(now); !ok || !got.Equal(first) {
		t.Fatalf("expected deadline to remain %v, got (%v, %v)", first, got, ok)
	}

	// Third activation with a later base — should extend.
	later := now.Add(2 * time.Hour)
	e.activateClaudeUsageLimit(3, nil, later)
	want := later.Add(claudeUsageLimitFallbackBackoff)
	if got, ok := e.claudeUsageLimitedUntilTime(now); !ok || !got.Equal(want) {
		t.Fatalf("expected deadline to extend to %v, got (%v, %v)", want, got, ok)
	}
}

func TestClearClaudeUsageLimit(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	e.activateClaudeUsageLimit(1, nil, now)
	if _, ok := e.claudeUsageLimitedUntilTime(now); !ok {
		t.Fatal("expected usage-limit hold to be active")
	}

	e.clearClaudeUsageLimit("test clear")
	if _, ok := e.claudeUsageLimitedUntilTime(now); ok {
		t.Fatal("expected usage-limit hold to be cleared")
	}

	// Clearing again is a harmless no-op.
	e.clearClaudeUsageLimit("test clear again")
}

// TestActivateClaudeUsageLimit_Concurrent drives N goroutines racing to
// activate the usage-limit hold with different reset times; the final deadline
// must be the max deadline any goroutine computed, and go test -race must be
// clean (validates claudeUsageLimitMu actually guards the field).
func TestActivateClaudeUsageLimit_Concurrent(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Vary the "now" base per goroutine so deadlines differ.
			e.activateClaudeUsageLimit(i, nil, now.Add(time.Duration(i)*time.Minute))
		}(i)
	}
	wg.Wait()

	want := now.Add(time.Duration(n-1) * time.Minute).Add(claudeUsageLimitFallbackBackoff)
	got, ok := e.claudeUsageLimitedUntilTime(now)
	if !ok {
		t.Fatal("expected usage-limit hold to be active")
	}
	if !got.Equal(want) {
		t.Errorf("final deadline = %v, want max deadline %v", got, want)
	}
}

// TestActivateClaudeUsageLimit_StructuredResetSetsDeadline verifies the
// usage-limit hold runs until the structured reset instant, not the fixed fallback,
// and that extend-never-shorten still holds against it (ADR-1815, R1/R5).
func TestActivateClaudeUsageLimit_StructuredResetSetsDeadline(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
	resetAt := now.Add(90 * time.Minute)

	e.activateClaudeUsageLimit(1, &claudeUsageLimitError{Message: "m", ResetAt: resetAt}, now)
	if got, ok := e.claudeUsageLimitedUntilTime(now); !ok || !got.Equal(resetAt) {
		t.Fatalf("deadline = (%v, %v), want (%v, true)", got, ok, resetAt)
	}

	// An earlier structured instant never shortens the usage-limit hold.
	e.activateClaudeUsageLimit(2, &claudeUsageLimitError{Message: "m", ResetAt: now.Add(10 * time.Minute)}, now)
	if got, _ := e.claudeUsageLimitedUntilTime(now); !got.Equal(resetAt) {
		t.Fatalf("deadline shortened to %v, want it to remain %v", got, resetAt)
	}

	// A later one extends it.
	later := now.Add(3 * time.Hour)
	e.activateClaudeUsageLimit(3, &claudeUsageLimitError{Message: "m", ResetAt: later}, now)
	if got, _ := e.claudeUsageLimitedUntilTime(now); !got.Equal(later) {
		t.Fatalf("deadline = %v, want extended to %v", got, later)
	}
}
