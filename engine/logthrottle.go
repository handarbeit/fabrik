package engine

import (
	"fmt"
	"sync"
	"time"
)

// logThrottleMinInterval is the default minimum time between repeated
// emissions of the identical message under the same key, once the message
// itself has stopped changing. Chosen to collapse the kind of 39/sec
// alternating-line spam observed in #1716 (rate-limit warning + "wake
// requested" pairs) down to roughly one line per poll-relevant interval,
// without hiding a message that's actually changing (e.g. a countdown).
const logThrottleMinInterval = 30 * time.Second

// logThrottleEntry records the last emission for one throttle key.
type logThrottleEntry struct {
	message string
	at      time.Time
}

// logThrottleState is a mutex-guarded dedup map, one entry per throttle key.
// Zero value is ready to use — no constructor needed, mirroring the
// zero-value-ready shape of Engine's other guarded maps.
type logThrottleState struct {
	mu      sync.Mutex
	entries map[string]logThrottleEntry
}

// shouldLog reports whether a message under the given key should actually be
// emitted right now: yes on the key's first occurrence, yes whenever the
// message text differs from the last emission under that key (a real state
// change should never be suppressed), and yes once minInterval has elapsed
// since the last emission even if the message is unchanged (so a
// still-relevant warning doesn't go silent forever — it just stops repeating
// every single poll). Records the emission as a side effect whenever it
// returns true.
func (s *logThrottleState) shouldLog(key, message string, now time.Time, minInterval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]logThrottleEntry)
	}
	prev, ok := s.entries[key]
	if !ok || prev.message != message || now.Sub(prev.at) >= minInterval {
		s.entries[key] = logThrottleEntry{message: message, at: now}
		return true
	}
	return false
}

// logfThrottled emits a log line via logf, but only when shouldLog reports
// this is not a suppressible repeat of the same message under key. Uses
// e.now() (not time.Now()) so throttling stays controllable from tests/sim's
// injected Clock, consistent with every other time-based gate in this
// package. issueNumber/tag/format/args are forwarded verbatim to logf on the
// emitting path — a suppressed call is a complete no-op, including no write
// to the persistent log file.
func (e *Engine) logfThrottled(key string, issueNumber int, tag, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !e.logThrottle.shouldLog(key, msg, e.now(), logThrottleMinInterval) {
		return
	}
	e.logf(issueNumber, tag, "%s", msg)
}
