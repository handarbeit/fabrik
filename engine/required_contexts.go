package engine

import (
	gh "github.com/handarbeit/fabrik/github"
)

// requiredContextsForRepo returns the configured required status/check-run
// context names for owner/repo (ADR-071), or nil if none are configured —
// nil means gh.ClassifyRequiredContexts is a no-op, preserving today's
// permissive behavior for the common vanilla-GHA case.
func (e *Engine) requiredContextsForRepo(owner, repo string) []string {
	if e.cfg.RequiredStatusContexts == nil {
		return nil
	}
	return e.cfg.RequiredStatusContexts[owner+"/"+repo]
}

// classifyRequiredContexts evaluates required-context readiness for headSHA.
// It fetches classic commit statuses only when at least one required context
// name isn't already resolvable from checkRuns alone — avoiding an
// unconditional extra API call on every settle pass for repos whose required
// contexts are all ordinary check runs.
func (e *Engine) classifyRequiredContexts(itemNumber int, owner, repo, headSHA string, checkRuns []gh.CheckRun) (status gh.RequiredContextStatus, missing, pending, failed []string) {
	required := e.requiredContextsForRepo(owner, repo)
	if len(required) == 0 {
		return gh.RequiredContextsSatisfied, nil, nil, nil
	}

	presentInCheckRuns := make(map[string]bool, len(checkRuns))
	for _, cr := range checkRuns {
		presentInCheckRuns[cr.Name] = true
	}
	needsStatusFetch := false
	for _, name := range required {
		if !presentInCheckRuns[name] {
			needsStatusFetch = true
			break
		}
	}

	var statuses []gh.CommitStatus
	if needsStatusFetch {
		var err error
		statuses, err = e.readClient.FetchCombinedStatus(owner, repo, headSHA)
		if err != nil {
			e.logf(itemNumber, "required-contexts", "could not fetch combined status for %s: %v — unresolved required contexts will read as pending\n",
				headSHA[:min(8, len(headSHA))], err)
		}
	}

	return gh.ClassifyRequiredContexts(required, checkRuns, statuses)
}
