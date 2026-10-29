package engine

import (
	"sync"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// intakeStats counts what the delivery intake did since process start (#2427,
// ADR 2427-a). The counters are in memory only, never persisted, and exist so a
// live test can assert "this delivery was applied / was a duplicate / was
// matched as an echo" through fabrik_health instead of parsing engine logs.
// Tests compare deltas between two reads, never absolute values.
type intakeStats struct {
	mu          sync.Mutex
	accepted    int64
	duplicates  int64
	dropped     int64
	echoMatched int64
	applied     map[string]int64
}

func (s *intakeStats) noteAccepted() {
	s.mu.Lock()
	s.accepted++
	s.mu.Unlock()
}

func (s *intakeStats) noteDuplicate() {
	s.mu.Lock()
	s.duplicates++
	s.mu.Unlock()
}

func (s *intakeStats) noteDropped() {
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
}

// noteEchoMatched counts an inbound delivery that the echo registry recognised
// as the webhook echo of a write Fabrik made.
func (s *intakeStats) noteEchoMatched() {
	s.mu.Lock()
	s.echoMatched++
	s.mu.Unlock()
}

// noteApplied counts a delivery whose apply function ran to completion.
func (s *intakeStats) noteApplied(eventType string) {
	s.mu.Lock()
	if s.applied == nil {
		s.applied = make(map[string]int64)
	}
	s.applied[eventType]++
	s.mu.Unlock()
}

// counts returns the accepted, applied (all event types) and dropped totals.
func (s *intakeStats) counts() (accepted, applied, dropped int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.applied {
		applied += v
	}
	return s.accepted, applied, s.dropped
}

// snapshot copies the counters into the wire type.
func (s *intakeStats) snapshot() *localapi.IntakeHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &localapi.IntakeHealth{
		Accepted:    s.accepted,
		Duplicates:  s.duplicates,
		Dropped:     s.dropped,
		EchoMatched: s.echoMatched,
		Applied:     make(map[string]int64, len(s.applied)),
	}
	for k, v := range s.applied {
		out.Applied[k] = v
	}
	return out
}

// intakeHealth is the intake's counters for fabrik_health; nil when the daemon
// is poll-only (no intake).
func (e *Engine) intakeHealth() *localapi.IntakeHealth {
	if e.intake == nil {
		return nil
	}
	return e.intake.stats.snapshot()
}
