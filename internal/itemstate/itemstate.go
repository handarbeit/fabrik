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
	// LabelsKnown is true once a producer that actually observed labels has
	// populated the record (#2123). A label-less record (probe bootstrap, a
	// minimal opened item) is not baselined, so the first label-bearing
	// population is silent instead of emitting every label as a delta.
	LabelsKnown bool
	// Status is the project board column ("Specify", "Implement", etc.).
	Status string
	// StatusEnteredAt is the time this item's Status was last observed to change
	// (record-on-write, sibling of LabelAppliedAt/#1314, applied to Status rather
	// than to a label). Set to time.Now() at the moment a Status mutation is
	// applied, inside the same "if item.Status != new" guard every Status writer
	// already uses for no-op detection — so a no-op reconcile of an unchanged
	// Status never touches this field. Generic and Queued-agnostic by design:
	// this package has no notion of which Status name is a merge-train holding
	// stage; consumers (e.g. engine's Queued-batch ordering, #1833) attach that
	// meaning themselves. Reset to "now" on every fresh Store.Reset (bootstrap/
	// restart) for every item, since there is no durable cross-restart source
	// for "when did this item truly enter its current Status" — see ADR-1833.
	// It is "first seen by this process" (attention, local API); the merge-train
	// order uses StatusAtGitHub where known (ADR-2246).
	StatusEnteredAt time.Time
	// StatusAtGitHub is GitHub's own time for the move into the current Status:
	// the Status single-select value's updatedAt from a listing or probe, or the
	// projects_v2_item webhook's updated_at (#2246, ADR-2246). Zero means unknown
	// (direct-GraphQL path, engine's own write, a payload without the time) and
	// callers fall back to StatusEnteredAt. Assigned only inside the same
	// "Status changed" guard as StatusEnteredAt, so it is once per transition and
	// never moves backwards on a stale re-read. Second resolution: same-second
	// moves tie.
	StatusAtGitHub time.Time
	// EligibilityChangedAt is the time the item's eligibility to proceed last
	// changed while this process was watching: a fabrik:cruise, fabrik:yolo,
	// fabrik:blocked or fabrik:paused label, or a stage:*:complete label, was
	// added or removed (#2120 R3). Daemon time, stamped only from a before/after
	// label diff of an item the store already held — a population or baseline
	// never stamps — so it is zero ("unknown") after a restart and for changes
	// made while the daemon was down. The classifier takes the later of this and
	// the GitHub time of the deciding fact, so an unknown stamp can only make an
	// age longer, never shorter.
	EligibilityChangedAt time.Time
	// StatusMovedAt is the time the item was seen to move between two known board
	// columns while this process was watching (#2120 R3). Like EligibilityChangedAt
	// it is stamped only for an item the store already held with a known Status, so
	// a population never stamps and it is zero ("unknown") after a restart. Unlike
	// StatusEnteredAt it therefore says a real move happened, which is what lets the
	// classifier date "the blocker was parked" instead of only "first seen".
	StatusMovedAt time.Time
	// UpdatedAt is max(issue.updatedAt, projectItem.updatedAt, linkedPR.updatedAt).
	UpdatedAt time.Time

	// IssueLayerUpdatedAt, LinkedPRUpdatedAt and IssueCommentTotal(Known) are the
	// layered board evidence the cache-serve decision compares (#2180): the same
	// UpdatedAt split into its issue/project-item and linked-PR parts, plus
	// GitHub's issue comment total. Written by the probe and board listings
	// (and carried through a deep fetch); zero / unknown when the producer did
	// not capture them. Mirrors of the last board read, not engine state.
	IssueLayerUpdatedAt    time.Time
	LinkedPRUpdatedAt      time.Time
	IssueCommentTotal      int
	IssueCommentTotalKnown bool

	// BlockedBy holds issues that must be closed before this one can advance.
	BlockedBy []gh.Dependency

	// Comments holds all comments on this issue.
	Comments []gh.Comment

	// Milestone is the issue's milestone (#1967 R10). Nil means "no milestone"
	// only when MilestoneKnown is true; with MilestoneKnown false the cache has
	// never captured it and readers must report it as unknown, never as none.
	// Written by the board fetch (applyProjectItem/applyShallowItem, only when
	// the incoming item has MilestoneKnown) and the milestoned/demilestoned/
	// opened webhook deltas, so write paths that do not carry the field cannot
	// wipe it.
	Milestone      *gh.Milestone
	MilestoneKnown bool

	// LinkedPR is the state of the closing PR; nil if none.
	LinkedPR *LinkedPRState

	// -------- Engine state (fabrik's local control) --------

	// Lock is nil when unlocked; non-nil when this instance or another holds a lock.
	Lock *LockState
	// StageState holds per-stage attempt and cycle counters.
	StageState StageState
	// CooldownAt maps reason → expiry time (e.g. "retry", "review-blocked", "ci-await").
	// An expired entry is consumed (deleted) when poll admission acts on it
	// (ExpiredCooldownsConsumed, #2096), so it admits an item once; the only
	// entry deliberately kept past expiry is "archive-eligible-at", an absolute
	// "eligible since" timestamp rather than a gate.
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
	// EngineWriteAt records when the engine itself last wrote a cached field
	// through the write-through path (#2159): keys are "label:<name>" (add or
	// remove), "status" and "closed". A reconcile listing fetched before the
	// write (plus a grace margin for GitHub's board-listing lag) must not undo
	// it; see engine_write.go. Distinct from LabelAppliedAt, which is adds-only,
	// keep-first and read by gates — this map is read only by the reconcile
	// protection. In memory only; entries expire after the grace period.
	EngineWriteAt map[string]time.Time
	// ObservedAt records, per fact, the authority time of the last applied
	// observation of any producer (#2348, ADR 2348): keys "status", "closed",
	// "label:<name>", "labels" (the baseline of a full label set), "head",
	// "linkedpr", "reviews", "milestone" and "blockedBy". A write whose authority
	// time is older than the recorded one (the later of this and EngineWriteAt) is
	// dropped; see observed.go. In memory only; bookkeeping, excluded from no-op
	// detection. Label entries expire after observedLabelRetention.
	ObservedAt map[string]time.Time
	// AddedAt is when this running Store first created the record. Reconcile
	// never removes an item added after its listing's fetch started (#2348, R4).
	// Zero for records predating the field.
	AddedAt time.Time
	// Worker is present when a worker is in-flight for this item — or queued
	// for a worker slot (Worker.Queued, #2121): the handle is the claim on the
	// item, and the dispatch guards rely on it being non-nil from dispatch.
	Worker *WorkerHandle

	// PendingRetry is the retry the engine has actually scheduled after a failed
	// invocation (#2121): written from emitRetryScheduled whether or not a
	// channel hub runs, cleared on dispatch, completion, pause and close. In
	// memory only.
	PendingRetry *PendingRetry
	// GateHold is the advance/landing gate currently holding the item (#2121),
	// written from noteGateHold, cleared by noteGateClear. In memory only.
	GateHold *GateHold
	// ConflictResolution is set while a merge-train conflict resolution for this
	// member is queued for, or holding, a worker slot (#2121). A separate field
	// rather than a Worker handle on purpose: the stale-worker detector,
	// shutdown pause, janitor and overseer refusals all treat Worker as a stage
	// worker. In memory only.
	ConflictResolution *ConflictResolution
	// RebaseCause records why fabrik:rebase-needed was applied when the engine
	// knows ("after-landing": a post-landing merge-tree conflict, #2047);
	// empty = unknown. Cleared when the label is removed. In memory only.
	RebaseCause string

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
	// the next probe cycle. Only SelfWriteObserved is monotonic: ItemDeepFetched
	// assigns the fetched value unconditionally (it may lower a clock-anchored
	// baseline, which is how a real refetch cures both masking and clock skew),
	// and DeepFetchInvalidated's zero-reset always wins against a stale
	// SelfWriteObserved racing behind it.
	//
	// This is the ISSUE-layer baseline (#2180): the engine writes the issue and
	// project item, so only that layer is allowed to be self-write anchored. The
	// linked PR's updatedAt is compared against LastSeenPRUpdatedAt instead.
	LastSeenSourceUpdatedAt time.Time

	// LastSeenPRUpdatedAt is the linked PR's own updatedAt observed at the last
	// deep fetch (#2180). It is written only from GitHub values (ItemDeepFetched)
	// and never by SelfWriteObserved, so an engine write on the issue layer
	// cannot hide a later PR-side change (review, thread comment, PR comment).
	// Zero means "not captured": the cache then falls back to the overall
	// updatedAt compare against LastSeenSourceUpdatedAt.
	LastSeenPRUpdatedAt time.Time

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
	// as a turn-limit exit (see LastInvocationTurnLimited) or a tool-permission-denial
	// exit (see ToolsDeniedRetries; #1523) — i.e. a genuine fault. This is recorded
	// independently of LastInvocationCompleted: a stage can complete
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

