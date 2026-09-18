//go:build !windows

package engine

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// descendantScanInterval is how often trackWorkerDescendants polls the
// process table for new session-scoped descendants of a live worker.
// Test-overridable (mirrors claudeInactivityTimeout's pattern).
var descendantScanInterval = 3 * time.Second

// pidFingerprintFn is a package-level function-var seam (mirroring
// listProcessArgvFn/sentinelProbeFn's existing convention in this package) so
// tests can inject a transient failure for a specific PID without needing to
// make the real `ps` binary fail on demand. Production code must never
// reassign this outside tests.
var pidFingerprintFn = pidFingerprint

// sessionScopedDescendants returns the PID of every live process (other than
// workerPID itself) whose session ID equals workerPID.
//
// This deliberately does not use `ps`'s own session-ID column (`sid=` on
// GNU/Linux, `sess=` on BSD/macOS): on a recent macOS release tested during
// development, `ps -eo sess=` reports 0 for every process unconditionally —
// including PID 1 — evidently masked at the ps-output layer rather than
// genuinely unset. The underlying getsid(2) syscall is not masked (verified
// directly: it returns real, distinct session IDs on the same host where
// `ps`'s own SESS/sess column reads uniformly 0), so this walks the live PID
// list from listProcessArgv (already-portable, already-tested — see
// sentinel_probe_unix.go) and calls unix.Getsid per candidate instead of
// parsing it out of `ps`.
func sessionScopedDescendants(workerPID int) ([]int, error) {
	procs, err := listProcessArgv()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		if p.PID == workerPID {
			continue
		}
		sid, err := unix.Getsid(p.PID)
		if err != nil {
			continue // process exited between listing and the Getsid call
		}
		if sid == workerPID {
			pids = append(pids, p.PID)
		}
	}
	return pids, nil
}

// pidFingerprint returns a single live process's command name and start time
// via `ps -p <pid> -o comm=,lstart=`, used as an identity fingerprint: the
// pair is re-checked immediately before every kill (R5) so a PID reused for
// an unrelated process after the original exited is never mistaken for it.
//
// This is a single-PID query, not folded into listProcessArgv's own bulk
// `ps -eo pid=,args=` call: lstart's value contains embedded spaces (e.g.
// "Wed Sep 18 12:00:00 2026"), which cannot be combined unambiguously with
// another variable-width field like a full argv list. comm is always the
// line's first token, so everything after it is unambiguously lstart,
// regardless of lstart's own internal spacing.
func pidFingerprint(pid int) (comm, lstart string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), sentinelProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "comm=,lstart=").Output()
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", "", errProcessNotFound
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", errProcessNotFound
	}
	return fields[0], strings.Join(fields[1:], " "), nil
}

var errProcessNotFound = &processNotFoundError{}

type processNotFoundError struct{}

func (*processNotFoundError) Error() string { return "process not found" }

