package simgh

import gh "github.com/handarbeit/fabrik/github"

func cloneMilestone(m *gh.Milestone) *gh.Milestone {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

// SeedIssueMilestone sets (m != nil) or clears (m == nil) an existing issue's
// milestone, as a human doing so in the GitHub UI would (#1967 R10). It bumps
// the issue's updatedAt like any other issue edit. Fixture-only: Fabrik itself
// never writes a milestone.
func (s *Sim) SeedIssueMilestone(ownerRepo string, issueNumber int, m *gh.Milestone) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, repo, err := splitOwnerRepo(ownerRepo)
	if err != nil {
		s.fail("simgh: SeedIssueMilestone: %v", err)
		return s
	}
	iss, err := s.issueLocked(owner, repo, issueNumber)
	if err != nil {
		s.fail("simgh: SeedIssueMilestone: %v", err)
		return s
	}
	iss.milestone = cloneMilestone(m)
	iss.updatedAt = s.now()
	return s
}
