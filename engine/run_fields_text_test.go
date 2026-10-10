package engine

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLastRunLine(t *testing.T) {
	cases := []struct {
		name string
		o    runOutcome
		want string
	}{
		{"completed with turns and duration", runOutcome{StageName: "Validate", Completed: true, TurnsUsed: 42, MaxTurns: 250, Duration: 18 * time.Minute}, "Validate · completed · 42/250 turns · 18m"},
		{"turn-limited", runOutcome{StageName: "Implement", TurnLimited: true, TurnsUsed: 250, MaxTurns: 250, Duration: 3 * time.Hour}, "Implement · turn-limited · 250/250 turns"},
		{"blocked on input", runOutcome{StageName: "Review", Blocked: true, TurnsUsed: 5, MaxTurns: 100, Duration: time.Minute}, "Review · blocked on input"},
		{"failed", runOutcome{StageName: "Plan", Errored: true, TurnsUsed: 3, MaxTurns: 100}, "Plan · failed"},
		{"clean exit without the marker", runOutcome{StageName: "Plan", TurnsUsed: 3, MaxTurns: 100}, "Plan · incomplete"},
		{"turn-limited beats completed", runOutcome{StageName: "Plan", Completed: true, TurnLimited: true, TurnsUsed: 9, MaxTurns: 9}, "Plan · turn-limited · 9/9 turns"},
		{"completed beats a non-zero exit", runOutcome{StageName: "Plan", Completed: true, Errored: true, TurnsUsed: 2, MaxTurns: 10, Duration: 30 * time.Second}, "Plan · completed · 2/10 turns · 30s"},
		{"unlimited budget", runOutcome{StageName: "Plan", Completed: true, TurnsUsed: 7}, "Plan · completed · 7 turns"},
		{"no turns recorded", runOutcome{StageName: "Plan", Completed: true, MaxTurns: 10, Duration: 2 * time.Minute}, "Plan · completed · 2m"},
		{"comment review", runOutcome{StageName: "Validate", IsComment: true, Completed: true, TurnsUsed: 3, MaxTurns: 15, Duration: 4 * time.Minute}, "Validate · comment review · completed · 3/15 turns · 4m"},
		{"comment review failed", runOutcome{StageName: "Validate", IsComment: true, Errored: true}, "Validate · comment review · failed"},
		{"no stage name", runOutcome{Completed: true}, "Run · completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastRunLine(tc.o); got != tc.want {
				t.Errorf("lastRunLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLastRunLine_TruncatesByRunes(t *testing.T) {
	got := lastRunLine(runOutcome{StageName: strings.Repeat("é", 80), Completed: true})
	if n := utf8.RuneCountInString(got); n > statusLineMaxLen {
		t.Errorf("line has %d runes, want <= %d: %q", n, statusLineMaxLen, got)
	}
	if !strings.HasSuffix(got, statusLineEllipsis) || !utf8.ValidString(got) {
		t.Errorf("want a valid, ellipsised line, got %q", got)
	}
}

func TestFormatRunDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:                      "45s",
		59*time.Second + 400*time.Millisecond: "59s",
		59*time.Second + 600*time.Millisecond: "1m",
		time.Minute:                           "1m",
		18*time.Minute + 20*time.Second:       "18m",
		18*time.Minute + 40*time.Second:       "19m",
		59*time.Minute + 40*time.Second:       "1h",
		time.Hour:                             "1h",
		time.Hour + 5*time.Minute:             "1h5m",
		26*time.Hour + 30*time.Minute:         "26h30m",
	}
	for d, want := range cases {
		if got := formatRunDuration(d); got != want {
			t.Errorf("formatRunDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLastActivityDateIsUTC(t *testing.T) {
	loc := time.FixedZone("UTC+10", 10*3600)
	ts := time.Date(2026, 10, 11, 5, 0, 0, 0, loc) // 2026-10-10 19:00 UTC
	if got := lastActivityDate(ts); got != "2026-10-10" {
		t.Errorf("lastActivityDate = %q, want 2026-10-10", got)
	}
}
