package engine

import (
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Exemptions for the SHA-invalidation scan (#2158, ADR-2158). The scan strips
// stage:Validate:complete when the linked PR's head moves past the SHA recorded at
// Validate completion. Two heads moves are not external pushes and must not do that:
//
//   - the work has already landed (closed issue, cleanup stage, merged PR, landing
//     markers) — the merge train's singleton catch-up moves the head and then lands the
//     member, and the scan used to fire on the just-landed Done item;
//   - the head is the engine's own pure singleton catch-up merge (#2044), recognised by
//     the same trust rules as dropCatchUpFeedback: the engine-authored marker names the
//     live head with pure=true; the commit trailer is never trusted.

// validateItemLanded returns a non-empty reason when any cached fact — from the shallow
// board item or the store snapshot, ORed — says the item has landed or is terminal. A
// disagreement between the two sources therefore resolves to "landed": skipping is the
// safe polarity for a closed/Done/merged item and needs no live read.
func (e *Engine) validateItemLanded(item gh.ProjectItem, snap itemstate.Snapshot, lpr *itemstate.LinkedPRState) string {
	if item.IsClosed || snap.IsClosed() {
		return "issue closed"
	}
	for _, status := range []string{item.Status, snap.Status()} {
		if status == "" {
			continue
		}
		if st := stages.FindStage(e.cfg.Stages, status); st != nil && st.CleanupWorktree {
			return "in cleanup stage " + status
		}
	}
	if lpr != nil && lpr.Merged {
		return "linked PR merged"
	}
	for _, labels := range [][]string{item.Labels, snap.Labels()} {
		for _, l := range labels {
			if l == awaitingLandingVerificationLabel || strings.HasPrefix(l, creditedPRLabelPrefix) {
				return "carries " + l
			}
		}
	}
	return ""
}

// headIsPureCatchUp reports whether the PR's live head is the engine's own pure
// singleton catch-up merge. A marker read error is returned so the caller can defer.
func (e *Engine) headIsPureCatchUp(owner, repo string, lpr *itemstate.LinkedPRState) (bool, error) {
	if lpr == nil || lpr.Number == 0 || lpr.HeadSHA == "" {
		return false, nil
	}
	mk, ok, err := e.catchUpMarkerCached(owner, repo, lpr.Number, lpr.HeadSHA)
	if err != nil || !ok {
		return false, err
	}
	return mk.Pure && mk.Head == lpr.HeadSHA, nil
}
