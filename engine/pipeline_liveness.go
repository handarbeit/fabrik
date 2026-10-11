package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
)

// pipelineOverdueFactor is K (#2124): the poll loop is overdue once a poll has
// been in progress for longer than K × the configured poll interval, or once no
// poll has started within K × that interval of the time the engine itself said
// the next poll was due. It is a single documented constant, not an operator
// setting. The merge-train partition stall threshold reuses it.
const pipelineOverdueFactor = 2

// Poll overdue causes, reported in health and on the pipeline-stalled event.
const (
	pollCauseInProgress = "poll-in-progress"
	pollCauseNoPoll     = "no-poll-started"
)

// Backoff reasons published with the next-poll-due time.
const (
	backoffReasonIdle    = "idle"
	backoffReasonGraphQL = "graphql"
	backoffReasonREST    = "rest"
)

// pollVerdictInput is everything pollVerdict needs. Zero times mean unknown.
type pollVerdictInput struct {
	Now time.Time
	// Interval is the configured poll interval; zero means unknown.
	Interval time.Duration
	// DaemonStartedAt is the baseline before any poll has run.
	DaemonStartedAt time.Time
	InProgress      bool
	PollStartedAt   time.Time
	// NextDueAt is when the engine itself said the next poll was due (it
	// includes idle, GraphQL and REST back-off); zero before the first poll.
	NextDueAt time.Time
}

// pollVerdict is the overdue verdict.
type pollVerdict struct {
	// Known is false when the verdict cannot be computed (unknown interval or
	// baseline). An unknown verdict is never reported as healthy or overdue.
	Known        bool
	Overdue      bool
	Cause        string
	Blocked      time.Duration
	OverdueAfter time.Duration
}

// computePollVerdict computes the overdue verdict. It is a pure function.
func computePollVerdict(in pollVerdictInput) pollVerdict {
	if in.Interval <= 0 || in.Now.IsZero() {
		return pollVerdict{}
	}
	after := pipelineOverdueFactor * in.Interval
	v := pollVerdict{OverdueAfter: after}
	if in.InProgress {
		if in.PollStartedAt.IsZero() {
			return pollVerdict{}
		}
		v.Known = true
		v.Blocked = in.Now.Sub(in.PollStartedAt)
		if v.Blocked > after {
			v.Overdue, v.Cause = true, pollCauseInProgress
		}
		return v
	}
	baseline := in.NextDueAt
	if baseline.IsZero() {
		baseline = in.DaemonStartedAt
	}
	if baseline.IsZero() {
		return pollVerdict{}
	}
	v.Known = true
	if d := in.Now.Sub(baseline); d > 0 {
		v.Blocked = d
	}
	if in.Now.Sub(baseline) > after {
		v.Overdue, v.Cause = true, pollCauseNoPoll
	}
	return v
}

// pollVerdictFor computes the verdict from a health snapshot.
func (e *Engine) pollVerdictFor(h healthSnapshot, now time.Time) pollVerdict {
	return computePollVerdict(pollVerdictInput{
		Now:             now,
		Interval:        e.pollInterval(),
		DaemonStartedAt: h.StartedAt,
		InProgress:      h.PollInProgress,
		PollStartedAt:   h.PollStartedAt,
		NextDueAt:       h.NextPollDueAt,
	})
}

// pollInterval is the configured poll interval; zero when unset (test engines).
func (e *Engine) pollInterval() time.Duration {
	return time.Duration(e.cfg.PollSeconds) * time.Second
}

// pollStallDescription renders a poll stall for event content.
func pollStallDescription(v pollVerdict, step string) string {
	s := fmt.Sprintf("the poll loop is overdue (%s, blocked for %d s, overdue after %d s)",
		v.Cause, int64(v.Blocked.Seconds()), int64(v.OverdueAfter.Seconds()))
	if step != "" {
		s += ", in step " + step
	}
	return s
}

// setPollStep records the phase the in-progress poll is in (#2124). It is a
// name only and is published through the health mirror; "" means unknown.
func (e *Engine) setPollStep(step string) { e.health.setPollStep(step) }

// Idle reasons for a merge-train partition that has queued members and no
// active run (#2124). Anything not recognised is "unknown": duplicating the
// dispatch gating logic in the read path would be the worse option.
const (
	idleReasonPollOverdue = "poll-overdue"
	idleReasonInfra       = "infra-abandon-cooldown"
	idleReasonUnknown     = "unknown"
)

// partitionLivenessInput is everything computePartitionLiveness needs.
type partitionLivenessInput struct {
	Now time.Time
	// Threshold is K × the configured interval; zero disables stall detection.
	Threshold    time.Duration
	Members      int
	OldestQueued time.Time
	// RunActive is an open durable run or an in-flight worker.
	RunActive bool
	Poll      pollVerdict
	// PollDeliberatelyPending is true when the engine itself set a later poll
	// (back-off) that has not come due yet.
	PollDeliberatelyPending bool
	InfraHeld               bool
	InfraRemaining          time.Duration
}

