package engine

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/tui"
)

// waitingSampleInterval is how often the waiting-rows publisher samples the store.
const waitingSampleInterval = time.Second

// waitingRowsMax bounds the waiting section of the In Progress pane (#2238 R5).
const waitingRowsMax = 5

// waitingTextMax bounds a row's description so a fallback summary cannot dominate the line.
const waitingTextMax = 90

// waitingFacts are the cache-only details the row text needs beyond the
// classifier's (State, Code). Every field is optional: an unknown fact is left
// zero and omitted from the text, never guessed (#2238 FR-009).
type waitingFacts struct {
	prNumber    int
	checksDone  int
	checksTotal int
	reviewers   []string
	since       time.Time // when the engine began waiting on the gate; zero = unknown
	retryDue    time.Time
	trainPos    int
	trainOf     int
	trainStage  string
	limitUntil  time.Time
}

// waitingRow is one classified, displayable wait before sorting and capping.
type waitingRow struct {
	item     tui.WaitingItem
	rank     int       // human rows only
	deadline time.Time // earliest known deadline; zero = unknown
	since    time.Time // when the wait began; zero = unknown
	ref      string
}

// waitingClock formats an absolute time at minute resolution, so a row's text
// is stable between samples and the publisher does not re-emit every second.
func waitingClock(t time.Time) string { return t.Local().Format("15:04") }

func sinceSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return ", since " + waitingClock(t)
}

// waitingText is the pure description of what an item is waiting for, built from
// the classifier's output and a few cache-only facts (#2238). It deliberately
// does not use the status-line text (never read back, ADR 2048) and falls back
// to the classifier's Summary only for codes it has no tailored wording for.
func waitingText(res attention.Result, f waitingFacts) string {
	switch res.Code {
	case "fabrik:awaiting-ci":
		var parts []string
		if f.checksTotal > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d checks", f.checksDone, f.checksTotal))
		}
		detail := strings.Join(parts, ", ") + sinceSuffix(f.since)
		detail = strings.TrimPrefix(detail, ", ")
		text := "CI"
		if f.prNumber > 0 {
			text += fmt.Sprintf(" on PR #%d", f.prNumber)
		}
		if detail != "" {
			text += " (" + detail + ")"
		}
		return text
	case "fabrik:awaiting-review":
		text := "review"
		if len(f.reviewers) > 0 {
			text += " from " + strings.Join(f.reviewers, ", ")
		}
		if !f.since.IsZero() {
			text += " (since " + waitingClock(f.since) + ")"
		}
		return text
	case attention.CodeRetryPending, attention.CodeRetryWaitingForSlot:
		text := "retry"
		if res.Code == attention.CodeRetryWaitingForSlot {
			text += " (waiting for a worker slot)"
			return text
		}
		if !f.retryDue.IsZero() {
			text += " at " + waitingClock(f.retryDue)
		}
		if res.Cause == attention.CauseMaxTurns || res.Cause == attention.CauseWallTimeKill {
			text += " (time slice)"
		}
		return text
	case "fabrik:rebase-needed":
		return "rebase (fabrik:rebase-needed)"
	case "queued":
		stage := f.trainStage
		if stage == "" {
			stage = "Queued"
		}
		switch {
		case f.trainPos > 0 && f.trainOf > 0:
			return fmt.Sprintf("landing in merge train (%s, position %d of %d)", stage, f.trainPos, f.trainOf)
		default:
			return fmt.Sprintf("landing in merge train (%s)", stage)
		}
	case "fabrik:blocked":
		return "dependencies (fabrik:blocked)"
	case "fabrik:claude-limit":
		if !f.limitUntil.IsZero() {
			return "Claude usage limit until " + waitingClock(f.limitUntil)
		}
		return "Claude usage limit"
	case attention.CodeWaitingForPoll:
		return "next poll"
	case attention.CodeDecisionPending:
		return "human decision"
	case attention.CodeParkedBlocker:
		return "blocking issue parked (human decision)"
	case attention.CodeAwaitingMergeDecision:
		return "human merge decision"
	case "fabrik:paused", "fabrik:awaiting-input":
		return "human reply (paused)"
	}
	if res.State == attention.NeedsHuman || res.State == attention.Escalated {
		return "human reply (" + res.Code + ")"
	}
	return trimWaitingSummary(res.Summary)
}

// trimWaitingSummary is the fallback text: the first clause of a classifier
// summary, bounded in length.
func trimWaitingSummary(s string) string {
	if i := strings.IndexAny(s, ";"); i > 0 {
		s = s[:i]
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) > waitingTextMax {
		r = append(r[:waitingTextMax-1], '…')
	}
	return string(r)
}

// waitingKept reports whether a classification is shown as a waiting row, and
// as which kind. Working (has its own running row), Idle, Stalled (surfaced by
// the stall events and health), a red-CI overlay and the between-polls
// dispatch states carry no operator-relevant wait.
func waitingKept(res attention.Result) (tui.WaitingKind, bool) {
	switch res.Code {
	case attention.CodeCIRed, attention.CodeWaitingForDispatch, attention.CodeQueuedForSlot, "idle":
		return 0, false
	}
	switch res.State {
	case attention.NeedsHuman, attention.Escalated, attention.Suspended:
		return tui.WaitingHuman, true
	case attention.Waiting:
		return tui.WaitingExternal, true
	}
	return 0, false
}

