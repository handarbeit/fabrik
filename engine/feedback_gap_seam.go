package engine

import (
	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// maskedReviewReadClient hides one review from the dispatcher's cache-sourced
// reads until the feedback gate has repaired the item's input (#2160). It models
// the state an engine self-write leaves behind: the cache judges itself fresh
// against a baseline that post-dates the review, so nothing but the gate's
// DeepFetchInvalidated makes it look again. Ordinary store writes and "stale"
// refetches keep hiding the review, which a sim cannot otherwise hold in place
// deterministically (every later store write moves the baseline past it).
type maskedReviewReadClient struct {
	boardcache.ReadClient
	e        *Engine
	reviewID int
}

// repaired reports whether the gate has recorded a dispatcher-input repair.
func (m *maskedReviewReadClient) repaired() bool {
	m.e.feedbackGapMu.Lock()
	defer m.e.feedbackGapMu.Unlock()
	return len(m.e.feedbackGaps) > 0
}

func (m *maskedReviewReadClient) FetchItemDetails(item *gh.ProjectItem) error {
	if err := m.ReadClient.FetchItemDetails(item); err != nil {
		return err
	}
	if m.repaired() {
		return nil
	}
	kept := make([]gh.PRReview, 0, len(item.LinkedPRReviews))
	for _, r := range item.LinkedPRReviews {
		if r.DatabaseID != m.reviewID {
			kept = append(kept, r)
		}
	}
	item.LinkedPRReviews = kept
	return nil
}

// MaskReviewFromCacheForTest makes the dispatcher's board-cache reads omit the
// review with the given database ID until the feedback gate has repaired the
// item's input (the #2160 repair). Test seam only (tests/sim); production never
// calls it.
func (e *Engine) MaskReviewFromCacheForTest(reviewID int) {
	e.readClient = &maskedReviewReadClient{ReadClient: e.readClient, e: e, reviewID: reviewID}
}
