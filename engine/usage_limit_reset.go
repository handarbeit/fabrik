package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"time"

	"github.com/handarbeit/fabrik/internal/claudeerr"
)

// usageLimitMaxResetHorizon is the ceiling applied to a structured reset
// instant (R4, ADR-1815): the longest window the CLI reports is seven_day, so a
// genuinely exhausted window can legitimately be up to 7 days away; one day of
// margin is added so a slightly-early detection is never clamped. Anything
// further out is treated as an absurd value and clamped rather than stranding
// the account. The largest seven_day reset observed in captured transcripts is
// about 5.3 days out.
const usageLimitMaxResetHorizon = 8 * 24 * time.Hour

// usageLimitMaxResetsAtSeconds bounds a decoded resetsAt (Unix seconds) before
// it is converted to an int64: ~year 5138. It exists so a millisecond-scale or
// otherwise absurd float cannot overflow the conversion; such a value is
// classified malformed rather than clamped.
const usageLimitMaxResetsAtSeconds = 1e11

// Deadline sources reported by resolveUsageLimitDeadline.
const (
	usageLimitSourceExact    = "exact"
	usageLimitSourceClamped  = "clamped"
	usageLimitSourceFallback = "fallback"
)

// usageLimitReasonPast is the fallback reason for a structured reset instant
// that is not after now. It is decided at resolve time (the other reasons are
// decided at decode time, see claudeerr.ResetReason*).
const usageLimitReasonPast = "past"

// usageLimitWindow is one entry of rate_limit_info.unifiedWindows. Pointers
// distinguish a missing field from a zero one. The CLI writes utilization as a
// bare JSON integer (1) when a window is exhausted, which float64 decodes fine.
type usageLimitWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *float64 `json:"resetsAt"`
}

// extractUsageLimitReset scans the CLI's raw NDJSON stream for the structured
// reset instant of an exhausted usage window (ADR-1815). unifiedWindows is not
// on the terminal result line: it sits on a separate {"type":"rate_limit_event"}
// line that arrives before it, so this is a scan of the raw stream, independent
// of parseClaudeJSON (which discards a whole object on any unmarshal error —
// nothing here can make classification fail).
//
// Only called after classifyUsageLimitExit has already fired: the trigger stays
// purely structural and this scan only supplies the deadline. It never reads
// assistant text, the result string, or overageStatus/overageResetsAt (R2).
//
// The last rate_limit_event carrying unifiedWindows wins (in every captured 429
// stream the last event is the rejected one). Within it the reset is the latest
// resetsAt among windows whose utilization is >= 1 — windows below 1 must not
// contribute, since seven_day sits below 1 and resets days after five_hour
// (R1a). Window keys are read generically. When no instant results, the second
// return value is the claudeerr.ResetReason* naming why.
func extractUsageLimitReset(rawOutput []byte) (time.Time, string) {
	scanner := bufio.NewScanner(bytes.NewReader(rawOutput))
	// A line can never be longer than the whole input; see forEachAssistantText.
	scanner.Buffer(make([]byte, 0, 64*1024), len(rawOutput)+1)

	resetAt, reason := time.Time{}, claudeerr.ResetReasonAbsent
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.Contains(line, []byte("rate_limit_event")) {
			continue
		}
		at, r, carried := evalRateLimitEvent(line)
		// A malformed line never displaces an earlier valid instant.
		if carried && !(r == claudeerr.ResetReasonMalformed && !resetAt.IsZero()) {
			resetAt, reason = at, r
		}
	}
	return resetAt, reason
}

