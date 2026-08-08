package itemstate

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ItemState is the canonical per-item state. All mutations flow through Store.Apply;
// all reads flow through Store.Get or change subscriptions.
//
// Field grouping by lifecycle:
//   - Identity: never changes after first Apply
//   - GitHub state: mirrors GitHub's view of issue, project, and linked PR
//   - Engine state: fabrik's local control state (locks, retries, cycle counts, cooldowns)
//   - TUI state: mirrors last-invocation outcomes for display
type ItemState struct {
	// -------- Identity (immutable post-creation) --------

	// Repo is "owner/repo".
	Repo string
	// Number is the issue number.
	Number int
	// ID is the GitHub content node ID (e.g. "I_kwDO..." for issues, "PR_kwDO..." for PRs).
	// Used by github.Client.FetchItemDetails for its GraphQL node lookup.
	ID string
	// ItemID is the GitHub Project item node ID (empty if not on the board).
	ItemID string

	// -------- GitHub state (mirrored from GitHub via webhook deltas + reconcile) --------

	Title     string
	Body      string
	URL       string
	Author    string
	Assignees []string
	// State is "open" or "closed".
	State    string
	IsClosed bool
	IsPR     bool
	Labels   []string
	// Status is the project board column ("Specify", "Implement", etc.).
	Status string
	// UpdatedAt is max(issue.updatedAt, projectItem.updatedAt, linkedPR.updatedAt).
	UpdatedAt time.Time

	// BlockedBy holds issues that must be closed before this one can advance.
	BlockedBy []gh.Dependency

	// Comments holds all comments on this issue.
	Comments []gh.Comment

	// LinkedPR is the state of the closing PR; nil if none.
	LinkedPR *LinkedPRState

	// -------- Engine state (fabrik's local control) --------

	// Lock is nil when unlocked; non-nil when this instance or another holds a lock.
	Lock *LockState
	// StageState holds per-stage attempt and cycle counters.
	StageState StageState
	// CooldownAt maps reason → expiry time (e.g. "retry", "review-blocked", "ci-await").
	CooldownAt map[string]time.Time
	// LabelAppliedAt maps label name → the time the engine itself most recently
	// applied that label to this issue (record-at-write, #1314). Deliberately a
	// separate map from CooldownAt, not a repurposing of it: CooldownAt's
	// HasExpiredCooldown treats any non-zero, past timestamp as "wake this item"
	// (engine/poll.go), but an applied-at timestamp is always in the past the
	// instant it's recorded — aliasing the two would make every recorded label
	// application look like a permanently expired cooldown. Populated only for
	// labels the engine writes exclusively through applyLabelAdd or an explicit
	// recordLabelAppliedAtNow call (engine/mutate.go); a label this map has no
	// entry for simply falls back to the live FetchLabelAppliedAt REST fetch.
	LabelAppliedAt map[string]time.Time
	// Worker is present when a worker is in-flight for this item.
	Worker *WorkerHandle

	// Terminal is set by the engine after a deep-fetch confirms the item satisfies
	// the terminal predicate (cleanup-stage status + stage:<Name>:complete label + no
	// transient lifecycle labels). While set, the poll loop skips deep-fetch entirely
	// for this item. Cleared automatically when the item's status changes.
	Terminal bool

	// -------- TUI / invocation mirror state --------

	// LastDeepFetchAt is the time of the last successful deep fetch for this item.
	// A zero value means no deep fetch has occurred. Replaces boardcache.deepFetched[key].
	LastDeepFetchAt time.Time

	// LastSeenSourceUpdatedAt is the value of pi.UpdatedAt observed at the moment
	// of the last successful deep fetch. The cache compares this against the fresh
	// pi.UpdatedAt from each board read to decide whether the cached deep fields
	// are still authoritative; a later board updatedAt forces a deep re-fetch.
	// This is the "GraphQL fetching is primary; updatedAt makes the staleness check
	// cheap" contract — webhooks remain an optimization that keeps the cache fresh
	// between polls but never replace the polling refresh path.
	//
	// Has a second writer besides ItemDeepFetched: SelfWriteObserved (#1090)
	// advances it to time.Now() at each of Fabrik's own self-write call sites
	// (label add/remove, comment post, issue body edit, board status move) so a
	// self-caused GitHub updatedAt bump isn't mistaken for external staleness on
	// the next probe cycle. Both writers share the same monotonic guard in
	// applyToItem, and DeepFetchInvalidated's zero-reset always wins against a
	// stale SelfWriteObserved racing behind it.
	LastSeenSourceUpdatedAt time.Time

	// LastDeepFetchFailureAt is the time the most recent deep fetch attempt failed.
	// Replaces engine.deepFetchFailureTime[iKey].
	LastDeepFetchFailureAt time.Time

	// LastInvocationCompleted records whether the most recent Claude invocation
	// emitted FABRIK_STAGE_COMPLETE. Replaces engine.lastCompleted[iKey].
	LastInvocationCompleted bool

	// LastInvocationBlocked records whether the most recent Claude invocation
	// emitted FABRIK_BLOCKED_ON_INPUT. Replaces engine.lastBlocked[iKey].
	LastInvocationBlocked bool

	// LastInvocationIsComment is true when the most recent invocation processed a
	// user comment rather than running a full stage. Mirrors InvocationRecorded.IsComment.
	LastInvocationIsComment bool

	// LastInvocationDuration is the wall-clock time of the most recent Claude invocation.
	// Zero when not recorded (comment-processing paths that don't track start time).
	LastInvocationDuration time.Duration

	// LastInvocationErrored records whether the most recent Claude invocation exited
	// with a non-zero status (process error, timeout kill, etc.) AND was not classified
	// as a turn-limit exit (see LastInvocationTurnLimited) — i.e. a genuine fault. This
	// is recorded independently of LastInvocationCompleted: a stage can complete
	// (FABRIK_STAGE_COMPLETE emitted) even when the process exits non-zero — e.g. a
	// timeout kill after the stage finished. The error is surfaced as
	// JobCompletedEvent.Success=false in history.
	LastInvocationErrored bool

	// LastInvocationTurnLimited records whether the most recent Claude invocation
	// exited because it exhausted its configured turn budget (CLI subtype
	// error_max_turns), as opposed to a genuine failure. A turn-limited invocation
	// is incomplete but resumable — surfaced as JobCompletedEvent.TurnLimited in
	// history, rendered distinctly from a genuine error. See ADR-1178.
	LastInvocationTurnLimited bool

	// LastTokenUsage holds token consumption from the most recent Claude invocation.
	// Replaces engine.lastUsage[iKey].
	LastTokenUsage TokenUsage

	// BaseBranchWarned tracks which base-branch overrides have already produced a
	// "branch not found" warning comment. Replaces engine.baseBranchWarnedSet.
	BaseBranchWarned map[string]bool

	// CommentBreaker tracks comment-processing invocations for the runaway-loop
	// circuit breaker (#1089). Scoped to the item as a whole, not per-stage — the
	// breaker must survive a legitimate stage transition mid-window.
	CommentBreaker CommentBreakerState
}

