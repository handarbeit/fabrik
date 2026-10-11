package engine

import (
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// The poll-side recorder of GitHub times for pending decisions (#2120 R3,
// ADR-2120).
//
// A suspended item's age is anchored on the GitHub time its deciding fact
// became true (the stage:<X>:complete label, or fabrik:blocked). The local read
// API and the channel deriver must make no GitHub call (R7/R9; the source scans
// pin it), and the store's LabelAppliedAt map is record-on-write — empty after a
// restart. So the one place that may fetch the time is the poll goroutine, here:
// settleDecisionAnchors reads each missing time once through labelAppliedAt,
// which writes it into the store's LabelAppliedAt map, where the cache-only
// classifier then finds it. This file is deliberately not named localapi* or
// channel_*, so those scans are not weakened.
//
// Cost is bounded: only items that would be suspended are looked up, at most
// decisionAnchorPerPoll reads per poll, and a miss or an error is remembered and
// not retried for decisionAnchorRetry.

const (
	decisionAnchorPerPoll = 8
	decisionAnchorRetry   = 10 * time.Minute
)

type decisionAnchorState struct {
	mu sync.Mutex
	// retryAt maps "repo#N|label" to the earliest time the lookup is tried again.
	retryAt map[string]time.Time
}

func (s *decisionAnchorState) due(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.retryAt[key])
}

func (s *decisionAnchorState) backoff(key string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retryAt == nil {
		s.retryAt = map[string]time.Time{}
	}
	s.retryAt[key] = until
}

func (s *decisionAnchorState) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.retryAt, key)
}

// settleDecisionAnchors fills in the missing GitHub label times of items the
// attention classifier would call suspended. Runs every poll on the poll
// goroutine; a read failure only leaves the time unknown (the classifier then
// falls back to the stage comment's time, then to "at least since first seen").
func (e *Engine) settleDecisionAnchors(board *gh.ProjectBoard) {
	now := e.now()
	look := e.peekBlockerLookup()
	budget := decisionAnchorPerPoll
	for _, item := range board.Items {
		if budget <= 0 {
			return
		}
		if item.IsClosed || item.IsPR || item.Status == "" {
			continue
		}
		repo := itemOwnerRepoString(item, e.defaultRepo())
		snap, ok := e.store.Peek(repo, item.Number)
		if !ok {
			continue
		}
		st := snap.State()
		for _, label := range e.decisionAnchorLabels(&st, look) {
			if budget <= 0 {
				return
			}
			if !snap.LabelAppliedAt(label).IsZero() {
				continue
			}
			key := issueRef(repo, item.Number) + "|" + label
			if !e.decisionAnchors.due(key, now) {
				continue
			}
			budget--
			owner, name := itemOwnerRepo(item, e.defaultRepo())
			at, err := e.labelAppliedAt(item, owner, name, label)
			if err != nil || at.IsZero() {
				if err != nil {
					e.logf(item.Number, "decision-anchor", "could not read when %s was applied: %v\n", label, err)
				}
				e.decisionAnchors.backoff(key, now.Add(decisionAnchorRetry))
				continue
			}
			e.decisionAnchors.forget(key)
		}
	}
}

// decisionAnchorLabels lists the labels whose GitHub time the item's pending
// decision would be anchored on, or nil when the item would not be suspended
// (so no read is spent on it). It mirrors the classifier's two cases with the
// engine's own predicates, over the store alone.
func (e *Engine) decisionAnchorLabels(st *itemstate.ItemState, look blockerLookup) []string {
	if st.IsPR || st.IsClosed || st.Worker != nil || hasLabelStr(st.Labels, "fabrik:paused") {
		return nil
	}
	stage := e.stageByName(st.Status)
	if stage == nil || e.attentionStageKind(st.Status) != attention.KindManaged {
		return nil
	}
	var out []string
	if complete := "stage:" + st.Status + ":complete"; hasLabelStr(st.Labels, complete) &&
		st.Status != "Validate" && e.decisionNextStage(st.Status) != "" && !stageAutoAdvances(e.cfg.Yolo, st.Labels, stage) {
		out = append(out, complete)
	}
	if hasLabelStr(st.Labels, "fabrik:blocked") {
		if _, only := e.resolveParkedBlockers(st, look); only {
			out = append(out, "fabrik:blocked")
		}
	}
	return out
}
