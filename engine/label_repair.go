package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// Repair of an engine label write that did not stick (#2551, ADR 2551).
//
// The trigger is the existing cache-divergence detector (channel_anomaly.go):
// when a later listing contradicts an engine label write, onChange hands the
// divergence here. This file never adds a detector. Because the detector lives in
// the channel consumer, the repair exists exactly when the channel hub does
// (production Run() always starts one).
//
// The hand-off is asynchronous. The observer runs on the writer's goroutine, so a
// GitHub call there would stall every Store write behind a network round trip; the
// observer only marks the label (cheap, memory) and queues a record. One worker
// goroutine does the work, one record at a time:
//
//  1. recurrence: the same (item, label) acted on within the divergence window is
//     reported and not touched again (this is also the once-per-episode bound);
//  2. intent: the engine's last write must still be the direction that diverged,
//     and still current (intentCurrent);
//  3. one live label read (liveLabels, which also applies the read to the Store);
//  4. if GitHub already matches the intent the listing lagged: log "stale-read",
//     write nothing;
//  5. otherwise it is a lost write: re-check the intent once more (a release can
//     race the read), re-apply once through the echo-reserving writers, and log
//     "lost-write".
//
// Failures (read, re-apply) and recurrences are reported, never paused on.

const (
	// labelDivergenceWarningType is the warnings-panel type of an unresolved
	// engine-label divergence. Subjects are "<ref>|<label>" and the set is
	// reconciled, so a resolved one disappears.
	labelDivergenceWarningType = "label_divergence"
	// labelRepairQueueSize bounds the hand-off from the observer. A full queue
	// drops the record (logged): the detector would raise a fresh divergence if
	// the contradiction persists, and the trust gate's mark is already set.
	labelRepairQueueSize = 64
)

// labelDivergence is one detected contradiction of an engine label write.
type labelDivergence struct {
	repo    string
	number  int
	label   string
	written string // "added" or "removed": what the engine wrote
	at      time.Time
}

// SetLabelRepairDisabledForTest neutralises the confirm-and-re-apply (R1) so a
// test can show the twin fails without it. Per Engine.
func (e *Engine) SetLabelRepairDisabledForTest(disabled bool) {
	e.labelRepairOff.Store(disabled)
}

// handleLabelDivergence is called from the itemstate observer for every
// divergence the detector raised. It is void, non-blocking and never calls
// GitHub: it marks an in-scope label as diverged and queues it for the worker.
func (e *Engine) handleLabelDivergence(recs []labelDivergence) {
	defer func() { _ = recover() }() // an observer hook must never take the writer down
	for _, r := range recs {
		if !e.labelInScope(r.label) {
			continue
		}
		g := &e.labels
		ref := issueRef(r.repo, r.number)
		g.mark(ref, r.label, r.at)
		if e.labelRepairOff.Load() {
			continue
		}
		g.mu.Lock()
		q := g.queue
		g.mu.Unlock()
		if q == nil {
			continue
		}
		g.inFlight.Add(1)
		select {
		case q <- r:
		default:
			g.inFlight.Add(-1)
			e.logf(r.number, "label-repair", "queue full — dropping the divergence on %s (it is raised again if it persists)\n", r.label)
		}
	}
}

// startLabelRepair starts the repair worker. Called once the channel hub is up.
func (e *Engine) startLabelRepair() {
	g := &e.labels
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return
	}
	g.queue = make(chan labelDivergence, labelRepairQueueSize)
	g.stop = make(chan struct{})
	g.running = true
	g.wg.Add(1)
	go e.runLabelRepair(g.queue, g.stop)
}

// stopLabelRepair stops the worker and drops what is still queued. Idempotent.
func (e *Engine) stopLabelRepair() {
	g := &e.labels
	g.mu.Lock()
	if !g.running {
		g.mu.Unlock()
		return
	}
	g.running = false
	stop := g.stop
	g.queue = nil
	g.mu.Unlock()
	close(stop)
	g.wg.Wait()
	g.inFlight.Store(0)
}

func (e *Engine) runLabelRepair(q chan labelDivergence, stop chan struct{}) {
	g := &e.labels
	defer g.wg.Done()
	for {
		select {
		case <-stop:
			return
		case r := <-q:
			func() {
				defer g.inFlight.Add(-1)
				defer func() {
					if p := recover(); p != nil {
						e.logf(r.number, "label-repair", "repair of %s panicked: %v\n", r.label, p)
					}
				}()
				e.repairLabel(r)
			}()
		}
	}
}

// DrainLabelRepairForTest blocks until the repair worker has nothing queued or
// executing, or the timeout passes. It reports whether it drained.
func (e *Engine) DrainLabelRepairForTest(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.labels.inFlight.Load() <= 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return e.labels.inFlight.Load() <= 0
}