// PRFacts are the linked PR's state facts as last observed by the poll (#2122).
// The zero value (ObservedAt zero) means "never captured" and renders unknown.
type PRFacts struct {
	// State is "open" or "closed"; Merged is true for a merged PR.
	State  string
	Draft  bool
	Merged bool
	// Mergeable is nil when GitHub has not computed it (UNKNOWN).
	Mergeable *bool
	// MergeableState uses REST mergeable_state vocabulary ("clean", "blocked",
	// ...); empty when unknown.
	MergeableState string
	// HeadSHA is the head the facts were read for.
	HeadSHA string
	// ObservedAt is when the poll last read these facts from GitHub.
	ObservedAt time.Time
}

// CIVerdictRecord is the green-and-complete CI verdict for one PR head,
// computed poll-side by the gates' own classifier (#2122). It is valid only for
// HeadSHA and is cleared when the head changes.
type CIVerdictRecord struct {
	// Verdict is "green", "red", "pending", "none" or "unknown"; empty = never
	// computed.
	Verdict string
	// HeadSHA is the head the verdict was computed for.
	HeadSHA string
	// Failing names the failing checks when the verdict is red.
	Failing []string
	// RunsFingerprint identifies the check-run set the verdict was computed from,
	// so an unchanged set is not re-classified.
	RunsFingerprint string
	// ObservedAt is when the verdict was last confirmed.
	ObservedAt time.Time
	// LiveReadAt is when the check runs behind the verdict were last read from
	// GitHub rather than from the cache. A verdict may be restamped without a
	// read only while this is recent: a cached READY set is served on trust
	// forever and nothing on a poll-only deployment refreshes it, so a rerun
	// that turns red on the same head would otherwise never be seen.
	LiveReadAt time.Time
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

	// Facts and CIVerdict are the overseer-facing PR facts (#2122, ADR-2122).
	// They are separate from State/Draft/Merged/Mergeable/MergeableState/
	// CheckRuns above, which the gates read: nothing a gate consults is written
	// by PRFactsObserved or CIVerdictObserved, so populating them cannot change
	// gate semantics. Written only by the poll goroutine; read by the local API.
	Facts     PRFacts
	CIVerdict CIVerdictRecord

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
	// (pre-feature, or a restart that has not yet been rebuilt).
	//
	// After a restart the engine rebuilds it from the Validate stage comment's commit:
	// header before the landing decision (#2343, ADR-2343). That header holds an
	// ABBREVIATED SHA, so the rebuilt value is the full live head when the head is
	// unchanged and the abbreviated commit otherwise. Consumers must therefore only
	// compare it for (in)equality with a full head; never use it as a git revision.
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
	// nothing further; settleAwaitingCIScan's CIBackstopTimeout remains the
	// backstop if CI never resolves (ADR-1410 — a confirmed CI failure never
	// consults CIWaitTimeout). Cleared implicitly once HeadSHA advances past
	// it. Empty means "no no-op recorded for the current SHA."
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

// ObservedHead is the head SHA the overseer view reports for the PR: the one
// the engine's own paths recorded, else the head the PR facts were read for
// (an engine without the board probe never records one outside a gate). It
// only reads; nothing the gates consult is written from it (#2122).
func (l *LinkedPRState) ObservedHead() string {
	if l.HeadSHA != "" {
		return l.HeadSHA
	}
	return l.Facts.HeadSHA
}

// AgreedHead is the head SHA a CI verdict may be computed for or shown against.
// It is ObservedHead unless the engine-recorded head and the head the PR facts
// were read for are both known and disagree: then it is "" (unknown), because
// neither side is known to be the newer one, and a verdict for either could be
// a false green for a PR that has moved on (#2122).
func (l *LinkedPRState) AgreedHead() string {
	if l.HeadSHA != "" && l.Facts.HeadSHA != "" && l.HeadSHA != l.Facts.HeadSHA {
		return ""
	}
	return l.ObservedHead()
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
	// BackoffExempt marks a stage whose last invocation ended as a time slice
	// (turn-cap or max_wall_time stop, #2232): the dispatch retry backoff does not
	// apply to it. Rewritten by every StageAttempted; in memory only.
	BackoffExempt map[string]bool
	// PausedByEngine records engine-initiated pauses (vs user-initiated).
	// Replaces engine.pausedDueToRetries[stageKey].
	PausedByEngine map[string]bool
	// PRCreationFailed records that Claude completed but the draft PR could not
	// be created. In-memory only — does not survive restart. When set, the next
	// retry first attempts PR creation before re-invoking Claude.
	PRCreationFailed map[string]bool
	// ReviewCycles counts how many review iterations each stage has completed.
	// Replaces engine.reviewCycleCount[stageKey]. Subject to the #1045
	// ReviewCycleDecremented refund whenever a reinvoke lands no new commit —
	// see ReviewBlockedCycles below for the counter that refund cannot mask.
	ReviewCycles map[string]int
	// ReviewBlockedCycles counts review reinvokes dispatched while the review
	// gate itself was still blocked/timed-out (an authoritative gate with an
	// unresolved verdict, or a plain "nobody has responded" block that has
	// aged past ReviewWaitTimeout). Unlike ReviewCycles, this counter is never
	// refunded — a no-op-on-HEAD reinvoke that happened while the gate was
	// still blocking is genuine non-convergence evidence, not the harmless
	// junk-overview no-op #1045's ReviewCycleDecremented exists to forgive
	// (that shape is dispatched with the gate already clear, so it never
	// increments this counter). handleReviewGate compares
	// max(ReviewCycles, ReviewBlockedCycles) against MaxReviewCycles so a
	// refund-masked loop still terminates in pauseForReviewCycleLimit instead
	// of falling through to pauseForReviewTimeout. See ADR-1518.
	ReviewBlockedCycles map[string]int
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
	// ToolsDeniedRetries counts how many consecutive tool-permission-denial
	// exits (claudeToolsDeniedError) each stage has hit. Bounded by
	// MaxToolsDeniedRetries, independently of Attempts/MaxRetries — the
	// condition is an environmental permission misconfiguration, not a
	// stage failure (#1523), so it must not count against the failure
	// counter, but a non-converging (never-fixed) misconfiguration still
	// needs its own bound. Mirrors SliceRetries structurally.
	ToolsDeniedRetries map[string]int
	// NoOpCommentCycles counts consecutive comment-processing invocations for
	// a stage that produced no observable progress (no new commit, no
	// issue-body update, no FABRIK_STAGE_COMPLETE) — success-agnostic (#1555,
	// sibling of #1382/#1413/#1414): a cleanly-exiting invocation that
	// redelivers and re-"processes" an already-addressed comment as a no-op
	// counts exactly the same as a failing one. Unlike CommentBreaker's
	// rolling 30-minute window (#1089), this counter is never time-pruned —
	// mirrors ReviewBlockedCycles' never-refunded shape (ADR-1518) so a loop
	// whose invocations are spaced sparser than any window (e.g. triggered
	// only by a self-upgrade restart re-admitting a stale review body) still
	// trips it. Reset to zero on genuine progress (see NoOpCommentCycleReset)
	// or by EngineCyclesCleared on a manual unpause.
	NoOpCommentCycles map[string]int
	// DidNotRunReinvokes counts reinvokes (review, CI-fix, rebase) for a stage
	// whose Claude invocation provably never ran — a usage-limit, api_error or
	// apiKeyHelper exit (#1812). The cycle counters are refunded for these, so
	// this never-refunded tally is the only remaining evidence for the
	// cycle-limit pause messages to say "these invocations never ran". Cleared
	// by EngineCyclesCleared, and reset by DidNotRunReinvokesReset whenever a
	// reinvoke stays charged, so it only describes the streak since the last
	// genuine cycle.
	DidNotRunReinvokes map[string]int
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
	// on every invocation. Used only to phrase the stall hint/comment correctly
	// (capped vs. uncapped predecessor) — no longer part of the arming precondition
	// itself (#1767; previously it was, see ADR-1146).
	LastTurnsCapped map[string]bool
	// LastTurnsClean records whether the most recently completed incomplete
	// invocation for a stage stopped cleanly (err == nil or a turn-cap exit,
	// as opposed to a generic error partway through). Overwritten on every
	// invocation, clean or not — a non-clean attempt is recorded with
	// LastTurnsClean=false, which invalidates the arming precondition for the
	// next comparison exactly as LastTurnsCapped=false did for the pre-#1767
	// prevCapped precondition (ADR-1146's "Why must a non-clean attempt still
	// be recorded"). Replaces LastTurnsCapped as the "was the predecessor
	// arm-eligible" signal for stall detection (#1146, #1767): a clean
	// incomplete predecessor is arm-eligible whether or not it was turn-capped.
	LastTurnsClean map[string]bool
	// StallHintPending marks a stage for which a stall was detected and a
	// corrective hint should be injected into the next invocation's prompt. Cleared
	// (consumed) as soon as that invocation is dispatched. In-memory only — does
	// not survive restart (#1146).
	StallHintPending map[string]bool
	// StallEpisodeArmed marks a stage for which a corrective hint has already been
	// armed once during the current retry episode (#1767). Since loosening the
	// arming precondition from "predecessor turn-capped" to "predecessor clean"
	// removed the old mechanism's emergent self-limiting property (the arming
	// attempt is no longer guaranteed to reset the precondition — see ADR-1146
	// Consequences), this is the explicit R4 guard: detectAndArmStallHint checks
	// it before arming and skips if already set, preventing re-arming on every
	// subsequent declining attempt within the same episode. Cleared by
	// StageRetryCleared alongside the other stall-detection fields, at the same
	// episode boundary (stage success or operator unpause).
	StallEpisodeArmed map[string]bool
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
	// StartedAt is the time the worker started running. For a queued worker
	// (Queued) it is the dispatch time; WorkerSlotAcquired re-stamps it to the
	// slot-acquired time (#2121).
	StartedAt time.Time
	// LastSignAt is updated by worker heartbeats; a stale heartbeat implies the worker died.
	LastSignAt time.Time
	// Kind is the worker kind (the WorkerKind* constants); empty = unknown.
	Kind string
	// Trigger names what the worker is addressing (comment or review-body ids,
	// comma separated); empty = none or unknown.
	Trigger string
	// Queued is true while the worker is waiting for a worker slot and holds
	// none (#2121). QueuedSince is when it began waiting.
	Queued      bool
	QueuedSince time.Time
	// TurnsUsed is the live turn count; meaningful only when TurnsKnown.
	TurnsUsed  int
	TurnsKnown bool
	// TurnsMax is the turn budget the current invocation actually runs with (the
	// stage's, a comment review's, or an extended one); 0 = unknown or unlimited.
	// It is set when an invocation starts, so it is known before the first turn.
	TurnsMax int
}

// Worker kinds (#2121). An empty Kind on a stage dispatch's handle means
// WorkerKindStageRun.
const (
	WorkerKindStageRun       = "stage-run"
	WorkerKindCommentReview  = "comment-review"
	WorkerKindReviewReinvoke = "review-reinvoke"
	WorkerKindCIFixReinvoke  = "ci-fix-reinvoke"
	WorkerKindRebaseReinvoke = "rebase-reinvoke"
	WorkerKindTrainConflict  = "merge-train-conflict"
)

// PendingRetry is a retry the engine has scheduled (#2121).
type PendingRetry struct {
	Stage string
	// Cause is the engine's cause for the retry: wall-time-kill, inactivity-kill,
	// max-turns, api-error, ...
	Cause string
	// Due is when the retry becomes dispatchable; zero = unknown.
	Due time.Time
	// At is when the retry was scheduled.
	At time.Time
}

// GateHold is an advance/landing gate holding an item (#2121).
type GateHold struct {
	// Gate is one of feedback, review, ci, merge.
	Gate string
	// Reason is a short engine-worded reason.
	Reason string
	// IDs are the pending feedback identifiers when the gate is the feedback gate.
	IDs []string
	// Since is when the hold was first recorded; ConfirmedAt when the gate last
	// confirmed it.
	Since       time.Time
	ConfirmedAt time.Time
}

// ConflictResolution marks a merge-train conflict resolution for a member item
// (#2121). Queued is true while it waits for a worker slot.
type ConflictResolution struct {
	Queued bool
	Since  time.Time
	// GeneralSlot is true when the running resolution holds a slot of the general
	// (max_concurrent) pool rather than the reserved merge-train slot; health
	// counts it in in_use only then.
	GeneralSlot bool
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
