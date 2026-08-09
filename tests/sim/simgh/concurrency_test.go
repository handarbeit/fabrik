package simgh

import (
	"fmt"
	"sync"
	"testing"
)

// The engine's poll loop dispatches up to MaxConcurrent workers against one
// GitHubClient, so the model has to survive that. Two hazards are being
// checked here, and they are different: the in-memory model needs a mutex, and
// git itself needs its subprocess calls serialised per repository — git's
// index and worktree administration are not concurrency-safe, so a Go-level
// lock over the model alone would not be enough.
//
// Operation counts are kept modest on purpose: creating and tearing down a
// throwaway worktree is by far the most expensive thing the model does, and an
// unbounded fan-out here would make the suite slow without testing anything
// extra.

func TestConcurrentModelAccess(t *testing.T) {
	s, _ := newSim(t)

	const (
		repoA     = "acme/widgets"
		repoB     = "acme/gadgets"
		issuesPer = 4
	)
	s.SeedRepo(repoA).
		SeedRepo(repoB).
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"})
	for i := 1; i <= issuesPer; i++ {
		s.SeedIssue(repoA, IssueSeed{Number: i, Title: fmt.Sprintf("A%d", i), Status: "Implement"}).
			SeedIssue(repoB, IssueSeed{Number: i, Title: fmt.Sprintf("B%d", i), Status: "Implement"})
	}
	// One PR per repo, on a real branch, so the goroutines below exercise the
	// git-backed paths concurrently and across two repos (whose gitMu locks are
	// independent).
	for _, repo := range []string{repoA, repoB} {
		s.SeedCommit(repo, "main", map[string]string{"base.txt": "base\n"}, "base").
			SeedCommit(repo, issueBranch(1), map[string]string{"feature.txt": "feature\n"}, "feature").
			SeedPR(repo, PRSeed{Number: 100, Head: issueBranch(1), Base: "main"})
	}
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	field, err := s.FetchStatusField(board.ProjectID)
	if err != nil {
		t.Fatalf("FetchStatusField: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Writers: label churn, comments and reactions, and body updates, spread
	// across both repos and every issue.
	for _, repo := range []string{repoA, repoB} {
		owner, name, err := splitOwnerRepo(repo)
		if err != nil {
			t.Fatalf("splitOwnerRepo: %v", err)
		}
		for i := 1; i <= issuesPer; i++ {
			wg.Add(1)
			go func(owner, name string, num int) {
				defer wg.Done()
				const label = "stage:Implement:in_progress"
				if err := s.AddLabelToIssue(owner, name, num, label); err != nil {
					errs <- fmt.Errorf("AddLabelToIssue: %w", err)
					return
				}
				id, err := s.AddComment(owner, name, num, "worker output")
				if err != nil {
					errs <- fmt.Errorf("AddComment: %w", err)
					return
				}
				if err := s.AddCommentReaction(owner, name, id, "ROCKET"); err != nil {
					errs <- fmt.Errorf("AddCommentReaction: %w", err)
					return
				}
				if err := s.UpdateIssueBody(owner, name, num, fmt.Sprintf("body %d", num)); err != nil {
					errs <- fmt.Errorf("UpdateIssueBody: %w", err)
					return
				}
				if err := s.RemoveLabelFromIssue(owner, name, num, label); err != nil {
					errs <- fmt.Errorf("RemoveLabelFromIssue: %w", err)
				}
			}(owner, name, i)
		}
	}

	// Board movers, contending on the same project state.
	for i := 1; i <= issuesPer; i++ {
		wg.Add(1)
		go func(num int) {
			defer wg.Done()
			itemID, _, err := s.LookupIssueProjectItem(board.ProjectID, repoA, num)
			if err != nil {
				errs <- fmt.Errorf("LookupIssueProjectItem: %w", err)
				return
			}
			if itemID == "" {
				errs <- fmt.Errorf("issue %d not found on the board", num)
				return
			}
			if err := s.UpdateProjectItemStatus(board.ProjectID, itemID, field.FieldID, field.Options["Review"]); err != nil {
				errs <- fmt.Errorf("UpdateProjectItemStatus: %w", err)
			}
		}(i)
	}

	// Readers, including full board projections that touch git for head SHAs.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.FetchProjectBoard("acme", "widgets", 2, "organization"); err != nil {
				errs <- fmt.Errorf("FetchProjectBoard: %w", err)
			}
			if _, _, err := s.ProbeProjectBoard("acme", "widgets", 2, "organization"); err != nil {
				errs <- fmt.Errorf("ProbeProjectBoard: %w", err)
			}
		}()
	}

	// Git-backed readers. These are the ones that would corrupt each other
	// without per-repo serialisation of the git subprocess calls: two
	// concurrent trial merges in the same repository share its object store
	// and worktree administration.
	for _, repo := range []string{repoA, repoB} {
		owner, name, err := splitOwnerRepo(repo)
		if err != nil {
			t.Fatalf("splitOwnerRepo: %v", err)
		}
		for range 2 {
			wg.Add(1)
			go func(owner, name string) {
				defer wg.Done()
				mergeable, err := s.FetchPRMergeable(owner, name, 100)
				if err != nil {
					errs <- fmt.Errorf("FetchPRMergeable: %w", err)
					return
				}
				if mergeable == nil || !*mergeable {
					errs <- fmt.Errorf("FetchPRMergeable = %v, want true", mergeable)
					return
				}
				if _, err := s.FetchCommitsBehind(owner, name, "main", issueBranch(1)); err != nil {
					errs <- fmt.Errorf("FetchCommitsBehind: %w", err)
				}
			}(owner, name)
		}
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Every write must have landed — a lost update under contention is the
	// failure this test exists to catch, and it would not show up as a race
	// report.
	for _, repo := range []string{repoA, repoB} {
		owner, name, err := splitOwnerRepo(repo)
		if err != nil {
			t.Fatalf("splitOwnerRepo: %v", err)
		}
		for i := 1; i <= issuesPer; i++ {
			body, err := s.GetIssueBody(owner, name, i)
			if err != nil {
				t.Fatalf("GetIssueBody(%s#%d): %v", repo, i, err)
			}
			if want := fmt.Sprintf("body %d", i); body != want {
				t.Errorf("%s#%d body = %q, want %q", repo, i, body, want)
			}
			issue, err := s.FetchIssue(owner, name, i)
			if err != nil {
				t.Fatalf("FetchIssue(%s#%d): %v", repo, i, err)
			}
			if issue.Comments != 1 {
				t.Errorf("%s#%d has %d comments, want 1", repo, i, issue.Comments)
			}
		}
	}
	statuses, err := s.FetchProjectItemStatusBatch(board.ProjectID)
	if err != nil {
		t.Fatalf("FetchProjectItemStatusBatch: %v", err)
	}
	moved := 0
	for _, status := range statuses {
		if status == "Review" {
			moved++
		}
	}
	if moved != issuesPer {
		t.Errorf("%d items moved to Review, want %d", moved, issuesPer)
	}
}

