package engine

import (
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// Ambiguous label writes (#2550, ADR 2550; amends ADR 2393).
//
// GitHub can answer a label write with a 5xx or a timeout and still have applied
// it. For an engine lock or state label (fabrik:editing, fabrik:locked:<id>,
// stage:<name>:in_progress) the engine used to treat every failed add as "label
// absent" and every failed removal as "label still there". When that was wrong,
// the label sat on the item with no holder and (for fabrik:editing) blocked
// dispatch until a restart. reconcileLabelWrite asks GitHub what actually
// happened, once, after a transport-class failure, and the write sites act on
// the answer.

// labelWriteOutcome is what reconcileLabelWrite learned about a failed write.
type labelWriteOutcome int

const (
	// labelWriteUnreconciled: no re-read was made (the error is not
	// transport-class, or the reconcile is neutralised). The caller keeps its
	// pre-#2550 behaviour.
	labelWriteUnreconciled labelWriteOutcome = iota
	// labelWriteApplied: the live labels show the write took effect (an add whose
	// label is present, a removal whose label is absent).
	labelWriteApplied
	// labelWriteNotApplied: the live labels show the write did not take effect.
	labelWriteNotApplied
	// labelWriteUnknown: the re-read itself failed. The caller keeps its pre-#2550
	// behaviour; the runtime orphan reclaim (lock_orphan.go) is the backstop.
	labelWriteUnknown
)

func (o labelWriteOutcome) String() string {
	switch o {
	case labelWriteApplied:
		return "applied"
	case labelWriteNotApplied:
		return "not applied"
	case labelWriteUnknown:
		return "unknown"
	default:
		return "unreconciled"
	}
}

// isEngineLockStateLabel reports the labels R1 covers: fabrik:editing,
// fabrik:locked:<id> and stage:<name>:in_progress. Any other label (completion,
// pause, awaiting-* and cause labels) is never reconciled, so a 5xx storm does not
// add a live read to every failed unrelated removal.
func isEngineLockStateLabel(label string) bool {
	return label == "fabrik:editing" || strings.HasPrefix(label, "fabrik:locked:") ||
		(strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":in_progress"))
}

// reconcileLabelWrite re-reads the item's labels live after writeErr, a failed
// add (wantPresent true) or removal (wantPresent false) of label. It re-reads
// only after a transport-class failure (gh.IsTransportError: 5xx or timeout) —
// a 4xx or a rate limit is an answer, not an ambiguity — so a healthy GitHub
// never sees the extra call. The read goes through liveLabels, so the Store
// receives it. One log line per decision.
func (e *Engine) reconcileLabelWrite(item gh.ProjectItem, label string, wantPresent bool, writeErr error) labelWriteOutcome {
	if writeErr == nil || e.ambiguousWriteReconcileOff.Load() || !isEngineLockStateLabel(label) || !gh.IsTransportError(writeErr) {
		return labelWriteUnreconciled
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	verb := "add"
	if !wantPresent {
		verb = "removal"
	}
	labels, err := e.liveLabels(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "lock", "%s of %q failed (%v) and the live label re-read failed too (%v) — outcome unknown\n", verb, label, writeErr, err)
		return labelWriteUnknown
	}
	present := hasLabel(labels, label)
	if present == wantPresent {
		e.logf(item.Number, "lock", "%s of %q failed (%v) but GitHub applied it — treating the write as done\n", verb, label, writeErr)
		return labelWriteApplied
	}
	e.logf(item.Number, "lock", "%s of %q failed (%v) and the live labels confirm it did not land\n", verb, label, writeErr)
	return labelWriteNotApplied
}

// SetAmbiguousWriteReconcileDisabledForTest neutralises the live re-read after a
// transport-class label-write failure (R1, #2550), restoring the pre-#2550
// behaviour at every write site. It is the non-vacuity seam the sim twin uses.
func (e *Engine) SetAmbiguousWriteReconcileDisabledForTest(disabled bool) {
	e.ambiguousWriteReconcileOff.Store(disabled)
}