// CommentBreakerState tracks comment-processing invocation timestamps used by
// the engine's circuit breaker to detect a non-advancing comment-processing
// loop. The engine (not this package) owns the threshold/window business
// logic and prunes-and-counts InvocationsAt on every read — mirroring the
// existing mergeTrainTrials runaway-guard precedent (ADR-059 D8).
type CommentBreakerState struct {
	// InvocationsAt holds the timestamp of each recorded comment-processing
	// invocation since the last reset.
	InvocationsAt []time.Time
	// LastAuthor is the author of the comment that triggered the most recent
	// recorded invocation. Surfaced in the trip comment so the operator knows
	// who/what to look at.
	LastAuthor string
}

// LinkedPRState holds the state of the closing pull request for an issue.
type LinkedPRState struct {
	Number int
	// Title, State ("open"/"closed"), Merged, and Draft mirror gh.PRDetails fields
	// that were previously stored only in CacheImpl.linkedPRs. Populated by PRDetailsUpdated.
	Title  string
	State  string
	Merged bool
	Draft  bool
	// Mergeable is nil when unknown; true/false once GitHub resolves mergeability.
	Mergeable      *bool
	MergeableState string // "clean", "unstable", "blocked", etc.
	HeadSHA        string
	Reviews        []gh.PRReview
	ReviewRequests []gh.ReviewRequest
	// ThreadComments holds unresolved review-thread comments.
	ThreadComments      []gh.Comment
	ResolvedThreadCount int
	CheckRuns           []gh.CheckRun

	// IsMergeQueueEnabled is true when the repository has the merge queue feature
	// enabled. Populated from GraphQL via FetchItemDetails; zero until first deep fetch.
	IsMergeQueueEnabled bool
	// IsInMergeQueue is true when the PR is currently in the merge queue.
	IsInMergeQueue bool
	// MergeQueueEntry holds queue position and state when the PR is enqueued.
	// Nil when not in queue; set to nil again after dequeueing.
	MergeQueueEntry *gh.MergeQueueEntry

	// HasHadChecks records whether this PR has ever had CI check runs reported.
	// Replaces engine.prHasHadChecks[iKey].
	HasHadChecks bool

	// CIMergePendingSince records when the engine began waiting for the merge
	// to complete after CI passed. Zero if not currently pending.
	// Replaces engine.ciMergePendingSince[iKey].
	CIMergePendingSince time.Time

	// ValidateCompletedSHA records the HEAD SHA of the linked PR at the moment
	// stage:Validate:complete was last applied. The SHA-invalidation scan in
	// engine/poll.go compares this against the current HeadSHA to detect force-pushes
	// or external commits after Validate finished. Empty string means "not recorded"
	// (pre-feature or no Validate completion in this session).
	ValidateCompletedSHA string

	// LastHeadSHAUpdate records when the linked PR's HeadSHA was last observed to
	// change via a PRHeadSHAUpdated mutation. Zero means the SHA has never changed
	// (cold start or post-restart). Used by the post-push dwell guard in checkCIGate
	// to block gate-clearance during the brief window after a force-push when GitHub
	// has not yet computed mergeability or started CI for the new SHA.
	LastHeadSHAUpdate time.Time

	// LastEnqueuedSHA records the PR head SHA at the moment the engine last enqueued
	// (or re-enqueued) this PR into GitHub's native merge queue (ADR-058 D4 FR-3).
	// The convergence monitor uses it to distinguish a genuine post-resolution
	// re-enqueue (head SHA changed since the last enqueue → enqueue fresh) from the
	// brief post-enqueue consistency window where GitHub has not yet reflected
	// isInMergeQueue=true (same SHA → suppress a spurious re-enqueue). Empty until the
	// first enqueue.
	LastEnqueuedSHA string

	// LastCIFixNoOpSHA records the head SHA for which the most recent CI-fix
	// reinvoke (engine.dispatchCIFixReinvoke) observed no new commit pushed
	// (HEAD unchanged before/after processComments). While the head SHA
	// stays at this value, handleMergeAndCIGates skips further CI-fix
	// dispatch/cycle-increment for it — a repeated no-op reinvoke burns
	// nothing further; CIWaitTimeout remains the backstop if CI never
	// resolves. Cleared implicitly once HeadSHA advances past it. Empty
	// means "no no-op recorded for the current SHA."
	LastCIFixNoOpSHA string

	// LastCIProgressAt records when CI was last observed to make progress: a
	// new check-run ID appeared, an existing one's Status/Conclusion
	// transitioned, or the linked PR's HeadSHA advanced (a fresh push resets
	// CI entirely, which is itself progress). Set by applyCheckRunCompleted
	// and the PRHeadSHAUpdated case in store.go, both gated on an actual
	// content change so a duplicate/no-op observation never bumps it. Zero
	// means no progress has been observed since this process started — the
	// safe cold-start default (ADR-1410) is to never escalate on a zero
	// timestamp, only to re-observe, since the in-memory store has no
	// persistence across a restart and GitHub exposes no change-history
	// equivalent to backfill it from.
	LastCIProgressAt time.Time
}