// TestConcurrentTrialMergesOnOneRepo hammers the single most git-hostile path:
// repeated trial merges against one repository from several goroutines. Each
// probe checks out a throwaway worktree; without per-repo serialisation these
// collide on the repo's worktree administration and produce either a git error
// or a wrong answer.
func TestConcurrentTrialMergesOnOneRepo(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo(repoName).
		SeedCommit(repoName, "main", map[string]string{"shared.txt": "original\n"}, "ancestor").
		SeedCommit(repoName, headBranch, map[string]string{"shared.txt": "feature\n"}, "feature edit").
		SeedCommit(repoName, "main", map[string]string{"shared.txt": "main\n"}, "main edit").
		SeedCommit(repoName, "clean-branch", map[string]string{"other.txt": "other\n"}, "unrelated").
		SeedPR(repoName, PRSeed{Number: 1, Head: headBranch, Base: "main"}).
		SeedPR(repoName, PRSeed{Number: 2, Head: "clean-branch", Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The conflicting PR must report false every time...
			conflicting, err := s.FetchPRMergeable("acme", "widgets", 1)
			if err != nil {
				errs <- fmt.Errorf("conflicting probe: %w", err)
				return
			}
			if conflicting == nil || *conflicting {
				errs <- fmt.Errorf("conflicting PR reported mergeable = %v, want false", conflicting)
			}
			// ...and the clean one true, concurrently, in the same repo.
			clean, err := s.FetchPRMergeable("acme", "widgets", 2)
			if err != nil {
				errs <- fmt.Errorf("clean probe: %w", err)
				return
			}
			if clean == nil || !*clean {
				errs <- fmt.Errorf("clean PR reported mergeable = %v, want true", clean)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestConcurrentReviewMutations covers the review surfaces, which the review
// gate reads and writes from several code paths in one poll pass.
func TestConcurrentReviewMutations(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo(repoName).
		SeedCommit(repoName, headBranch, map[string]string{"a.go": "package a\n"}, "work").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	reviewers := []string{"alice", "bob", "carol", "dependabot"}
	for _, login := range reviewers {
		wg.Add(1)
		go func(login string) {
			defer wg.Done()
			if err := s.AddReviewRequest("acme", "widgets", 42, []string{login}); err != nil {
				errs <- fmt.Errorf("AddReviewRequest(%s): %w", login, err)
			}
		}(login)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.FetchPRReviewRequests("acme", "widgets", 42); err != nil {
				errs <- fmt.Errorf("FetchPRReviewRequests: %w", err)
			}
			if _, err := s.FetchPRReviews("acme", "widgets", 42); err != nil {
				errs <- fmt.Errorf("FetchPRReviews: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	requests, err := s.FetchPRReviewRequests("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(requests) != len(reviewers) {
		t.Fatalf("%d review requests recorded, want %d — a concurrent add was lost", len(requests), len(reviewers))
	}
	for _, req := range requests {
		if req.Login == "dependabot" && !req.IsBot {
			t.Error("dependabot was not classified as a bot")
		}
	}
}
