package simgh

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// This file models the slice of GitHub Actions the merge train's
// infrastructure-failure handling (#2052) reads and writes: workflow runs by
// head SHA, "reopening a PR fires a fresh pull_request run", and
// rerun-failed-jobs. See FIDELITY.md ("Workflow runs") for what it does not
// model.

// ReopenStep is what one reopen of a scripted PR produces: the next scheduled
// outcome of that PR's CI. Steps are consumed one per reopen, in order. A
// reopen with no step queued produces nothing — the model of a workflow whose
// `on.pull_request.types` omits `reopened`, or of a repo with no CI.
type ReopenStep struct {
	// SHA is the PR head the new runs attach to. Reopening does not change the
	// head, so scenarios pass the SHA the trial PR already carries.
	SHA string
	// Delay is how long after the reopen the outcome lands.
	Delay time.Duration
	// WorkflowRuns and CheckRuns are added Delay after the reopen. A
	// WorkflowRun with a zero ID is auto-assigned one. Leave both empty to
	// model a reopen that starts nothing.
	WorkflowRuns []gh.WorkflowRun
	CheckRuns    []gh.CheckRun
}

// ReadyForReviewStep is what marking a draft PR ready produces (#2297): marking ready fires
// pull_request: ready_for_review, which starts a new workflow run on the same head, and for
// a short window GitHub reports mergeable_state "blocked". Steps are consumed one per
// draft→ready transition, in order; a transition with no step queued produces nothing (a
// workflow whose `on.pull_request.types` omits ready_for_review, or a repo with no CI).
type ReadyForReviewStep struct {
	// BlockedReads is how many mergeable-field reads report "blocked" before the real
	// derived state shows. It models the not-yet-visible check and GitHub not yet counting
	// it; it is read-count driven, so it needs no clock advance. Note that MergePR's own
	// mergeability check is such a read too.
	BlockedReads int
	// SHA is the head the new check run attaches to; empty means the PR's last observed
	// head, and with none known no check run is added. Scenarios that script CheckName pass it.
	SHA string
	// CheckName, when non-empty, adds a pending check run of that name on SHA at once (give
	// it a name in the base's required contexts for the PR to read blocked while it is
	// pending). It completes with Conclusion Delay later, on the sim clock.
	CheckName  string
	Delay      time.Duration
	Conclusion string // default "success"; "skipped" models a workflow that does nothing
}

// RerunStep is what re-running a workflow run's failed jobs produces.
type RerunStep struct {
	// Delay is how long after the call the re-run finishes.
	Delay time.Duration
	// Conclusion is the run's conclusion once the re-run finishes ("success" if
	// empty).
	Conclusion string
	// SHA is the commit the new check runs attach to.
	SHA string
	// CheckRuns are added when the re-run finishes. Give them a new ID or none
	// (auto-assigned, so strictly higher), under the same names as the failed
	// ones: production keeps the highest ID per name, so these supersede.
	CheckRuns []gh.CheckRun
	// Jobs, when non-nil, replace the run's jobs (the new attempt's jobs and
	// steps, #2614) when the re-run finishes.
	Jobs []gh.WorkflowJob
}

func cloneWorkflowJobs(in []gh.WorkflowJob) []gh.WorkflowJob {
	out := make([]gh.WorkflowJob, len(in))
	for i, j := range in {
		out[i] = j
		out[i].Steps = append([]gh.WorkflowStep(nil), j.Steps...)
	}
	return out
}

// SeedWorkflowRunJobs records the jobs, with their steps, of a workflow run's
// latest attempt (#2614). A job with an empty Status is "completed". Reading a
// run with no seeded jobs yields an empty list, like a run whose jobs were
// never created.
func (s *Sim) SeedWorkflowRunJobs(ownerRepo string, runID int64, jobs ...gh.WorkflowJob) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := cloneWorkflowJobs(jobs)
	for i := range cp {
		if cp[i].Status == "" {
			cp[i].Status = "completed"
		}
	}
	r.workflowJobs[runID] = cp
	return s
}

