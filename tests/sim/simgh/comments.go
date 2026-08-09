package simgh

import "fmt"

// AddComment posts a comment on an issue, or on a pull request when the number
// identifies one. GitHub's issue-comment endpoints are shared between issues
// and PRs, and the engine relies on that (it posts stage output to a PR
// through the same call), so the model resolves the number against both
// collections rather than requiring the caller to say which it meant.
//
// Returns the comment's database ID, which the engine round-trips into
// reaction and update calls.
func (s *Sim) AddComment(owner, repo string, issueNumber int, body string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return 0, err
	}

	id := s.nextCommentDatabaseID
	s.nextCommentDatabaseID++
	now := s.now()
	c := &commentRecord{
		databaseID: id,
		author:     simActor,
		body:       body,
		createdAt:  now,
		reactions:  make(map[string]int),
	}

	if iss, ok := r.issues[issueNumber]; ok {
		iss.comments = append(iss.comments, c)
		iss.updatedAt = now
		return id, nil
	}
	if pr, ok := r.prs[issueNumber]; ok {
		c.fromPR = issueNumber
		pr.comments = append(pr.comments, c)
		pr.updatedAt = now
		return id, nil
	}
	return 0, fmt.Errorf("simgh: no issue or PR %s#%d to comment on", repoKey(owner, repo), issueNumber)
}

// UpdateComment rewrites an existing comment's body, searching issue, PR, and
// review-thread comments across the repo the way the REST endpoint does (it
// takes only a comment ID, not an issue number).
func (s *Sim) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, iss, pr, err := s.findCommentWithOwnerLocked(owner, repo, commentDatabaseID)
	if err != nil {
		return err
	}
	c.body = body
	// Editing a comment bumps the parent issue's updated_at on GitHub, and the
	// engine really does take this path — it rewrites an existing stage comment
	// rather than posting a new one (engine/comments.go, engine/dependencies.go).
	// Leaving it unbumped would make an edit invisible to any read that watches
	// updatedAt to notice the board changed.
	//
	// Reactions deliberately do not bump it: GitHub does not treat a reaction as
	// an update to the parent.
	switch {
	case iss != nil:
		iss.updatedAt = s.now()
	case pr != nil:
		pr.updatedAt = s.now()
	}
	return nil
}

// AddCommentReaction adds a reaction to an issue or PR comment. Reactions are
// counted, and the engine's durable "already processed" state (the 🚀 rocket)
// is exactly a reaction, so this must be observable on every subsequent read.
func (s *Sim) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	return s.addReaction(owner, repo, commentDatabaseID, content)
}

// AddPRReviewCommentReaction adds a reaction to an inline review-thread
// comment. GitHub routes these through a separate REST endpoint from ordinary
// comment reactions; the model keeps the distinction at the call site but
// stores both the same way.
func (s *Sim) AddPRReviewCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	return s.addReaction(owner, repo, commentDatabaseID, content)
}

func (s *Sim) addReaction(owner, repo string, commentDatabaseID int, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCommentLocked(owner, repo, commentDatabaseID)
	if err != nil {
		return err
	}
	// GitHub records one reaction per actor per content; the sim posts as a
	// single actor, so a repeated reaction is idempotent rather than additive.
	if c.reactions[content] == 0 {
		c.reactions[content] = 1
	}
	return nil
}

// ResolveReviewThread marks a review thread resolved and bumps the PR's
// resolved-thread count, which the engine's progress detection reads during
// turn extension.
func (s *Sim) ResolveReviewThread(threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.repos {
		for _, pr := range r.prs {
			for _, c := range pr.reviewThreadComment {
				if c.reviewThreadID != threadID {
					continue
				}
				if c.threadResolved {
					return nil
				}
				c.threadResolved = true
				pr.resolvedThreads++
				pr.updatedAt = s.now()
				return nil
			}
		}
	}
	return fmt.Errorf("simgh: review thread %q not found", threadID)
}

// findCommentLocked searches a repo's issue, PR, and review-thread comments.
// Caller must hold mu.
func (s *Sim) findCommentLocked(owner, repo string, databaseID int) (*commentRecord, error) {
	c, _, _, err := s.findCommentWithOwnerLocked(owner, repo, databaseID)
	return c, err
}

// findCommentWithOwnerLocked is findCommentLocked plus whichever record owns the
// comment, so a mutation can bump the parent's updatedAt. Exactly one of the
// issue and the PR is non-nil. Caller must hold mu.
func (s *Sim) findCommentWithOwnerLocked(owner, repo string, databaseID int) (*commentRecord, *issueRecord, *prRecord, error) {
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, iss := range r.issues {
		for _, c := range iss.comments {
			if c.databaseID == databaseID {
				return c, iss, nil, nil
			}
		}
	}
	for _, pr := range r.prs {
		for _, c := range pr.comments {
			if c.databaseID == databaseID {
				return c, nil, pr, nil
			}
		}
		for _, c := range pr.reviewThreadComment {
			if c.databaseID == databaseID {
				return c, nil, pr, nil
			}
		}
	}
	return nil, nil, nil, fmt.Errorf("simgh: comment %d not found in %s", databaseID, repoKey(owner, repo))
}
