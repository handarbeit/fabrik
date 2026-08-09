package simgh

import (
	gh "github.com/handarbeit/fabrik/github"
)

// FetchPRReviews returns the **latest review per author**, in first-submission
// order — not the raw submission history.
//
// The collapsing is the load-bearing part, and it is production's behaviour
// rather than a convenience: github.Client.FetchPRReviews reads a REST endpoint
// that returns every submission and reduces it to one entry per author, so that
// the result matches GraphQL's latestReviews semantics. The engine's review-gate
// call sites (engine/reviews.go) consume that result assuming the reduction has
// already happened. Returning the raw list here would leave a superseded
// CHANGES_REQUESTED visible forever, blocking a gate that real GitHub would have
// cleared — and disagreeing with this package's own FetchPRReviewDecision, which
// reduces correctly. Two reads of one model reporting two verdicts is a bug the
// sim would be introducing.
func (s *Sim) FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	s.drainReviews(pr)
	return latestReviewsByAuthor(pr.reviews), nil
}

// latestReviewsByAuthor reduces a submission history to one entry per author,
// reproducing github.Client.FetchPRReviews's rule exactly:
//
//   - the most recent submission wins, and authors keep first-submission order;
//   - except that a COMMENTED follow-up never supersedes an author's existing
//     formal verdict (APPROVED, CHANGES_REQUESTED, DISMISSED). GitHub treats
//     COMMENTED as informational, not a state transition, so a reviewer who
//     requests changes and later comments still has an active
//     CHANGES_REQUESTED. Only when an author's *first* submission is COMMENTED
//     does it become their entry.
func latestReviewsByAuthor(reviews []gh.PRReview) []gh.PRReview {
	latest := make(map[string]gh.PRReview, len(reviews))
	order := make([]string, 0, len(reviews))
	for _, rev := range reviews {
		if rev.Author == "" {
			continue
		}
		if _, seen := latest[rev.Author]; !seen {
			order = append(order, rev.Author)
		} else if rev.State == "COMMENTED" {
			continue
		}
		latest[rev.Author] = rev
	}
	out := make([]gh.PRReview, 0, len(order))
	for _, author := range order {
		out = append(out, latest[author])
	}
	return out
}

// FetchPRReviewRequests returns the reviewers still outstanding on a PR.
//
// Note what this does *not* include: self-submitting review bots (Pruefer,
// Gemini, CodeRabbit, Copilot) never appear here on real GitHub, because they
// are never formally requested. That absence is load-bearing — it is why
// stages have to declare expected_reviewers (ADR-1283) — so the model reports
// only what was actually requested and never synthesises a bot entry.
func (s *Sim) FetchPRReviewRequests(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	s.drainReviews(pr)
	out := make([]gh.ReviewRequest, len(pr.reviewRequests))
	copy(out, pr.reviewRequests)
	return out, nil
}

// AddReviewRequest requests reviews from the given logins. Re-requesting an
// already-outstanding reviewer is a no-op.
func (s *Sim) AddReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	// Drain before mutating: a step already due must land before this
	// withdrawal or addition, not after it. See schedule.go's drainReviews.
	s.drainReviews(pr)
	changed := false
	for _, login := range reviewers {
		already := false
		for _, existing := range pr.reviewRequests {
			if existing.Login == login {
				already = true
				break
			}
		}
		if already {
			continue
		}
		pr.reviewRequests = append(pr.reviewRequests, gh.ReviewRequest{
			Login: login,
			IsBot: gh.IsBotLogin(login),
		})
		changed = true
	}
	// A true no-op must not bump the timestamp, following AddLabelToIssue's
	// convention: engine gates anchor on observable change, so a spurious bump
	// is a change a scenario cannot distinguish from a real one.
	if changed {
		pr.updatedAt = s.now()
	}
	return nil
}

// DeleteReviewRequest withdraws outstanding review requests.
func (s *Sim) DeleteReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return err
	}
	s.drainReviews(pr)
	kept := pr.reviewRequests[:0:0]
	for _, existing := range pr.reviewRequests {
		if !contains(reviewers, existing.Login) {
			kept = append(kept, existing)
		}
	}
	if len(kept) == len(pr.reviewRequests) {
		// Nothing was outstanding; same no-op rule as AddReviewRequest.
		return nil
	}
	pr.reviewRequests = kept
	pr.updatedAt = s.now()
	return nil
}

// FetchPRReviewDecision returns GitHub's branch-protection-derived review
// decision: "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED", or "" when the
// base branch defines no review requirement.
//
// The empty case is the important one. GraphQL's reviewDecision is null unless
// branch protection actually requires reviews, which is why the engine's
// authoritative review gate (ADR-1250) prefers reviewDecision where it exists
// and falls back to its own no-CHANGES_REQUESTED computation otherwise. A
// model that always returned a decision would hide that fallback entirely.
//
// Only each reviewer's latest review counts, matching GitHub's own rollup —
// via the same reduction FetchPRReviews uses, so the two cannot disagree.
func (s *Sim) FetchPRReviewDecision(owner, repo string, prNumber int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return "", err
	}
	s.drainReviews(pr)
	required, ok := r.requiredApprovals[pr.base]
	if !ok {
		// No review requirement configured on the base branch — GraphQL
		// reports null here.
		return "", nil
	}

	// Reduce first, then roll up — deliberately sharing latestReviewsByAuthor
	// with FetchPRReviews rather than reducing inline. Filtering the raw history
	// down to APPROVED/CHANGES_REQUESTED before collapsing would let a later
	// DISMISSED be skipped instead of superseding that author's earlier verdict,
	// leaving a dismissed approval counted in the tally. That is not a rounding
	// error: reviewGateAuthorityVerdict (engine/reviews.go) trusts an APPROVED
	// decision outright, so a scenario that dismisses an approval and expects
	// the landing gate to hold would see it clear. Sharing the reduction is what
	// stops this read and FetchPRReviews describing two different PRs.
	approvals := 0
	for _, rev := range latestReviewsByAuthor(pr.reviews) {
		// A collapsed entry that is COMMENTED or DISMISSED is that author's
		// current state and simply carries no verdict.
		if rev.State == "CHANGES_REQUESTED" {
			return "CHANGES_REQUESTED", nil
		}
		if rev.State == "APPROVED" {
			approvals++
		}
	}
	if approvals >= required {
		return "APPROVED", nil
	}
	return "REVIEW_REQUIRED", nil
}
