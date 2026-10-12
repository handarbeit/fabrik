package localapi

import (
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Envelope is the freshness envelope every result carries (R3).
type Envelope struct {
	// AsOf is when the daemon took the snapshot this result describes.
	AsOf time.Time `json:"as_of"`
	// LastPollAttemptAt / LastPollSuccessAt are the daemon's poll-loop
	// timestamps; unknown until a poll has run in this process.
	LastPollAttemptAt Known[time.Time] `json:"last_poll_attempt_at"`
	LastPollSuccessAt Known[time.Time] `json:"last_poll_success_at"`
	// SinceLastPollSuccessSeconds is AsOf minus LastPollSuccessAt.
	SinceLastPollSuccessSeconds Known[int64] `json:"since_last_poll_success_seconds"`
	DaemonUptimeSeconds         int64        `json:"daemon_uptime_seconds"`
}

// ---- status ----

// StatusParams selects one issue: "owner/repo#N", or "N" / "#N" when the
// daemon's cached issues are all in exactly one repo.
type StatusParams struct {
	Issue string `json:"issue"`
}

// StatusResult is the fabrik_status result for one issue.
type StatusResult struct {
	Envelope
	Issue     string         `json:"issue"`
	Title     string         `json:"title"`
	URL       Known[string]  `json:"url"`
	Milestone MilestoneField `json:"milestone"`
	// Cache is how stale the daemon's copy of this item is.
	Cache CacheInfo `json:"cache"`

	Place      Place          `json:"place"`
	Limits     []Limit        `json:"limits"`
	Worker     WorkerInfo     `json:"worker"`
	LastInvoke InvocationInfo `json:"last_invocation"`
	PR         PRInfo         `json:"pr"`
	Blockers   BlockersInfo   `json:"blockers"`
	Attention  AttentionInfo  `json:"attention"`
}

// CacheInfo reports an item's cache age (R3: LastDeepFetchAt per item).
type CacheInfo struct {
	LastDeepFetchAt Known[time.Time] `json:"last_deep_fetch_at"`
	AgeSeconds      Known[int64]     `json:"age_seconds"`
}

// Place is identity-and-place: where the item sits on the board and its labels.
type Place struct {
	Status          Known[string]    `json:"status"`
	StatusEnteredAt Known[time.Time] `json:"status_entered_at"`
	// StatusEnteredBasis says what StatusEnteredAt measures: the daemon resets
	// it on every restart (ADR-1833), so it is "since this process first saw
	// the status", not true time-in-column.
	StatusEnteredBasis string   `json:"status_entered_basis"`
	StageLabels        []string `json:"stage_labels"`
	FabrikLabels       []string `json:"fabrik_labels"`
	// Autonomy is "cruise", "yolo" or "none" (cruise wins when both are set).
	Autonomy string `json:"autonomy"`
}

// Limit is one cycle counter reported as "n of max".
type Limit struct {
	Name string   `json:"name"`
	N    int      `json:"n"`
	Max  LimitMax `json:"max"`
}

// WorkerInfo describes the in-flight worker, if any.
type WorkerInfo struct {
	InFlight   bool             `json:"in_flight"`
	Stage      string           `json:"stage,omitempty"`
	StartedAt  Known[time.Time] `json:"started_at"`
	LastSignAt Known[time.Time] `json:"last_sign_at"`
	// TurnsUsed is always unknown for an in-flight worker: the daemon tracks
	// turns only for the last completed invocation.
	TurnsUsed Known[int] `json:"turns_used"`
	MaxTurns  Known[int] `json:"max_turns"`
	// LastTurnsUsed is the last completed invocation's turn count for the
	// stage, when the daemon has one.
	LastTurnsUsed Known[int] `json:"last_turns_used"`
}

// InvocationInfo is the outcome of the last completed Claude invocation.
type InvocationInfo struct {
	// Outcome is completed, blocked, errored, turn-limited, incomplete or unknown.
	Outcome         string         `json:"outcome"`
	IsComment       bool           `json:"is_comment"`
	DurationSeconds Known[float64] `json:"duration_seconds"`
	Tokens          Known[Tokens]  `json:"tokens"`
}

// Tokens is token consumption for one invocation.
type Tokens struct {
	Input         int     `json:"input"`
	Output        int     `json:"output"`
	CacheCreation int     `json:"cache_creation"`
	CacheRead     int     `json:"cache_read"`
	CostUSD       float64 `json:"cost_usd"`
	TurnsUsed     int     `json:"turns_used"`
	MaxTurns      int     `json:"max_turns"`
}

// PRInfo is the linked PR as the cache holds it.
type PRInfo struct {
	// State is "linked", "none" (the cache has fully read the item and it has
	// no PR) or "unknown" (never deep-fetched).
	State          string        `json:"state"`
	Number         Known[int]    `json:"number"`
	PRState        Known[string] `json:"pr_state"`
	Draft          Known[bool]   `json:"draft"`
	Merged         Known[bool]   `json:"merged"`
	Mergeable      Known[string] `json:"mergeable"`
	MergeableState Known[string] `json:"mergeable_state"`
	// CI is green, red, pending or none, from the cached check runs only.
	CI         Known[string]  `json:"ci"`
	HeadSHA    Known[string]  `json:"head_sha"`
	MergeTrain *TrainPosition `json:"merge_train,omitempty"`
}

// TrainPosition is the item's place in its (repo, base) merge-train partition.
type TrainPosition struct {
	Partition      string `json:"partition"`
	Position       int    `json:"position"`
	Of             int    `json:"of"`
	WorkerInFlight bool   `json:"worker_in_flight"`
	// Basis states how the partition was derived (cached base: label).
	Basis string `json:"basis"`
}

// BlockersInfo is the item's dependencies.
type BlockersInfo struct {
	// State is "known" (the item was deep-fetched) or "unknown".
	State string    `json:"state"`
	Items []Blocker `json:"items"`
}

// Blocker is one BlockedBy entry.
type Blocker struct {
	Issue string        `json:"issue"`
	State Known[string] `json:"state"`
}

// AttentionInfo answers "stuck, or waiting correctly?" (R4).
type AttentionInfo struct {
	State   string       `json:"state"`
	Code    string       `json:"code"`
	Summary string       `json:"summary"`
	Reasons []ReasonInfo `json:"reasons"`
	// LatestFabrikComment is the link to the most recent 🏭 comment; unknown
	// when the item's comments were never cached.
	LatestFabrikComment Known[string] `json:"latest_fabrik_comment"`
	Next                NextInfo      `json:"next"`
	Progress            ProgressInfo  `json:"progress"`
	// StallThresholdSeconds is the threshold the classification used.
	StallThresholdSeconds int64 `json:"stall_threshold_seconds"`
	// Pending describes a suspended item's decision; absent otherwise (#2120).
	Pending *PendingInfo `json:"pending,omitempty"`
	// Options lists how a human can act on a decision-bearing item, each with
	// the MCP tool that performs it when one exists (#2120 R-options-1).
	Options []OptionInfo `json:"options,omitempty"`
}

// OptionInfo is one way a human can act on a decision-bearing item. Tool is
// empty when no MCP tool performs it (the human acts directly, e.g. a merge).
type OptionInfo struct {
	Action string `json:"action"`
	Tool   string `json:"tool,omitempty"`
}

// PendingInfo describes a suspended item (#2120): a completed stage that does
// not auto-advance, or a dependency parked in an unmanaged column.
type PendingInfo struct {
	// Kind is "decision" or "parked-blocker".
	Kind           string `json:"kind"`
	CompletedStage string `json:"completed_stage"`
	// NextStage is the stage an advance would move to (decision kind only).
	NextStage string `json:"next_stage,omitempty"`
	// Since is when the wait began. SinceBasis says where it came from:
	// github-label, stage-comment, eligibility-change (all exact) or
	// first-seen (the daemon's first sight: the wait is at least this long).
	Since      Known[time.Time] `json:"since"`
	SinceBasis string           `json:"since_basis"`
	// AgeSeconds is the wait's age; unknown when Since is only a lower bound,
	// in which case AtLeastAgeSeconds carries that bound.
	AgeSeconds        Known[int64] `json:"age_seconds"`
	AtLeastAgeSeconds Known[int64] `json:"at_least_age_seconds"`
	// Blockers are the parked items (parked-blocker kind only).
	Blockers []PendingBlocker `json:"blockers,omitempty"`
}

// PendingBlocker is one parked item blocking another.
type PendingBlocker struct {
	Issue  string `json:"issue"`
	Column string `json:"column"`
}

// ReasonInfo is one label (or condition) with its one-line meaning.
type ReasonInfo struct {
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}

// NextInfo is what the engine will do next, and when.
type NextInfo struct {
	Action    string           `json:"action"`
	At        Known[time.Time] `json:"at"`
	Deadlines []DeadlineInfo   `json:"deadlines"`
}

// DeadlineInfo is one deadline the engine will act on.
type DeadlineInfo struct {
	Kind  string           `json:"kind"`
	At    Known[time.Time] `json:"at"`
	Basis string           `json:"basis"`
}

// ProgressInfo is the observable-progress anchor.
type ProgressInfo struct {
	At         Known[time.Time] `json:"at"`
	AgeSeconds Known[int64]     `json:"age_seconds"`
}

// ---- board ----

// Board views.
const (
	ViewAttention = "attention"
	ViewFlow      = "flow"
)

// BoardFilter narrows a board view. Zero fields match everything.
type BoardFilter struct {
	// Milestone matches the milestone title (exact), or "none" for items with
	// no milestone. Items whose milestone is unknown never match a milestone
	// filter and are counted in Excluded.UnknownMilestone.
	Milestone string `json:"milestone,omitempty"`
	// Label matches items carrying this exact label.
	Label string `json:"label,omitempty"`
	// Repo matches "owner/repo".
	Repo string `json:"repo,omitempty"`
	// Column matches the board Status exactly.
	Column string `json:"column,omitempty"`
	// HasOpenBlockers, when true, keeps only items with an open blocker.
	HasOpenBlockers bool `json:"has_open_blockers,omitempty"`
}

// BoardParams selects a view and filter. StallThresholdSeconds overrides the
// daemon's default for this request only (0 = daemon default).
type BoardParams struct {
	View                  string       `json:"view"`
	Filter                *BoardFilter `json:"filter,omitempty"`
	StallThresholdSeconds int64        `json:"stall_threshold_seconds,omitempty"`
}

// BoardResult is the fabrik_board result.
type BoardResult struct {
	Envelope
	View                  string      `json:"view"`
	Filter                BoardFilter `json:"filter"`
	StallThresholdSeconds int64       `json:"stall_threshold_seconds"`
	// ItemsScanned is how many cached items were considered before filtering.
	ItemsScanned int `json:"items_scanned"`
	// Excluded counts items a filter could not evaluate because the cache lacks
	// the field (never silently treated as a non-match or a match).
	Excluded ExcludedCounts `json:"excluded"`
	// StatusEnteredBasis: see Place.StatusEnteredBasis.
	StatusEnteredBasis string `json:"status_entered_basis"`

	Attention []AttentionEntry `json:"attention,omitempty"`
	Flow      []ColumnFlow     `json:"flow,omitempty"`
}

// ExcludedCounts reports items excluded for lack of cached data.
type ExcludedCounts struct {
	UnknownMilestone int `json:"unknown_milestone"`
	NotDeepFetched   int `json:"not_deep_fetched"`
}

// AttentionEntry is one item in the attention view.
type AttentionEntry struct {
	Issue      string           `json:"issue"`
	Title      string           `json:"title"`
	Status     Known[string]    `json:"status"`
	Milestone  MilestoneField   `json:"milestone"`
	State      string           `json:"state"`
	Code       string           `json:"code"`
	Reason     string           `json:"reason"`
	Rank       int              `json:"rank"`
	NextAction string           `json:"next_action"`
	NextAt     Known[time.Time] `json:"next_at"`
	Cache      CacheInfo        `json:"cache"`
	// Pending and Options mirror AttentionInfo's, for suspended and other
	// decision-bearing entries (#2120).
	Pending *PendingInfo `json:"pending,omitempty"`
	Options []OptionInfo `json:"options,omitempty"`
}

// ColumnFlow is one board column in the flow view.
type ColumnFlow struct {
	Column string `json:"column"`
	Count  int    `json:"count"`
	// OldestInColumn is the longest-resident item (by daemon-observed
	// StatusEnteredAt); unknown when the column is empty.
	OldestInColumn Known[OldestItem] `json:"oldest_in_column"`
	// InFlightWorkers lists the issues in this column with a worker running.
	InFlightWorkers []string `json:"in_flight_workers"`
}

// OldestItem is the longest-resident item of a column.
type OldestItem struct {
	Issue      string           `json:"issue"`
	Since      Known[time.Time] `json:"since"`
	AgeSeconds Known[int64]     `json:"age_seconds"`
}

// ---- health ----

// HealthParams is empty.
type HealthParams struct{}

// HealthResult is the fabrik_health result.
type HealthResult struct {
	Envelope
	Version string `json:"version"`

	Poll       PollHealth       `json:"poll"`
	Webhook    WebhookHealth    `json:"webhook"`
	Reconcile  ReconcileHealth  `json:"reconcile"`
	Claude     ClaudeHealth     `json:"claude_usage_limit"`
	Backoff    BackoffHealth    `json:"graphql_backoff"`
	Workers    WorkerSlots      `json:"workers"`
	MergeTrain []TrainPartition `json:"merge_train"`
}

// PollHealth describes the poll loop.
type PollHealth struct {
	LastAttemptAt       Known[time.Time] `json:"last_attempt_at"`
	LastSuccessAt       Known[time.Time] `json:"last_success_at"`
	SinceAttemptSeconds Known[int64]     `json:"since_attempt_seconds"`
	SinceSuccessSeconds Known[int64]     `json:"since_success_seconds"`

	// Liveness (#2124, ADR 2124). All additive; the fields above keep their meaning.
	//
	// IntervalSeconds is the configured poll interval (unknown when unset).
	IntervalSeconds Known[int64] `json:"interval_seconds"`
	// InProgress is true while a poll is running. CurrentStartedAt and
	// InProgressSeconds are "unknown" when none is.
	InProgress        bool             `json:"in_progress"`
	CurrentStartedAt  Known[time.Time] `json:"current_started_at"`
	InProgressSeconds Known[int64]     `json:"in_progress_seconds"`
	// LastCompletedAt is when the last poll returned, with or without error.
	LastCompletedAt Known[time.Time] `json:"last_completed_at"`
	// Step is what the in-progress poll is doing; unknown when no poll is
	// running or the engine does not know.
	Step Known[string] `json:"step"`
	// NextDueAt is when the engine itself expects the next poll (it includes
	// deliberate back-off). BackingOff/BackoffReason say when it is later than
	// the configured interval: "idle", "graphql" or "rest".
	NextDueAt     Known[time.Time] `json:"next_due_at"`
	BackingOff    bool             `json:"backing_off"`
	BackoffReason string           `json:"backoff_reason,omitempty"`
	// Overdue is the verdict: true when a poll has been in progress for longer
	// than OverdueAfterSeconds (K x interval) or none started within that long
	// of NextDueAt. Unknown, never false, when the interval or baseline is unknown.
	Overdue             Known[bool]  `json:"overdue"`
	OverdueAfterSeconds Known[int64] `json:"overdue_after_seconds"`
	// OverdueCause is "poll-in-progress" or "no-poll-started"; BlockedSeconds is
	// how long the poll has been in progress or absent.
	OverdueCause   string       `json:"overdue_cause,omitempty"`
	BlockedSeconds Known[int64] `json:"blocked_seconds"`
}

// WebhookHealth describes webhook ingestion.
type WebhookHealth struct {
	// Mode is "off", "healthy" or "unhealthy" (or "unknown" with no manager info).
	Mode              string           `json:"mode"`
	LastEventAt       Known[time.Time] `json:"last_event_at"`
	SinceEventSeconds Known[int64]     `json:"since_event_seconds"`
}

// ReconcileHealth describes the periodic board reconcile.
type ReconcileHealth struct {
	LastSuccessAt       Known[time.Time] `json:"last_success_at"`
	SinceSuccessSeconds Known[int64]     `json:"since_success_seconds"`
}

// ClaudeHealth is the account-wide usage-limit hold.
type ClaudeHealth struct {
	UsageLimited      bool             `json:"usage_limited"`
	UsageLimitedUntil Known[time.Time] `json:"usage_limited_until"`
}

// BackoffHealth mirrors the GraphQL rate-limit backoff state.
type BackoffHealth struct {
	// Observed is false until a poll has reported a rate-limit reading.
	Observed   bool           `json:"observed"`
	Remaining  Known[int]     `json:"remaining"`
	Low        Known[bool]    `json:"low"`
	RestPaused Known[bool]    `json:"rest_paused"`
	Ratio      Known[float64] `json:"ratio"`
	// AsOf is when the poll last refreshed Remaining/Low/Ratio (#2124); unknown
	// until the first refresh, never "now". RestPausedAsOf is the same for
	// RestPaused. Neither moves with the passage of time alone.
	AsOf           Known[time.Time] `json:"as_of"`
	RestPausedAsOf Known[time.Time] `json:"rest_paused_as_of"`
}

// WorkerSlots is worker concurrency.
type WorkerSlots struct {
	InUse         int        `json:"in_use"`
	MaxConcurrent Known[int] `json:"max_concurrent"`
	// TrainInUse / TrainMax describe the reserved merge-train slot (#2115, ADR 2115),
	// which sits on top of MaxConcurrent and is not counted in InUse. Both are a
	// known zero when merge_train is off.
	TrainInUse Known[int] `json:"train_in_use"`
	TrainMax   Known[int] `json:"train_max"`
}

// TrainPartition is one (repo, base) merge-train partition.
type TrainPartition struct {
	Repo           string   `json:"repo"`
	Base           string   `json:"base"`
	Members        []string `json:"members"`
	WorkerInFlight bool     `json:"worker_in_flight"`

	// Liveness (#2124). OldestQueuedSince is daemon-observed (it restarts with
	// the daemon). RunActive is true while an open run or a worker exists;
	// Phase is its durable phase ("trial-ci", "bisect", "one-at-a-time",
	// "landing") or "none". IdleReason is set only for queued members with no
	// active run: "poll-overdue", "infra-abandon-cooldown" or "unknown".
	OldestQueuedSince   Known[time.Time] `json:"oldest_queued_since"`
	OldestQueuedSeconds Known[int64]     `json:"oldest_queued_seconds"`
	RunActive           bool             `json:"run_active"`
	Phase               string           `json:"phase"`
	PhaseSince          Known[time.Time] `json:"phase_since"`
	IdleReason          string           `json:"idle_reason,omitempty"`
	IdleDetail          string           `json:"idle_detail,omitempty"`
	// Stalled is true while the partition meets the stall condition (the one that raises pipeline-stalled).
	Stalled bool `json:"stalled"`
}

// ---- streaming (protocol v2, #1968) ----

// SubscribeParams registers a subscription. Subscriber is the stable name the
// held queue is keyed on. Issues are "owner/repo#N", or a bare "N" when the
// daemon's cached issues are all in exactly one repo (the same resolver as
// status). ExcludeLabels
// distinguishes absent (nil: the default churn exclusions) from an explicit
// empty list (opt into every label).
type SubscribeParams struct {
	Subscriber    string    `json:"subscriber"`
	Repos         []string  `json:"repos,omitempty"`
	Issues        []string  `json:"issues,omitempty"`
	Milestone     string    `json:"milestone,omitempty"`
	Labels        []string  `json:"labels,omitempty"`
	ExcludeLabels *[]string `json:"exclude_labels,omitempty"`
	Events        []string  `json:"events,omitempty"`
	DigestSeconds int       `json:"digest_seconds,omitempty"`
	// ProgressViaColumn drops stage:*:complete / stage:*:in_progress label
	// events when column-changed is also taken (#2123).
	ProgressViaColumn bool `json:"progress_via_column,omitempty"`
	// RemindAfterSeconds opts into reminders for a still-pending
	// decision-pending episode (#2120 R6a); 0 = never.
	RemindAfterSeconds int `json:"remind_after_seconds,omitempty"`
}

// EventMapping reports a requested event type the daemon rewrote to its current
// name (a rename the caller's build predates).
type EventMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SubscribeResult echoes the stored subscription and everything the subscriber
// now has. Mapped and Rejected (#2168) are additive and omitted when empty: the
// event types of the request that were renamed (and to what), and those the
// daemon does not know and therefore did not subscribe to.
type SubscribeResult struct {
	Subscription  channelevents.Subscription   `json:"subscription"`
	Subscriptions []channelevents.Subscription `json:"subscriptions"`
	Mapped        []EventMapping               `json:"mapped,omitempty"`
	Rejected      []string                     `json:"rejected,omitempty"`
	// Notes are advisories about the request, e.g. label filters given without
	// label events being in effect (#2169).
	Notes []string `json:"notes,omitempty"`
}

// UnsubscribeParams removes one subscription by id, or all of a subscriber's
// when ID is empty.
type UnsubscribeParams struct {
	Subscriber string `json:"subscriber"`
	ID         string `json:"id,omitempty"`
}

// UnsubscribeResult reports how many subscriptions were removed.
type UnsubscribeResult struct {
	Removed       int                          `json:"removed"`
	Subscriptions []channelevents.Subscription `json:"subscriptions"`
}

// AttachParams binds the connection as the live session of a subscriber.
type AttachParams struct {
	Subscriber string `json:"subscriber"`
	// CatchUp asks for a snapshot of what is waiting on a human right now (items
	// settled at Validate, needs-human, escalated), tagged catch_up=true. The
	// client sets it on the first attach of a session only, never on a reconnect.
	// After a daemon restart the daemon queues one to every subscriber itself
	// once its state is ready (#2169), whatever this says.
	CatchUp bool `json:"catch_up,omitempty"`
}

// AttachResult acknowledges an attach and tells the client the heartbeat
// cadence so it can size its dead-peer watchdog.
type AttachResult struct {
	Subscriber        string `json:"subscriber"`
	HeartbeatMillis   int64  `json:"heartbeat_ms"`
	IdleTimeoutMillis int64  `json:"idle_timeout_ms"`
}

// ---- actions (protocol v2 additive, #1969) ----

// PromoteParams moves an item out of an unmanaged parking column into the
// pipeline column To. Subscriber is the requester's name (audit trail).
type PromoteParams struct {
	Subscriber string `json:"subscriber"`
	Issue      string `json:"issue"`
	To         string `json:"to"`
}

// SetAutonomyParams sets an item's autonomy to exactly Mode: "cruise", "yolo"
// or "none".
type SetAutonomyParams struct {
	Subscriber string `json:"subscriber"`
	Issue      string `json:"issue"`
	Mode       string `json:"mode"`
}

// RevalidateParams applies fabrik:revalidate to a Validate-column item.
type RevalidateParams struct {
	Subscriber string `json:"subscriber"`
	Issue      string `json:"issue"`
}

// AdvanceParams moves an item whose current stage is complete exactly one
// configured stage forward (#2126). There is no target: the engine's own
// advance decides the next stage.
type AdvanceParams struct {
	Subscriber string `json:"subscriber"`
	Issue      string `json:"issue"`
}

// MoveParkedParams moves an item from one unmanaged parking column to another
// (#2188). It starts no work and never touches a pipeline column.
type MoveParkedParams struct {
	Subscriber string `json:"subscriber"`
	Issue      string `json:"issue"`
	To         string `json:"to"`
}

// ClearClaudeLimitParams has no issue: the daemon picks the carrier item.
type ClearClaudeLimitParams struct {
	Subscriber string `json:"subscriber"`
}

// Audit-comment outcomes in ActionResult.AuditComment.
const (
	AuditPosted = "posted"
	AuditFailed = "failed"
	// AuditNone means no write happened (the item was already in the requested
	// state), so no comment was due.
	AuditNone = "none"
)

// ActionResult reports what an action did.
type ActionResult struct {
	Action string `json:"action"`
	Issue  string `json:"issue"`
	// Changed is false when the item was already in the requested state.
	Changed bool   `json:"changed"`
	Summary string `json:"summary"`
	// Writes lists the GitHub-visible writes performed, in order.
	Writes       []string `json:"writes,omitempty"`
	AuditComment string   `json:"audit_comment"`
	// Notes carry caveats: will auto-advance, yolo may auto-merge, deferred
	// while a worker is in flight, which item carried a clear-claude-limit.
	Notes []string `json:"notes,omitempty"`
}

// RefusalState is the Error.Data payload of a refused action: the item's
// current state as the daemon sees it.
type RefusalState struct {
	Issue  string `json:"issue,omitempty"`
	Status string `json:"status,omitempty"`
	// Labels are the item's fabrik-relevant labels.
	Labels []string `json:"labels,omitempty"`
	// ValidTargets lists the columns the requested action could name (promote:
	// pipeline columns; move_parked: other parking columns). It is set only when
	// the item is in a parking column; a refusal because the item is not parked
	// omits it, since no move is possible.
	ValidTargets []string `json:"valid_targets,omitempty"`
	Autonomy     string   `json:"autonomy,omitempty"`
}
