package simgh

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// A COMMENTED review supersedes the same author's stored COMMENTED review —
// only a stored formal verdict resists it. Before #1953 the sim skipped every
// later COMMENTED, hiding a bot's second body-only review (the #616 shape) from
// every read while production surfaced it.
func TestFetchPRReviewsCommentedSupersedesStoredCommented(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"}).
		SeedReview(repoName, 42, gh.PRReview{Author: "pruefer", State: "COMMENTED", Body: "review 1", DatabaseID: 1}).
		SeedReview(repoName, 42, gh.PRReview{Author: "pruefer", State: "COMMENTED", Body: "review 2", DatabaseID: 2}).
		SeedReview(repoName, 42, gh.PRReview{Author: "carol", State: "CHANGES_REQUESTED", Body: "no", DatabaseID: 3}).
		SeedReview(repoName, 42, gh.PRReview{Author: "carol", State: "COMMENTED", Body: "chatter", DatabaseID: 4})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	reviews, err := s.FetchPRReviews("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("got %d reviews, want 2: %+v", len(reviews), reviews)
	}
	if reviews[0].Author != "pruefer" || reviews[0].DatabaseID != 2 {
		t.Errorf("pruefer entry = %+v, want review 2 (later COMMENTED supersedes stored COMMENTED)", reviews[0])
	}
	if reviews[1].Author != "carol" || reviews[1].State != "CHANGES_REQUESTED" {
		t.Errorf("carol entry = %+v, want her CHANGES_REQUESTED preserved", reviews[1])
	}
}

func TestAddReviewReactionRecordsOnReview(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"}).
		SeedReview(repoName, 42, gh.PRReview{Author: "pruefer", State: "COMMENTED", Body: "x", DatabaseID: 9})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	reviews, _ := s.FetchPRReviews("acme", "widgets", 42)
	if len(reviews) != 1 || reviews[0].NodeID == "" {
		t.Fatalf("seeded review has no node ID: %+v", reviews)
	}
	if err := s.AddReviewReaction(reviews[0].NodeID, "eyes"); err != nil {
		t.Fatalf("AddReviewReaction: %v", err)
	}
	if err := s.AddReviewReaction(reviews[0].NodeID, "eyes"); err != nil {
		t.Fatalf("repeat AddReviewReaction: %v", err)
	}
	got := s.ReviewReactions(repoName, 42, 9)
	if got["eyes"] != 1 || len(got) != 1 {
		t.Errorf("reactions = %v, want exactly one eyes", got)
	}
	if err := s.AddReviewReaction("PRR_missing", "eyes"); err == nil {
		t.Error("unknown node ID: expected error")
	}
}

// Reviews seeded in one SeedReviewsAt batch must each get a distinct node ID:
// the IDs are assigned before any of them is appended to the PR, so deriving
// them from len(pr.reviews) alone gave the whole batch the same ID and made a
// reaction land on (and be reported for) the wrong review.
func TestSeedReviewsAtBatchGetsDistinctNodeIDs(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"}).
		SeedReviewsAt(repoName, 42, s.now().Add(-time.Minute),
			gh.PRReview{Author: "pruefer", State: "COMMENTED", Body: "a", DatabaseID: 1},
			gh.PRReview{Author: "gemini", State: "COMMENTED", Body: "b", DatabaseID: 2})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	reviews, err := s.FetchPRReviews("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("got %d reviews, want 2: %+v", len(reviews), reviews)
	}
	if reviews[0].NodeID == "" || reviews[0].NodeID == reviews[1].NodeID {
		t.Fatalf("batch node IDs not distinct: %q vs %q", reviews[0].NodeID, reviews[1].NodeID)
	}
	if err := s.AddReviewReaction(reviews[1].NodeID, "eyes"); err != nil {
		t.Fatalf("AddReviewReaction: %v", err)
	}
	if got := s.ReviewReactions(repoName, 42, 2); got["eyes"] != 1 {
		t.Errorf("reaction not recorded on the targeted review: %v", got)
	}
	if got := s.ReviewReactions(repoName, 42, 1); len(got) != 0 {
		t.Errorf("reaction leaked onto the other review in the batch: %v", got)
	}
}
