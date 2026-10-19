package simgh

import (
	"fmt"
	"time"
)

// Write stamps (#1945, ADR 1945).
//
// Production's *github.Client reports, for each engine write it completed, the
// time GitHub stamped on it (a comment's created_at, an issue PATCH's updated_at,
// the Date header of a label write, a Status move's projectV2Item.updatedAt), and
// the engine anchors the probe staleness baseline to that time. The sim mirrors
// it: every successful write by one of the five engine self-write methods records
// the very instant it stamped on the model's updatedAt — taken under the same
// lock and from the same clock read, so no other actor's write can be mistaken
// for it. The stamp is exact; production's label/Date value is second-truncated,
// which the sim does not model (see FIDELITY.md).
//
// The recording lives in the Sim methods, so a test that stands in for an
// *external* actor must not call them: use the Seed* helpers (SeedComment,
// SeedLabelAdded, SeedLabelRemoved, SeedBodyEdit, SeedCommentEdit), which stamp
// updatedAt exactly like a human's write and record nothing.

func issueStampKey(owner, repo string, n int) string {
	return fmt.Sprintf("%s#%d", repoKey(owner, repo), n)
}

// stampIssueWriteLocked records an engine write to owner/repo#n at t. Caller
// holds s.mu.
func (s *Sim) stampIssueWriteLocked(owner, repo string, n int, t time.Time) {
	if s.issueWriteStamps == nil {
		s.issueWriteStamps = make(map[string]time.Time)
	}
	k := issueStampKey(owner, repo, n)
	if t.After(s.issueWriteStamps[k]) {
		s.issueWriteStamps[k] = t
	}
}

// stampItemWriteLocked records an engine Status write to a project item at t.
// Caller holds s.mu.
func (s *Sim) stampItemWriteLocked(itemID string, t time.Time) {
	if s.itemWriteStamps == nil {
		s.itemWriteStamps = make(map[string]time.Time)
	}
	if t.After(s.itemWriteStamps[itemID]) {
		s.itemWriteStamps[itemID] = t
	}
}

// IssueWriteStamp implements engine.WriteStampSource: GitHub's time for the
// latest engine write to repo ("owner/repo") #number, zero when none.
func (s *Sim) IssueWriteStamp(repo string, number int) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueWriteStamps[fmt.Sprintf("%s#%d", repo, number)]
}

// ProjectItemWriteStamp implements engine.WriteStampSource.
func (s *Sim) ProjectItemWriteStamp(itemID string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.itemWriteStamps[itemID]
}

// SeedLabelAdded models a human adding a label (fixture-only: stamps updatedAt
// from the sim clock, records no engine write stamp). Idempotent like GitHub.
func (s *Sim) SeedLabelAdded(ownerRepo string, issueNumber int, label string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := r.issues[issueNumber]
	if !ok {
		s.fail("simgh: issue %s#%d not found", ownerRepo, issueNumber)
		return s
	}
	if contains(iss.labels, label) {
		return s
	}
	now := s.now()
	iss.labels = append(iss.labels, label)
	iss.labelAppliedAt[label] = now
	iss.recordLabelEvent(LabelEventLabeled, label, now)
	iss.updatedAt = now
	r.labelVocab[label] = true
	return s
}

// SeedLabelRemoved models a human removing a label (fixture-only).
func (s *Sim) SeedLabelRemoved(ownerRepo string, issueNumber int, label string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := r.issues[issueNumber]
	if !ok {
		s.fail("simgh: issue %s#%d not found", ownerRepo, issueNumber)
		return s
	}
	updated, removed := removeString(iss.labels, label)
	if !removed {
		return s
	}
	now := s.now()
	iss.labels = updated
	delete(iss.labelAppliedAt, label)
	iss.recordLabelEvent(LabelEventUnlabeled, label, now)
	iss.updatedAt = now
	return s
}

// SeedBodyEdit models a human editing the issue body (fixture-only).
func (s *Sim) SeedBodyEdit(ownerRepo string, issueNumber int, body string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := r.issues[issueNumber]
	if !ok {
		s.fail("simgh: issue %s#%d not found", ownerRepo, issueNumber)
		return s
	}
	iss.body = body
	iss.updatedAt = s.now()
	return s
}

// SeedCommentEdit models a human editing an existing issue comment (fixture-only).
// The comment total does not change, so only updatedAt can reveal it.
func (s *Sim) SeedCommentEdit(ownerRepo string, issueNumber int, commentDatabaseID int, body string) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := r.issues[issueNumber]
	if !ok {
		s.fail("simgh: issue %s#%d not found", ownerRepo, issueNumber)
		return s
	}
	for _, c := range iss.comments {
		if c.databaseID == commentDatabaseID {
			c.body = body
			iss.updatedAt = s.now()
			return s
		}
	}
	s.fail("simgh: comment %d not found on %s#%d", commentDatabaseID, ownerRepo, issueNumber)
	return s
}
