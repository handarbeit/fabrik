package engine

import (
	"sync"
	"testing"
	"time"
)

func TestParseUsageLimitResetTime(t *testing.T) {
	loc, err := time.LoadLocation("America/Edmonton")
	if err != nil {
		t.Fatalf("loading America/Edmonton: %v", err)
	}

	tests := []struct {
		name      string
		resetTime string
		now       time.Time
		wantOK    bool
		want      time.Time
	}{
		{
			name:      "same day, reset later today",
			resetTime: "10:20pm (America/Edmonton)",
			now:       time.Date(2026, 7, 27, 20, 0, 0, 0, loc),
			wantOK:    true,
			want:      time.Date(2026, 7, 27, 22, 20, 0, 0, loc),
		},
		{
			name:      "already past, rolls to next day",
			resetTime: "10:20pm (America/Edmonton)",
			now:       time.Date(2026, 7, 27, 23, 0, 0, 0, loc),
			wantOK:    true,
			want:      time.Date(2026, 7, 28, 22, 20, 0, 0, loc),
		},
		{
			name:      "unknown zone",
			resetTime: "10:20pm (Nowhere/Fakezone)",
			now:       time.Date(2026, 7, 27, 20, 0, 0, 0, loc),
			wantOK:    false,
		},
		{
			name:      "malformed, no parens",
			resetTime: "10:20pm America/Edmonton",
			now:       time.Date(2026, 7, 27, 20, 0, 0, 0, loc),
			wantOK:    false,
		},
		{
			name:      "malformed clock",
			resetTime: "not-a-time (America/Edmonton)",
			now:       time.Date(2026, 7, 27, 20, 0, 0, 0, loc),
			wantOK:    false,
		},
		{
			name:      "empty",
			resetTime: "",
			now:       time.Date(2026, 7, 27, 20, 0, 0, 0, loc),
			wantOK:    false,
		},
		{
			name:      "now in a different zone than the fragment",
			resetTime: "10:20pm (America/Edmonton)",
			// 2026-07-27 20:00 America/Edmonton == 2026-07-28 04:00 UTC.
			now:       time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC),
			wantOK:    true,
			want:      time.Date(2026, 7, 27, 22, 20, 0, 0, loc),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseUsageLimitResetTime(tt.resetTime, tt.now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got=%v)", ok, tt.wantOK, got)
			}
			if !ok {
				return
			}
			if !got.Equal(tt.want) {
				t.Errorf("deadline = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComputeUsageLimitResetDeadline(t *testing.T) {
	loc, err := time.LoadLocation("America/Edmonton")
	if err != nil {
		t.Fatalf("loading America/Edmonton: %v", err)
	}
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, loc)

	t.Run("parseable reset time", func(t *testing.T) {
		deadline, parsed := computeUsageLimitResetDeadline("10:20pm (America/Edmonton)", now)
		if !parsed {
			t.Fatal("expected parsed=true")
		}
		want := time.Date(2026, 7, 27, 22, 20, 0, 0, loc)
		if !deadline.Equal(want) {
			t.Errorf("deadline = %v, want %v", deadline, want)
		}
	})

	t.Run("empty reset time falls back", func(t *testing.T) {
		deadline, parsed := computeUsageLimitResetDeadline("", now)
		if parsed {
			t.Fatal("expected parsed=false")
		}
		want := now.Add(claudeUsageLimitFallbackBackoff)
		if !deadline.Equal(want) {
			t.Errorf("deadline = %v, want %v", deadline, want)
		}
	})

	t.Run("malformed reset time falls back", func(t *testing.T) {
		deadline, parsed := computeUsageLimitResetDeadline("garbage", now)
		if parsed {
			t.Fatal("expected parsed=false")
		}
		want := now.Add(claudeUsageLimitFallbackBackoff)
		if !deadline.Equal(want) {
			t.Errorf("deadline = %v, want %v", deadline, want)
		}
	})
}

func TestClaudeSuspendedUntilTime(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	if _, ok := e.claudeSuspendedUntilTime(now); ok {
		t.Fatal("expected not suspended before any activation")
	}

	e.activateClaudeSuspension(1, "", now)
	deadline := now.Add(claudeUsageLimitFallbackBackoff)

	if got, ok := e.claudeSuspendedUntilTime(now); !ok || !got.Equal(deadline) {
		t.Fatalf("claudeSuspendedUntilTime = (%v, %v), want (%v, true)", got, ok, deadline)
	}

	// Once now reaches the deadline, suspension has lazily cleared.
	if _, ok := e.claudeSuspendedUntilTime(deadline); ok {
		t.Fatal("expected suspension to have lazily expired at the deadline")
	}
}

func TestActivateClaudeSuspension_ExtendsNotShortens(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	// First activation: fallback deadline (now + 1h).
	e.activateClaudeSuspension(1, "", now)
	first := now.Add(claudeUsageLimitFallbackBackoff)

	// Second activation with an earlier fallback base — should NOT shorten.
	e.activateClaudeSuspension(2, "", now.Add(-30*time.Minute))
	if got, ok := e.claudeSuspendedUntilTime(now); !ok || !got.Equal(first) {
		t.Fatalf("expected deadline to remain %v, got (%v, %v)", first, got, ok)
	}

	// Third activation with a later base — should extend.
	later := now.Add(2 * time.Hour)
	e.activateClaudeSuspension(3, "", later)
	want := later.Add(claudeUsageLimitFallbackBackoff)
	if got, ok := e.claudeSuspendedUntilTime(now); !ok || !got.Equal(want) {
		t.Fatalf("expected deadline to extend to %v, got (%v, %v)", want, got, ok)
	}
}

func TestClearClaudeSuspension(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	e.activateClaudeSuspension(1, "", now)
	if _, ok := e.claudeSuspendedUntilTime(now); !ok {
		t.Fatal("expected suspension to be active")
	}

	e.clearClaudeSuspension("test clear")
	if _, ok := e.claudeSuspendedUntilTime(now); ok {
		t.Fatal("expected suspension to be cleared")
	}

	// Clearing again is a harmless no-op.
	e.clearClaudeSuspension("test clear again")
}

// TestActivateClaudeSuspension_Concurrent drives N goroutines racing to
// activate the suspension with different reset times; the final deadline
// must be the max deadline any goroutine computed, and go test -race must be
// clean (validates claudeSuspendMu actually guards the field).
func TestActivateClaudeSuspension_Concurrent(t *testing.T) {
	e := testEngine(t, nil, nil)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Vary the "now" base per goroutine so deadlines differ.
			e.activateClaudeSuspension(i, "", now.Add(time.Duration(i)*time.Minute))
		}(i)
	}
	wg.Wait()

	want := now.Add(time.Duration(n-1) * time.Minute).Add(claudeUsageLimitFallbackBackoff)
	got, ok := e.claudeSuspendedUntilTime(now)
	if !ok {
		t.Fatal("expected suspension to be active")
	}
	if !got.Equal(want) {
		t.Errorf("final deadline = %v, want max deadline %v", got, want)
	}
}
