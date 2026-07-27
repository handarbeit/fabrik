package github

// RequiredContextStatus classifies whether every configured required status
// context has been confirmed successful on a given head SHA.
type RequiredContextStatus int

const (
	// RequiredContextsSatisfied means every configured required context name
	// resolved to a confirmed success on the head SHA — or no required
	// contexts are configured for the repo at all (the zero value, so every
	// caller that never touches required-context config gets this for free).
	RequiredContextsSatisfied RequiredContextStatus = iota
	// RequiredContextsPending means at least one required context has not yet
	// reported a confirmed outcome: missing entirely, still queued/in_progress/
	// pending, or reported skipped/neutral. None of these are a regression —
	// they just haven't produced a confirmed pass yet.
	RequiredContextsPending
	// RequiredContextsFailed means at least one required context reported a
	// confirmed failure (a check run conclusion of failure/timed_out/
	// action_required, or a commit status state of failure/error).
	RequiredContextsFailed
)

// ClassifyRequiredContexts checks each name in required against the union of
// check-run names and commit-status contexts observed on the exact head SHA
// the caller fetched checkRuns/statuses for. Only a confirmed success (a
// check-run conclusion of "success", or a commit-status state of "success")
// counts as satisfied for that name — a skipped/neutral/absent/pending
// producer never does, even though ClassifyCheckRuns alone would treat
// skipped/neutral as "ready". This is what closes the local-CI-takeover hole:
// a required classic commit status that never posted for the new head must
// block, not silently pass.
//
// required with no configured entries always resolves to
// RequiredContextsSatisfied — this function is a no-op for repos that have
// not configured required_status_contexts, preserving today's permissive
// behavior for the common vanilla-GHA case.
func ClassifyRequiredContexts(required []string, checkRuns []CheckRun, statuses []CommitStatus) (status RequiredContextStatus, missing, pending, failed []string) {
	if len(required) == 0 {
		return RequiredContextsSatisfied, nil, nil, nil
	}

	crByName := make(map[string]CheckRun, len(checkRuns))
	for _, cr := range latestCheckRunsByName(checkRuns) {
		crByName[cr.Name] = cr
	}
	stByContext := make(map[string]CommitStatus, len(statuses))
	for _, st := range statuses {
		stByContext[st.Context] = st
	}

	for _, name := range required {
		if cr, ok := crByName[name]; ok {
			if cr.Status == "completed" {
				switch cr.Conclusion {
				case "success":
					continue
				case "failure", "timed_out", "action_required":
					failed = append(failed, name)
					continue
				}
			}
			pending = append(pending, name)
			continue
		}
		if st, ok := stByContext[name]; ok {
			switch st.State {
			case "success":
				continue
			case "failure", "error":
				failed = append(failed, name)
				continue
			}
			pending = append(pending, name)
			continue
		}
		missing = append(missing, name)
	}

	switch {
	case len(failed) > 0:
		status = RequiredContextsFailed
	case len(missing) > 0 || len(pending) > 0:
		status = RequiredContextsPending
	default:
		status = RequiredContextsSatisfied
	}
	return status, missing, pending, failed
}
