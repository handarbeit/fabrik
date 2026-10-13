package github

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// WorkflowRun is the CI-infrastructure-relevant slice of a GitHub Actions
// workflow run (#2052).
//
// It exists to see a state check runs cannot show: a run that failed before
// creating any job (`startup_failure`) leaves a `completed` workflow run with
// zero jobs and therefore zero check runs on the commit — indistinguishable,
// from the check-run list alone, from "CI has not started yet".
type WorkflowRun struct {
	ID         int64
	Name       string
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "" until completed; may be "startup_failure"
	// JobCount is the run's number of jobs, read for completed runs only
	// (0 for a run that is not completed, and for a startup_failure run, whose
	// job list is never fetched because the conclusion already says it).
	JobCount  int
	CreatedAt time.Time
	HTMLURL   string
}

// FetchWorkflowRuns lists the workflow runs for a commit SHA via the REST API
// (GET /repos/{o}/{r}/actions/runs?head_sha=…) and, for each completed run
// that did not conclude `startup_failure`, reads its job count
// (GET /repos/{o}/{r}/actions/runs/{id}/jobs, total_count only).
//
// Needs the `actions` permission, which the engine's GitHub App must hold
// (required at startup since #2105): a runtime 403/404 (the permission was
// revoked after startup) comes back wrapped as ErrForbidden/ErrNotFound so the
// caller can degrade to its pre-#2052 behaviour. Paginated and
// total_count-verified like FetchCheckRuns, with the same fail-closed
// polarity — an incomplete list is an error, never a verdict. There is no
// cache: workflow-run state has no webhook-fed source.
func (c *Client) FetchWorkflowRuns(owner, repo, sha string) ([]WorkflowRun, error) {
	type rawRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		CreatedAt  string `json:"created_at"`
		HTMLURL    string `json:"html_url"`
	}
	var all []rawRun
	totalCount := 0
	for page := 1; page <= restMaxPages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs?head_sha=%s&per_page=%d&page=%d",
			c.baseURL, owner, repo, sha, restPageSize, page)
		var raw struct {
			TotalCount   int      `json:"total_count"`
			WorkflowRuns []rawRun `json:"workflow_runs"`
		}
		if err := c.restGetJSON(apiURL, &raw); err != nil {
			return nil, fmt.Errorf("fetching workflow runs for %s: %w", sha, err)
		}
		totalCount = raw.TotalCount
		all = append(all, raw.WorkflowRuns...)
		if len(raw.WorkflowRuns) < restPageSize {
			break
		}
		if page == restMaxPages {
			return nil, fmt.Errorf("fetching workflow runs for %s: exceeded %d pages without reaching the end — refusing to return a truncated result",
				sha, restMaxPages)
		}
	}
	if totalCount > 0 && len(all) != totalCount {
		return nil, fmt.Errorf("fetching workflow runs for %s: collected %d of %d reported workflow runs — refusing to return an incomplete set",
			sha, len(all), totalCount)
	}

	out := make([]WorkflowRun, 0, len(all))
	for _, r := range all {
		run := WorkflowRun{
			ID:         r.ID,
			Name:       r.Name,
			Status:     r.Status,
			Conclusion: r.Conclusion,
			HTMLURL:    r.HTMLURL,
		}
		if r.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
				run.CreatedAt = t
			}
		}
		if r.Status == "completed" && r.Conclusion != "startup_failure" {
			n, err := c.fetchWorkflowRunJobCount(owner, repo, r.ID)
			if err != nil {
				return nil, err
			}
			run.JobCount = n
		}
		out = append(out, run)
	}
	return out, nil
}

// fetchWorkflowRunJobCount reads only the total_count of a run's job list.
func (c *Client) fetchWorkflowRunJobCount(owner, repo string, runID int64) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=1", c.baseURL, owner, repo, runID)
	var raw struct {
		TotalCount int `json:"total_count"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return 0, fmt.Errorf("fetching jobs for workflow run %d: %w", runID, err)
	}
	return raw.TotalCount, nil
}

// RerunFailedJobs re-runs only the failed jobs of a finished workflow run
// (POST /repos/{o}/{r}/actions/runs/{id}/rerun-failed-jobs, #2052 R5). Needs
// `actions: write`. GitHub answers 403 when the run is not finished, cannot be
// re-run (e.g. a startup_failure run) or the credential lacks the permission;
// that and a 404 are returned wrapped (ErrForbidden / ErrNotFound) so the
// caller can fall back to treating the failure as real.
func (c *Client) RerunFailedJobs(owner, repo string, runID int64) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/rerun-failed-jobs", c.baseURL, owner, repo, runID)
	if err := c.restPost(apiURL, nil); err != nil {
		return fmt.Errorf("re-running failed jobs of workflow run %d: %w", runID, err)
	}
	return nil
}

var actionsJobURLRe = regexp.MustCompile(`/actions/runs/(\d+)/job/\d+`)

// ActionsRunIDFromDetailsURL extracts the workflow run ID from a GitHub
// Actions check run's details URL (…/actions/runs/<id>/job/<job>). ok is false
// for any other URL — a third-party check whose result has no re-runnable
// workflow run behind it.
func ActionsRunIDFromDetailsURL(url string) (id int64, ok bool) {
	m := actionsJobURLRe.FindStringSubmatch(url)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