// FetchWorkflowRunJobs returns the jobs, with steps, of a workflow run's latest
// attempt. An unknown run is gh.ErrNotFound, as on GitHub.
func (s *Sim) FetchWorkflowRunJobs(owner, repo string, runID int64) ([]gh.WorkflowJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	s.drainCI(r)
	known := false
	for _, runs := range r.workflowRuns {
		for _, w := range runs {
			if w.ID == runID {
				known = true
			}
		}
	}
	if !known {
		return nil, fmt.Errorf("simgh: workflow run %d: %w", runID, gh.ErrNotFound)
	}
	return cloneWorkflowJobs(r.workflowJobs[runID]), nil
}

func (s *Sim) reserveWorkflowRun(r *repoState, run gh.WorkflowRun, defaultCreated time.Time) gh.WorkflowRun {
	if run.ID == 0 {
		run.ID = r.nextWorkflowRunID
		r.nextWorkflowRunID++
	} else if run.ID >= r.nextWorkflowRunID {
		r.nextWorkflowRunID = run.ID + 1
	}
	if run.Status == "" {
		if run.Conclusion == "" {
			run.Status = "in_progress"
		} else {
			run.Status = "completed"
		}
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = defaultCreated
	}
	return run
}

func upsertWorkflowRun(list []gh.WorkflowRun, run gh.WorkflowRun) []gh.WorkflowRun {
	for i := range list {
		if list[i].ID == run.ID {
			list[i] = run
			return list
		}
	}
	return append(list, run)
}

// SeedWorkflowRun attaches a workflow run to a commit SHA now. A zero ID is
// auto-assigned; an empty Status is "in_progress" (or "completed" when a
// Conclusion is given). A completed run with Conclusion "startup_failure" or a
// JobCount of 0 is the state #2052 detects.
func (s *Sim) SeedWorkflowRun(ownerRepo, sha string, run gh.WorkflowRun) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.workflowRuns[sha] = upsertWorkflowRun(r.workflowRuns[sha], s.reserveWorkflowRun(r, run, s.now()))
	return s
}

// SeedReopenSteps queues, for a PR, the outcomes its successive reopens
// produce. See ReopenStep.
func (s *Sim) SeedReopenSteps(ownerRepo string, prNumber int, steps ...ReopenStep) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.reopenScripts[prNumber] = append(r.reopenScripts[prNumber], steps...)
	return s
}

// SeedRerunStep scripts what rerun-failed-jobs does for one workflow run.
func (s *Sim) SeedRerunStep(ownerRepo string, runID int64, step RerunStep) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.rerunScripts[runID] = step
	return s
}

// FetchWorkflowRuns returns the workflow runs recorded against a commit SHA.
// An unknown SHA yields an empty slice. Drained on the CI schedule, so a
// scenario places a run on the same clock as the check runs.
func (s *Sim) FetchWorkflowRuns(owner, repo, sha string) ([]gh.WorkflowRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	s.drainCI(r)
	runs := r.workflowRuns[sha]
	out := make([]gh.WorkflowRun, len(runs))
	copy(out, runs)
	return out, nil
}

// RerunFailedJobs re-runs the failed jobs of a finished workflow run. Like
// GitHub it refuses (gh.ErrForbidden) a run that is not finished or that never
// had a job (a startup failure), and reports gh.ErrNotFound for an unknown
// run. An accepted run goes in_progress and, if a RerunStep is scripted,
// finishes Delay later with the scripted check runs; with none it stays
// in_progress.
func (s *Sim) RerunFailedJobs(owner, repo string, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupRepo(owner, repo)
	if err != nil {
		return err
	}
	s.drainCI(r)
	var sha string
	var run gh.WorkflowRun
	found := false
	for k, runs := range r.workflowRuns {
		for _, w := range runs {
			if w.ID == runID {
				sha, run, found = k, w, true
			}
		}
	}
	if !found {
		return fmt.Errorf("simgh: workflow run %d: %w", runID, gh.ErrNotFound)
	}
	if run.Status != "completed" {
		return fmt.Errorf("simgh: workflow run %d is not finished: %w", runID, gh.ErrForbidden)
	}
	if run.Conclusion == "startup_failure" || run.JobCount == 0 {
		return fmt.Errorf("simgh: workflow run %d cannot be retried: %w", runID, gh.ErrForbidden)
	}
	step, scripted := r.rerunScripts[runID]
	delete(r.rerunScripts, runID)
	run.Status = "in_progress"
	run.Conclusion = ""
	r.workflowRuns[sha] = upsertWorkflowRun(r.workflowRuns[sha], run)
	if !scripted {
		return nil
	}
	conclusion := step.Conclusion
	if conclusion == "" {
		conclusion = "success"
	}
	checks := make([]gh.CheckRun, 0, len(step.CheckRuns))
	for _, cr := range step.CheckRuns {
		checks = append(checks, s.reserveCheckRunID(cr))
	}
	stepSHA := step.SHA
	if stepSHA == "" {
		stepSHA = sha
	}
	r.ciSchedule.add(s.now().Add(step.Delay), func(rs *repoState) {
		done := run
		done.Status, done.Conclusion = "completed", conclusion
		rs.workflowRuns[sha] = upsertWorkflowRun(rs.workflowRuns[sha], done)
		for _, cr := range checks {
			rs.checkRuns[stepSHA] = upsertCheckRun(rs.checkRuns[stepSHA], cr)
		}
		if step.Jobs != nil {
			rs.workflowJobs[runID] = cloneWorkflowJobs(step.Jobs)
		}
	})
	return nil
}

