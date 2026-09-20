package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/claudeerr"
)

// rateLimitEventLine builds a {"type":"rate_limit_event"} NDJSON line with the
// given raw unifiedWindows JSON, shaped like the CLI's own output.
func rateLimitEventLine(unifiedWindows string) string {
	return `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","isUsingOverage":false,"unifiedWindows":` + unifiedWindows + `}}`
}

const (
	// 2026-09-18 03:30:00-04:00, the captured five_hour reset from the issue.
	testFiveHourReset = 1789716600
	// seven_day reset, days later.
	testSevenDayReset = 1790175600
)

func TestExtractUsageLimitReset(t *testing.T) {
	wantFive := time.Unix(testFiveHourReset, 0)
	wantSeven := time.Unix(testSevenDayReset, 0)
	window := func(util string, reset int64) string {
		return fmt.Sprintf(`{"utilization":%s,"resetsAt":%d}`, util, reset)
	}

	tests := []struct {
		name       string
		stream     string
		wantAt     time.Time
		wantReason string
	}{
		{
			// The over-suspension guard: seven_day is below 1 and resets days
			// later, so it must never win (R1a).
			name:   "real 429 shape: exhausted five_hour wins over later non-exhausted seven_day",
			stream: rateLimitEventLine(`{"five_hour":` + window("1", testFiveHourReset) + `,"seven_day":` + window("0.65", testSevenDayReset) + `}`),
			wantAt: wantFive,
		},
		{
			name:   "two exhausted windows: the later reset wins",
			stream: rateLimitEventLine(`{"five_hour":` + window("1", testFiveHourReset) + `,"seven_day":` + window("1", testSevenDayReset) + `}`),
			wantAt: wantSeven,
		},
		{
			name:   "unknown future window key that is exhausted is honoured",
			stream: rateLimitEventLine(`{"five_hour":` + window("0.2", testFiveHourReset) + `,"seven_day_opus":` + window("1.0", testSevenDayReset) + `}`),
			wantAt: wantSeven,
		},
		{
			name:   "float utilization and resetsAt decode",
			stream: rateLimitEventLine(`{"five_hour":{"utilization":1.0,"resetsAt":1789716600.0}}`),
			wantAt: wantFive,
		},
		{
			name:       "no exhausted window",
			stream:     rateLimitEventLine(`{"five_hour":` + window("0.99", testFiveHourReset) + `,"seven_day":` + window("0.65", testSevenDayReset) + `}`),
			wantReason: claudeerr.ResetReasonNoExhaustedWindow,
		},
		{
			name:       "no rate_limit_event at all",
			stream:     `{"type":"system","subtype":"init"}` + "\n" + `{"type":"result","is_error":true}`,
			wantReason: claudeerr.ResetReasonAbsent,
		},
		{
			name:       "rate_limit_event without unifiedWindows",
			stream:     `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789716600}}`,
			wantReason: claudeerr.ResetReasonAbsent,
		},
		{
			name:       "empty unifiedWindows object",
			stream:     rateLimitEventLine(`{}`),
			wantReason: claudeerr.ResetReasonAbsent,
		},
		{
			name:       "exhausted window with resetsAt 0",
			stream:     rateLimitEventLine(`{"five_hour":` + window("1", 0) + `}`),
			wantReason: claudeerr.ResetReasonZero,
		},
		{
			name:       "exhausted window with resetsAt missing",
			stream:     rateLimitEventLine(`{"five_hour":{"utilization":1}}`),
			wantReason: claudeerr.ResetReasonZero,
		},
		{
			name:       "unifiedWindows of the wrong type",
			stream:     rateLimitEventLine(`"five_hour"`),
			wantReason: claudeerr.ResetReasonMalformed,
		},
		{
			name:       "window of the wrong type",
			stream:     rateLimitEventLine(`{"five_hour":"full"}`),
			wantReason: claudeerr.ResetReasonMalformed,
		},
		{
			name:       "string resetsAt is malformed",
			stream:     rateLimitEventLine(`{"five_hour":{"utilization":1,"resetsAt":"1789716600"}}`),
			wantReason: claudeerr.ResetReasonMalformed,
		},
		{
			name:       "millisecond-scale resetsAt is malformed, not clamped",
			stream:     rateLimitEventLine(`{"five_hour":{"utilization":1,"resetsAt":1789716600000000}}`),
			wantReason: claudeerr.ResetReasonMalformed,
		},
		{
			name:       "undecodable line mentioning rate_limit_event",
			stream:     `{"type":"rate_limit_event","rate_limit_info":{`,
			wantReason: claudeerr.ResetReasonMalformed,
		},
		{
			name:   "wrong-typed window beside a valid exhausted one: the valid one wins",
			stream: rateLimitEventLine(`{"five_hour":` + window("1", testFiveHourReset) + `,"seven_day":"oops"}`),
			wantAt: wantFive,
		},
		{
			name: "last event wins",
			stream: rateLimitEventLine(`{"five_hour":`+window("1", testSevenDayReset)+`}`) + "\n" +
				rateLimitEventLine(`{"five_hour":`+window("1", testFiveHourReset)+`}`),
			wantAt: wantFive,
		},
		{
			name: "a later event without unifiedWindows does not displace an earlier one",
			stream: rateLimitEventLine(`{"five_hour":`+window("1", testFiveHourReset)+`}`) + "\n" +
				`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`,
			wantAt: wantFive,
		},
		{
			name: "a later junk line does not displace an earlier valid instant",
			stream: rateLimitEventLine(`{"five_hour":`+window("1", testFiveHourReset)+`}`) + "\n" +
				`garbage rate_limit_event {`,
			wantAt: wantFive,
		},
		{
			// R2: prose is ignored. The assistant text and the result string both
			// carry a different time; neither may be used.
			name: "prose times in assistant text and result are ignored",
			stream: `{"type":"assistant","message":{"content":[{"type":"text","text":"{\"type\":\"rate_limit_event\"} resets 11:59pm (America/Edmonton)"}]}}` + "\n" +
				`{"type":"result","is_error":true,"result":"You've hit your session limit · resets 11:59pm (America/Edmonton)"}`,
			wantReason: claudeerr.ResetReasonAbsent,
		},
		{
			name: "overage fields do not participate",
			stream: `{"type":"rate_limit_event","rate_limit_info":{"overageStatus":"allowed","overageResetsAt":1790175600,"unifiedWindows":{"five_hour":` +
				window("1", testFiveHourReset) + `}}}`,
			wantAt: wantFive,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := extractUsageLimitReset([]byte(tt.stream))
			if !got.Equal(tt.wantAt) {
				t.Errorf("resetAt = %v, want %v", got, tt.wantAt)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// TestExtractUsageLimitReset_VeryLongInterleavedLine proves the scanner buffer
// is sized to the input: a >64KB line before the event must not stop the scan.
func TestExtractUsageLimitReset_VeryLongInterleavedLine(t *testing.T) {
	long := `{"type":"user","message":{"content":"` + strings.Repeat("x", 200*1024) + `"}}`
	stream := long + "\n" + rateLimitEventLine(`{"five_hour":{"utilization":1,"resetsAt":1789716600}}`) + "\n" + long
	got, reason := extractUsageLimitReset([]byte(stream))
	if !got.Equal(time.Unix(testFiveHourReset, 0)) || reason != "" {
		t.Errorf("got (%v, %q), want (%v, \"\")", got, reason, time.Unix(testFiveHourReset, 0))
	}
}

func TestResolveUsageLimitDeadline(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
	fallback := now.Add(claudeUsageLimitFallbackBackoff)

	tests := []struct {
		name       string
		err        *claudeUsageLimitError
		want       time.Time
		wantSource string
		wantReason string
	}{
		{"nil error is absent", nil, fallback, usageLimitSourceFallback, claudeerr.ResetReasonAbsent},
		{"zero ResetAt is absent", &claudeUsageLimitError{}, fallback, usageLimitSourceFallback, claudeerr.ResetReasonAbsent},
		{"zero ResetAt keeps the decode-time reason", &claudeUsageLimitError{ResetFallbackReason: claudeerr.ResetReasonNoExhaustedWindow}, fallback, usageLimitSourceFallback, claudeerr.ResetReasonNoExhaustedWindow},
		{"already past", &claudeUsageLimitError{ResetAt: now.Add(-time.Minute)}, fallback, usageLimitSourceFallback, usageLimitReasonPast},
		{"exactly now is past", &claudeUsageLimitError{ResetAt: now}, fallback, usageLimitSourceFallback, usageLimitReasonPast},
		{"one second ahead is exact", &claudeUsageLimitError{ResetAt: now.Add(time.Second)}, now.Add(time.Second), usageLimitSourceExact, ""},
		{"ninety minutes ahead is exact", &claudeUsageLimitError{ResetAt: now.Add(90 * time.Minute)}, now.Add(90 * time.Minute), usageLimitSourceExact, ""},
		{"a legitimately exhausted seven_day (5 days) is not clamped", &claudeUsageLimitError{ResetAt: now.Add(5 * 24 * time.Hour)}, now.Add(5 * 24 * time.Hour), usageLimitSourceExact, ""},
		{"exactly at the ceiling is exact", &claudeUsageLimitError{ResetAt: now.Add(usageLimitMaxResetHorizon)}, now.Add(usageLimitMaxResetHorizon), usageLimitSourceExact, ""},
		{"far future is clamped", &claudeUsageLimitError{ResetAt: now.Add(400 * 24 * time.Hour)}, now.Add(usageLimitMaxResetHorizon), usageLimitSourceClamped, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source, reason := resolveUsageLimitDeadline(tt.err, now)
			if !got.Equal(tt.want) || source != tt.wantSource || reason != tt.wantReason {
				t.Errorf("got (%v, %q, %q), want (%v, %q, %q)", got, source, reason, tt.want, tt.wantSource, tt.wantReason)
			}
		})
	}
	if usageLimitMaxResetHorizon < 7*24*time.Hour {
		t.Errorf("ceiling %v is shorter than the seven_day window (R4)", usageLimitMaxResetHorizon)
	}
}

func TestUsageLimitResetSuffix(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
	resetAt := now.Add(90 * time.Minute)

	if got, want := usageLimitResetSuffix(&claudeUsageLimitError{ResetAt: resetAt}, now), " (resets "+formatUsageLimitReset(resetAt)+")"; got != want {
		t.Errorf("exact suffix = %q, want %q", got, want)
	}
	if got := usageLimitResetSuffix(&claudeUsageLimitError{ResetAt: now.Add(-time.Hour)}, now); got != "" {
		t.Errorf("past suffix = %q, want empty (must not claim an unsourced time)", got)
	}
	if got := usageLimitResetSuffix(&claudeUsageLimitError{}, now); got != "" {
		t.Errorf("absent suffix = %q, want empty", got)
	}
	// Clamped: shows the ceiling actually in force, not the absurd claimed time.
	far := &claudeUsageLimitError{ResetAt: now.Add(400 * 24 * time.Hour)}
	if got, want := usageLimitResetSuffix(far, now), " (resets "+formatUsageLimitReset(now.Add(usageLimitMaxResetHorizon))+")"; got != want {
		t.Errorf("clamped suffix = %q, want %q", got, want)
	}
}

// TestInterpretClaudeResult_UsageLimit_CarriesStructuredReset is the
// full-pipeline check: a real 429 stream (rate_limit_event line before the
// result line) yields the exhausted five_hour instant, and prose in result that
// names a different time is not used.
func TestInterpretClaudeResult_UsageLimit_CarriesStructuredReset(t *testing.T) {
	stream := rateLimitEventLine(`{"five_hour":{"utilization":1,"resetsAt":1789716600},"seven_day":{"utilization":0.65,"resetsAt":1790175600}}`) + "\n" +
		`{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","api_error_status":429,` +
		`"num_turns":1,"total_cost_usd":0,"result":"You've hit your session limit · resets 11:59pm (America/Edmonton)"}`

	for name, raw := range map[string]string{
		"429 api_error":  stream,
		"blocking_limit": strings.Replace(stream, `"api_error","api_error_status":429`, `"blocking_limit"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := interpretRaw(t, raw)
			var limitErr *claudeUsageLimitError
			if !errors.As(err, &limitErr) {
				t.Fatalf("errors.As(*claudeUsageLimitError) = false; err = %v", err)
			}
			if want := time.Unix(testFiveHourReset, 0); !limitErr.ResetAt.Equal(want) {
				t.Errorf("ResetAt = %v, want %v", limitErr.ResetAt, want)
			}
			if limitErr.ResetFallbackReason != "" {
				t.Errorf("ResetFallbackReason = %q, want empty", limitErr.ResetFallbackReason)
			}
		})
	}
}

// TestInterpretClaudeResult_UsageLimit_ExclusionsUnchanged checks that a
// rate_limit_event in the stream does not widen detection: a mid-session 429
// (real turns and cost) is still not a usage-limit exit.
func TestInterpretClaudeResult_UsageLimit_ExclusionsUnchanged(t *testing.T) {
	raw := rateLimitEventLine(`{"five_hour":{"utilization":1,"resetsAt":1789716600}}`) + "\n" +
		`{"type":"result","is_error":true,"terminal_reason":"api_error","api_error_status":429,"num_turns":12,"total_cost_usd":0.4,"result":"x"}`
	_, _, _, err := interpretClaudeResult(context.Background(), 1, []byte(raw), errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir(), "", 2, -1)
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("mid-session 429 was classified as a usage-limit exit: %v", err)
	}
}
