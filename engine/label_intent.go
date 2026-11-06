package engine

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Engine label intent and divergence marks (#2551, ADR 2551).
//
// The cache-divergence detector (channel_anomaly.go, ADR 2170) reports when a
// later listing contradicts an engine label write. Nothing used to act on it, so
// a write GitHub reported as successful but did not keep was lost for good once
// the observed-at grace passed (ADR 2348). This file holds the three small
// in-memory facts the repair (label_repair.go) and the trust gate
// (label_trust.go) need:
//
//   - the engine's last write per in-scope label (the intent), recorded at the
//     one funnel every engine label write goes through;
//   - which stage reworks are open (the Store carries no rework bracket; the
//     fabrik:editing / fabrik:reworking labels are subject to the same loss, so
//     they cannot be the ground truth for "the intent is still current");
//   - the labels currently marked as diverged.
//
// All of it is memory only, by design: a restart begins with no intent and so
// re-applies nothing, which is the safe direction.

// labelIntent is the engine's last write for one label on one item.
type labelIntent struct {
	added bool
	at    time.Time
}

// labelGuard is the per-Engine state of the repair and the trust gate. The zero
// value is usable.
type labelGuard struct {
	mu sync.Mutex
	// intents is keyed "<ref>|<label>".
	intents map[string]labelIntent
	// reworks maps an item ref to the stage whose rework bracket is open.
	reworks map[string]string
	// diverged holds the marked labels, keyed "<ref>|<label>", with when they
	// were marked.
	diverged map[string]time.Time
	// repaired is when a divergence on "<ref>|<label>" was last acted on (a
	// re-apply or a report); it is the once-per-episode and recurrence memory.
	repaired map[string]time.Time
	// problems are the unresolved (item, label) subjects shown in the warnings
	// panel (label_divergence), keyed "<ref>|<label>".
	problems map[string]string

	// The repair worker.
	queue   chan labelDivergence
	stop    chan struct{}
	wg      sync.WaitGroup
	running bool
	// inFlight counts queued plus executing repairs, for DrainLabelRepairForTest.
	inFlight atomic.Int64
}

func labelKey(ref, label string) string { return ref + "|" + label }

// labelInScope reports whether label is one the repair acts on: this instance's
// own lock label (never another instance's), fabrik:editing, the pause pair, and
// a stage's in_progress / complete labels.
func (e *Engine) labelInScope(label string) bool {
	switch label {
	case "fabrik:editing", "fabrik:paused", "fabrik:awaiting-input":
		return true
	}
	if label == e.lockLabel() && strings.HasPrefix(label, lockLabelPrefix) {
		return true
	}
	return strings.HasPrefix(label, "stage:") &&
		(strings.HasSuffix(label, ":complete") || strings.HasSuffix(label, ":in_progress"))
}

// noteLabelIntent records the engine's write of label on the item. Called from
// syncLabelAddBy / syncLabelRemovalBy, the funnel of every engine label write.
func (e *Engine) noteLabelIntent(repoKey string, number int, label string, added bool) {
	if !e.labelInScope(label) {
		return
	}
	g := &e.labels
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.intents == nil {
		g.intents = map[string]labelIntent{}
	}
	g.intents[labelKey(issueRef(repoKey, number), label)] = labelIntent{added: added, at: e.now()}
}

func (g *labelGuard) intent(ref, label string) (labelIntent, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	in, ok := g.intents[labelKey(ref, label)]
	return in, ok
}

// openRework / closeRework bracket a comment pass's stage rework window
// (beginStageRework .. endStageRework).
func (e *Engine) openRework(repoKey string, number int, stage string) {
	g := &e.labels
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reworks == nil {
		g.reworks = map[string]string{}
	}
	g.reworks[issueRef(repoKey, number)] = stage
}

func (e *Engine) closeRework(repoKey string, number int) {
	g := &e.labels
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.reworks, issueRef(repoKey, number))
}

// reworkOpenFor reports the stage whose rework is open for the item, or "".
func (g *labelGuard) reworkOpenFor(ref string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reworks[ref]
}

func (g *labelGuard) mark(ref, label string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.diverged == nil {
		g.diverged = map[string]time.Time{}
	}
	g.diverged[labelKey(ref, label)] = at
}

func (g *labelGuard) unmark(ref, label string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.diverged, labelKey(ref, label))
}

func (g *labelGuard) marked(ref, label string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.diverged[labelKey(ref, label)]
	return ok
}

// intentCurrent reports whether the engine's intent for label is still current:
// whether acting on it (re-applying) would restore what the engine means now
// rather than undo something that ended. It uses in-memory state only, except for
// the pause pair, where a human removal is indistinguishable from a lost write
// without the event log (see pauseLiftedSince).
//
//	lock / in_progress / editing added    a worker is held for the item, or a rework is open
//	stage:<S>:complete removed            a rework of S is open
//	fabrik:paused / awaiting-input added  nothing later removed it (checked by the caller)
//
// Every other direction is confirmed and logged by the repair, never re-applied.
func (e *Engine) intentCurrent(ref, repoKey string, number int, label string, in labelIntent) (current, needsPauseCheck bool) {
	g := &e.labels
	switch {
	case in.added && (label == "fabrik:editing" || label == e.lockLabel() ||
		(strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":in_progress"))):
		if g.reworkOpenFor(ref) != "" {
			return true, false
		}
		snap, ok := e.store.Peek(repoKey, number)
		return ok && snap.Worker() != nil, false
	case !in.added && strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":complete"):
		stage := strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":complete")
		return g.reworkOpenFor(ref) == stage, false
	case in.added && (label == "fabrik:paused" || label == "fabrik:awaiting-input"):
		return true, true
	}
	return false, false
}
