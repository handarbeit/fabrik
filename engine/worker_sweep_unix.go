//go:build !windows

package engine

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lstartLayout is the format `ps -o lstart=` prints (after pidFingerprint's
// strings.Fields/Join collapses the space-padded day into a single space).
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// invocationEndSweepBudget bounds how long the invocation-end sweep keeps
// rescanning for members that are still visible after SIGKILL (not yet
// reparented/reaped) or that forked between scan and kill.
// Test-overridable.
var invocationEndSweepBudget = 2 * time.Second

// invocationEndRescanGap is the pause between invocation-end rescans.
var invocationEndRescanGap = 50 * time.Millisecond

// parseLStart parses a `ps` lstart string. ps prints local time.
func parseLStart(s string) (time.Time, bool) {
	t, err := time.ParseInLocation(lstartLayout, s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// beginWorkerRecord writes the spawn-time worker record (#1814). It runs
// synchronously right after cmd.Start(), when pid belongs to exactly one
// process, so the fingerprint captured here cannot be a recycled PID's.
//
// If the fingerprint lookup fails, Comm/LStart stay empty (R9): a sweep must
// then never signal anything on a liveness-only basis while the PID is alive.
// A failed disk write is logged and non-fatal — the returned in-memory record
// still drives the invocation-end sweep, so correctness there does not depend
// on the write.
func beginWorkerRecord(pid, issueNumber int, repo, stage string) workerRecord {
	now := time.Now()
	rec := workerRecord{
		ID:          fmt.Sprintf("%d-%d", pid, now.UnixNano()),
		PID:         pid,
		IssueNumber: issueNumber,
		Repo:        repo,
		Stage:       stage,
		SpawnedAt:   now,
	}
	if comm, lstart, err := pidFingerprintFn(pid); err == nil {
		rec.Comm, rec.LStart = comm, lstart
	}
	if err := appendWorkerRecord(rec); err != nil {
		claudeLog(issueNumber, "warn", "could not persist worker record for PID %d: %v\n", pid, err)
	}
	return rec
}

// backfillWorkerFingerprint retries the worker's fingerprint capture every
// descendantScanInterval until it succeeds or ctx is cancelled, then persists
// it. Only started when beginWorkerRecord's own capture failed.
func backfillWorkerFingerprint(ctx context.Context, id string, pid int) {
	ticker := time.NewTicker(descendantScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			comm, lstart, err := pidFingerprintFn(pid)
			if err != nil {
				continue
			}
			_ = updateWorkerRecord(id, func(r *workerRecord) {
				if r.LStart == "" {
					r.Comm, r.LStart = comm, lstart
				}
			})
			return
		}
	}
}

type workerState int

const (
	// workerDead: the worker PID is not alive — its session members are orphans.
	workerDead workerState = iota
	// workerLive: the PID is alive and is the recorded worker (AC3).
	workerLive
	// workerRecycled: the PID is alive but its start time differs from the
	// record — the recorded worker is gone and a new process holds its PID (R8).
	workerRecycled
	// workerInconclusive: cannot tell; fail open.
	workerInconclusive
)

// classifyWorker decides whether rec's worker is dead. Every ambiguity fails
// open. For workerRecycled the live holder's lstart is returned.
//
// Only lstart — not comm — declares "recycled": comm can legitimately change
// over a live process's life, and misreading that as a dead worker would kill
// a live invocation's descendants.
func classifyWorker(rec workerRecord) (workerState, string) {
	if !isProcessAlive(rec.PID) {
		return workerDead, ""
	}
	if rec.LStart == "" {
		return workerInconclusive, "" // R9: no identity, PID alive
	}
	_, lstart, err := pidFingerprintFn(rec.PID)
	if err != nil {
		return workerInconclusive, ""
	}
	if lstart == rec.LStart {
		return workerLive, ""
	}
	return workerRecycled, lstart
}

// collectSessionMembers makes one Getsid pass over procs and returns, for each
// wanted session ID, the PIDs of live processes in that session other than the
// leader itself (a recycled PID's live holder must never be a candidate).
func collectSessionMembers(procs []procArgvEntry, want map[int]bool) map[int][]int {
	out := make(map[int][]int)
	for _, p := range procs {
		sid, err := unix.Getsid(p.PID)
		if err != nil || sid == p.PID || !want[sid] {
			continue
		}
		out[sid] = append(out[sid], p.PID)
	}
	return out
}

type sweepTally struct {
	members int // members still present (including ones just signalled)
	reaped  int // members signalled this call
	skipped int // members left alone as inconclusive/out of bounds
}

// reapWorkerSessionMembers signals every candidate that is provably a
// descendant of the dead worker rec. holderLStart is non-empty only for a
// recycled PID (R8), and adds the upper bound. signalled carries PIDs already
// killed by earlier rescans so they are counted as present but not re-killed.
//
// Per-candidate, immediately before SIGKILL: a fresh fingerprint lookup must
// succeed, Getsid must still equal rec.PID, and the member's start time must
// lie within [worker start, recycled-holder start). Any parse failure or equal
// timestamp skips the candidate. Kill scope is a single PID, as in ADR-1798.
func reapWorkerSessionMembers(rec workerRecord, pids []int, holderLStart string, signalled map[int]bool, reason string) sweepTally {
	var t sweepTally

	var lower time.Time
	lowerOK := false
	if rec.LStart != "" {
		lower, lowerOK = parseLStart(rec.LStart)
	} else if !rec.SpawnedAt.IsZero() {
		lower, lowerOK = rec.SpawnedAt.Truncate(time.Second).Add(-time.Second), true
	}
	var upper time.Time
	upperOK := true
	if holderLStart != "" {
		upper, upperOK = parseLStart(holderLStart)
	}

	for _, pid := range pids {
		if signalled[pid] {
			if isProcessAlive(pid) {
				t.members++
			}
			continue
		}
		t.members++
		comm, lstart, err := pidFingerprintFn(pid)
		if err != nil {
			if !isProcessAlive(pid) {
				t.members-- // gone
			} else {
				t.skipped++
			}
			continue
		}
		sid, err := unix.Getsid(pid)
		if err != nil || sid != rec.PID {
			t.members--
			continue
		}
		started, ok := parseLStart(lstart)
		if !ok || !lowerOK || !upperOK || started.Before(lower) || (holderLStart != "" && !started.Before(upper)) {
			t.skipped++
			continue
		}
		claudeLog(rec.IssueNumber, "kill", "sending SIGKILL to PID %d (%s) — orphaned session member of dead worker PID %d (reason=%s)\n", pid, comm, rec.PID, reason)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			claudeLog(rec.IssueNumber, "warn", "session sweep: could not kill PID %d (%s): %v\n", pid, comm, err)
			t.skipped++
			continue
		}
		signalled[pid] = true
		t.reaped++
	}
	return t
}

// sweepWorkerSessionAtInvocationEnd is R5's fix: after the worker has exited,
// enumerate the process table with a fresh, uncached scan (a cached snapshot
// could predate the very descendant that raced) and reap every member of the
// worker's session, independent of the descendant registry. It rescans within
// a short bound to catch members still visible after SIGKILL or forked between
// scan and kill, and prunes the worker record only after two consecutive clean
// empty scans with nothing skipped. Anything left is caught by the periodic
// sweep, which is why the record is kept in that case.
func sweepWorkerSessionAtInvocationEnd(rec workerRecord) (reaped int) {
	state, holder := classifyWorker(rec)
	if state != workerDead && state != workerRecycled {
		return 0
	}
	signalled := make(map[int]bool)
	deadline := time.Now().Add(invocationEndSweepBudget)
	cleanScans := 0
	for {
		procs, err := listProcessArgvFn()
		if err != nil {
			return reaped // fail open, keep the record
		}
		members := collectSessionMembers(procs, map[int]bool{rec.PID: true})[rec.PID]
		t := reapWorkerSessionMembers(rec, members, holder, signalled, "session_sweep_invocation_end")
		reaped += t.reaped
		if t.members == 0 && t.skipped == 0 {
			cleanScans++
			if cleanScans >= 2 {
				_ = removeWorkerRecords([]string{rec.ID})
				return reaped
			}
		} else {
			cleanScans = 0
		}
		if time.Now().After(deadline) {
			return reaped
		}
		time.Sleep(invocationEndRescanGap)
	}
}

// sweepOrphanedWorkerSessions is R1/R4's periodic, registry-independent sweep
// (the proc-janitor cadence). It reads every worker record from disk — so an
// orphan from a previous engine run is still reapable — and reaps the live
// session members of every record whose worker is confirmed dead or recycled.
// Live and inconclusive workers are left entirely alone.
//
// One process-table scan and one Getsid pass serve all records. The scan may be
// the shared cached one: every candidate is re-verified before any kill, and a
// stale snapshot can only cause a missed member, which the EmptyScans >= 2
// prune rule tolerates.
func sweepOrphanedWorkerSessions() (records, reaped, skipped, pruned int) {
	recs, err := allWorkerRecords()
	if err != nil || len(recs) == 0 {
		return 0, 0, 0, 0
	}
	records = len(recs)
	procs, err := sharedProcessTableScan()
	if err != nil {
		return records, 0, 0, 0 // fail open: change nothing
	}

	type verdict struct {
		state  workerState
		holder string
	}
	verdicts := make([]verdict, len(recs))
	want := make(map[int]bool)
	for i, r := range recs {
		s, h := classifyWorker(r)
		verdicts[i] = verdict{s, h}
		if s == workerDead || s == workerRecycled {
			want[r.PID] = true
		}
	}
	if len(want) == 0 {
		return records, 0, 0, 0
	}
	bySID := collectSessionMembers(procs, want)

	var prune []string
	emptyScans := make(map[string]int)
	for i, r := range recs {
		v := verdicts[i]
		if v.state != workerDead && v.state != workerRecycled {
			continue
		}
		t := reapWorkerSessionMembers(r, bySID[r.PID], v.holder, make(map[int]bool), "session_sweep_periodic")
		reaped += t.reaped
		skipped += t.skipped
		if t.members == 0 && t.skipped == 0 {
			if r.EmptyScans+1 >= 2 {
				prune = append(prune, r.ID)
			} else {
				emptyScans[r.ID] = r.EmptyScans + 1
			}
		} else if r.EmptyScans != 0 {
			emptyScans[r.ID] = 0
		}
	}
	pruned = len(prune)
	if pruned > 0 || len(emptyScans) > 0 {
		drop := make(map[string]bool, len(prune))
		for _, id := range prune {
			drop[id] = true
		}
		_ = mutateWorkerRecords(func(cur []workerRecord) []workerRecord {
			kept := cur[:0]
			for _, r := range cur {
				if drop[r.ID] {
					continue
				}
				if n, ok := emptyScans[r.ID]; ok {
					r.EmptyScans = n
				}
				kept = append(kept, r)
			}
			return kept
		})
	}
	return records, reaped, skipped, pruned
}
