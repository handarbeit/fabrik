//go:build !windows

package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// commandSessionSampleInterval is how often a running worker's process tree is
// sampled for the command sessions its Bash-tool shells create (#1989). Each
// sample is one process listing. A command that starts and orphans itself
// between two samples, after which the worker exits, is the residual gap — ADR
// 063's worktree-cwd reaper is the cover. Test-overridable.
var commandSessionSampleInterval = 2 * time.Second

// commandSessionDrainWait bounds how long finishCommandSessions waits for the
// just-SIGKILLed members of a command session to disappear. Test-overridable.
var commandSessionDrainWait = time.Second

// sessionReapWorkerSIDOnly restricts the session reap to the worker's own SID —
// the behaviour before command sessions were modelled. Test seam only: the
// neutralised twins set it to prove the command-session extension is
// load-bearing. Production never sets it.
var sessionReapWorkerSIDOnly bool

// startCommandSessionTracking registers a tracker for worker pid and starts its
// sampler under wg (stopped by ctx). Every newly observed command session is
// persisted as a child record of the worker's record, so the proc-janitor can
// still reach it after an engine crash or restart mid-invocation.
func startCommandSessionTracking(ctx context.Context, wg *sync.WaitGroup, pid, issueNumber int, rec workerRecord) *sessionreap.Tracker {
	tr := sessionreap.Track(pid, sessionReapOptions(issueNumber), func(ss []sessionreap.Session) {
		persistCommandSessions(rec, ss, issueNumber)
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		tr.Run(ctx, commandSessionSampleInterval)
	}()
	return tr
}

// persistCommandSessions appends one record per command session, linked to the
// worker by ParentID. A failed write is logged and non-fatal: the in-memory
// tracker still drives the invocation-end sweep.
func persistCommandSessions(parent workerRecord, ss []sessionreap.Session, issueNumber int) {
	now := time.Now()
	for _, s := range ss {
		rec := workerRecord{
			ID:          fmt.Sprintf("%d-%d-cmd", s.SID, now.UnixNano()),
			PID:         s.SID,
			ParentID:    parent.ID,
			StartToken:  s.Start,
			IssueNumber: issueNumber,
			Repo:        parent.Repo,
			Stage:       parent.Stage,
			SpawnedAt:   now,
		}
		if err := appendWorkerRecord(rec); err != nil {
			claudeLog(issueNumber, "warn", "could not persist command session %d of worker PID %d: %v\n", s.SID, parent.PID, err)
		}
	}
}

// finishCommandSessions runs after the post-exit sweep: it drops the persisted
// record of every command session that is now empty and unregisters the
// tracker. A session that is not empty (a member survived, or the listing
// failed) keeps its record for the proc-janitor.
func finishCommandSessions(tr *sessionreap.Tracker, parent workerRecord, issueNumber int) {
	if tr == nil {
		return
	}
	defer tr.Close()
	opts := sessionReapOptions(issueNumber)
	done := make(map[int]bool)
	// The post-exit SIGKILL has only just been sent; give the members a moment to
	// actually disappear so their records can be dropped now rather than left for
	// the janitor. Bounded, and only ever waits while a session still has members.
	deadline := time.Now().Add(commandSessionDrainWait)
	for {
		for _, s := range tr.Sessions() {
			if !done[s.SID] && sessionreap.Empty(s, opts) {
				done[s.SID] = true
			}
		}
		if len(done) == len(tr.Sessions()) || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(done) == 0 {
		return
	}
	if err := mutateWorkerRecords(func(recs []workerRecord) []workerRecord {
		kept := recs[:0]
		for _, r := range recs {
			if r.ParentID == parent.ID && r.ParentID != "" && done[r.PID] {
				continue
			}
			kept = append(kept, r)
		}
		return kept
	}); err != nil {
		claudeLog(issueNumber, "warn", "could not prune command session records of worker PID %d: %v\n", parent.PID, err)
	}
}

// sweepOrphanedCommandSessions is the janitor half of the command-session
// reap: for every persisted command session whose parent worker is confirmed
// dead, recycled, or no longer recorded, SIGKILL its members (leader included)
// provided its SID still names the sampled session (sessionreap.Reap
// re-validates the leader's start token). A session whose worker is live or
// inconclusive is left entirely alone — it belongs to a running invocation.
// parentGone reports that per worker record ID; prune/emptyScans accumulate the
// same two-consecutive-empty-scans bookkeeping the worker records use.
func sweepOrphanedCommandSessions(cmds []workerRecord, parentGone func(parentID string) bool, prune map[string]bool, emptyScans map[string]int) (reaped, skipped int) {
	for _, c := range cmds {
		if !parentGone(c.ParentID) {
			continue
		}
		s := sessionreap.Session{SID: c.PID, Start: c.StartToken}
		signalled, remaining := sessionreap.Reap(s, sessionReapOptions(c.IssueNumber))
		if signalled > 0 {
			claudeLog(c.IssueNumber, "kill", "reaped %d member(s) of orphaned command session %d of dead worker record %s (reason=session_sweep_periodic)\n", signalled, c.PID, c.ParentID)
		}
		reaped += signalled
		skipped += remaining
		if remaining == 0 {
			if c.EmptyScans+1 >= 2 {
				prune[c.ID] = true
			} else {
				emptyScans[c.ID] = c.EmptyScans + 1
			}
		} else if c.EmptyScans != 0 {
			emptyScans[c.ID] = 0
		}
	}
	return reaped, skipped
}
