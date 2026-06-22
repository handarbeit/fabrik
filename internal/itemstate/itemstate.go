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
	// with a non-zero status (process error, timeout kill, etc.). This is recorded
	// independently of LastInvocationCompleted: a stage can complete (FABRIK_STAGE_COMPLETE
	// emitted) even when the process exits non-zero — e.g. a timeout kill after the stage
	// finished. The error is surfaced as JobCompletedEvent.Success=false in history.
	LastInvocationErrored bool

	// LastTokenUsage holds token consumption from the most recent Claude invocation.
	// Replaces engine.lastUsage[iKey].
	LastTokenUsage TokenUsage

	// BaseBranchWarned tracks which base-branch overrides have already produced a
	// "branch not found" warning comment. Replaces engine.baseBranchWarnedSet.
	BaseBranchWarned map[string]bool
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
	// ProcessedComments maps comment ID to the time Fabrik finished processing it.
	ProcessedComments map[string]time.Time
	// LinkageHealAttempted maps stage name to the PR head SHA for which a linkage
	// auto-heal was attempted. In-memory only — does not survive restart. Keyed by
	// stage name so force-push (new SHA) clears the guard naturally.
	LinkageHealAttempted map[string]string
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
