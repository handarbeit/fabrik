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

	// -------- TUI / invocation mirror state --------

	// LastDeepFetchAt is the time of the last successful deep fetch for this item.
	// A zero value means no deep fetch has occurred. Replaces boardcache.deepFetched[key].
	LastDeepFetchAt time.Time

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
	ThreadComments       []gh.Comment
	ResolvedThreadCount  int
	CheckRuns            []gh.CheckRun

	// HasHadChecks records whether this PR has ever had CI check runs reported.
	// Replaces engine.prHasHadChecks[iKey].
	HasHadChecks bool

	// CIMergePendingSince records when the engine began waiting for the merge
	// to complete after CI passed. Zero if not currently pending.
	// Replaces engine.ciMergePendingSince[iKey].
	CIMergePendingSince time.Time
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
	// ReviewCycles counts how many review iterations each stage has completed.
	// Replaces engine.reviewCycleCount[stageKey].
	ReviewCycles map[string]int
	// CIFixCycles counts how many CI-fix iterations each stage has completed.
	// Replaces engine.ciFixCycleCount[stageKey].
	CIFixCycles map[string]int
	// RebaseCycles counts how many rebase iterations each stage has completed.
	// Replaces engine.rebaseCycleCount[stageKey].
	RebaseCycles map[string]int
	// ProcessedComments maps comment ID to the time Fabrik finished processing it.
	ProcessedComments map[string]time.Time
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