// trackWorkerDescendants runs as a goroutine parallel to runClaude's existing
// inactivity watchdog, live for the life of one invocation. It periodically
// scans the process table and records (in the durable registry) every
// process whose session ID equals workerPID's own PID — every descendant the
// worker has ever forked, since only an explicit setsid() call by a
// descendant itself changes SID (the cwd reaper's domain, unaffected here).
//
// This must observe the tree WHILE the invocation is live, not just once at
// teardown: a nohup/disown-detached descendant is reparented to init on a
// sub-second timescale, well before a post-hoc walk could see the transient
// worker-PID ancestry — recording candidates as they're discovered is what
// makes the mechanism work at all for that detachment style (#1798 R1/AC8).
//
// Stops when ctx is cancelled (watchdogCtx, cancelled right after cmd.Wait
// returns in runClaude) — mirrors the inactivity watchdog's own shutdown.
func trackWorkerDescendants(ctx context.Context, workerPID, issueNumber int, repo, stage string) {
	ticker := time.NewTicker(descendantScanInterval)
	defer ticker.Stop()
	seen := make(map[int]bool)
	// The worker's own identity fingerprint, stamped onto every discovered
	// descendant. This is what lets sweepStaleDescendants (R3) tell "this
	// worker PID is still the same worker" apart from "this worker PID was
	// reused by an unrelated process after the original worker died" (R5) —
	// isProcessAlive alone cannot make that distinction, since it only
	// checks liveness, not identity.
	//
	// It cannot change for the life of workerPID, so once successfully
	// fetched it is never re-fetched — but the fetch itself is retried every
	// tick until it succeeds, rather than attempted only once: a transient
	// failure on a single attempt (the same host-contention condition this
	// reaper exists to handle) must not permanently and silently degrade
	// every descendant of this invocation to the liveness-only fallback for
	// the invocation's entire lifetime. Until it succeeds, workerComm/
	// workerLStart stay empty and newly-discovered descendants are recorded
	// with that fallback.
	//
	// A descendant can be discovered on the very same tick the worker's own
	// fingerprint attempt fails (they race within one tick body, and a
	// descendant's own fingerprint call can succeed independently of the
	// worker's) — so "self-correcting on a later tick" is not automatic:
	// without an explicit backfill, that descendant's registry entry would
	// keep its empty WorkerComm/WorkerLStart forever, since seen[pid] stops
	// it from ever being reprocessed. pendingIdentityBackfill tracks exactly
	// the descendants this invocation itself recorded before the worker's
	// identity became known, and is re-upserted with the correct fingerprint
	// the moment it does.
	var workerComm, workerLStart string
	workerIdentityKnown := false
	var pendingIdentityBackfill []trackedDescendant
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !workerIdentityKnown {
				if c, l, ferr := pidFingerprintFn(workerPID); ferr == nil {
					workerComm, workerLStart = c, l
					workerIdentityKnown = true
					for _, d := range pendingIdentityBackfill {
						d.WorkerComm = workerComm
						d.WorkerLStart = workerLStart
						_ = upsertTrackedDescendant(d)
					}
					pendingIdentityBackfill = nil
				}
			}
			pids, err := sessionScopedDescendants(workerPID)
			if err != nil {
				continue // transient ps failure; try again next tick
			}
			for _, pid := range pids {
				if seen[pid] {
					continue
				}
				comm, lstart, ferr := pidFingerprintFn(pid)
				if ferr != nil {
					// pidFingerprint's single-PID `ps` call cannot
					// distinguish "the process exited between the Getsid
					// check above and this call" from "the ps invocation
					// itself failed transiently" (e.g. the 3s
					// sentinelProbeTimeout expiring under exactly the host
					// contention this issue describes) — both surface as a
					// generic error. Deliberately do NOT mark seen[pid]
					// here: a permanent "seen" mark would make a transient
					// probe failure indistinguishable from "recorded," so a
					// live descendant could be silently and permanently
					// dropped from tracking under load — precisely the
					// scenario this reaper exists to handle. Leaving it
					// unmarked means the next tick retries; if the process
					// really did exit, sessionScopedDescendants naturally
					// stops returning its PID on its own, so no unbounded
					// retry risk exists either way.
					continue
				}
				seen[pid] = true
				d := trackedDescendant{
					PID:          pid,
					Comm:         comm,
					LStart:       lstart,
					WorkerPID:    workerPID,
					WorkerComm:   workerComm,
					WorkerLStart: workerLStart,
					IssueNumber:  issueNumber,
					Repo:         repo,
					Stage:        stage,
					DiscoveredAt: time.Now(),
				}
				_ = upsertTrackedDescendant(d)
				if !workerIdentityKnown {
					pendingIdentityBackfill = append(pendingIdentityBackfill, d)
				}
			}
		}
	}
}