// repairLabel is one repair, steps 1-5 of the file comment.
func (e *Engine) repairLabel(r labelDivergence) {
	g := &e.labels
	ref := issueRef(r.repo, r.number)
	key := labelKey(ref, r.label)
	owner, repo, ok := strings.Cut(r.repo, "/")
	if !ok {
		return
	}
	now := e.now()

	// 1. Recurrence.
	g.mu.Lock()
	last, seen := g.repaired[key]
	g.mu.Unlock()
	if seen && now.Sub(last) <= e.cacheDivergenceWindow() {
		e.reportLabelProblem(ref, r.label, fmt.Sprintf("the engine's %s of %s was contradicted again within %s of the last repair; not re-applying a second time", r.written, r.label, e.cacheDivergenceWindow()))
		return
	}

	// 2. Intent.
	wantAdded := r.written == "added"
	in, ok := g.intent(ref, r.label)
	if !ok || in.added != wantAdded {
		e.logf(r.number, "label-repair", "%s: the engine's intent changed since the divergence (superseded by a later write) — nothing to restore\n", r.label)
		e.clearDivergence(ref, r.label)
		return
	}
	cur, needPause := e.intentCurrent(ref, r.repo, r.number, r.label, in)
	if !cur {
		e.logf(r.number, "label-repair", "%s: contradicted %s ago, but the intent behind that write is no longer current — not re-applying\n", r.label, now.Sub(in.at).Round(time.Second))
		e.clearDivergence(ref, r.label)
		return
	}

	// 3. One live read.
	labels, err := e.liveLabels(owner, repo, r.number)
	if err != nil {
		// Keep the mark: the label stays untrusted. A person is told.
		e.reportLabelProblem(ref, r.label, fmt.Sprintf("could not confirm %s with a live read: %v", r.label, err))
		return
	}
	present := hasLabel(labels, r.label)

	// 4. A lagging listing: GitHub already matches the intent.
	if present == in.added {
		e.logf(r.number, "label-repair", "stale-read: %s %s on GitHub as the engine intended; the listing that contradicted it lagged — nothing written\n", r.label, presentWord(present))
		e.clearDivergence(ref, r.label)
		return
	}

	// The pause pair: a human removal looks like a lost write, so a removal event
	// after the engine's write means a person lifted it (ADR 1813).
	if needPause {
		// live-read-audited: the event log is the only evidence that tells a human removal from a lost write; a failed read is doubt, and doubt re-applies nothing
		removedAt, rerr := e.client.FetchLabelRemovedAt(owner, repo, r.number, r.label)
		if rerr != nil {
			e.reportLabelProblem(ref, r.label, fmt.Sprintf("could not read the removal history of %s: %v", r.label, rerr))
			return
		}
		if !removedAt.IsZero() && removedAt.After(in.at) {
			e.logf(r.number, "label-repair", "%s: removed on GitHub after the engine's write (a person lifted it) — not re-applying\n", r.label)
			e.clearDivergence(ref, r.label)
			return
		}
	}

	// 5. A lost write. The intent is re-checked straight before writing: a release
	// racing the read must not put a lock back.
	in2, ok2 := g.intent(ref, r.label)
	cur2, _ := e.intentCurrent(ref, r.repo, r.number, r.label, in2)
	if !ok2 || in2.added != in.added || !cur2 {
		e.logf(r.number, "label-repair", "%s: the intent ended while it was being confirmed — not re-applying\n", r.label)
		e.clearDivergence(ref, r.label)
		return
	}
	g.mu.Lock()
	if g.repaired == nil {
		g.repaired = map[string]time.Time{}
	}
	g.repaired[key] = now
	g.mu.Unlock()

	item := gh.ProjectItem{Number: r.number, Repo: r.repo}
	var werr error
	if in.added {
		werr = e.addLabelChecked(item, r.label)
	} else {
		werr = e.removeLabelChecked(item, r.label)
	}
	if werr != nil {
		e.reportLabelProblem(ref, r.label, fmt.Sprintf("lost write on %s and the re-apply failed: %v", r.label, werr))
		return
	}
	e.logf(r.number, "label-repair", "lost-write: GitHub %s %s although the engine wrote it %s ago (live read, Store value contradicted by a listing) — re-applied once\n",
		absentWord(present), r.label, now.Sub(in.at).Round(time.Second))
	e.clearDivergence(ref, r.label)
}

func presentWord(present bool) string {
	if present {
		return "has"
	}
	return "lacks"
}

func absentWord(present bool) string {
	if present {
		return "still carries"
	}
	return "does not carry"
}

// reportLabelProblem records an unresolved divergence (a failed read or
// re-apply, or a recurrence) in the warnings panel and the log. The item is not
// paused.
func (e *Engine) reportLabelProblem(ref, label, detail string) {
	g := &e.labels
	key := labelKey(ref, label)
	g.mu.Lock()
	if g.problems == nil {
		g.problems = map[string]string{}
	}
	g.problems[key] = detail
	g.mu.Unlock()
	e.logf(0, "label-repair", "%s %s: %s\n", ref, label, detail)
	e.syncLabelProblems()
}

// clearDivergence ends an episode: the label is trusted again and any unresolved
// warning for it leaves the panel. Every exit that drops the divergence mark goes
// through it, so a problem reported earlier (a failed read, say) cannot outlive the
// episode it described.
func (e *Engine) clearDivergence(ref, label string) {
	e.labels.unmark(ref, label)
	e.resolveLabelProblem(ref, label)
}

func (e *Engine) resolveLabelProblem(ref, label string) {
	g := &e.labels
	g.mu.Lock()
	_, had := g.problems[labelKey(ref, label)]
	delete(g.problems, labelKey(ref, label))
	g.mu.Unlock()
	if had {
		e.syncLabelProblems()
	}
}

// syncLabelProblems reconciles the panel with the unresolved set. The check ran
// to completion (it is a snapshot of in-memory state), so an empty set clears the
// type.
func (e *Engine) syncLabelProblems() {
	g := &e.labels
	g.mu.Lock()
	keys := make([]string, 0, len(g.problems))
	for k := range g.problems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	conds := make([]warnings.Condition, 0, len(keys))
	for _, k := range keys {
		conds = append(conds, warnings.Condition{
			Subject:     k,
			Title:       "An engine label write did not hold: " + strings.Replace(k, "|", " ", 1),
			Detail:      g.problems[k],
			Fingerprint: k,
		})
	}
	g.mu.Unlock()
	if err := warnings.Reconcile(e.warnWriter("label-repair"), labelDivergenceWarningType, conds); err != nil {
		e.logf(0, "label-repair", "could not record the label divergence warning: %v\n", err)
	}
}
