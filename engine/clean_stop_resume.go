package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Startup resume of daemon clean-stop pauses (#2486, ADR 2486; amends ADR 1393's
// "a human lifts the pause" for the daemon-stop cause). A SIGINT/SIGTERM stop
// pauses every in-flight issue with fabrik:paused + fabrik:awaiting-input and the
// pauseCauseDaemonStopMarker comment (runShutdownPause). Nothing but a human
// lifted that pause, so a deliberate stop/start parked every interrupted issue
// until someone noticed. resumeCleanStopPauses lifts it on the first poll with a
// board, decided from durable GitHub state only and failing closed on any doubt.
const (
	// cleanStopResumeMaxPerPoll bounds the candidates evaluated in one poll: each
	// costs a comments read plus four label-event reads.
	cleanStopResumeMaxPerPoll = 10
	// cleanStopResumeMaxAttempts is how many polls a candidate whose reads or
	// writes failed is retried before it is dropped for the process lifetime.
	cleanStopResumeMaxAttempts = 3

	cleanStopResumeCommentPrefix = "🏭 **Fabrik — resumed after restart**"
)

// cleanStopResumeBody is the one short comment posted per resumed issue. Its
// wording must match neither removePausedRE nor commentResumeRE (the pause-comment
// detectors) and carries no pause marker; the 🏭 **Fabrik prefix keeps it out of
// human steering (findNewComments, filterHuman, resumeAuthorised — FR-012).
const (
	cleanStopResumeDetail = "This issue was paused when the daemon was stopped cleanly. The daemon has restarted, so the interrupted stage is being redispatched; no action is needed."
	cleanStopResumeBody   = cleanStopResumeCommentPrefix + "\n\n" + cleanStopResumeDetail
)

// cleanStopResumeState is the scan's in-memory state: the candidate set captured
// from the first populated board and each candidate's failed-attempt count.
type cleanStopResumeState struct {
	mu         sync.Mutex
	captured   bool
	candidates map[string]*cleanStopCandidate
}

type cleanStopCandidate struct {
	item     gh.ProjectItem
	attempts int
}

// cleanStopEvidence is everything the verdict reads, all from live GitHub reads.
type cleanStopEvidence struct {
	Labels   []string
	Comments []gh.Comment
	// PausedLabeledAt/PausedUnlabeledAt/AwaitingLabeledAt/AwaitingUnlabeledAt are
	// the newest event times of the two pause labels; zero = no such event.
	PausedLabeledAt     time.Time
	PausedUnlabeledAt   time.Time
	AwaitingLabeledAt   time.Time
	AwaitingUnlabeledAt time.Time
}

// cleanStopPauseVerdict decides whether the item's current pause was set by a
// daemon clean stop and has not been superseded. It returns ("", true) to resume,
// else a reason to stay paused. Pure.
//
// The engine writes fabrik:paused / fabrik:awaiting-input / fabrik:pause:daemon-stop
// before the marker comment (pauseInterruptedIssue, labelFirst), so every engine
// pause-label event is at or before the comment's created_at. An event strictly
// later is a later actor's, and the pause is no longer the stop's. A tie is the
// engine's own write (second resolution), so it resumes. A human comment never
// supersedes (FR-005). Actor logins are not used: in PAT mode the engine and the
// operator share one.
func cleanStopPauseVerdict(ev cleanStopEvidence) (string, bool) {
	if !hasLabel(ev.Labels, "fabrik:paused") {
		return "not paused", false
	}
	for _, l := range pauseCauseLabelsIn(ev.Labels) {
		if l != pauseCauseDaemonStop.label() {
			return "pause cause is " + l, false
		}
	}
	marker, ok := newestPauseMarkerComment(ev.Comments, pauseCauseDaemonStopMarker)
	if !ok {
		return "newest engine pause comment has no daemon-stop marker", false
	}
	at := marker.CreatedAt
	if at.IsZero() {
		return "marker comment has no timestamp", false
	}
	if ev.PausedLabeledAt.IsZero() {
		return "no fabrik:paused labeled event found", false
	}
	for _, t := range []struct {
		name string
		at   time.Time
	}{
		{"fabrik:paused labeled", ev.PausedLabeledAt},
		{"fabrik:paused unlabeled", ev.PausedUnlabeledAt},
		{"fabrik:awaiting-input labeled", ev.AwaitingLabeledAt},
		{"fabrik:awaiting-input unlabeled", ev.AwaitingUnlabeledAt},
	} {
		if t.at.After(at) {
			return t.name + " after the clean-stop comment", false
		}
	}
	return "", true
}

// resumeCleanStopPauses runs from poll(). On the first call with a populated board
// it captures the open, non-PR items already carrying fabrik:paused (so a pause
// applied later by anyone is never examined and the scan ends); on that and each
// following poll it evaluates up to cleanStopResumeMaxPerPoll candidates and lifts
// the verified ones. A candidate whose reads or writes fail is retried on later
// polls up to cleanStopResumeMaxAttempts times; a candidate that is decided
// (resumed, or kept paused for a reason) is dropped.
//
// Lift order: fabrik:paused first (the real gate — a failure leaves the item
// paused, never half-resumed, and a removed label ends candidacy so a pause is
// resumed at most once), then fabrik:awaiting-input (failure tolerated; the
// orphan scan mops up), the cause label, the audit log line, and last the
// comment — a crash between removal and comment loses the comment, never
// duplicates it. It deliberately does not call markSessionReset,
// StageLastAttemptCleared, EngineCyclesCleared or resetCommentBreaker: the
// interrupted run's session meta reads as a continuation (ADR 2300), which is the
// point.
func (e *Engine) resumeCleanStopPauses(board *gh.ProjectBoard) {
	if !e.cfg.ResumeAfterCleanStop || e.cleanStopResumeOff.Load() || board == nil || len(board.Items) == 0 {
		return
	}
	st := &e.cleanStopResume
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.captured {
		st.captured = true
		st.candidates = map[string]*cleanStopCandidate{}
		for _, item := range board.Items {
			if item.IsPR || item.IsClosed || !hasLabel(item.Labels, "fabrik:paused") {
				continue
			}
			st.candidates[itemOwnerRepoString(item, e.defaultRepo())+"#"+strconv.Itoa(item.Number)] = &cleanStopCandidate{item: item}
		}
	}
	if len(st.candidates) == 0 {
		return
	}
	keys := make([]string, 0, len(st.candidates))
	for k := range st.candidates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	evaluated := 0
	for _, k := range keys {
		if evaluated >= cleanStopResumeMaxPerPoll {
			break
		}
		cand := st.candidates[k]
		evaluated++
		done := e.evaluateCleanStopCandidate(cand)
		if !done {
			cand.attempts++
			if cand.attempts >= cleanStopResumeMaxAttempts {
				e.logf(cand.item.Number, "resume", "clean-stop resume: giving up after %d failed attempts; left paused (#2486)\n", cand.attempts)
				done = true
			}
		}
		if done {
			delete(st.candidates, k)
		}
	}
}

// evaluateCleanStopCandidate decides and, when verified, lifts one candidate.
// It reports done=true when the candidate is settled (resumed or kept paused for
// a reason) and false when a read or write failed and a later poll should retry.
func (e *Engine) evaluateCleanStopCandidate(cand *cleanStopCandidate) (done bool) {
	item := cand.item
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	labels, err := e.liveLabels(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "clean-stop resume: could not fetch labels: %v\n", err)
		return false
	}
	// live-read-audited: the verdict needs the whole comment thread, not the Store's first page; fail closed on error
	comments, err := e.client.FetchIssueComments(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "clean-stop resume: could not fetch comments: %v\n", err)
		return false
	}
	ev := cleanStopEvidence{Labels: labels, Comments: comments}
	reads := []struct {
		dst  *time.Time
		fn   func(owner, repo string, n int, label string) (time.Time, error)
		name string
	}{
		// live-read-audited: label event times are the durable ordering evidence; a failed read is doubt
		{&ev.PausedLabeledAt, e.client.FetchLabelAppliedAt, "fabrik:paused"},
		// live-read-audited: as above
		{&ev.PausedUnlabeledAt, e.client.FetchLabelRemovedAt, "fabrik:paused"},
		// live-read-audited: as above
		{&ev.AwaitingLabeledAt, e.client.FetchLabelAppliedAt, "fabrik:awaiting-input"},
		// live-read-audited: as above
		{&ev.AwaitingUnlabeledAt, e.client.FetchLabelRemovedAt, "fabrik:awaiting-input"},
	}
	for _, r := range reads {
		at, err := r.fn(owner, repo, item.Number, r.name)
		if err != nil {
			e.logf(item.Number, "warn", "clean-stop resume: could not read %s label events: %v\n", r.name, err)
			return false
		}
		*r.dst = at
	}

	if reason, ok := cleanStopPauseVerdict(ev); !ok {
		e.logf(item.Number, "resume", "clean-stop resume: leaving paused (%s) (#2486)\n", reason)
		return true
	}

	if err := e.removeLabelChecked(item, "fabrik:paused"); err != nil && !errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "clean-stop resume: could not remove fabrik:paused: %v\n", err)
		return false
	}
	if err := e.removeLabelChecked(item, "fabrik:awaiting-input"); err != nil && !errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "clean-stop resume: could not remove fabrik:awaiting-input (the orphan scan will): %v\n", err)
	}
	e.clearPauseCause(item)
	e.logf(item.Number, "resume", "resumed after restart: pause was set by a daemon clean stop (#2486)\n")
	e.postItemComment(item, fmt.Sprintf("🏭 **Fabrik — resumed after restart**\n\n%s", cleanStopResumeDetail), false) // literal header: TestAddCommentCompliance; pinned to cleanStopResumeBody by a test
	return true
}
