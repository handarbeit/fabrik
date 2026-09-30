package simgh

import (
	"fmt"

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
//     *formal verdict* (APPROVED, CHANGES_REQUESTED, DISMISSED). GitHub treats
//     COMMENTED as informational, not a state transition, so a reviewer who
//     requests changes and later comments still has an active
//     CHANGES_REQUESTED. A stored COMMENTED entry, by contrast, IS superseded
//     by a newer COMMENTED — the newer body is the author's current review.
//     (Before #1953 the sim skipped any later COMMENTED, which hid a bot's
//     second body-only review — the #616 shape — from every read.)
func latestReviewsByAuthor(reviews []gh.PRReview) []gh.PRReview {
	latest := make(map[string]gh.PRReview, len(reviews))
	order := make([]string, 0, len(reviews))
	for _, rev := range reviews {
		if rev.Author == "" {
			continue
		}
		stored, seen := latest[rev.Author]
		if !seen {
			order = append(order, rev.Author)
		} else if rev.State == "COMMENTED" && isFormalVerdict(stored.State) {
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

// isFormalVerdict mirrors github.isFormalReviewVerdict (unexported there).
func isFormalVerdict(state string) bool {
	switch state {
	case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		return true
	}
	return false
}

// ensureReviewNodeID gives a review a GraphQL node ID when the scenario did not
// set one, mirroring production where every review carries one. Derived from
// the PR and the review's position so it is stable and unique per PR. offset is the
// number of reviews about to be appended ahead of this one (0 for a single
// review; the batch index for SeedReviewsAt, whose reviews are all appended
// only after every ID is assigned). Caller must hold s.mu.
func ensureReviewNodeID(pr *prRecord, rev *gh.PRReview, offset int) {
	if rev.NodeID != "" {
		return
	}
	rev.NodeID = fmt.Sprintf("PRR_sim_%d_%d", pr.number, len(pr.reviews)+offset+1)
}

func cloneReviewReactions(in map[string]map[string]int) map[string]map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]int, len(in))
	for id, m := range in {
		dup := make(map[string]int, len(m))
		for k, v := range m {
			dup[k] = v
		}
		out[id] = dup
	}
	return out
}

// AddReviewReaction reacts to a review by its GraphQL node ID (#1953 R8).
// Reactions are stored per review and are idempotent per content, like comment
// reactions. An unknown node ID is an error, as GraphQL would report NOT_FOUND.
func (s *Sim) AddReviewReaction(subjectNodeID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.repos {
		for _, pr := range r.prs {
			s.drainReviews(pr)
			for _, rev := range pr.reviews {
				if rev.NodeID != subjectNodeID {
					continue
				}
				if pr.reviewReactions == nil {
					pr.reviewReactions = make(map[string]map[string]int)
				}
				if pr.reviewReactions[subjectNodeID] == nil {
					pr.reviewReactions[subjectNodeID] = make(map[string]int)
				}
				pr.reviewReactions[subjectNodeID][content] = 1
				return nil
			}
		}
	}
	return fmt.Errorf("simgh: review node %q not found", subjectNodeID)
}

// ReviewReactions returns the reactions recorded on the review with the given
// database ID on ownerRepo#prNumber, keyed by content. It is the assertion
// accessor for R8 scenarios; nil when the review has none or does not exist.
func (s *Sim) ReviewReactions(ownerRepo string, prNumber, reviewDatabaseID int) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[ownerRepo]
	if !ok {
		return nil
	}
	pr, ok := r.prs[prNumber]
	if !ok {
		return nil
	}
	s.drainReviews(pr)
	for _, rev := range pr.reviews {
		if rev.DatabaseID == reviewDatabaseID {
			out := make(map[string]int)
			for k, v := range pr.reviewReactions[rev.NodeID] {
				out[k] = v
			}
			return out
		}
	}
	return nil
}
