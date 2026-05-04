package itemstate

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Snapshot is an immutable copy of an ItemState returned by Store.Get and Store.Apply.
// Because it wraps a value (not a pointer), callers can hold it as long as needed
// without blocking writes and without risk of concurrent mutation.
//
// All slice and map fields are deep-copied when a Snapshot is constructed, so
// mutations to the Store's internal state do not bleed into held Snapshots.
type Snapshot struct {
	// state is a deep copy — not a pointer — so Snapshot is safe to hold indefinitely.
	state ItemState
}

// newSnapshot constructs a Snapshot from an ItemState, deep-copying all
// reference-typed fields to satisfy invariant I5 (snapshot immutability).
//
// Slice / map fields that require deep copy:
//   - ItemState.Labels
//   - ItemState.Assignees
//   - ItemState.Comments
//   - ItemState.BlockedBy
//   - ItemState.CooldownAt
//   - ItemState.BaseBranchWarned
//   - ItemState.StageState.Attempts
//   - ItemState.StageState.LastAttemptAt
//   - ItemState.StageState.PausedByEngine
//   - ItemState.StageState.ReviewCycles
//   - ItemState.StageState.CIFixCycles
//   - ItemState.StageState.RebaseCycles
//   - ItemState.StageState.ProcessedComments
//   - LinkedPRState.Reviews
//   - LinkedPRState.ReviewRequests
//   - LinkedPRState.ThreadComments
//   - LinkedPRState.CheckRuns
func newSnapshot(s ItemState) Snapshot {
	c := s

	c.Labels = copyStrings(s.Labels)
	c.Assignees = copyStrings(s.Assignees)
	c.Comments = copyComments(s.Comments)
	c.BlockedBy = copyDeps(s.BlockedBy)
	c.CooldownAt = copyTimeMap(s.CooldownAt)
	c.BaseBranchWarned = copyBoolMap(s.BaseBranchWarned)
	c.StageState = copyStageState(s.StageState)

	if s.LinkedPR != nil {
		lpr := *s.LinkedPR
		lpr.Reviews = copyPRReviews(s.LinkedPR.Reviews)
		lpr.ReviewRequests = copyReviewRequests(s.LinkedPR.ReviewRequests)
		lpr.ThreadComments = copyComments(s.LinkedPR.ThreadComments)
		lpr.CheckRuns = copyCheckRuns(s.LinkedPR.CheckRuns)
		c.LinkedPR = &lpr
	}

	if s.Lock != nil {
		lock := *s.Lock
		c.Lock = &lock
	}

	if s.Worker != nil {
		w := *s.Worker
		c.Worker = &w
	}

	return Snapshot{state: c}
}

// State returns a deep copy of the underlying ItemState value.
// Mutating the returned value — including its slice and map fields — does not
// affect the Snapshot or the Store.
func (s Snapshot) State() ItemState {
	return newSnapshot(s.state).state
}

// Repo returns the "owner/repo" identifier.
func (s Snapshot) Repo() string { return s.state.Repo }

// Number returns the issue number.
func (s Snapshot) Number() int { return s.state.Number }

// Status returns the current project board column.
func (s Snapshot) Status() string { return s.state.Status }

// Labels returns a copy of the label slice.
func (s Snapshot) Labels() []string { return copyStrings(s.state.Labels) }

// IsClosed reports whether the issue is closed.
func (s Snapshot) IsClosed() bool { return s.state.IsClosed }

// Worker returns the active WorkerHandle, or nil if no worker is in-flight.
func (s Snapshot) Worker() *WorkerHandle {
	if s.state.Worker == nil {
		return nil
	}
	w := *s.state.Worker
	return &w
}

// Lock returns the current LockState, or nil if unlocked.
func (s Snapshot) Lock() *LockState {
	if s.state.Lock == nil {
		return nil
	}
	l := *s.state.Lock
	return &l
}

// LinkedPR returns the LinkedPRState, or nil if no closing PR exists.
func (s Snapshot) LinkedPR() *LinkedPRState {
	if s.state.LinkedPR == nil {
		return nil
	}
	lpr := *s.state.LinkedPR
	lpr.Reviews = copyPRReviews(s.state.LinkedPR.Reviews)
	lpr.ReviewRequests = copyReviewRequests(s.state.LinkedPR.ReviewRequests)
	lpr.ThreadComments = copyComments(s.state.LinkedPR.ThreadComments)
	lpr.CheckRuns = copyCheckRuns(s.state.LinkedPR.CheckRuns)
	return &lpr
}

