package engine

import (
	"fmt"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// The live-read helper family (#2350, ADR 2350, audit §3.1–3.4).
//
// A gate that reads GitHub live used to decide on what it read and throw the
// result away, so the next decision fell back to the stale cached value. Every
// such read now goes through one of these helpers: the request start time is
// captured first, the result is applied to the Store through a mutation that
// carries an authority time (so the observed-at ordering rule of ADR 2348
// governs it: an older read cannot revert a newer fact and a newer fact cannot be
// reverted by it), and the apply is tagged FromLiveRead so it wakes nothing. A
// read that failed is never applied — the caller keeps its fail-closed or
// fail-open behaviour on the error it gets back.
//
// TestLiveReadsAreOnlyMadeThroughTheHelpers forbids the raw calls elsewhere in
// the engine (an audited exception list names each remaining site and why).

// liveReadState holds the engine-clock stamps of the last live read of a fact,
// which bound how often a per-poll caller may repeat one.
type liveReadState struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (s *liveReadState) note(key string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = make(map[string]time.Time)
	}
	s.at[key] = at
}

func (s *liveReadState) fresh(key string, now time.Time, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[key]
	return ok && now.Sub(t) < ttl && !now.Before(t)
}

func liveKey(fact, repo string, number int) string {
	return fmt.Sprintf("%s:%s#%d", fact, repo, number)
}

// liveFreshFor is how long a live read of a fact stays trusted by a caller that
// runs every poll (the advance review gate). One tenth of the API-cost knob of
// ADR 1831: a stage-complete item that waits for a human does not pay a read per
// poll, while a transition (an advance, a landing, a dispatch) is always
// confirmed live by the callers that guard one.
func (e *Engine) liveFreshFor() time.Duration {
	if e.liveFreshOverride.Load() > 0 {
		return time.Duration(e.liveFreshOverride.Load())
	}
	return e.githubRecheckInterval()
}

// SetLiveFreshForTest overrides liveFreshFor (zero restores it). Test seam only.
func (e *Engine) SetLiveFreshForTest(d time.Duration) { e.liveFreshOverride.Store(int64(d)) }

// SetLiveReadApplyDisabledForTest makes the helpers read and return but not
// apply, reproducing the pre-#2350 "read, decide, discard" shape so a test can
// show it is not vacuous. Test seam only.
func (e *Engine) SetLiveReadApplyDisabledForTest(off bool) { e.liveApplyOff.Store(off) }

// storeHolds reports whether the Store already has a record for the item. A
// helper never creates one: an item the engine does not track is only read.
func (e *Engine) storeHolds(repo string, number int) bool {
	_, ok := e.store.Peek(repo, number)
	return ok
}

func (e *Engine) applyLive(m itemstate.Mutation) {
	if e.liveApplyOff.Load() {
		return
	}
	e.store.Apply(itemstate.FromLiveRead{Inner: m})
}

// liveLabels reads the item's labels from GitHub, applies the full set to the
// Store and returns the Store's view: the read, except where the engine wrote a
// label after the read began (the engine-write protection keeps that write). It
// returns the raw read when the Store does not hold the item.
func (e *Engine) liveLabels(owner, repo string, number int) ([]string, error) {
	start := time.Now()
	labels, err := e.client.FetchLabels(owner, repo, number)
	if err != nil {
		return nil, err
	}
	repoKey := owner + "/" + repo
	if e.liveApplyOff.Load() || !e.storeHolds(repoKey, number) {
		return labels, nil
	}
	e.applyLive(itemstate.LabelsRead{Repo: repoKey, Number: number, Labels: labels, FetchedAt: start})
	if snap, ok := e.store.Peek(repoKey, number); ok && snap.LabelsKnown() {
		return snap.Labels(), nil
	}
	return labels, nil
}

// liveItem reads the whole item (FetchItemDetails) into item and applies the
// deep-owned facts to the Store (comments, blockedBy, linked PR, reviews, and
// the labels and closed state a live read carries) when the Store tracks the
// item; a read of an untracked item is not turned into a record. It is applyDeepFetched
// without the cacheless listing step: item is a copy captured earlier, so its
// Status is not a fresh listing and must not be written back.
func (e *Engine) liveItem(item *gh.ProjectItem) error {
	if err := e.client.FetchItemDetails(item); err != nil {
		return err
	}
	repo := itemOwnerRepoString(*item, e.defaultRepo())
	if !e.liveApplyOff.Load() && e.storeHolds(repo, item.Number) {
		e.applyLive(itemstate.ItemDeepFetched{Repo: repo, Number: item.Number, FreshState: *item})
	}
	item.DeepFetchLive = false
	item.DeepFetchStartedAt = time.Time{}
	e.live.note(liveKey("item", repo, item.Number), e.now())
	return nil
}

// livePRMerged reads whether the PR is merged. A confirmed merge is applied to
// the Store (State and Merged only, never Title or Draft) when the PR is the
// item's linked PR; "not merged" is never applied, because the single-PR read
// says nothing about a PR the list endpoint has not caught up with.
func (e *Engine) livePRMerged(owner, repo string, issueNumber, prNumber int) (bool, error) {
	start := time.Now()
	merged, err := e.client.FetchPRMerged(owner, repo, prNumber)
	if err != nil {
		return false, err
	}
	repoKey := owner + "/" + repo
	if merged && !e.liveApplyOff.Load() && issueNumber > 0 {
		if snap, ok := e.store.Peek(repoKey, issueNumber); ok {
			if lpr := snap.LinkedPR(); lpr != nil && lpr.Number == prNumber {
				e.applyLive(itemstate.LinkedPRStateObserved{Repo: repoKey, Number: issueNumber, PRNumber: prNumber, State: "closed", Merged: true, At: start})
			}
		}
	}
	return merged, nil
}

// liveReviews reads the PR's reviews and review requests. Both halves must be
// read: a partial read returns an error and applies nothing. The result is
// applied to the Store (when it holds the item) and the raw lists are returned,
// the shape both review gates already decide on.
func (e *Engine) liveReviews(owner, repo string, issueNumber, prNumber int) ([]gh.PRReview, []gh.ReviewRequest, error) {
	start := time.Now()
	reviews, rerr := e.readClient.FetchPRReviews(owner, repo, prNumber)
	if rerr != nil {
		return nil, nil, rerr
	}
	requests, qerr := e.readClient.FetchPRReviewRequests(owner, repo, prNumber)
	if qerr != nil {
		return nil, nil, qerr
	}
	repoKey := owner + "/" + repo
	if issueNumber > 0 && e.storeHolds(repoKey, issueNumber) {
		e.applyLive(itemstate.PRReviewsRead{Repo: repoKey, Number: issueNumber, PRNumber: prNumber, Reviews: reviews, Requests: requests, FetchedAt: start})
		e.live.note(liveKey("reviews", repoKey, issueNumber), e.now())
	}
	return reviews, requests, nil
}

// liveReviewsFresh reports whether the item's reviews were read live within
// liveFreshFor, so a per-poll caller can skip repeating the read.
func (e *Engine) liveReviewsFresh(repoKey string, issueNumber int) bool {
	return e.live.fresh(liveKey("reviews", repoKey, issueNumber), e.now(), e.liveFreshFor())
}