// applyReopenStep consumes the next scripted outcome for a reopened PR.
// Caller must hold mu.
func (s *Sim) applyReopenStep(r *repoState, prNumber int) {
	queue := r.reopenScripts[prNumber]
	if len(queue) == 0 {
		return
	}
	step := queue[0]
	r.reopenScripts[prNumber] = queue[1:]
	at := s.now().Add(step.Delay)
	runs := make([]gh.WorkflowRun, 0, len(step.WorkflowRuns))
	for _, w := range step.WorkflowRuns {
		runs = append(runs, s.reserveWorkflowRun(r, w, at))
	}
	checks := make([]gh.CheckRun, 0, len(step.CheckRuns))
	for _, cr := range step.CheckRuns {
		checks = append(checks, s.reserveCheckRunID(cr))
	}
	if len(runs) == 0 && len(checks) == 0 {
		return
	}
	sha := step.SHA
	r.ciSchedule.add(at, func(rs *repoState) {
		for _, w := range runs {
			rs.workflowRuns[sha] = upsertWorkflowRun(rs.workflowRuns[sha], w)
		}
		for _, cr := range checks {
			rs.checkRuns[sha] = upsertCheckRun(rs.checkRuns[sha], cr)
		}
	})
}

// SeedReadyForReviewSteps queues, for a PR, the outcomes its successive draft→ready
// transitions produce. See ReadyForReviewStep.
func (s *Sim) SeedReadyForReviewSteps(ownerRepo string, prNumber int, steps ...ReadyForReviewStep) *Sim {
	r, ok := s.repoForSeed(ownerRepo)
	if !ok {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.readyScripts[prNumber] = append(r.readyScripts[prNumber], steps...)
	return s
}

// applyReadyForReviewStep consumes the next scripted ready_for_review outcome for pr.
// Caller must hold mu.
func (s *Sim) applyReadyForReviewStep(r *repoState, pr *prRecord) {
	queue := r.readyScripts[pr.number]
	if len(queue) == 0 {
		return
	}
	step := queue[0]
	r.readyScripts[pr.number] = queue[1:]
	pr.readyBlockedReads = step.BlockedReads
	if step.CheckName == "" {
		return
	}
	sha := step.SHA
	if sha == "" {
		sha = pr.lastHeadSHA
	}
	if sha == "" {
		return // no head known without a git read, which mu forbids here; scenarios pass SHA
	}
	conclusion := step.Conclusion
	if conclusion == "" {
		conclusion = "success"
	}
	pending := s.reserveCheckRunID(gh.CheckRun{Name: step.CheckName, Status: "in_progress"})
	r.checkRuns[sha] = upsertCheckRun(r.checkRuns[sha], pending)
	done := pending
	done.Status, done.Conclusion = "completed", conclusion
	r.ciSchedule.add(s.now().Add(step.Delay), func(rs *repoState) {
		rs.checkRuns[sha] = upsertCheckRun(rs.checkRuns[sha], done)
	})
}
