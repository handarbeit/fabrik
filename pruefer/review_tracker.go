package pruefer

import (
	"strings"
	"sync"
)

// ReviewTracker is an in-memory, process-lifetime-only record of exactly
// which (owner, repo, PR number, head SHA) tuples this Pruefer process has
// itself successfully submitted a review for. It exists as a second,
// independent source of truth for "have I already reviewed this head" —
// one that Pruefer controls itself and never needs to ask GitHub to
// re-confirm — alongside (never in place of) the GitHub-derived
// alreadyReviewedAtHead check in select.go.
//
// #1631's confirmed root cause was a GitHub outage returning
// successful-but-partial FetchPRReviews responses that omitted the bot's
// own prior review at the current head: alreadyReviewedAtHead correctly,
// but wrongly, reported "no prior review" on every poll for the outage's
// duration, driving an unbounded re-review loop (14 reviews on one PR, 25
// on another). A response ReviewPR itself trusted was the thing that was
// wrong; nothing about the guard's own comparison logic was. This tracker
// closes that gap the way ADR-1615 closes an analogous one for merge-train
// identity: by asserting state from a fact the system itself controls,
// never from external data that can independently be incomplete. Once
// ReviewTracker has recorded a submission, a subsequent degraded-but-200
// FetchPRReviews read can no longer matter — ReviewPR never asks it.
//
// Deliberately in-memory, not persisted: cmd/pruefer/README.md documents
// "review state is derived from GitHub itself... not stored locally — a
// restart never causes a review storm" (ADR-1113). This tracker is
// additive to that guarantee, not a silent contradiction of it — a
// restart clears it and Pruefer falls back to exactly the pre-#1631,
// GitHub-derived-only behavior. That is never worse than before this
// issue, only better within one process's uptime (see
// adrs/1631-pruefer-local-review-tracker-backstop.md for the accepted
// residual gap: a restart mid-outage still costs one wasted review per
// affected head, not thirty-five).
//
// The cap is fixed at "once, ever, this process" — not a time window or
// configurable count. /pruefer review (ForceReview) is the sole, explicit
// escape hatch for a genuinely-wanted re-review of an unchanged head,
// mirroring alreadyReviewedAtHead's own bypass semantics; ReviewPR checks
// forceReview before consulting the tracker, exactly as it already does
// for the GitHub-derived check.
//
// Nil-safe: a nil *ReviewTracker (the zero value ReviewPR's existing
// ~45 test call sites now pass) behaves as if the backstop doesn't exist
// — Recall always reports false, Record is a no-op — so this is a purely
// additive dependency, not a required one.
type ReviewTracker struct {
	mu       sync.Mutex
	reviewed map[reviewKey]struct{}
}

// reviewKey identifies one (owner, repo, PR, head SHA) tuple. owner/repo are
// lowercased at construction so tracker lookups are case-insensitive,
// matching alreadyReviewedAtHead's strings.EqualFold comparisons elsewhere
// in this package. headSHA is compared verbatim — SHAs are already
// case-normalized by git/GitHub.
type reviewKey struct {
	owner, repo string
	prNumber    int
	headSHA     string
}

// NewReviewTracker returns an empty, ready-to-use tracker.
func NewReviewTracker() *ReviewTracker {
	return &ReviewTracker{reviewed: make(map[reviewKey]struct{})}
}

func newReviewKey(owner, repo string, prNumber int, headSHA string) reviewKey {
	return reviewKey{
		owner:    strings.ToLower(owner),
		repo:     strings.ToLower(repo),
		prNumber: prNumber,
		headSHA:  headSHA,
	}
}

// Recall reports whether this process has already recorded a successful
// review submission for owner/repo#prNumber at headSHA. A nil receiver
// always reports false.
func (t *ReviewTracker) Recall(owner, repo string, prNumber int, headSHA string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.reviewed[newReviewKey(owner, repo, prNumber, headSHA)]
	return ok
}

// Record notes that owner/repo#prNumber has been successfully reviewed at
// headSHA. A nil receiver is a no-op — safe to call unconditionally from
// ReviewPR regardless of whether a tracker was injected.
func (t *ReviewTracker) Record(owner, repo string, prNumber int, headSHA string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reviewed == nil {
		t.reviewed = make(map[reviewKey]struct{})
	}
	t.reviewed[newReviewKey(owner, repo, prNumber, headSHA)] = struct{}{}
}
