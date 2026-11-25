package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Fail-fast-cancelled runs with a failed step are red (#2614, ADR 2614;
// amends ADR 2296).
//
// Some repositories cancel the whole workflow run from a final step once a job
// fails, so GitHub reports the check runs — often including the job that
// really failed — as `cancelled`. ADR 2296 reads a cancelled run as "not a
// pass, and not the members' fault", which for such a run is wrong: the real
// failure is only visible in the run's job steps.
//
// promoteFailFast sits between "read the check runs" and "classify them". For
// each completed, cancelled check run that has an Actions run behind it, it
// reads the run's jobs; if any step of any job concluded `failure`, the check
// run is returned as a copy with Conclusion "failure" (ObservedConclusion keeps
// the real "cancelled", FailedJob/FailedStep name the culprit). Every existing
// consumer then sees an ordinary failure, so the train, the catch-up wait, the
// landing/admission paths, the stage gate and ci-status.md cannot disagree.
//
// Fail safe (R5): a read that fails, a client without the capability, a check
// run with no Actions run behind it and a run with no failed step all leave
// the check run exactly as GitHub reported it.

// workflowJobFetcher is the optional capability of a GitHubClient that can read
// a workflow run's jobs with their steps. Asserted for rather than added to
// GitHubClient (like checkRunAnnotationFetcher) so the interface and every
// mock stay unchanged; a client without it yields "unreadable".
type workflowJobFetcher interface {
	FetchWorkflowRunJobs(owner, repo string, runID int64) ([]gh.WorkflowJob, error)
}

const (
	// failFastMemoMax bounds the per-check-run result memo.
	failFastMemoMax = 512
	// failFastLogTTL is how long a head's "jobs read failed" log line is
	// remembered, so the line is once per head, not once per evaluation.
	failFastLogTTL = 24 * time.Hour
)

// failFastResult is a memoised, definitive answer for one cancelled check run.
type failFastResult struct {
	failed bool
	job    string
	step   string
}

// failFastState is the engine's memo and once-per-head log set.
type failFastState struct {
	mu     sync.Mutex
	memo   map[int64]failFastResult // keyed by check-run ID: a re-run gets a new ID
	order  []int64                  // insertion order for the size bound
	logged map[string]time.Time     // owner/repo@sha -> when the read failure was logged
}

func (s *failFastState) get(id int64) (failFastResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.memo[id]
	return r, ok
}

func (s *failFastState) put(id int64, r failFastResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memo == nil {
		s.memo = make(map[int64]failFastResult)
	}
	if _, ok := s.memo[id]; !ok {
		s.order = append(s.order, id)
		for len(s.order) > failFastMemoMax {
			delete(s.memo, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.memo[id] = r
}

// firstFailureLog reports whether this is the first read failure logged for the
// head within the TTL, recording it.
func (s *failFastState) firstFailureLog(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logged == nil {
		s.logged = make(map[string]time.Time)
	}
	if at, ok := s.logged[key]; ok && now.Sub(at) < failFastLogTTL {
		return false
	}
	for k, at := range s.logged {
		if now.Sub(at) >= failFastLogTTL {
			delete(s.logged, k)
		}
	}
	s.logged[key] = now
	return true
}

// failedStepOf is the pure attribution core: the job and step that concluded
// `failure` in a run's jobs. The check run's own job is preferred (job ID equal
// to the check-run ID, then equal name); otherwise the run's first failed job
// is named. ok is false when no step of any job failed.
func failedStepOf(jobs []gh.WorkflowJob, cr gh.CheckRun) (job, step string, ok bool) {
	failedStep := func(j gh.WorkflowJob) (string, bool) {
		for _, s := range j.Steps {
			if s.Conclusion == "failure" {
				return s.Name, true
			}
		}
		return "", false
	}
	for _, j := range jobs {
		if j.ID == cr.ID && cr.ID != 0 {
			if s, ok := failedStep(j); ok {
				return j.Name, s, true
			}
		}
	}
	for _, j := range jobs {
		if j.Name == cr.Name {
			if s, ok := failedStep(j); ok {
				return j.Name, s, true
			}
		}
	}
	for _, j := range jobs {
		if s, ok := failedStep(j); ok {
			return j.Name, s, true
		}
	}
	return "", "", false
}

// allJobsCompleted reports whether every job of the run has finished, i.e. a
// "no failed step" answer is final and may be memoised. An empty list shows
// nothing about the run, so it is never final.
func allJobsCompleted(jobs []gh.WorkflowJob) bool {
	if len(jobs) == 0 {
		return false
	}
	for _, j := range jobs {
		if j.Status != "completed" {
			return false
		}
	}
	return true
}

// promoteFailFast returns runs with every fail-fast-cancelled run promoted to
// a failure (see the package comment). The input slice is never mutated; when
// nothing is promoted it is returned as is.
func (e *Engine) promoteFailFast(owner, repo, sha string, runs []gh.CheckRun) []gh.CheckRun {
	if e.failFastOff.Load() || len(runs) == 0 {
		return runs
	}
	// Only the latest run per name is classified; a superseded cancelled run
	// must not cost a read or be promoted.
	latest := make(map[string]int64, len(runs))
	for _, cr := range runs {
		if cr.ID > latest[cr.Name] {
			latest[cr.Name] = cr.ID
		}
	}
	var candidates []int
	for i, cr := range runs {
		if cr.Status != "completed" || cr.Conclusion != "cancelled" || cr.ID != latest[cr.Name] {
			continue
		}
		if _, ok := gh.ActionsRunIDFromDetailsURL(cr.DetailsURL); !ok {
			continue
		}
		candidates = append(candidates, i)
	}
	if len(candidates) == 0 {
		return runs
	}

	fetcher, _ := e.client.(workflowJobFetcher)
	repoKey := owner + "/" + repo
	jobsByRun := make(map[int64][]gh.WorkflowJob)
	readFailed := make(map[int64]bool)
	var out []gh.CheckRun
	for _, i := range candidates {
		cr := runs[i]
		res, ok := e.failFast.get(cr.ID)
		if !ok {
			runID, _ := gh.ActionsRunIDFromDetailsURL(cr.DetailsURL)
			if fetcher == nil || readFailed[runID] {
				continue
			}
			jobs, have := jobsByRun[runID]
			if !have {
				var err error
				jobs, err = fetcher.FetchWorkflowRunJobs(owner, repo, runID)
				if err != nil {
					readFailed[runID] = true
					if e.failFast.firstFailureLog(repoKey+"@"+sha, e.now()) {
						e.logfRepo(repoKey, "ci", "warn: could not read the jobs of workflow run %d for %s (%v) — a cancelled check run keeps its non-passing classification\n",
							runID, shortSHA(sha), err)
					}
					continue
				}
				jobsByRun[runID] = jobs
			}
			job, step, failed := failedStepOf(jobs, cr)
			res = failFastResult{failed: failed, job: job, step: step}
			if failed || allJobsCompleted(jobs) {
				e.failFast.put(cr.ID, res)
			}
		}
		if !res.failed {
			continue
		}
		if out == nil {
			out = make([]gh.CheckRun, len(runs))
			copy(out, runs)
		}
		p := cr
		p.ObservedConclusion = cr.Conclusion
		p.Conclusion = "failure"
		p.FailedJob = res.job
		p.FailedStep = res.step
		out[i] = p
	}
	if out == nil {
		return runs
	}
	return out
}

// failedStepText renders the "step X of job Y failed" text of a promoted run,
// "" for a run that was not promoted.
func failedStepText(cr gh.CheckRun) string {
	if cr.ObservedConclusion == "" || cr.FailedStep == "" {
		return ""
	}
	return fmt.Sprintf("step %q of job %q failed; the run was then cancelled (fail-fast)",
		strings.TrimSpace(cr.FailedStep), strings.TrimSpace(cr.FailedJob))
}
