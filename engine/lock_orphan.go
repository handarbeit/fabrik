package engine

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Runtime orphan-lock reclaim (#2550, ADR 2550; amends ADR 2393's "existing
// sweeps remove it").
//
// An engine lock label (fabrik:editing, this daemon's own fabrik:locked:<id>)
// that is on an item while no worker of this daemon holds the item blocks
// dispatch for fabrik:editing (a foreign or stray editing lock) and is invisible
// to every status surface. Before this file only the startup scan removed one, so
// a running daemon held the item until a person removed the label. The label can
// get there through an ambiguous write whose live re-read also failed
// (label_reconcile.go) or through any path not foreseen.
//
// The orphan clock below records when this daemon first saw a label with no
// worker; settleOrphanedLocks removes it once it is older than the grace period,
// and the attention classifier reads the same clock, so the item reads `stalled`
// at about the time the reclaim becomes due and the stall clears when the reclaim
// succeeds. In memory only: a restart restarts the clock (the startup cleanup
// covers a restart).

const (
	// lockOrphanMinGrace is the floor of the grace period: long enough that a
	// worker that has just exited, a handle still registering and replica lag are
	// never mistaken for an orphan.
	lockOrphanMinGrace = 3 * time.Minute
	// lockOrphanGracePolls scales the grace with a slow poll cadence.
	lockOrphanGracePolls = 3
	// lockOrphanMaxPerPoll bounds the removals (and live reads) of one poll.
	lockOrphanMaxPerPoll = 10
	// lockOrphanFailLogEvery bounds the log noise of a reclaim that keeps failing.
	lockOrphanFailLogEvery = 5 * time.Minute
)

// lockOrphanGrace is how long an engine lock label may sit with no worker before
// it is reclaimed and reported stalled: max(3m, 3 × poll interval).
func (e *Engine) lockOrphanGrace() time.Duration {
	if d := time.Duration(e.lockOrphanGraceOverride.Load()); d > 0 {
		return d
	}
	g := time.Duration(e.cfg.PollSeconds) * time.Second * lockOrphanGracePolls
	if g < lockOrphanMinGrace {
		g = lockOrphanMinGrace
	}
	return g
}

// lockOrphanEntry is one (item, label) pair seen with no worker.
type lockOrphanEntry struct {
	repo   string
	number int
	label  string
	since  time.Time
	// lastFail is when a failed reclaim was last logged (throttle).
	lastFail time.Time
}

// lockOrphanClock is the engine's record of when each orphan-candidate label was
// first seen. Mutex-guarded: the poll goroutine writes it, the local API reads it.
type lockOrphanClock struct {
	mu      sync.Mutex
	entries map[string]*lockOrphanEntry
}

func lockOrphanKey(repo string, number int, label string) string {
	return fmt.Sprintf("%s#%d|%s", repo, number, label)
}

// sync replaces the set of tracked entries with observed: a pair still observed
// keeps its original time, a new one starts at now, one no longer observed (the
// label went, or a worker appeared) is forgotten.
func (c *lockOrphanClock) sync(observed []lockOrphanEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]*lockOrphanEntry, len(observed))
	for _, o := range observed {
		k := lockOrphanKey(o.repo, o.number, o.label)
		if old, ok := c.entries[k]; ok {
			next[k] = old
			continue
		}
		o.since = now
		cp := o
		next[k] = &cp
	}
	c.entries = next
}

func (c *lockOrphanClock) forget(repo string, number int, label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, lockOrphanKey(repo, number, label))
}

func (c *lockOrphanClock) setSince(repo string, number int, label string, since time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if en, ok := c.entries[lockOrphanKey(repo, number, label)]; ok {
		en.since = since
	}
}

// due returns a copy of every entry whose age has reached grace, oldest first.
func (c *lockOrphanClock) due(now time.Time, grace time.Duration) []lockOrphanEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []lockOrphanEntry
	for _, en := range c.entries {
		if now.Sub(en.since) >= grace {
			out = append(out, *en)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].since.Equal(out[j].since) {
			return out[i].since.Before(out[j].since)
		}
		return lockOrphanKey(out[i].repo, out[i].number, out[i].label) < lockOrphanKey(out[j].repo, out[j].number, out[j].label)
	})
	return out
}

// oldest returns the oldest tracked label of the item, if any.
func (c *lockOrphanClock) oldest(repo string, number int) (lockOrphanEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *lockOrphanEntry
	for _, en := range c.entries {
		if en.repo != repo || en.number != number {
			continue
		}
		if best == nil || en.since.Before(best.since) {
			best = en
		}
	}
	if best == nil {
		return lockOrphanEntry{}, false
	}
	return *best, true
}

func (c *lockOrphanClock) noteFailure(repo string, number int, label string, now time.Time) (log bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	en, ok := c.entries[lockOrphanKey(repo, number, label)]
	if !ok {
		return true
	}
	if en.lastFail.IsZero() || now.Sub(en.lastFail) >= lockOrphanFailLogEvery {
		en.lastFail = now
		return true
	}
	return false
}

// lockOrphanFact is the attention input for an item: the oldest engine lock label
// that is present with no worker, with this engine's grace. nil when none.
func (e *Engine) lockOrphanFact(st *itemstate.ItemState) *attention.LockOrphanFact {
	en, ok := e.lockOrphans.oldest(st.Repo, st.Number)
	if !ok {
		return nil
	}
	return &attention.LockOrphanFact{Label: en.label, Since: en.since, Grace: e.lockOrphanGrace()}
}

