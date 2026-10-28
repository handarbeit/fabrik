package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// Stage start failures (#2499, ADR 2499). When a stage's Claude process cannot
// be launched the stage never ran: no StageAttempted, no max_retries charge, no
// stage:<name>:failed. Because StageAttempted stays unstamped (it drives the
// worktree-sync skip and the session-resume decision), nothing else delays a
// re-dispatch, so the retry owns two things of its own:
//
//   - an admission cooldown recording the instant it falls due (#2232 FR-001),
//     so the log line, emitRetryScheduled and the recorded instant agree; and
//   - a per-item consecutive-failure hold, read by both dispatch gates, so a
//     persistent failure (the binary really is missing) backs off instead of
//     looping at poll rate through any admission route.
//
// A daemon-wide count raises one warnings-panel entry from the second
// consecutive failure; the next real start resolves it. All state is in memory:
// a restart begins again from the first failure.

const (
	startFailureWarningType    = "claude_start"
	startFailureWarningSubject = "claude"
)

// isClaudeStartFailure reports whether err means the Claude process could not
// be launched at all. It is the classification finalizeStageOutcome has always
// used, shared so the success hook cannot drift from it.
func isClaudeStartFailure(err error) bool {
	if err == nil {
		return false
	}
	var startErr *exec.Error
	if errors.As(err, &startErr) {
		return true
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr) || errors.Is(err, exec.ErrNotFound)
}

// claudeProcessStarted reports whether an invocation's outcome proves the
// Claude binary launched: not a start failure, and not a refusal made before
// any process was spawned.
func claudeProcessStarted(err error) bool {
	if isClaudeStartFailure(err) {
		return false
	}
	var preSpawnAPIKey *apiKeyHelperDetectedError
	var preSpawnSkill *skillUnresolvedError
	return !errors.As(err, &preSpawnAPIKey) && !errors.As(err, &preSpawnSkill)
}

type startFailureItem struct {
	consecutive int
	holdUntil   time.Time
}

type startFailureState struct {
	mu       sync.Mutex
	items    map[string]*startFailureItem
	daemon   int    // consecutive start failures across all items since the last start
	reported bool   // the claude_start panel entry is active
	lastErr  string // newest error text
	disabled bool   // test seam: the dispatch gate is neutralised
}

// startFailureDelay is the wait before the n-th consecutive start failure of an
// item is retried: the first is already due (like a time slice), then
// stageRetryBackoff doubling up to the cap. The cap is never below the backoff
// and at most the periodic re-evaluation, so that safety net is never the
// effective retry again.
func (e *Engine) startFailureDelay(n int) time.Duration {
	if n <= 1 {
		return 0
	}
	base := e.stageRetryBackoff()
	limit := e.githubRecheckInterval()
	if limit < base {
		limit = base
	}
	d := base
	for i := 2; i < n && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d
}

// recordStartFailure counts a start failure for item, sets its hold and
// returns the delay to log, emit and record. It raises the panel entry from the
// second consecutive daemon-wide failure.
func (e *Engine) recordStartFailure(item gh.ProjectItem, err error) time.Duration {
	key := issueKey(item, e.defaultRepo())
	now := e.now()

	s := &e.startFailures
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*startFailureItem)
	}
	// Prune entries that lapsed long ago, so the map cannot grow without bound.
	prune := e.githubRecheckInterval()
	if b := e.stageRetryBackoff(); b > prune {
		prune = b
	}
	for k, it := range s.items {
		if now.Sub(it.holdUntil) > prune {
			delete(s.items, k)
		}
	}
	it := s.items[key]
	if it == nil {
		it = &startFailureItem{}
		s.items[key] = it
	}
	it.consecutive++
	delay := e.startFailureDelay(it.consecutive)
	it.holdUntil = now.Add(delay)
	s.daemon++
	s.lastErr = err.Error()
	count, lastErr := s.daemon, s.lastErr
	report := count >= 2
	if report {
		s.reported = true
	}
	s.mu.Unlock()

	if report {
		e.reportStartFailure(count, lastErr)
	}
	return delay
}

func (e *Engine) reportStartFailure(count int, lastErr string) {
	if rerr := warnings.Report(e.warnWriter("claude-start"), warnings.Condition{
		Type:    startFailureWarningType,
		Subject: startFailureWarningSubject,
		Title:   "Claude could not be started",
		Detail: fmt.Sprintf("The Claude process failed to start %d consecutive times: %s. "+
			"Stages are retried with a growing delay (they are not counted against max_retries); "+
			"fix the Claude installation or PATH. The entry clears on the next successful start.",
			count, lastErr),
		Fingerprint: startFailureWarningType,
	}); rerr != nil {
		e.logf(0, "claude-start", "could not record the warning: %v", rerr)
	}
}

// startFailureRetryIn is the remaining hold on item after a start failure, or 0.
// Both dispatch gates read it, so every admission route is bounded.
func (e *Engine) startFailureRetryIn(item gh.ProjectItem) time.Duration {
	s := &e.startFailures
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return 0
	}
	it := s.items[issueKey(item, e.defaultRepo())]
	if it == nil {
		return 0
	}
	if remaining := it.holdUntil.Sub(e.now()); remaining > 0 {
		return remaining
	}
	return 0
}

// noteClaudeStarted records that a Claude process launched for item: the item's
// and the daemon's consecutive counts reset and an active warning is resolved.
func (e *Engine) noteClaudeStarted(item gh.ProjectItem) {
	s := &e.startFailures
	s.mu.Lock()
	delete(s.items, issueKey(item, e.defaultRepo()))
	s.daemon = 0
	wasReported := s.reported
	s.reported = false
	s.mu.Unlock()

	if !wasReported {
		return
	}
	if rerr := warnings.Resolve(warnings.Condition{Type: startFailureWarningType, Subject: startFailureWarningSubject}.Key()); rerr != nil {
		e.logf(0, "claude-start", "could not resolve the warning: %v", rerr)
	}
}

// SetStartFailureGateDisabledForTest neutralises (or restores) the per-item
// consecutive-failure hold, leaving only the admission cooldown (non-vacuity of
// the persistent-failure bound, #2499).
func (e *Engine) SetStartFailureGateDisabledForTest(disabled bool) {
	e.startFailures.mu.Lock()
	e.startFailures.disabled = disabled
	e.startFailures.mu.Unlock()
}
