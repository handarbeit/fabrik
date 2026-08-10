package simgh

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// FetchLatestRelease returns the repo's seeded latest release, or nil when
// none has been seeded — GitHub returns 404 for a repo with no releases, which
// production surfaces as a nil release rather than a hard failure.
func (s *Sim) FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	if r.latestRelease == nil {
		return nil, nil
	}
	rel := *r.latestRelease
	rel.Assets = append([]gh.ReleaseAsset(nil), r.latestRelease.Assets...)
	return &rel, nil
}

// FetchRepoAccess returns the repo's auto-merge and push permissions.
func (s *Sim) FetchRepoAccess(owner, repo string) (gh.RepoAccess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return gh.RepoAccess{}, err
	}
	return r.access, nil
}

// RepoBareDir returns the on-disk path of ownerRepo's ("owner/repo") backing
// bare git repository, or an error if it has not been seeded.
//
// This exists for tests/sim's harness wiring, not for anything Sim needs
// internally: the harness reproduces production's own git topology (a real
// bare-clone-of-a-bare-clone acting as the engine's "origin", exactly as
// ensureBareClone bare-clones the real GitHub remote) by running
// `git clone --bare` against this path. Exported rather than duplicating
// repoDir's path formula in the harness package, so the two can never drift
// apart.
func (s *Sim) RepoBareDir(ownerRepo string) (string, error) {
	owner, repo, err := splitOwnerRepo(ownerRepo)
	if err != nil {
		return "", fmt.Errorf("simgh: RepoBareDir: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookupRepo(owner, repo); err != nil {
		return "", err
	}
	return s.repoDir(owner, repo), nil
}

// RateLimitStats returns the REST and GraphQL budgets.
//
// Modelled as static: the budgets never deplete as calls are made, and no
// method ever fails with a rate-limit error. Exhaustion and its backoff
// behaviour are therefore not reachable in this layer — see FIDELITY.md. The
// Reset and UpdatedAt timestamps still come from the injected clock so a test
// controlling time sees coherent values.
func (s *Sim) RateLimitStats() (gh.RateLimitStats, gh.RateLimitStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	toStats := func(b rateBudget) gh.RateLimitStats {
		return gh.RateLimitStats{
			Limit:     b.limit,
			Remaining: b.remaining,
			Used:      b.used,
			Reset:     now.Add(b.resetIn),
			UpdatedAt: now,
		}
	}
	return toStats(s.restRate), toStats(s.graphqlRate)
}

// DeleteForwardingHooks removes webhook forwarding hooks from a repo.
//
// The model has no webhook subsystem, so there is never anything to delete and
// this always succeeds for a seeded repo. Recorded in FIDELITY.md as absent
// rather than modelled: a test cannot use this layer to prove hooks were
// cleaned up.
func (s *Sim) DeleteForwardingHooks(owner, repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookupRepo(owner, repo); err != nil {
		return fmt.Errorf("simgh: DeleteForwardingHooks: %w", err)
	}
	return nil
}
