package simgh

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// simActor is the login the model attributes its own writes to — the
// equivalent of the account Fabrik's token authenticates as.
const simActor = "simgh-bot"

// GetIssueBody returns an issue's current body.
func (s *Sim) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return "", err
	}
	return iss.body, nil
}

// UpdateIssueBody rewrites an issue's body. The Specify stage's
// FABRIK_ISSUE_UPDATE round-trip depends on the new body being visible to
// every subsequent read.
func (s *Sim) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return err
	}
	iss.body = body
	iss.updatedAt = s.now()
	return nil
}

// CloseIssue closes an issue. Closing an already-closed issue is a no-op, as
// on GitHub.
func (s *Sim) CloseIssue(owner, repo string, issueNumber int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return err
	}
	if iss.state == "CLOSED" {
		return nil
	}
	iss.state = "CLOSED"
	iss.updatedAt = s.now()
	return nil
}

// CreateIssue creates an issue and returns its number and node ID. Used by the
// engine's child-spawn path, which then feeds the node ID to
// AddProjectV2ItemById and AddBlockedByIssue.
//
// assignees is stored and surfaces on ProjectItem.Assignees, matching
// production, which posts the field only when non-empty. The engine's spawn
// path assigns each child to the configured user, so a scenario that asserts
// on a spawned child's assignee reads a real value rather than nil.
func (s *Sim) CreateIssue(owner, repo, title, body string, assignees []string) (int, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return 0, "", err
	}
	num := r.allocNumber()
	now := s.now()
	iss := &issueRecord{
		number:         num,
		title:          title,
		body:           body,
		state:          "OPEN",
		author:         simActor,
		assignees:      cloneStrings(assignees),
		labelAppliedAt: make(map[string]time.Time),
		createdAt:      now,
		updatedAt:      now,
	}
	r.issues[num] = iss
	return num, iss.nodeID(repoKey(owner, repo)), nil
}

// FetchIssue returns an issue's scalar fields. Comments is a count, matching
// the REST shape production reads — not the full comment bodies, which reach
// the engine through ProjectItem instead.
func (s *Sim) FetchIssue(owner, repo string, issueNumber int) (*gh.IssueData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	// REST reports issue state lowercase ("open"/"closed"), unlike GraphQL's
	// uppercase enum which the model stores.
	state := "open"
	if iss.state == "CLOSED" {
		state = "closed"
	}
	return &gh.IssueData{
		Number:   iss.number,
		Title:    iss.title,
		State:    state,
		Labels:   cloneStrings(iss.labels),
		Comments: len(iss.comments),
	}, nil
}