// CooldownAt returns the expiry time for a given cooldown reason, or zero if none.
func (s Snapshot) CooldownAt(reason string) time.Time {
	return s.state.CooldownAt[reason]
}

// LastAttemptAt returns the last invocation timestamp for a given stage, or zero.
func (s Snapshot) LastAttemptAt(stageName string) time.Time {
	return s.state.StageState.LastAttemptAt[stageName]
}

// Attempts returns the failed attempt count for a given stage, or zero.
func (s Snapshot) Attempts(stageName string) int {
	return s.state.StageState.Attempts[stageName]
}

// PausedByEngine reports whether the engine has paused this stage due to
// repeated failures. This flag is in-memory only and does not survive restart.
func (s Snapshot) PausedByEngine(stageName string) bool {
	return s.state.StageState.PausedByEngine[stageName]
}

// ReviewCycles returns the review re-invocation cycle count for a given stage.
func (s Snapshot) ReviewCycles(stageName string) int {
	return s.state.StageState.ReviewCycles[stageName]
}

// CIFixCycles returns the CI-fix re-invocation cycle count for a given stage.
func (s Snapshot) CIFixCycles(stageName string) int {
	return s.state.StageState.CIFixCycles[stageName]
}

// RebaseCycles returns the rebase re-invocation cycle count for a given stage.
func (s Snapshot) RebaseCycles(stageName string) int {
	return s.state.StageState.RebaseCycles[stageName]
}

// CommentProcessed returns the timestamp when a comment was last processed,
// or zero if it has not been seen.
func (s Snapshot) CommentProcessed(commentID string) time.Time {
	return s.state.StageState.ProcessedComments[commentID]
}

// ---- deep-copy helpers ----

func copyStrings(src []string) []string {
	if src == nil {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

func copyComments(src []gh.Comment) []gh.Comment {
	if src == nil {
		return nil
	}
	dst := make([]gh.Comment, len(src))
	copy(dst, src)
	// gh.Comment.Reactions is a slice; copy it so Snapshot elements don't alias src.
	for i := range dst {
		if src[i].Reactions != nil {
			dst[i].Reactions = make([]gh.ReactionGroup, len(src[i].Reactions))
			copy(dst[i].Reactions, src[i].Reactions)
		}
	}
	return dst
}

func copyDeps(src []gh.Dependency) []gh.Dependency {
	if src == nil {
		return nil
	}
	dst := make([]gh.Dependency, len(src))
	copy(dst, src)
	return dst
}

func copyPRReviews(src []gh.PRReview) []gh.PRReview {
	if src == nil {
		return nil
	}
	dst := make([]gh.PRReview, len(src))
	copy(dst, src)
	return dst
}

func copyReviewRequests(src []gh.ReviewRequest) []gh.ReviewRequest {
	if src == nil {
		return nil
	}
	dst := make([]gh.ReviewRequest, len(src))
	copy(dst, src)
	return dst
}

func copyCheckRuns(src []gh.CheckRun) []gh.CheckRun {
	if src == nil {
		return nil
	}
	dst := make([]gh.CheckRun, len(src))
	copy(dst, src)
	return dst
}

func copyTimeMap(src map[string]time.Time) map[string]time.Time {
	if src == nil {
		return nil
	}
	dst := make(map[string]time.Time, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func copyBoolMap(src map[string]bool) map[string]bool {
	if src == nil {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func copyIntMap(src map[string]int) map[string]int {
	if src == nil {
		return nil
	}
	dst := make(map[string]int, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func copyStageState(s StageState) StageState {
	return StageState{
		Attempts:          copyIntMap(s.Attempts),
		LastAttemptAt:     copyTimeMap(s.LastAttemptAt),
		PausedByEngine:    copyBoolMap(s.PausedByEngine),
		ReviewCycles:      copyIntMap(s.ReviewCycles),
		CIFixCycles:       copyIntMap(s.CIFixCycles),
		RebaseCycles:      copyIntMap(s.RebaseCycles),
		ProcessedComments: copyTimeMap(s.ProcessedComments),
	}
}
