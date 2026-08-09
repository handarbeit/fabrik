package simgh

import (
	gh "github.com/handarbeit/fabrik/github"
)

// FetchPRReviews returns the reviews submitted on a PR, in submission order.
func (s *Sim) FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	out := make([]gh.PRReview, len(pr.reviews))
	copy(out, pr.reviews)
	return out, nil
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
	}
	pr.updatedAt = s.now()
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
	kept := pr.reviewRequests[:0:0]
	for _, existing := range pr.reviewRequests {
		if !contains(reviewers, existing.Login) {
			kept = append(kept, existing)
		}
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
// Only each reviewer's latest review counts, matching GitHub's own rollup.
func (s *Sim) FetchPRReviewDecision(owner, repo string, prNumber int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, pr, err := s.prLocked(owner, repo, prNumber)
	if err != nil {
		return "", err
	}
	required, ok := r.requiredApprovals[pr.base]
	if !ok {
		// No review requirement configured on the base branch — GraphQL
		// reports null here.
		return "", nil
	}

	latest := make(map[string]string)
	var order []string
	for _, rev := range pr.reviews {
		if rev.State != "APPROVED" && rev.State != "CHANGES_REQUESTED" {
			// COMMENTED and DISMISSED reviews do not participate in the
			// decision rollup.
			continue
		}
		if _, seen := latest[rev.Author]; !seen {
			order = append(order, rev.Author)
		}
		latest[rev.Author] = rev.State
	}

	approvals := 0
	for _, author := range order {
		if latest[author] == "CHANGES_REQUESTED" {
			return "CHANGES_REQUESTED", nil
		}
		if latest[author] == "APPROVED" {
			approvals++
		}
	}
	if approvals >= required {
		return "APPROVED", nil
	}
	return "REVIEW_REQUIRED", nil
}