// StageState holds per-stage attempt counters and cycle counts.
// Keys are stage names (e.g. "Implement", "Review").
type StageState struct {
	// Attempts counts how many times Claude was invoked for each stage.
	// Replaces engine.retryCount[stageKey].
	Attempts map[string]int
	// LastAttemptAt records the last invocation timestamp per stage.
	// Replaces engine.processedSet[stageKey] for retry-suppression.
	LastAttemptAt map[string]time.Time
	// PausedByEngine records engine-initiated pauses (vs user-initiated).
	// Replaces engine.pausedDueToRetries[stageKey].
	PausedByEngine map[string]bool
	// PRCreationFailed records that Claude completed but the draft PR could not
	// be created. In-memory only — does not survive restart. When set, the next
	// retry first attempts PR creation before re-invoking Claude.
	PRCreationFailed map[string]bool
	// ReviewCycles counts how many review iterations each stage has completed.
	// Replaces engine.reviewCycleCount[stageKey].
	ReviewCycles map[string]int
	// CIFixCycles counts how many CI-fix iterations each stage has completed.
	// Replaces engine.ciFixCycleCount[stageKey].
	CIFixCycles map[string]int
	// RebaseCycles counts how many rebase iterations each stage has completed.
	// Replaces engine.rebaseCycleCount[stageKey].
	RebaseCycles map[string]int
	// EnqueueCycles counts how many times the engine has re-enqueued the linked PR
	// into GitHub's native merge queue after an ejection (ADR-058 D4 FR-3). Bounds a
	// queue-thrash loop (enqueue→eject→re-enqueue→eject) independently of the
	// RebaseCycles/CIFixCycles that the conflict/CI sub-paths increment.
	EnqueueCycles map[string]int
	// SliceRetries counts how many turn-cap preemptions (CLI subtype
	// error_max_turns) each stage has hit. Bounded by MaxSliceRetries,
	// independently of Attempts/MaxRetries — a turn-cap exit is a resumable
	// time-slice, not a failure (#1199), so it must not count against the
	// failure counter, but a non-converging job still needs its own bound.
	SliceRetries map[string]int
	// ProcessedComments maps comment ID to the time Fabrik finished processing it.
	ProcessedComments map[string]time.Time
	// LinkageHealAttempted maps stage name to the PR head SHA for which a linkage
	// auto-heal was attempted. In-memory only — does not survive restart. Keyed by
	// stage name so force-push (new SHA) clears the guard naturally.
	LinkageHealAttempted map[string]string
	// LastTurnsUsed records the most recently completed invocation's TurnsUsed for
	// each stage. In-memory only — does not survive restart. Used by the stall
	// detector (#1146) to compare consecutive incomplete attempts.
	LastTurnsUsed map[string]int
	// LastTurnsCapped records whether the most recently completed invocation for a
	// stage was turn-capped (TurnsUsed >= MaxTurns without completing). Overwritten
	// on every invocation, which makes stall detection self-limiting to a single
	// corrective hint per episode (#1146): the declining attempt that triggers
	// detection is itself uncapped, so it immediately clears the precondition for
	// the next comparison.
	LastTurnsCapped map[string]bool
	// StallHintPending marks a stage for which a stall was detected and a
	// corrective hint should be injected into the next invocation's prompt. Cleared
	// (consumed) as soon as that invocation is dispatched. In-memory only — does
	// not survive restart (#1146).
	StallHintPending map[string]bool
}

// LockState describes who holds the fabrik:locked:<user> label on this issue.
type LockState struct {
	// HolderUser is the user identity from the fabrik:locked:<user> label.
	HolderUser string
	// HeldByThis is true if HolderUser matches this engine instance's user and Worker != nil.
	HeldByThis bool
	AcquiredAt time.Time
}

// WorkerHandle identifies an in-flight Claude invocation for this item.
type WorkerHandle struct {
	PID       int
	StageName string
	StartedAt time.Time
	// LastSignAt is updated by worker heartbeats; a stale heartbeat implies the worker died.
	LastSignAt time.Time
}

// TokenUsage records Claude API token counts for a single invocation.
// Fields mirror engine.TokenUsage to enable zero-cost assignment in Phase 3-E.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	CostUSD             float64
	TurnsUsed           int
	MaxTurns            int
}
