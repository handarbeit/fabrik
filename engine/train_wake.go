package engine

import (
	"encoding/json"
	"strings"
)

// Open-trial idle cap and trial-PR wake (#2542, ADR 2542).
//
// A merge-train trial's CI usually finishes in seconds, but the verdict is read
// only when a poll runs (settleTrainRuns, ADR 2051). Two things delayed that poll:
// the idle backoff (an open trial is neither activity nor a cooldown), and the
// #2483 heal guard, which drops every delivery for a fabrik/merge-train/ head so
// nothing woke the poll when the trial's checks completed.
//
//   - R1 (the guarantee): while any live run has an open trial, PollWithBackoff
//     caps the idle interval at one configured interval, after the cooldown caps.
//   - R2 (an acceleration): a completed check_run / check_suite, or any
//     pull_request event, for the open trial's PR requests an early poll. The
//     match is made here, in the engine, against the persisted run record; the
//     board cache and its #2483 guard are not touched, so the delivery can never
//     reach resolvePRLinkage.

// openTrialPresent reports whether any live merge-train run has an open trial.
// The run record is the authority (ADR 2051): a run between trials, or a
// finished one, does not count.
func (e *Engine) openTrialPresent() bool {
	for _, r := range e.trainRuns.liveRuns() {
		if r.finished.Load() {
			continue
		}
		if r.trial() != nil {
			return true
		}
	}
	return false
}

// trialEventTarget is what a wake needs from a delivery: where it points, never
// anything the board cache would act on.
type trialEventTarget struct {
	repo    string // owner/repo
	headSHA string
	prNum   int // 0 when the payload carries none (fork PR, check_suite)
}

// trialWakeTarget decodes the repo, head SHA and (when present) PR number of a
// pull_request, check_run or check_suite delivery. ok is false for any other
// event, an unreadable payload, or a check event whose action is not "completed".
func trialWakeTarget(eventType string, payload []byte) (t trialEventTarget, ok bool) {
	var p struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		CheckRun struct {
			HeadSHA string `json:"head_sha"`
			PRs     []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_run"`
		CheckSuite struct {
			HeadSHA string `json:"head_sha"`
			PRs     []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
	}
	switch eventType {
	case "pull_request", "check_run", "check_suite":
	default:
		return t, false
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return t, false
	}
	t.repo = p.Repository.FullName
	switch eventType {
	case "pull_request":
		t.headSHA = p.PullRequest.Head.SHA
		t.prNum = p.PullRequest.Number
	case "check_run":
		if p.Action != "completed" {
			return t, false
		}
		t.headSHA = p.CheckRun.HeadSHA
		if len(p.CheckRun.PRs) > 0 {
			t.prNum = p.CheckRun.PRs[0].Number
		}
	case "check_suite":
		if p.Action != "completed" {
			return t, false
		}
		t.headSHA = p.CheckSuite.HeadSHA
		if len(p.CheckSuite.PRs) > 0 {
			t.prNum = p.CheckSuite.PRs[0].Number
		}
	}
	if t.repo == "" || t.headSHA == "" {
		return t, false
	}
	return t, true
}

// matchesOpenTrial reports whether t names the open trial of any live run: same
// repo, same head SHA, and the same PR number when the payload carries one. A
// trial that was replaced has a new head, so events for its predecessor do not match.
func (e *Engine) matchesOpenTrial(t trialEventTarget) bool {
	for _, r := range e.trainRuns.liveRuns() {
		if r.finished.Load() {
			continue
		}
		tr := r.trial()
		if tr == nil || tr.HeadSHA == "" || !strings.EqualFold(r.repoKey(), t.repo) {
			continue
		}
		if tr.HeadSHA != t.headSHA {
			continue
		}
		if t.prNum != 0 && tr.PRNum != t.prNum {
			continue
		}
		return true
	}
	return false
}

// wakeForOpenTrial requests an early poll when a delivery concerns an open
// trial's PR. It reads the run table only (copies under their own locks), makes
// no GitHub call and never blocks; the send is the same non-blocking, one-slot
// wake every other source uses, so rate-limit gating (ADR 1716) and the minimum
// poll interval apply unchanged. It is not a Store change and never calls into
// the board cache.
func (e *Engine) wakeForOpenTrial(eventType string, payload []byte) {
	if e.trainRuns == nil || e.wakeCh == nil || e.trialWakeOff.Load() {
		return
	}
	t, ok := trialWakeTarget(eventType, payload)
	if !ok || !e.matchesOpenTrial(t) {
		return
	}
	select {
	case e.wakeCh <- struct{}{}:
	default:
	}
}
