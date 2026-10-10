package engine

import (
	"fmt"
	"sync"
	"time"
)

// Probe linkage-drift ledger (#2080, ADR 2080).
//
// runProbeAndDeepFetch invalidates an item's deep cache when the probe's linked
// PR number disagrees with the cached LinkedPR.Number. The deep fetch that
// follows can legitimately keep the cached number (a closed or unmerged PR,
// found by the REST lookup, that closedByPullRequestsReferences omits), so the
// same disagreement would recur on every poll. The ledger remembers the
// (cached, probe) pair last invalidated per item: a repeat of that pair is the
// same probe reading and is not invalidated again; a different pair — a genuine
// link change — invalidates once; agreement forgets the pair. It also counts
// invalidations per item over a sliding window so an unforeseen loop shows up as
// one loud warning (R4) instead of a quiet drain on the GraphQL budget.

const (
	// probeDriftLoopThreshold is the number of drift invalidations one item may
	// cause within probeDriftLoopWindow before a loop warning is logged.
	probeDriftLoopThreshold = 10
	// probeDriftLoopWindow is the sliding window the threshold is counted over.
	probeDriftLoopWindow = time.Hour
)

// probeDriftEntry is the ledger's record for one item.
type probeDriftEntry struct {
	hasPair    bool // a pair has been invalidated and not yet reconciled
	cached     int
	probe      int
	skipLogged bool // the disagreement for this pair has been logged once
	times      []time.Time
	warned     bool // the loop warning is armed off until the count falls back
}

// probeDriftLedger is the engine-local, mutex-guarded probe drift bookkeeping.
// The zero value is ready to use. In memory only: a restart costs one extra
// invalidation per affected item.
type probeDriftLedger struct {
	mu    sync.Mutex
	items map[string]*probeDriftEntry
	// disabled is the neutralisation seam: a repeated pair is treated as new.
	disabled bool
}

// probeDriftVerdict is the ledger's decision for one observed drift.
type probeDriftVerdict struct {
	// Invalidate reports whether the caller should invalidate the deep cache.
	Invalidate bool
	// LogSkip is set on the first repeat of an already-invalidated pair: the
	// caller logs the persistent disagreement once.
	LogSkip bool
	// Warn is set when this invalidation pushed the item over the loop threshold.
	Warn bool
	// Count is the number of invalidations in the window (valid when Warn).
	Count int
}

// observe records one drift reading (cached != probe) for key at now.
func (l *probeDriftLedger) observe(key string, cached, probe int, now time.Time) probeDriftVerdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.items == nil {
		l.items = make(map[string]*probeDriftEntry)
	}
	en := l.items[key]
	if en == nil {
		en = &probeDriftEntry{}
		l.items[key] = en
	}
	if en.hasPair && en.cached == cached && en.probe == probe && !l.disabled {
		v := probeDriftVerdict{LogSkip: !en.skipLogged}
		en.skipLogged = true
		return v
	}
	en.hasPair, en.cached, en.probe, en.skipLogged = true, cached, probe, false
	cutoff := now.Add(-probeDriftLoopWindow)
	kept := en.times[:0]
	for _, t := range en.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	en.times = append(kept, now)
	v := probeDriftVerdict{Invalidate: true, Count: len(en.times)}
	if len(en.times) <= probeDriftLoopThreshold {
		en.warned = false
	} else if !en.warned {
		en.warned = true
		v.Warn = true
	}
	return v
}

// setDisabled flips the neutralisation seam (see disabled).
func (l *probeDriftLedger) setDisabled(v bool) {
	l.mu.Lock()
	l.disabled = v
	l.mu.Unlock()
}

// converged records that cached and probe agree for key: the pair is forgotten,
// so a later return to a previously seen pair invalidates again. The
// invalidation history is kept for the loop counter.
func (l *probeDriftLedger) converged(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if en := l.items[key]; en != nil {
		en.hasPair, en.skipLogged = false, false
	}
}

// forget drops everything held for key (item removed from the board, or
// terminal).
func (l *probeDriftLedger) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.items, key)
}

// logProbeDriftLoop writes the R4 warning naming the item and the two values.
func (e *Engine) logProbeDriftLoop(number int, cached, probe, count int) {
	e.logf(number, "warn", "probe: linkage drift loop — %d invalidations in the last %s (cached PR #%d, probe PR #%d); "+
		"the deep cache and the probe keep disagreeing and each invalidation costs a deep fetch\n",
		count, probeDriftLoopWindow, cached, probe)
}

// probeDriftKey is the ledger key for an item.
func probeDriftKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}
