package simgh

import gh "github.com/handarbeit/fabrik/github"

// Check runs and classic commit statuses are two genuinely separate
// collections here, keyed by SHA, because production treats them separately: a
// required context can be posted through the classic Statuses API rather than
// as an Actions check run, and FetchCombinedStatus is Fabrik's only visibility
// into that case (ADR-933). Collapsing them into one collection would make
// that path untestable while still looking green.

// FetchCheckRuns returns the check runs recorded against a commit SHA.
// An unknown SHA yields an empty slice, not an error — GitHub reports zero
// check runs for a commit no CI has touched yet.
func (s *Sim) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	runs := r.checkRuns[sha]
	out := make([]gh.CheckRun, len(runs))
	copy(out, runs)
	return out, nil
}

// FetchCombinedStatus returns the classic commit statuses for a ref.
func (s *Sim) FetchCombinedStatus(owner, repo, ref string) ([]gh.CommitStatus, error) {
	s.mu.Lock()
	statuses, err := func() ([]gh.CommitStatus, error) {
		r, err := s.lookupRepo(owner, repo)
		if err != nil {
			return nil, err
		}
		if direct, ok := r.commitStatuses[ref]; ok {
			out := make([]gh.CommitStatus, len(direct))
			copy(out, direct)
			return out, nil
		}
		return nil, nil
	}()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if statuses != nil {
		return statuses, nil
	}

	// The ref may be a branch name rather than a SHA — the combined-status
	// endpoint accepts either. Resolve it and retry.
	sha, err := s.resolveRefSHA(owner, repo, ref)
	if err != nil || sha == "" {
		return []gh.CommitStatus{}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	out := make([]gh.CommitStatus, len(r.commitStatuses[sha]))
	copy(out, r.commitStatuses[sha])
	return out, nil
}

// FetchCommitsBehind reports how many commits base has that head does not —
// GitHub's behind_by from the compare endpoint, computed here with rev-list
// against the backing repository rather than declared.
func (s *Sim) FetchCommitsBehind(owner, repo, base, head string) (int, error) {
	r, err := s.repoByKey(repoKey(owner, repo))
	if err != nil {
		return 0, err
	}
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	return r.commitsBehind(base, head)
}

// resolveRefSHA resolves a branch name to a SHA, returning "" when it is not a
// branch. Takes gitMu and must not be called while holding mu.
func (s *Sim) resolveRefSHA(owner, repo, ref string) (string, error) {
	r, err := s.repoByKey(repoKey(owner, repo))
	if err != nil {
		return "", err
	}
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	return r.headSHA(ref), nil
}

// contextState is a check run or commit status reduced to the two things the
// mergeable-state derivation cares about: its name, and whether it is green,
// red, or still running.
type contextState struct {
	name string
	// verdict is one of "success", "failure", or "pending".
	verdict string
}

// checkVerdict maps a check run's status/conclusion pair onto a verdict.
// "neutral" and "skipped" are treated as success, matching GitHub's own
// treatment of them as non-blocking.
func checkVerdict(run gh.CheckRun) string {
	if run.Status != "completed" {
		return "pending"
	}
	switch run.Conclusion {
	case "success", "neutral", "skipped":
		return "success"
	case "":
		return "pending"
	default:
		return "failure"
	}
}

// statusVerdict maps a classic commit status state onto a verdict.
func statusVerdict(st gh.CommitStatus) string {
	switch st.State {
	case "success":
		return "success"
	case "pending", "":
		return "pending"
	default: // "failure", "error"
		return "failure"
	}
}

// contextsForSHA collects both collections for a SHA into one verdict list.
// Caller must hold mu.
func (r *repoState) contextsForSHA(sha string) []contextState {
	out := make([]contextState, 0, len(r.checkRuns[sha])+len(r.commitStatuses[sha]))
	for _, run := range r.checkRuns[sha] {
		out = append(out, contextState{name: run.Name, verdict: checkVerdict(run)})
	}
	for _, st := range r.commitStatuses[sha] {
		out = append(out, contextState{name: st.Context, verdict: statusVerdict(st)})
	}
	return out
}