// partitionLiveness is the liveness verdict for one partition.
type partitionLiveness struct {
	IdleReason string
	IdleDetail string
	// Stalled: members, no active run, no intentional hold, and queued for
	// longer than the threshold.
	Stalled bool
	// Cause is the stall cause carried on the partition's pipeline-stalled event.
	Cause string
}

// computePartitionLiveness is a pure function. Health and the channel deriver
// both call it so they cannot disagree.
func computePartitionLiveness(in partitionLivenessInput) partitionLiveness {
	var out partitionLiveness
	if in.Members == 0 || in.RunActive {
		return out
	}
	switch {
	case in.Poll.Overdue:
		out.IdleReason = idleReasonPollOverdue
		out.IdleDetail = fmt.Sprintf("%s, blocked for %d s", in.Poll.Cause, int64(in.Poll.Blocked.Seconds()))
	case in.InfraHeld:
		out.IdleReason = idleReasonInfra
		out.IdleDetail = fmt.Sprintf("%d s of the post-abandon cooldown remain", int64(in.InfraRemaining.Seconds()))
	default:
		out.IdleReason = idleReasonUnknown
	}
	// The infra-abandon cooldown is an intentional hold, and a deliberate poll
	// back-off is not a reason to call the train stalled.
	if in.InfraHeld || in.PollDeliberatelyPending || in.Threshold <= 0 || in.OldestQueued.IsZero() {
		return out
	}
	if in.Now.Sub(in.OldestQueued) > in.Threshold {
		out.Stalled = true
		out.Cause = "merge-train-idle"
		if out.IdleReason != idleReasonUnknown {
			out.Cause = out.IdleReason
		}
	}
	return out
}

// partitionState is one (repo, base) partition as the read paths see it.
type partitionState struct {
	Repo, Base, Key string
	Members         []trainRow
	OldestQueued    time.Time
	RunActive       bool
	Phase           string
	PhaseSince      time.Time
	Live            partitionLiveness
}

// partitionStates collects every partition that has queued members or an
// active run. It reads only the Store (via trainRows), the durable run phases,
// the worker markers, the non-mutating infra-cooldown peek and the health
// snapshot — never GitHub.
func (e *Engine) partitionStates(now time.Time, h healthSnapshot) []partitionState {
	type pk struct{ repo, base string }
	states := map[pk]*partitionState{}
	get := func(repo, base string) *partitionState {
		k := pk{repo, base}
		st := states[k]
		if st == nil {
			st = &partitionState{Repo: repo, Base: base, Key: mergeTrainKey(repo, base), Phase: "none"}
			states[k] = st
		}
		return st
	}
	for _, r := range (localAPIBackend{e: e}).trainRows() {
		st := get(r.repo, r.base)
		st.Members = append(st.Members, r)
		if !r.entered.IsZero() && (st.OldestQueued.IsZero() || r.entered.Before(st.OldestQueued)) {
			st.OldestQueued = r.entered
		}
	}
	for _, k := range e.store.RepoWorkerKeys() {
		repo, base, _ := strings.Cut(k, ":")
		st := get(repo, base)
		st.RunActive = true
	}
	for _, ph := range e.TrainRunPhases() {
		repo, base, _ := strings.Cut(ph.TrainKey, ":")
		st := get(repo, base)
		st.RunActive = true
		st.Phase, st.PhaseSince = ph.Phase, ph.Since
	}

	verdict := e.pollVerdictFor(h, now)
	threshold := verdict.OverdueAfter
	pending := h.BackoffReason != "" && h.NextPollDueAt.After(now) && !h.PollInProgress
	out := make([]partitionState, 0, len(states))
	for _, st := range states {
		remaining, held := e.infraCooldownPeek(st.Key)
		st.Live = computePartitionLiveness(partitionLivenessInput{
			Now:                     now,
			Threshold:               threshold,
			Members:                 len(st.Members),
			OldestQueued:            st.OldestQueued,
			RunActive:               st.RunActive,
			Poll:                    verdict,
			PollDeliberatelyPending: pending,
			InfraHeld:               held,
			InfraRemaining:          remaining,
		})
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Base < out[j].Base
	})
	return out
}

// attentionPollStall is the poll-stall input for the attention classifier: the
// stall while the poll is overdue, and the end of the last overdue episode.
func (e *Engine) attentionPollStall(now time.Time) (attention.PollStall, time.Time) {
	h := e.health.snapshot()
	v := e.pollVerdictFor(h, now)
	var ps attention.PollStall
	if v.Overdue {
		ps = attention.PollStall{Active: true, Blocked: v.Blocked, Step: h.PollStep}
	}
	return ps, h.LastStallEndAt
}