// waitingFactsOf copies the cache-only details for one item. st may point at live
// Store memory (inside Scan): everything is copied, nothing retained.
func (e *Engine) waitingFactsOf(st *itemstate.ItemState, res attention.Result, trainRows []trainRow, limitUntil time.Time) waitingFacts {
	f := waitingFacts{limitUntil: limitUntil}
	if pr := st.LinkedPR; pr != nil {
		f.prNumber = pr.Number
		for _, c := range pr.CheckRuns {
			f.checksTotal++
			if c.Status == "completed" {
				f.checksDone++
			}
		}
		for _, r := range pr.ReviewRequests {
			f.reviewers = append(f.reviewers, r.Login)
		}
	}
	if t, ok := st.LabelAppliedAt[res.Code]; ok {
		f.since = t
	}
	if st.PendingRetry != nil {
		f.retryDue = st.PendingRetry.Due
	}
	if res.Code == "queued" {
		f.trainStage = st.Status
		if pos, of, ok := trainSlot(trainRows, st); ok {
			f.trainPos, f.trainOf = pos, of
		}
	}
	return f
}

// sampleWaiting classifies every open managed item from the Store alone and
// returns the ordered, capped snapshot (#2238). No GitHub call is made: it reads
// item state through Scan, the same way buildSituation does, and uses the
// shared attentionInput/Classify path, so it adds no detection of its own.
func (e *Engine) sampleWaiting(now time.Time) tui.WaitingItemsEvent {
	lb := localAPIBackend{e: e}
	threshold := lb.stallThreshold(0)
	usageLimited := lb.usageLimitedUntil(now)

	// Both are built by their own Scan: resolving them inside the visitor below
	// would nest the store's read lock.
	look := e.scanBlockerIndex()
	trainRows := lb.trainRows()

	var rows []waitingRow
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsPR || st.IsClosed || !st.LabelsKnown {
			return
		}
		switch e.attentionStageKind(st.Status) {
		case attention.KindManaged, attention.KindHolding:
		default:
			return
		}
		// Dependency-blocked rows keep their own event path (IssueBlockedEvent).
		if hasLabelStr(st.Labels, "fabrik:blocked") {
			return
		}
		res := attention.Classify(e.attentionInput(st, now, threshold, usageLimited, look))
		kind, ok := waitingKept(res)
		if !ok {
			return
		}
		f := e.waitingFactsOf(st, res, trainRows, usageLimited)
		row := waitingRow{
			item: tui.WaitingItem{
				Repo: st.Repo, Number: st.Number, Title: st.Title, Stage: st.Status,
				Kind: kind, Text: waitingText(res, f),
			},
			rank:     res.Rank,
			deadline: res.Next.At,
			since:    f.since,
			ref:      issueRef(st.Repo, st.Number),
		}
		if row.since.IsZero() {
			row.since = st.StatusEnteredAt
		}
		rows = append(rows, row)
	})

	sortWaitingRows(rows)

	ev := tui.WaitingItemsEvent{}
	for _, r := range rows {
		if r.item.Kind == tui.WaitingHuman {
			ev.HumanCount++
		} else {
			ev.WaitCount++
		}
	}
	if len(rows) > waitingRowsMax {
		ev.Omitted = len(rows) - waitingRowsMax
		rows = rows[:waitingRowsMax]
	}
	for _, r := range rows {
		ev.Items = append(ev.Items, r.item)
	}
	return ev
}

// sortWaitingRows orders human rows first (attention rank, then ref), then
// external waits. Waiting results are all unranked, so a rank sort would put them
// ahead of everything: they are ordered by earliest known deadline, then
// longest-waiting, then ref.
func sortWaitingRows(rows []waitingRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.item.Kind != b.item.Kind {
			return a.item.Kind == tui.WaitingHuman
		}
		if a.item.Kind == tui.WaitingHuman {
			if a.rank != b.rank {
				return a.rank < b.rank
			}
			return a.ref < b.ref
		}
		if a.deadline.IsZero() != b.deadline.IsZero() {
			return !a.deadline.IsZero()
		}
		if !a.deadline.Equal(b.deadline) {
			return a.deadline.Before(b.deadline)
		}
		if a.since.IsZero() != b.since.IsZero() {
			return !a.since.IsZero()
		}
		if !a.since.Equal(b.since) {
			return a.since.Before(b.since)
		}
		return a.ref < b.ref
	})
}

// runWaitingPublisher samples the waiting items once per interval and emits a
// tui.WaitingItemsEvent whenever the snapshot changes, until ctx ends (#2238,
// ADR 2238). A full replacement is sent each time, so rows appear and disappear
// with no bookkeeping. Display only: nothing reads the value back.
func (e *Engine) runWaitingPublisher(ctx context.Context, interval time.Duration) {
	var last tui.WaitingItemsEvent
	first := true
	emit := func() {
		cur := e.sampleWaiting(e.now())
		if !first && reflect.DeepEqual(cur, last) {
			return
		}
		first, last = false, cur
		e.emitStructural(cur)
	}
	emit()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit()
		}
	}
}
