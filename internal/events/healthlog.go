package events

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// HealthErrorReminderInterval is how long an unchanged error is suppressed
// before HealthErrorLog reports it again with a count.
const HealthErrorReminderInterval = 5 * time.Minute

// healthErrMaxRunes caps the logged error text. Session-creation errors can
// embed up to 4 KiB of an HTTP response body.
const healthErrMaxRunes = 300

// SanitizeErrText prepares error text for a single log line: whitespace runs
// (including newlines) collapse to one space and the result is truncated on a
// rune boundary with a trailing "…". It is for logging only; callers must not
// use it to rewrite HealthEvent.Err or to compare errors.
func SanitizeErrText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= healthErrMaxRunes {
		return s
	}
	return string(r[:healthErrMaxRunes]) + "…"
}

// HealthErrorLog coalesces the Err text of HealthEvents into log lines. It
// logs nothing itself: Observe returns the line and the caller writes it
// through its own logger (callers own their log routing, ADR 1142). The zero
// value is ready to use and safe for concurrent use.
//
// An episode runs from the first Reconnecting event after a Connected one
// (or from the start) until the next Connected event. Within an episode the
// first error is reported, a changed error text is reported at once, and an
// unchanged one is reported again only after HealthErrorReminderInterval,
// with the number of occurrences since the previous report.
type HealthErrorLog struct {
	// Now supplies the time when an event's At is zero; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	active   bool
	lastErr  string
	lastAt   time.Time
	suppress int
}

// Observe feeds one health event and returns the line to log, if any. The
// line has no trailing newline. A Connected event resets the episode and never
// yields a line; a Reconnecting event with empty Err yields none either.
func (l *HealthErrorLog) Observe(ev HealthEvent) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if ev.State == HealthConnected {
		l.active = false
		l.lastErr = ""
		l.suppress = 0
		return "", false
	}
	if ev.Err == "" {
		return "", false
	}
	at := ev.At
	if at.IsZero() {
		if l.Now != nil {
			at = l.Now()
		} else {
			at = time.Now()
		}
	}

	if !l.active || ev.Err != l.lastErr {
		l.active = true
		l.lastErr = ev.Err
		l.lastAt = at
		l.suppress = 0
		return "connection error: " + SanitizeErrText(ev.Err), true
	}

	l.suppress++
	if at.Sub(l.lastAt) < HealthErrorReminderInterval {
		return "", false
	}
	n := l.suppress
	l.lastAt = at
	l.suppress = 0
	return "connection still failing: " + SanitizeErrText(ev.Err) +
		" (" + strconv.Itoa(n) + " occurrences since last report)", true
}
