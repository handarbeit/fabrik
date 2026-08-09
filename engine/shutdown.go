package engine

import (
	"fmt"
	"os"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// waitGroupTimeout waits for wg to complete, bounded by timeout. Returns true
// if wg.Wait() returned within the deadline, false on timeout. This is the
// standard Go idiom (goroutine closes a channel after Wait, caller selects
// against time.After) — no equivalent existed anywhere in the codebase before
// #1393/ADR-1393 (R4), since e.wg.Wait() was previously called unconditionally
// (bounded only by a second Ctrl-C forcing os.Exit).
//
// On timeout, the internal goroutine blocked on wg.Wait() is abandoned — it
// leaks until the process actually exits. This is accepted, not fixed: R5
// requires force-quit to remain the operator's unconditional escape hatch, so
// a timed-out drain must let the caller (drainAndExit) return promptly rather
// than wait for the leaked goroutine, exactly as an abandoned worker/pause
// goroutine already would on the pre-existing force-quit path.
func waitGroupTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// runShutdownPause is the R1/R2 (#1393, ADR-1393) daemon-wide clean-stop
// pause. Launched exactly once per shutdown episode, from the SIGINT/SIGTERM
// goroutine (poll.go) immediately after cancel() fires — never from
// registerSighupHandler, so a SIGHUP restart-in-place never pauses in-flight
// issues (see the ADR's SIGHUP-inheritance decision). Tracked on e.wg (the
// caller does e.wg.Add(1) before launching this as a goroutine) so
// drainAndExit's single waitGroupTimeout call bounds both the ordinary
// worker drain and this write phase together.
//
// R9: when no issue has an active Worker(), this returns immediately having
// written nothing — a stop with an idle queue must not become slower or
// noisier than before this issue.
//
// Per-issue writes are parallelized (one goroutine per in-flight issue) so
// N issues' worth of GitHub REST calls (up to 2 label ops + 1 comment each)
// don't serialize inside the single overall drain deadline; see the ADR's
// "known limitation" note on REST rate-limit pressure at high MaxConcurrent,
// which this issue deliberately does not add throttling for.
func (e *Engine) runShutdownPause() {
	defer e.wg.Done()

	var inFlight []itemstate.Snapshot
	for _, snap := range e.store.All() {
		if snap.Worker() != nil {
			inFlight = append(inFlight, snap)
		}
	}
	if len(inFlight) == 0 {
		return
	}

	e.logf(0, "shutdown", "clean stop: pausing %d in-flight issue(s)\n", len(inFlight))

	var wg sync.WaitGroup
	for _, snap := range inFlight {
		wg.Add(1)
		go func(snap itemstate.Snapshot) {
			defer wg.Done()
			e.pauseIssueForDaemonShutdown(snap)
		}(snap)
	}
	wg.Wait()
}

// pauseIssueForDaemonShutdown performs the R1/R2 per-issue work for one
// in-flight issue found by runShutdownPause: clear stage:<Name>:in_progress
// directly (R2 — not delegated to the cancelled worker goroutine's own
// release(), which is a race, not a guarantee; safe against a concurrent
// release() because applyLabelRemove already treats gh.ErrNotFound as
// success), then pause + comment via the shared pauseInterruptedIssue
// primitive (R1, R3).
func (e *Engine) pauseIssueForDaemonShutdown(snap itemstate.Snapshot) {
	repoStr := snap.Repo()
	number := snap.Number()
	owner, repo := parseOwnerRepo(repoStr)

	stageName := ""
	if w := snap.Worker(); w != nil {
		stageName = w.StageName
	}
	if stageName != "" {
		e.removeInProgressLabel(owner, repo, number, stageName)
	}

	item := gh.ProjectItem{Number: number, Repo: repoStr}

	// AC2 idempotency guard: re-read live labels (removeInProgressLabel above
	// may have mutated the cache, but fabrik:paused is unaffected by it) —
	// consistent with handleStopRequest's identical check.
	alreadyPaused := hasLabel(snap.Labels(), "fabrik:paused")

	comment := fmt.Sprintf(
		"🏭 **Fabrik — paused by a daemon clean stop**\n\nStage **%s** was interrupted by a daemon clean stop (SIGINT/SIGTERM). "+
			"In-progress worktree changes are committed and pushed automatically as part of the stop. Remove `fabrik:paused` to resume.",
		stageName,
	)
	e.pauseInterruptedIssue(item, alreadyPaused, comment)
}

// drainAndExit is the single shared drain tail (#1393/ADR-1393) replacing the
// four identical inline copies that previously lived in Run()'s poll loop.
// Order: cleanupLockedIssues() (unchanged, pre-existing lock cleanup) ->
// waitGroupTimeout(&e.wg, e.drainDeadline()) (R4 — bounds both the ordinary
// worker drain and, when this shutdown was triggered by SIGINT/SIGTERM, the
// runShutdownPause write phase tracked on the same WaitGroup) -> SIGHUP
// restart if one was requested. A timed-out drain logs a warning and
// proceeds to return exactly as a completed drain would — R5 requires this
// path to always terminate; abandoning any still-running goroutines on
// timeout is deliberate (see waitGroupTimeout's doc comment).
func (e *Engine) drainAndExit(lockFile *os.File, restartDone chan struct{}) error {
	e.cleanupLockedIssues()
	if !waitGroupTimeout(&e.wg, e.drainDeadline()) {
		e.logf(0, "shutdown", "drain deadline (%s) exceeded — some in-flight work may not have finished cleanly\n", e.drainDeadline())
	}
	if e.sighupRequested.Load() {
		performSighupRestart(e, lockFile)
		close(restartDone) // reached only on exec failure
	}
	return nil
}
