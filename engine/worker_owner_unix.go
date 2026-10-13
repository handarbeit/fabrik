//go:build !windows

package engine

import (
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/internal/workerenv"
)

// markedReapGrace is how long a marked process gets between SIGTERM and SIGKILL.
// Test-overridable.
var markedReapGrace = time.Second

// sessionReapIgnoreMarkers makes the marker reap find nothing. Test seam only:
// the neutralised twins set it to prove the marker scan is load-bearing.
// Production never sets it.
var sessionReapIgnoreMarkers bool

// markedReapEnviron is a test seam for the environment reader; nil uses the real
// platform reader.
var markedReapEnviron func(pid int) ([]string, error)

func markedReapOptions(issueNumber int) sessionreap.Options {
	o := sessionReapOptions(issueNumber)
	o.IgnoreMarkers = sessionReapIgnoreMarkers
	o.Environ = markedReapEnviron
	return o
}

// ownerSet returns the distinct non-empty owner markers of recs.
func ownerSet(recs []workerRecord) map[string]struct{} {
	out := map[string]struct{}{}
	for _, r := range recs {
		if r.Owner != "" {
			out[r.Owner] = struct{}{}
		}
	}
	return out
}

// reapMarkedOrphans signals every live process carrying one of owners'
// FABRIK_WORKER_OWNER markers and returns how many it signalled, plus, per
// marker, how many marked processes are still present afterwards (the prune
// rule must not drop a record whose orphans are still alive).
//
// CALLER CONTRACT (R4): every record the owners came from must already be
// classified workerDead or workerRecycled. A marker alone never authorises a
// kill — a backgrounded command of a LIVE worker carries the same marker and
// looks exactly like an orphan by ppid/pgid, which is the #2097 failure. The
// two call sites below are the only ones, and both gate on classifyWorker.
func reapMarkedOrphans(owners map[string]struct{}, reason string, issueNumber int) (signalled int, remaining map[string]int) {
	if len(owners) == 0 {
		return 0, nil
	}
	o := markedReapOptions(issueNumber)
	signalled = sessionreap.ReapMarked(workerenv.OwnerVar, owners, reason, markedReapGrace, o)
	left, err := sessionreap.MarkedPIDs(workerenv.OwnerVar, owners, o)
	if err != nil {
		// Unknown: treat every marker as still populated so no record is pruned
		// on a failed look (fail toward keeping the record).
		remaining = make(map[string]int, len(owners))
		for k := range owners {
			remaining[k] = 1
		}
		return signalled, remaining
	}
	remaining = make(map[string]int, len(left))
	for k, pids := range left {
		remaining[k] = len(pids)
	}
	return signalled, remaining
}