// reapTrackedDescendants is R2: called unconditionally at invocation end
// (runClaude, immediately after the existing killProcGroup call), regardless
// of whether the invocation exited cleanly or was killed. Loads every
// descendant recorded against workerPID, re-verifies each one's identity
// fingerprint immediately before killing it (R5 — a mismatch means the
// process already exited or the PID was reused for something unrelated;
// such entries are dropped, never signalled), and SIGKILLs the survivors.
// Every kill (and the reason it matched) is logged via the existing
// "[#N kill]" convention (R6).
func reapTrackedDescendants(workerPID, issueNumber int) (reaped, skipped int) {
	entries, err := descendantsForWorker(workerPID)
	if err != nil || len(entries) == 0 {
		return 0, 0
	}
	var processed []int
	for _, d := range entries {
		processed = append(processed, d.PID)
		comm, lstart, ferr := pidFingerprint(d.PID)
		if ferr != nil || comm != d.Comm || lstart != d.LStart {
			// Process already gone, or the PID was reused for something
			// else since we recorded it — fail closed, never signal.
			skipped++
			continue
		}
		claudeLog(issueNumber, "kill", "sending SIGKILL to PID %d (%s) — session-scoped descendant of worker PID %d (reason=invocation_end)\n", d.PID, comm, workerPID)
		if err := syscall.Kill(d.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			claudeLog(issueNumber, "warn", "reapTrackedDescendants: could not kill PID %d (%s): %v\n", d.PID, comm, err)
		}
		reaped++
	}
	_ = removeTrackedDescendants(processed)
	return reaped, skipped
}

// workerIdentityStillMatches reports whether the live process at d.WorkerPID
// is still plausibly the same worker that discovered d — not a different
// process the OS has since reused that PID number for. A registry entry
// recorded before this field existed (WorkerComm/WorkerLStart both empty)
// falls back to liveness-only, matching this sweep's pre-existing behavior
// rather than treating an old-format entry as a mismatch.
func workerIdentityStillMatches(d trackedDescendant) bool {
	if d.WorkerComm == "" && d.WorkerLStart == "" {
		return true
	}
	comm, lstart, err := pidFingerprint(d.WorkerPID)
	if err != nil {
		return false
	}
	return comm == d.WorkerComm && lstart == d.WorkerLStart
}

// sweepStaleDescendants is R3's backstop: called periodically (the janitor's
// existing JanitorIntervalHours cadence) to catch orphans that escaped R1/R2
// — including ones left behind by a previous engine run, since the durable
// registry survives a restart while in-memory state does not.
//
// An entry whose WorkerPID is still alive — and still the *same* worker, per
// its recorded identity fingerprint (R5) — is left alone: that invocation is
// still in flight and R2 will reap its descendants at its own invocation end
// — sweeping it early risks killing a subprocess the worker still genuinely
// needs. A live PID whose fingerprint no longer matches means the original
// worker exited and this PID was since reused for something unrelated — R5
// requires identity be reverified, not assumed from the PID number alone, so
// that case is treated as "worker gone" rather than trusted at face value.
// Only entries whose worker has genuinely exited (crashed before reaching
// its own R2 reap, from a prior engine run entirely, or whose PID was
// reused) are eligible here. Every eligible entry's own identity fingerprint
// is then re-verified immediately before killing it (R5), exactly as
// reapTrackedDescendants does.
func sweepStaleDescendants() (scanned, reaped, skipped int) {
	entries, err := allTrackedDescendants()
	if err != nil {
		return 0, 0, 0
	}
	scanned = len(entries)
	var processed []int
	for _, d := range entries {
		if isProcessAlive(d.WorkerPID) && workerIdentityStillMatches(d) {
			// Owning invocation still in flight — leave it for R2.
			continue
		}
		processed = append(processed, d.PID)
		comm, lstart, ferr := pidFingerprint(d.PID)
		if ferr != nil || comm != d.Comm || lstart != d.LStart {
			skipped++
			continue
		}
		claudeLog(d.IssueNumber, "kill", "sending SIGKILL to PID %d (%s) — orphaned session-scoped descendant of dead worker PID %d (reason=backstop_sweep)\n", d.PID, comm, d.WorkerPID)
		if err := syscall.Kill(d.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			claudeLog(d.IssueNumber, "warn", "sweepStaleDescendants: could not kill PID %d (%s): %v\n", d.PID, comm, err)
		}
		reaped++
	}
	_ = removeTrackedDescendants(processed)
	return scanned, reaped, skipped
}
