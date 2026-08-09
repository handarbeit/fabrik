package simgh

import (
	"fmt"
	"time"
)

// FetchLabels returns the labels currently applied to an issue.
func (s *Sim) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	return cloneStrings(iss.labels), nil
}

// AddLabelToIssue applies a label, stamping the application time from the
// injected clock. Idempotent: re-adding an already-applied label leaves the
// original applied-at intact, matching GitHub (which emits no new "labeled"
// event for a label already present). Three engine mechanisms anchor timeouts
// on that timestamp, so refreshing it here would silently reset their clocks.
func (s *Sim) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return err
	}
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return err
	}
	if contains(iss.labels, labelName) {
		return nil
	}
	iss.labels = append(iss.labels, labelName)
	iss.labelAppliedAt[labelName] = s.now()
	iss.updatedAt = s.now()
	r.labelVocab[labelName] = true
	return nil
}

// RemoveLabelFromIssue removes a label. Removing an absent label is a no-op
// rather than an error, matching how the engine's ensure/remove helpers use it.
func (s *Sim) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return err
	}
	updated, removed := removeString(iss.labels, labelName)
	if !removed {
		return nil
	}
	iss.labels = updated
	delete(iss.labelAppliedAt, labelName)
	iss.updatedAt = s.now()
	return nil
}

// FetchLabelAppliedAt returns when a label was applied to an issue.
//
// This is not incidental metadata: the review-gate timeout and bot re-prompt
// ladder, the CI settle scan, and the Done-archive scan all anchor their
// deadlines on it. Returning a zero time for an absent label mirrors
// production's "no such event found" outcome rather than erroring.
func (s *Sim) FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return time.Time{}, err
	}
	return iss.labelAppliedAt[labelName], nil
}

// SeedLabels ensures the repo's label vocabulary contains the static and
// per-stage labels the engine expects. The model records the vocabulary but
// does not enforce it on AddLabelToIssue — see FIDELITY.md.
func (s *Sim) SeedLabels(owner, repo string, stageNames []string, lockedUser string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return err
	}
	for _, stage := range stageNames {
		for _, suffix := range []string{"in_progress", "complete", "failed"} {
			r.labelVocab[fmt.Sprintf("stage:%s:%s", stage, suffix)] = true
		}
	}
	if lockedUser != "" {
		r.labelVocab["fabrik:locked:"+lockedUser] = true
	}
	return nil
}

// issueLocked resolves an issue. Caller must hold mu.
func (s *Sim) issueLocked(owner, repo string, issueNumber int) (*issueRecord, error) {
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	iss, ok := r.issues[issueNumber]
	if !ok {
		return nil, fmt.Errorf("simgh: issue %s#%d not found", repoKey(owner, repo), issueNumber)
	}
	return iss, nil
}

// prLocked resolves a pull request. Caller must hold mu.
func (s *Sim) prLocked(owner, repo string, prNumber int) (*repoState, *prRecord, error) {
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, nil, err
	}
	pr, ok := r.prs[prNumber]
	if !ok {
		return nil, nil, fmt.Errorf("simgh: PR %s#%d not found", repoKey(owner, repo), prNumber)
	}
	return r, pr, nil
}