// settleOrphanedLocks observes every engine lock label that is on an item with no
// worker (the clock the attention classifier shares) and, unless neutralised,
// held by the forge hold or draining, reclaims the ones older than the grace
// period, at most lockOrphanMaxPerPoll per poll. It reads the Store only
// (All, never Get); the removal is preceded by a live label confirmation.
func (e *Engine) settleOrphanedLocks() {
	now := e.now()
	own := e.lockLabel()
	var observed []lockOrphanEntry
	for _, snap := range e.store.All() {
		if snap.Worker() != nil || !snap.LabelsKnown() {
			continue
		}
		labels := snap.Labels()
		for _, l := range []string{"fabrik:editing", own} {
			if l != "" && hasLabel(labels, l) {
				observed = append(observed, lockOrphanEntry{repo: snap.Repo(), number: snap.Number(), label: l})
			}
		}
	}
	e.lockOrphans.sync(observed, now)

	if e.lockOrphanReclaimOff.Load() || e.forgeHeld() || e.isDraining() {
		return
	}
	grace := e.lockOrphanGrace()
	// #2532: the grace has no event; wake a poll when it runs out.
	noted := map[string]bool{}
	for _, ob := range observed {
		k := lockOrphanKey(ob.repo, ob.number, "")
		if noted[k] {
			continue
		}
		noted[k] = true
		if en, ok := e.lockOrphans.oldest(ob.repo, ob.number); ok && now.Sub(en.since) < grace {
			e.noteTimerDeadline(ob.repo, ob.number, timerLockOrphan, grace-now.Sub(en.since))
		}
	}
	for i, en := range e.lockOrphans.due(now, grace) {
		if i >= lockOrphanMaxPerPoll {
			break
		}
		e.reclaimOrphanedLock(en, now, grace)
	}
}

// reclaimOrphanedLock removes one due orphan label after re-checking, immediately
// before the write, that no worker holds the item and that GitHub still shows the
// label. Nothing but the label removal, the audit line and the re-admission is
// written: no retry counter, breaker, StageAttempted, failed or pause label
// (FR-009). gh.ErrNotFound is success, as in the startup cleanup.
func (e *Engine) reclaimOrphanedLock(en lockOrphanEntry, now time.Time, grace time.Duration) {
	snap, ok := e.store.Peek(en.repo, en.number)
	if !ok || snap.Worker() != nil {
		e.lockOrphans.forget(en.repo, en.number, en.label)
		return
	}
	owner, repoName := parseOwnerRepo(en.repo)
	item := gh.ProjectItem{Number: en.number, Repo: en.repo}

	labels, err := e.liveLabels(owner, repoName, en.number)
	if err != nil {
		if e.lockOrphans.noteFailure(en.repo, en.number, en.label, now) {
			e.logf(en.number, "lock-reclaim", "could not confirm orphaned %s (will retry): %v\n", en.label, err)
		}
		return
	}
	if !hasLabel(labels, en.label) {
		e.lockOrphans.forget(en.repo, en.number, en.label)
		return
	}
	// fabrik:editing carries no owner: another Fabrik instance's live comment pass
	// looks the same. Its label age (GitHub's event log) is the extra evidence; a
	// young label restarts the clock. A failed read proceeds on the in-memory clock.
	if en.label == "fabrik:editing" {
		// live-read-audited: the label's event time is the only owner-less evidence against another instance's live pass
		if at, aerr := e.client.FetchLabelAppliedAt(owner, repoName, en.number, en.label); aerr == nil && !at.IsZero() && !at.After(now) && now.Sub(at) < grace {
			e.lockOrphans.setSince(en.repo, en.number, en.label, at)
			return
		}
	}
	if err := e.removeLabelChecked(item, en.label); err != nil && !errors.Is(err, gh.ErrNotFound) {
		if e.lockOrphans.noteFailure(en.repo, en.number, en.label, now) {
			e.logf(en.number, "lock-reclaim", "could not remove orphaned %s (will retry): %v\n", en.label, err)
		}
		return
	}
	e.lockOrphans.forget(en.repo, en.number, en.label)
	e.logf(en.number, "lock-reclaim", "reclaimed orphaned %s: present with no worker for %s (grace %s) (#2550)\n",
		en.label, now.Sub(en.since).Round(time.Second), grace)
	// Removing a label wakes nothing and records no admission: re-admit the item so
	// the pending comment pass or stage runs on the next poll (R3). The already-due
	// lock-write-failed cooldown also clears any earlier back-off for it.
	e.requeueDeferredDispatch([]string{issueKey(item, e.defaultRepo())})
	e.store.Apply(itemstate.CooldownRecorded{Repo: en.repo, Number: en.number, Reason: lockWriteRetryReason, Until: now})
}

// SetLockOrphanReclaimDisabledForTest neutralises the runtime reclaim (R2, #2550):
// the clock still runs, so the attention verdict is unaffected. Non-vacuity seam.
func (e *Engine) SetLockOrphanReclaimDisabledForTest(disabled bool) {
	e.lockOrphanReclaimOff.Store(disabled)
}

// SetLockOrphanGraceForTest overrides the grace period (0 restores the default).
func (e *Engine) SetLockOrphanGraceForTest(d time.Duration) {
	e.lockOrphanGraceOverride.Store(int64(d))
}