// evalRateLimitEvent evaluates one candidate stream line. carried is false when
// the line is not a rate_limit_event, or is one without unifiedWindows — those
// never displace an earlier event's result.
func evalRateLimitEvent(line []byte) (resetAt time.Time, reason string, carried bool) {
	var envelope struct {
		Type          string          `json:"type"`
		RateLimitInfo json.RawMessage `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		// The line mentions rate_limit_event but is not decodable JSON.
		return time.Time{}, claudeerr.ResetReasonMalformed, true
	}
	if envelope.Type != "rate_limit_event" || isJSONNullOrEmpty(envelope.RateLimitInfo) {
		return time.Time{}, "", false
	}
	var info struct {
		UnifiedWindows json.RawMessage `json:"unifiedWindows"`
	}
	if err := json.Unmarshal(envelope.RateLimitInfo, &info); err != nil {
		return time.Time{}, claudeerr.ResetReasonMalformed, true
	}
	if isJSONNullOrEmpty(info.UnifiedWindows) {
		return time.Time{}, "", false
	}
	var windows map[string]json.RawMessage
	if err := json.Unmarshal(info.UnifiedWindows, &windows); err != nil {
		return time.Time{}, claudeerr.ResetReasonMalformed, true
	}
	if len(windows) == 0 {
		return time.Time{}, "", false
	}

	var latest float64
	var sawMalformed, sawZero, sawWindow bool
	for _, raw := range windows {
		var w usageLimitWindow
		if err := json.Unmarshal(raw, &w); err != nil || w.Utilization == nil {
			sawMalformed = true
			continue
		}
		sawWindow = true
		if *w.Utilization < 1 {
			continue
		}
		switch {
		case w.ResetsAt == nil || *w.ResetsAt <= 0:
			sawZero = true
		case *w.ResetsAt > usageLimitMaxResetsAtSeconds:
			sawMalformed = true
		case *w.ResetsAt > latest:
			latest = *w.ResetsAt
		}
	}
	// A valid exhausted window wins over a sibling that is malformed.
	switch {
	case latest > 0:
		return time.Unix(int64(latest), 0), "", true
	case sawMalformed:
		return time.Time{}, claudeerr.ResetReasonMalformed, true
	case sawZero:
		return time.Time{}, claudeerr.ResetReasonZero, true
	case sawWindow:
		return time.Time{}, claudeerr.ResetReasonNoExhaustedWindow, true
	}
	return time.Time{}, claudeerr.ResetReasonMalformed, true
}

func isJSONNullOrEmpty(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// resolveUsageLimitDeadline computes the deadline a usage-limit suspension
// should run until. It is the single place R3 (absent/zero/malformed/past/no
// exhausted window degrade to claudeUsageLimitFallbackBackoff) and R4 (clamp to
// usageLimitMaxResetHorizon) are applied. limitErr may be nil (no structured
// reset). source is one of usageLimitSource*; reason is set only for
// usageLimitSourceFallback and names the R3 condition that applied.
func resolveUsageLimitDeadline(limitErr *claudeUsageLimitError, now time.Time) (deadline time.Time, source, reason string) {
	if limitErr == nil || limitErr.ResetAt.IsZero() {
		reason = claudeerr.ResetReasonAbsent
		if limitErr != nil && limitErr.ResetFallbackReason != "" {
			reason = limitErr.ResetFallbackReason
		}
		return now.Add(claudeUsageLimitFallbackBackoff), usageLimitSourceFallback, reason
	}
	if !limitErr.ResetAt.After(now) {
		return now.Add(claudeUsageLimitFallbackBackoff), usageLimitSourceFallback, usageLimitReasonPast
	}
	if ceiling := now.Add(usageLimitMaxResetHorizon); limitErr.ResetAt.After(ceiling) {
		return ceiling, usageLimitSourceClamped, ""
	}
	return limitErr.ResetAt, usageLimitSourceExact, ""
}

// formatUsageLimitReset renders a resolved suspension deadline for the
// operator-facing log line and comment (R6). It is formatted from the
// structured instant in engine-local time — never copied from result prose.
func formatUsageLimitReset(deadline time.Time) string {
	return deadline.Local().Format("2006-01-02 15:04 MST")
}

// usageLimitResetSuffix is the " (resets X)" fragment for the usage-limit log
// line and comment (R6). It is rendered from the structured instant actually in
// force (exact or clamped), never from result prose, and is "" on the fixed
// fallback so the text never claims a time Fabrik did not source.
func usageLimitResetSuffix(limitErr *claudeUsageLimitError, now time.Time) string {
	deadline, source, _ := resolveUsageLimitDeadline(limitErr, now)
	if source == usageLimitSourceFallback {
		return ""
	}
	return " (resets " + formatUsageLimitReset(deadline) + ")"
}
