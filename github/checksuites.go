package github

import (
	"fmt"
	"time"
)

// CheckSuite is the CI-gate-relevant slice of a GitHub check suite (#1822).
//
// A check suite is the roll-up GitHub keeps per (head SHA, app). Unlike a
// check run it exists from the moment a workflow is queued, so it is the only
// signal that says "more checks are coming" while a needs:-dependent job is
// still waiting for a runner and has not yet created its check run.
type CheckSuite struct {
	ID         int64
	AppSlug    string // the owning GitHub App's slug, e.g. "github-actions"
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "" until completed
	// LatestCheckRunsCount is how many check runs the suite currently has. A
	// suite with 0 runs is either about to register its first job (the
	// post-push window) or belongs to an installed-but-inert App that never
	// will — see OutstandingCheckSuites.
	LatestCheckRunsCount int
	// CreatedAt is GitHub's own creation timestamp. Zero when the response
	// omitted or malformed it.
	CreatedAt time.Time
}

// FetchCheckSuites retrieves the check suites for a commit SHA via the REST API
// (GET /repos/{o}/{r}/commits/{ref}/check-suites). It needs only the
// `checks: read` permission the engine already holds.
//
// Paginated and total_count-verified exactly like FetchCheckRuns (#1539), and
// with the same fail-closed polarity: a truncated suite list is as dangerous as
// a truncated check-run list, so an incomplete set is an error, never a
// verdict.
//
// There is deliberately no cache behind this: the check_suite webhook is a
// documented no-op in boardcache (coarse aggregates), so the only correct
// source is a live read.
func (c *Client) FetchCheckSuites(owner, repo, sha string) ([]CheckSuite, error) {
	type rawCheckSuite struct {
		ID                   int64  `json:"id"`
		Status               string `json:"status"`
		Conclusion           string `json:"conclusion"`
		LatestCheckRunsCount int    `json:"latest_check_runs_count"`
		CreatedAt            string `json:"created_at"`
		App                  struct {
			Slug string `json:"slug"`
		} `json:"app"`
	}
	var all []rawCheckSuite
	totalCount := 0
	for page := 1; page <= restMaxPages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-suites?per_page=%d&page=%d",
			c.baseURL, owner, repo, sha, restPageSize, page)
		var raw struct {
			TotalCount  int             `json:"total_count"`
			CheckSuites []rawCheckSuite `json:"check_suites"`
		}
		if err := c.restGetJSON(apiURL, &raw); err != nil {
			return nil, fmt.Errorf("fetching check suites for %s: %w", sha, err)
		}
		totalCount = raw.TotalCount
		all = append(all, raw.CheckSuites...)
		if len(raw.CheckSuites) < restPageSize {
			break
		}
		if page == restMaxPages {
			return nil, fmt.Errorf("fetching check suites for %s: exceeded %d pages without reaching the end — refusing to return a truncated result",
				sha, restMaxPages)
		}
	}
	// Same absent-vs-zero reasoning as FetchCheckRuns: only a real, positive
	// disagreement is an error.
	if totalCount > 0 && len(all) != totalCount {
		return nil, fmt.Errorf("fetching check suites for %s: collected %d of %d reported check suites — refusing to return an incomplete set",
			sha, len(all), totalCount)
	}
	out := make([]CheckSuite, len(all))
	for i, cs := range all {
		var created time.Time
		if cs.CreatedAt != "" {
			// A malformed timestamp leaves the zero value, which
			// OutstandingCheckSuites treats as "young" — the fail-safe (hold)
			// direction.
			if t, err := time.Parse(time.RFC3339, cs.CreatedAt); err == nil {
				created = t
			}
		}
		out[i] = CheckSuite{
			ID:                   cs.ID,
			AppSlug:              cs.App.Slug,
			Status:               cs.Status,
			Conclusion:           cs.Conclusion,
			LatestCheckRunsCount: cs.LatestCheckRunsCount,
			CreatedAt:            created,
		}
	}
	return out, nil
}

// OutstandingCheckSuites returns the suites that represent real workflow work
// not yet reflected in the check-run set: the reason a set of all-green check
// runs must not yet be read as a complete pass (#1822).
//
// A suite is outstanding when it is not `completed` AND any of:
//
//   - it has produced at least one check run (LatestCheckRunsCount > 0). This
//     is the incident case: a needs:-dependent job sat in the Actions queue for
//     minutes with no check run while the suite stayed in_progress. Or
//
//   - it has no check runs yet but is younger than dwell (measured from the
//     suite's own CreatedAt, so it survives an engine restart). This is the
//     post-push window in which a real suite exists before its first job
//     registers. Or
//
//   - it belongs to the github-actions App (#1829). GitHub creates exactly one
//     check suite per workflow run, so a non-completed github-actions suite
//     with zero runs always means a job has not yet been scheduled onto a
//     runner — never an idle installation — no matter how long that takes
//     (the incident this closes saw a 17-minute wait for the first runner,
//     well past any plausible fixed dwell). Age and run count are irrelevant
//     for this App slug: only Status == "completed" clears it.
//
// An old, non-completed suite with zero runs from any other App is inert — an
// installed App (cursor, claude, coderabbitai, github-pages …) whose suite
// sits `queued` forever — and is ignored. The naive rule "every suite must be
// completed" would deadlock on those permanently and is explicitly rejected.
//
// A zero CreatedAt on a zero-run suite counts as young (hold): an unreadable
// age is ambiguous, and ambiguity must hold. The caller's CI backstop bounds
// the wait in both cases.
func OutstandingCheckSuites(suites []CheckSuite, now time.Time, dwell time.Duration) []CheckSuite {
	var out []CheckSuite
	for _, s := range suites {
		if s.Status == "completed" {
			continue
		}
		if s.AppSlug == "github-actions" {
			out = append(out, s)
			continue
		}
		if s.LatestCheckRunsCount > 0 || s.CreatedAt.IsZero() || now.Sub(s.CreatedAt) < dwell {
			out = append(out, s)
		}
	}
	return out
}
